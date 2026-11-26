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
	hook("SessionStart", map[string]any{"source": source})
	prompt()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(in.Text()))
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

// hook runs every command registered for an event, with its JSON on stdin, as Claude Code does.
func hook(event string, fields map[string]any) {
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
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "fakeclaude: hook %s: %v\r\n", event, err)
			}
		}
	}
}
