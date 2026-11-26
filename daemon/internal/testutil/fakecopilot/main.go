// Command fakecopilot stands in for the GitHub Copilot CLI (`copilot`) in tests. It logs launches to
// $FAKECOPILOT_ARGV_LOG, enforces `--deny-tool` rules, draws Copilot's gutter input box (`╻▄▄` / `┃` / `╹▀▀`, with
// dim placeholder text when $FAKECOPILOT_PLACEHOLDER is set), optionally draws the folder-trust dialog first
// ($FAKECOPILOT_TRUST), writes Copilot's events file ($COPILOT_HOME/session-state/<id>/events.jsonl) and acts out
// turns from lines typed into its terminal:
//
//	any text   user.message, a turn, a final answer
//	/hold      a user message and an open turn, until the next line, which completes it
//	/402       session.error with Copilot's 402 quota error
//	/policy    session.error with the 403 policy refusal
//	/limit     prints a rate-limit line on the screen, writes no turn
//	/edit      writes blocked.txt unless `write` is denied
//	/push      prints "pushed" unless `shell(git push:*)` is denied
//	/exit      exits 0
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	sessionID string
	model     string
	deny      []string
	events    *os.File
	cwd, _    = os.Getwd()
)

func main() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session-id" && i+1 < len(args):
			i++
			sessionID = args[i]
		case a == "--model" && i+1 < len(args):
			i++
			model = args[i]
		case strings.HasPrefix(a, "--deny-tool="):
			deny = append(deny, strings.TrimPrefix(a, "--deny-tool="))
		case a == "--deny-tool":
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				deny = append(deny, args[i])
			}
		}
	}
	if p := os.Getenv("FAKECOPILOT_ARGV_LOG"); p != "" {
		rec := map[string]any{"argv": args, "session": sessionID, "model": model, "deny": deny, "cwd": cwd,
			"home": os.Getenv("COPILOT_HOME"), "token": os.Getenv("COPILOT_GITHUB_TOKEN")}
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(rec)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if sessionID == "" {
		sessionID = newID()
	}
	if d, err := time.ParseDuration(os.Getenv("FAKECOPILOT_START_DELAY")); err == nil && d > 0 {
		time.Sleep(d)
	}
	in := bufio.NewScanner(os.Stdin)
	fmt.Printf("fake copilot (%s)\r\n", model)
	if os.Getenv("FAKECOPILOT_TRUST") != "" {
		fmt.Print("╭──────────────────────────────────────────╮\r\n" +
			"│ Confirm folder trust                     │\r\n" +
			"│ Do you trust the files in this folder?   │\r\n" +
			"│ ❯ 1. Yes                                 │\r\n" +
			"│   2. Yes, and remember this folder       │\r\n" +
			"│   3. No (Esc)                            │\r\n" +
			"╰──────────────────────────────────────────╯\r\n")
		if !in.Scan() {
			return
		}
		note("trust answered")
		fmt.Print("\x1b[2J\x1b[H")
	}
	openEvents()
	prompt()
	for in.Scan() {
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(in.Text()))
		if line == "" {
			prompt()
			continue
		}
		switch line {
		case "/exit":
			emit("session.shutdown", map[string]any{"shutdownType": "routine"})
			fmt.Print("bye\r\n")
			return
		case "/limit":
			fmt.Print("You've hit your rate limit. Resets in 2h.\r\n")
			prompt()
			continue
		}
		emit("user.message", map[string]any{"content": line, "messageId": newID()})
		switch line {
		case "/402":
			emit("session.error", map[string]any{"errorType": "quota", "statusCode": 402,
				"message": "402 You have no quota left this month"})
			fmt.Print("Error: 402 You have no quota left this month\r\n")
		case "/policy":
			emit("session.error", map[string]any{"errorType": "query", "message": `Execution failed: model resolution leaf failed: GenericFailure, {"kind":"http","status":403,"statusText":"Forbidden","body":"unauthorized: not authorized to use this Copilot feature\n"}`})
			fmt.Print("Failed to load models: access denied by Copilot policy.\r\n")
		case "/hold":
			emit("assistant.turn_start", map[string]any{"turnId": "0"})
			fmt.Print("holding\r\n")
			in.Scan()
			finish("resumed")
		case "/edit":
			if denied("write") {
				fmt.Print("edit denied\r\n")
			} else {
				_ = os.WriteFile(filepath.Join(cwd, "blocked.txt"), []byte("hi"), 0o600)
				fmt.Print("wrote blocked.txt\r\n")
			}
			turn("edit")
		case "/push":
			if denied("shell(git push:*)") {
				fmt.Print("git push denied\r\n")
			} else {
				fmt.Print("pushed\r\n")
			}
			turn("push")
		default:
			turn("done: " + line)
			fmt.Printf("done: %s\r\n", line)
		}
		prompt()
	}
}

func denied(rule string) bool {
	for _, d := range deny {
		if d == rule {
			return true
		}
	}
	return false
}

// turn writes a whole turn: it starts, calls a tool, and ends with a final answer.
func turn(answer string) {
	emit("assistant.turn_start", map[string]any{"turnId": "0"})
	finish(answer)
}

func finish(answer string) {
	emit("assistant.message", map[string]any{"messageId": newID(), "content": answer, "toolRequests": []any{}, "phase": "final_answer"})
	emit("assistant.turn_end", map[string]any{"turnId": "0"})
}

func openEvents() {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		return
	}
	dir := filepath.Join(home, "session-state", sessionID)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	events, _ = os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	emit("session.start", map[string]any{"sessionId": sessionID, "selectedModel": model})
}

func emit(typ string, data map[string]any) {
	if events == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"type": typ, "id": newID(), "data": data,
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")})
	events.Write(append(b, '\n'))
}

func note(s string) {
	if p := os.Getenv("FAKECOPILOT_ARGV_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(map[string]any{"note": s})
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// prompt draws Copilot's input box: a gutter of `╻▄▄`, `┃` rows and `╹▀▀`, the placeholder in dim text.
func prompt() {
	fmt.Print("\r\n╻▄▄▄▄▄▄▄▄\r\n")
	if os.Getenv("FAKECOPILOT_PLACEHOLDER") != "" {
		fmt.Print("┃ \x1b[2mType @ to mention files, / for commands\x1b[0m\r\n")
	} else {
		fmt.Print("┃ \r\n")
	}
	fmt.Print("╹▀▀▀▀▀▀▀▀\r\n")
}
