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
	"strings"
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
	source := "startup"
	for i := 0; i < len(args); i++ {
		switch args[i] {
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
		switch line {
		case "":
		case "/exit":
			hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
			fmt.Print("bye\r\n")
			return
		case "/crash":
			os.Exit(7)
		case "/clear":
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
