// Command fakeclaude stands in for Claude Code in tests. It takes the flags the Claude adapter passes (and, when
// $FAKECLAUDE_ARGV_LOG is set, appends its argv there as one JSON line per launch, and likewise its environment
// to $FAKECLAUDE_ENV_LOG), runs the
// hooks from its --settings the way Claude Code does (a shell command with the event's JSON on stdin), and acts
// out turns from lines typed into its terminal:
//
//	any text      UserPromptSubmit, PreToolUse, PostToolUse, Stop
//	/ask          UserPromptSubmit, then Notification (permission_prompt) and waits for a line; then
//	              PostToolUse and Stop
//	/clear        SessionEnd (clear), then SessionStart (clear) under a new session id
//	/compact <focus>
//	              PreCompact, SubagentStop, then SessionStart (compact) under the same session id, and a
//	              compact_boundary line appended to the session's transcript under $HOME/.claude/projects; with
//	              $FAKECLAUDE_COMPACT=none it only prints "Not enough messages" and fires nothing
//	/hold         UserPromptSubmit and PreToolUse, then waits for a line (a long turn); then PostToolUse and
//	              Stop
//	/exit         SessionEnd, then exits 0
//	/crash        exits 7 without any hook
//	/limit <text> UserPromptSubmit, then StopFailure (rate_limit, <text> as the message): a usage limit
//	/ratelimits <json>
//	              invokes settings.statusLine.command with rate_limits JSON on stdin
//	/bash <cmd>   UserPromptSubmit, PreToolUse (Bash, <cmd>), then PostToolUse and Stop; "denied: <reason>" is
//	              printed instead of the tool's, when a PreToolUse hook printed a permission denial
//
// With $FAKECLAUDE_INPUT_LOG set, every line typed into it is appended there, one per line.
//
// With $FAKECLAUDE_COMPACT_LOG set, the focus of every typed /compact is appended there, one per line.
//
// With $FAKECLAUDE_HANG_FILE naming a file that exists, a fresh start (not a resume) draws nothing and fires no hook
// for an hour: a launch too slow to reach its prompt.
//
// With $FAKECLAUDE_CLEAR_CRASH_FILE naming a file that exists, /clear exits 7 before any hook.
//
// With $FAKECLAUDE_FAIL_FILE naming a file that exists, a fresh start (not a resume) exits 3 before anything is
// drawn or reported: a launch that fails.
//
// With -p (print mode: the last argument is the prompt) it runs no hooks and reads no input: a prompt that starts
// with /compact appends the compact_boundary line and exits 0 (1 with $FAKECLAUDE_PRINT_FAIL set), as a headless
// compaction does.
//
// With $FAKECLAUDE_TRUST_DIALOG set it draws the folder-trust dialog and fires no hook until a line is typed,
// unless $HOME/.claude.json trusts the folder or a parent (`projects[<path>].hasTrustDialogAccepted`).
//
// With $FAKECLAUDE_PLACEHOLDER set it draws Claude Code's empty-box placeholder after its prompt, as the real one
// does at a fresh pane: text that looks like a draft to anything reading the screen.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type settings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
	StatusLine *struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	} `json:"statusLine"`
}

var (
	session string
	conf    settings
	cwd, _  = os.Getwd()
)

func main() {
	args := os.Args[1:]
	if log := os.Getenv("FAKECLAUDE_ARGV_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			line, _ := json.Marshal(args)
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	if log := os.Getenv("FAKECLAUDE_ENV_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			line, _ := json.Marshal(os.Environ())
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	source, printMode := "startup", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			printMode = true
		case "--session-id":
			i++
			session = args[i]
		case "--resume":
			i++
			session, source = args[i], "resume"
		case "--settings":
			i++
			if err := json.Unmarshal([]byte(args[i]), &conf); err != nil {
				fmt.Fprintf(os.Stderr, "fakeclaude: bad --settings: %v\n", err)
				os.Exit(2)
			}
		case "--model", "--effort":
			i++
		case "--disallowedTools":
			// Several values, up to the next flag.
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		}
	}
	if printMode {
		prompt := args[len(args)-1]
		if strings.HasPrefix(prompt, "/compact") {
			boundary()
			fmt.Printf("compacted: %s\n", prompt)
		}
		if os.Getenv("FAKECLAUDE_PRINT_FAIL") != "" {
			fmt.Fprintln(os.Stderr, "fakeclaude: print mode failed")
			os.Exit(1)
		}
		return
	}
	if file := os.Getenv("FAKECLAUDE_FAIL_FILE"); file != "" && source == "startup" {
		if _, err := os.Stat(file); err == nil {
			os.Exit(3)
		}
	}
	if file := os.Getenv("FAKECLAUDE_HANG_FILE"); file != "" && source == "startup" {
		if _, err := os.Stat(file); err == nil {
			time.Sleep(time.Hour) // a fresh start that never reaches its prompt
		}
	}
	fmt.Printf("fake claude %s\r\n", session)
	if os.Getenv("FAKECLAUDE_TRUST_DIALOG") != "" && !trusted() {
		fmt.Print("Quick safety check: Is this a project you created or one you trust?\r\n" +
			"❯ 1. No, exit\r\n  2. Yes, I trust this folder\r\n")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	hook("SessionStart", map[string]any{"source": source})
	prompt()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(in.Text()))
		if log := os.Getenv("FAKECLAUDE_INPUT_LOG"); log != "" && line != "" {
			if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.WriteString(line + "\n")
				f.Close()
			}
		}
		switch {
		case strings.HasPrefix(line, "/limit "):
			hook("UserPromptSubmit", map[string]any{"prompt": line})
			hook("StopFailure", map[string]any{"error": "rate_limit", "last_assistant_message": strings.TrimPrefix(line, "/limit ")})
			fmt.Print("limit\r\n")
			prompt()
			continue
		case strings.HasPrefix(line, "/ratelimits "):
			if conf.StatusLine != nil && conf.StatusLine.Command != "" {
				raw := strings.TrimSpace(strings.TrimPrefix(line, "/ratelimits "))
				if !strings.Contains(raw, `"rate_limits"`) {
					raw = fmt.Sprintf(`{"session_id":%q,"rate_limits":%s}`, session, raw)
				}
				cmd := exec.Command("sh", "-c", conf.StatusLine.Command)
				cmd.Stdin = strings.NewReader(raw)
				out, err := cmd.Output()
				if err != nil {
					fmt.Fprintf(os.Stderr, "fakeclaude: statusline: %v\r\n", err)
				} else if len(out) > 0 {
					fmt.Printf("%s\r\n", strings.TrimSpace(string(out)))
				}
			}
			prompt()
			continue
		case strings.HasPrefix(line, "/bash "):
			hook("UserPromptSubmit", map[string]any{"prompt": line})
			out := hook("PreToolUse", map[string]any{"tool_name": "Bash",
				"tool_input": map[string]any{"command": strings.TrimPrefix(line, "/bash ")}})
			var d struct {
				H struct {
					Decision string `json:"permissionDecision"`
					Reason   string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if json.Unmarshal(out, &d) == nil && d.H.Decision == "deny" {
				fmt.Printf("denied: %s\r\n", d.H.Reason)
			} else {
				fmt.Print("ran\r\n")
				hook("PostToolUse", map[string]any{"tool_name": "Bash"})
			}
			hook("Stop", map[string]any{"stop_hook_active": false})
			prompt()
			continue
		}
		if strings.HasPrefix(line, "/compact ") {
			compact(strings.TrimPrefix(line, "/compact "))
			prompt()
			continue
		}
		switch line {
		case "":
		case "/exit":
			hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
			fmt.Print("bye\r\n")
			return
		case "/crash":
			os.Exit(7)
		case "/compact":
			compact("")
		case "/clear":
			if file := os.Getenv("FAKECLAUDE_CLEAR_CRASH_FILE"); file != "" {
				if _, err := os.Stat(file); err == nil {
					os.Exit(7) // dies on /clear, before any hook
				}
			}
			hook("SessionEnd", map[string]any{"reason": "clear"})
			session += "-cleared"
			hook("SessionStart", map[string]any{"source": "clear"})
		case "/hold":
			hook("UserPromptSubmit", map[string]any{"prompt": line})
			hook("PreToolUse", map[string]any{"tool_name": "Bash"})
			fmt.Print("holding\r\n")
			in.Scan()
			hook("PostToolUse", map[string]any{"tool_name": "Bash"})
			hook("Stop", map[string]any{"stop_hook_active": false})
		case "/ask":
			hook("UserPromptSubmit", map[string]any{"prompt": line})
			hook("Notification", map[string]any{"notification_type": "permission_prompt",
				"message": "Claude needs your permission to use Bash"})
			fmt.Print("allow Bash? \r\n")
			in.Scan()
			hook("PostToolUse", map[string]any{"tool_name": "Bash"})
			hook("Stop", map[string]any{"stop_hook_active": false})
		default:
			hook("UserPromptSubmit", map[string]any{"prompt": line})
			fmt.Printf("working on: %s\r\n", line)
			hook("PreToolUse", map[string]any{"tool_name": "Bash"})
			hook("PostToolUse", map[string]any{"tool_name": "Bash"})
			fmt.Printf("done: %s\r\n", line)
			hook("Stop", map[string]any{"stop_hook_active": false})
		}
		prompt()
	}
}

func prompt() {
	fmt.Print("> ")
	if os.Getenv("FAKECLAUDE_PLACEHOLDER") != "" {
		// Dim text, then the cursor back to the start of it, as the TUI draws a placeholder.
		const hint = `Try "fix lint errors"`
		fmt.Printf("\x1b[2m%s\x1b[22m\x1b[%dD", hint, len(hint))
	}
}

// trusted reports whether $HOME/.claude.json trusts the working folder or a parent of it.
func trusted() bool {
	b, err := os.ReadFile(os.Getenv("HOME") + "/.claude.json")
	if err != nil {
		return false
	}
	var c struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &c) != nil {
		return false
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if c.Projects[dir].Trusted {
			return true
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}

// hook runs every command registered for an event, with its JSON on stdin, as Claude Code does, and returns what
// they printed.
func hook(event string, fields map[string]any) []byte {
	var printed bytes.Buffer
	body := map[string]any{"session_id": session, "hook_event_name": event, "cwd": cwd,
		"transcript_path": "/dev/null"}
	for k, v := range fields {
		body[k] = v
	}
	payload, _ := json.Marshal(body)
	for _, group := range conf.Hooks[event] {
		for _, h := range group.Hooks {
			cmd := exec.Command("sh", "-c", h.Command)
			cmd.Stdin = bytes.NewReader(payload)
			cmd.Stdout, cmd.Stderr = &printed, os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "fakeclaude: hook %s: %v\r\n", event, err)
			}
		}
	}
	os.Stderr.Write(printed.Bytes())
	return printed.Bytes()
}

// compact acts out a `/compact <focus>`: the hooks Claude Code 2.1.332 fires for one, and the transcript's
// boundary line.
func compact(focus string) {
	if os.Getenv("FAKECLAUDE_COMPACT") == "none" {
		fmt.Print("Not enough messages to compact\r\n")
		return
	}
	fmt.Printf("compacting: %s\r\n", focus)
	if log := os.Getenv("FAKECLAUDE_COMPACT_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(focus + "\n")
			f.Close()
		}
	}
	hook("PreCompact", map[string]any{"trigger": "manual"})
	boundary()
	hook("SubagentStop", map[string]any{})
	hook("SessionStart", map[string]any{"source": "compact"})
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// boundary appends a compact_boundary line to the session's transcript, as Claude Code does, when the transcript
// exists (a test that sizes a conversation writes one).
func boundary() {
	home := os.Getenv("HOME")
	if home == "" || session == "" {
		return
	}
	path := filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(cwd, "-"), session+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(`{"type":"system","subtype":"compact_boundary"}` + "\n")
}
