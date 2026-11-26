// Command fakeagy stands in for Antigravity CLI (`agy`) in tests. It supports `agy models`, logs argv
// to $FAKEAGY_ARGV_LOG, checks `--settings` trustedWorkspaces, fires NO hooks at PTY startup, fires
// SessionStart only on the first prompt of a fresh conversation (never on `--conversation` resume), runs
// `.agents/hooks.json` with fail-closed `PreToolUse` semantics, and acts out turns from lines typed into its
// terminal ($FAKEAGY_START_DELAY pauses before first output; $FAKEAGY_DRAW_FOR keeps drawing that long at start-up):
//
//	any text      SessionStart (first fresh turn only), PreInvocation, PreToolUse (read_file), PostToolUse, PostInvocation, Stop
//	/edit         PreInvocation, PreToolUse (write_file on blocked.txt; creates file only if allowed), PostInvocation, Stop
//	/push         PreInvocation, PreToolUse (run_command "git push origin HEAD"), PostInvocation, Stop
//	/hold         PreInvocation, PreToolUse (read_file), waits for a line, then PostToolUse, PostInvocation, Stop
//	/limit        PreInvocation, prints RESOURCE_EXHAUSTED (code 429), Stop (terminationReason: "ERROR")
//	/error        PreInvocation, Stop (terminationReason: "ERROR", error: "internal error")
//	/exit         exits 0
//	/crash        exits 7
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroup struct {
	Matcher string    `json:"matcher"`
	Hooks   []hookCmd `json:"hooks"`
}

type hookSpec struct {
	Enabled        *bool       `json:"enabled"`
	SessionStart   []hookCmd   `json:"SessionStart"`
	PreInvocation  []hookCmd   `json:"PreInvocation"`
	PostInvocation []hookCmd   `json:"PostInvocation"`
	PreToolUse     []hookGroup `json:"PreToolUse"`
	PostToolUse    []hookGroup `json:"PostToolUse"`
	Stop           []hookCmd   `json:"Stop"`
}

var (
	convID   string
	resumed  bool
	model    string
	cwd, _   = os.Getwd()
	hooksMap map[string]hookSpec
	invNum   int
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "models" {
		fmt.Print("gemini-4-neon-high\tGemini 4 Neon (High)\n" +
			"gemini-4-neon-low\tGemini 4 Neon (Low)\n" +
			"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n" +
			"gemini-4-carbon-high\tGemini 4 Carbon (High)\n" +
			"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
			"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n")
		return
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-p" && args[i+1] == "/usage" {
			if p := os.Getenv("FAKEAGY_USAGE_FILE"); p != "" {
				b, _ := os.ReadFile(p)
				fmt.Print(string(b))
			} else {
				fmt.Print(os.Getenv("FAKEAGY_USAGE"))
			}
			return
		}
	}
	if log := os.Getenv("FAKEAGY_ARGV_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			line, _ := json.Marshal(args)
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	var settingsJSON string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--conversation", "--resume", "-r":
			i++
			convID = args[i]
			resumed = true
		case "--model":
			i++
			model = args[i]
		case "--settings":
			i++
			settingsJSON = args[i]
		case "--effort":
			fmt.Fprintln(os.Stderr, "error: invalid model selection: --effort is not supported")
			os.Exit(1)
		}
	}
	if convID == "" {
		convID = newUUID()
	}
	loadHooks()

	in := bufio.NewScanner(os.Stdin)
	if !isTrusted(settingsJSON) {
		fmt.Printf("Do you trust the contents of this project?\r\n> Yes, I trust this folder\r\n")
		if !in.Scan() {
			return
		}
	}

	if d, err := time.ParseDuration(os.Getenv("FAKEAGY_START_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
	fmt.Printf("fake agy %s (%s)\r\n", convID, model)
	if d, err := time.ParseDuration(os.Getenv("FAKEAGY_DRAW_FOR")); err == nil {
		// A TUI still drawing at start-up: the daemon must not mark it idle until it is quiet.
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			fmt.Print(".")
		}
	}
	prompt()

	firstTurn := true
	for in.Scan() {
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(in.Text()))
		if line == "" {
			prompt()
			continue
		}
		if line == "/exit" {
			fmt.Print("bye\r\n")
			return
		}
		if line == "/crash" {
			os.Exit(7)
		}
		if firstTurn && !resumed {
			runHooks("SessionStart", map[string]any{"lastUserInput": line})
		}
		firstTurn = false
		invNum++
		runHooks("PreInvocation", map[string]any{"invocationNum": invNum, "lastUserInput": line})

		switch line {
		case "/hold":
			if allowTool("read_file", map[string]any{"Path": filepath.Join(cwd, "README.md")}, line) {
				fmt.Print("holding\r\n")
				in.Scan()
				runHooks("PostToolUse", map[string]any{"toolCall": map[string]any{"name": "read_file"}, "lastUserInput": line})
			}
			runHooks("PostInvocation", map[string]any{"invocationNum": invNum, "lastUserInput": line})
			runHooks("Stop", map[string]any{"terminationReason": "NO_TOOL_CALL", "error": "", "lastUserInput": line})
		case "/edit":
			if allowTool("write_file", map[string]any{"Path": "blocked.txt", "Content": "hi"}, line) {
				os.WriteFile(filepath.Join(cwd, "blocked.txt"), []byte("hi"), 0o600)
				runHooks("PostToolUse", map[string]any{"toolCall": map[string]any{"name": "write_file"}, "lastUserInput": line})
				fmt.Print("wrote blocked.txt\r\n")
			} else {
				fmt.Print("write_file denied\r\n")
			}
			runHooks("PostInvocation", map[string]any{"invocationNum": invNum, "lastUserInput": line})
			runHooks("Stop", map[string]any{"terminationReason": "NO_TOOL_CALL", "error": "", "lastUserInput": line})
		case "/push":
			if allowTool("run_command", map[string]any{"CommandLine": "git push origin HEAD"}, line) {
				runHooks("PostToolUse", map[string]any{"toolCall": map[string]any{"name": "run_command"}, "lastUserInput": line})
				fmt.Print("pushed\r\n")
			} else {
				fmt.Print("git push denied\r\n")
			}
			runHooks("PostInvocation", map[string]any{"invocationNum": invNum, "lastUserInput": line})
			runHooks("Stop", map[string]any{"terminationReason": "NO_TOOL_CALL", "error": "", "lastUserInput": line})
		case "/limit":
			errText := "RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 2h."
			fmt.Printf("⚠ %s\r\n", errText)
			runHooks("Stop", map[string]any{"terminationReason": "ERROR", "error": errText, "lastUserInput": line})
		case "/error":
			fmt.Print("error: internal failure\r\n")
			runHooks("Stop", map[string]any{"terminationReason": "ERROR", "error": "internal error", "lastUserInput": line})
		default:
			if allowTool("read_file", map[string]any{"Path": filepath.Join(cwd, "README.md")}, line) {
				runHooks("PostToolUse", map[string]any{"toolCall": map[string]any{"name": "read_file"}, "lastUserInput": line})
			}
			runHooks("PostInvocation", map[string]any{"invocationNum": invNum, "lastUserInput": line})
			fmt.Printf("done: %s\r\n", line)
			runHooks("Stop", map[string]any{"terminationReason": "NO_TOOL_CALL", "error": "", "lastUserInput": line})
		}
		prompt()
	}
}

func prompt() {
	fmt.Print("\r\n> ")
	if os.Getenv("FAKEAGY_PLACEHOLDER") != "" {
		const hint = `Auto mode: edits & safe commands auto-approved (shift+tab to cycle)`
		fmt.Printf("\x1b[2m%s\x1b[22m\x1b[%dD", hint, len(hint))
	}
}

func isTrusted(settingsJSON string) bool {
	if settingsJSON == "" {
		return false
	}
	var cfg struct {
		TrustedWorkspaces []string `json:"trustedWorkspaces"`
	}
	if json.Unmarshal([]byte(settingsJSON), &cfg) != nil {
		return false
	}
	realCwd, _ := filepath.EvalSymlinks(cwd)
	for _, w := range cfg.TrustedWorkspaces {
		rw, _ := filepath.EvalSymlinks(w)
		if w == cwd || (realCwd != "" && rw == realCwd) {
			return true
		}
	}
	return false
}

func loadHooks() {
	b, err := os.ReadFile(filepath.Join(cwd, ".agents", "hooks.json"))
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &hooksMap)
}

func runHooks(event string, extra map[string]any) {
	_ = execEvent(event, extra, false)
}

func allowTool(tool string, args map[string]any, lastInput string) bool {
	return execEvent("PreToolUse", map[string]any{
		"toolCall":      map[string]any{"name": tool, "args": args},
		"lastUserInput": lastInput,
	}, true)
}

func execEvent(event string, extra map[string]any, failClosed bool) bool {
	body := map[string]any{
		"conversationId": convID,
		"cwd":            cwd,
		"hookEventName":  event,
		"modelName":      model,
	}
	for k, v := range extra {
		body[k] = v
	}
	payload, _ := json.Marshal(body)
	agentsDir := filepath.Join(cwd, ".agents")
	for _, spec := range hooksMap {
		if spec.Enabled != nil && !*spec.Enabled {
			continue
		}
		var cmds []hookCmd
		switch event {
		case "SessionStart":
			cmds = spec.SessionStart
		case "PreInvocation":
			cmds = spec.PreInvocation
		case "PostInvocation":
			cmds = spec.PostInvocation
		case "Stop":
			cmds = spec.Stop
		case "PreToolUse":
			toolName := extractToolName(extra)
			for _, g := range spec.PreToolUse {
				if matcherMatches(g.Matcher, toolName) {
					cmds = append(cmds, g.Hooks...)
				}
			}
		case "PostToolUse":
			toolName := extractToolName(extra)
			for _, g := range spec.PostToolUse {
				if matcherMatches(g.Matcher, toolName) {
					cmds = append(cmds, g.Hooks...)
				}
			}
		}
		for _, h := range cmds {
			to := 30 * time.Second
			if h.Timeout > 0 {
				to = time.Duration(h.Timeout) * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), to)
			cmd := exec.CommandContext(ctx, "sh", "-c", h.Command)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if cmd.Process != nil {
					return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
				return nil
			}
			cmd.Dir = agentsDir
			cmd.Stdin = bytes.NewReader(payload)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			cancel()
			if failClosed {
				if err != nil {
					return false
				}
				out := bytes.TrimSpace(stdout.Bytes())
				if len(out) > 0 {
					var dec struct {
						Decision string `json:"decision"`
					}
					if json.Unmarshal(out, &dec) != nil || dec.Decision == "deny" {
						return false
					}
				}
			}
		}
	}
	return true
}

func extractToolName(extra map[string]any) string {
	if tc, ok := extra["toolCall"].(map[string]any); ok {
		name, _ := tc["name"].(string)
		return name
	}
	return ""
}

func matcherMatches(matcher, toolName string) bool {
	if matcher == "" || matcher == "*" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	if err != nil {
		return false
	}
	return re.MatchString(toolName)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
