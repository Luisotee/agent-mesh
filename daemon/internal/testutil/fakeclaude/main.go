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
//
// With $FAKECLAUDE_BOX set it draws Claude Code's input box at the bottom of the screen (a rule, the prompt line
// with room to wrap, a rule and a footer) and redraws the screen at each prompt, so what a user types (the tty
// echoes it in the box) is a draft a reader of the screen can see, and a doorbell typed and not submitted is one
// too. It also makes these available, for tests of delivery:
//
//	$FAKECLAUDE_DROP=n        the first n doorbells (lines starting with 📬) typed are dropped: no turn, nothing
//	                          left in the box
//	$FAKECLAUDE_STUCK=n       the first n doorbells typed stay in the box, as if Enter was not taken; an empty line
//	                          (Enter alone) then submits what is there
//	$FAKECLAUDE_DIALOG=1      a dropped line leaves a dialog on the screen instead of the box, and every line
//	                          after it is dropped (there is no box to type into)
//	$FAKECLAUDE_LINES_LOG     appends each line typed, as JSON, to this file
//
// With $FAKECLAUDE_INBOX set (a folder for its socket) it binds a Claude Code-style cross-session inbox: a socket
// there, and `sessions/<pid>.json` in $CLAUDE_CONFIG_DIR (else $HOME/.claude) with the pid, its start in clock ticks
// and `messagingSocketPath`. A frame written to it (`{"type":"user","message":{"role":"user","content":…}}`) runs a
// turn like a typed line but with no UserPromptSubmit (the assumption: a frame may not fire one). With
// $FAKECLAUDE_INBOX_HOLD set it reads the frames and drops them, as a session that holds them does.
//
// With $FAKECLAUDE_SPIN set it keeps drawing a dot in the corner of the screen (a spinner), as a session does
// while it works.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

	box        = os.Getenv("FAKECLAUDE_BOX") != ""
	transcript []string // what the box mode shows above the box
	pending    string   // text left in the box (a line whose Enter wasn't taken)
	dialog     bool     // the screen shows a dialog, not the box
	turnMu     sync.Mutex
)

func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

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
	lines := make(chan string)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- in.Text()
		}
		close(lines)
	}()
	if os.Getenv("FAKECLAUDE_SPIN") != "" {
		go func() {
			for {
				time.Sleep(150 * time.Millisecond)
				fmt.Print("\x1b7\x1b[1;118H.\x1b8")
			}
		}()
	}
	frames := make(chan string, 8)
	if dir := os.Getenv("FAKECLAUDE_INBOX"); dir != "" {
		serveInbox(dir, frames)
	}
	dropped, stuck := 0, 0
	for {
		var raw string
		select {
		case l, ok := <-lines:
			if !ok {
				return
			}
			raw = l
		case f := <-frames:
			turnMu.Lock()
			// A frame is a prompt that fires no UserPromptSubmit.
			say("working on: %s\r\n", f)
			hook("PreToolUse", map[string]any{"tool_name": "Bash"})
			hook("PostToolUse", map[string]any{"tool_name": "Bash"})
			say("done: %s\r\n", f)
			hook("Stop", map[string]any{"stop_hook_active": false})
			prompt()
			turnMu.Unlock()
			continue
		}
		turnMu.Lock()
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(raw))
		if log := os.Getenv("FAKECLAUDE_LINES_LOG"); log != "" && line != "" {
			if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				b, _ := json.Marshal(line)
				f.Write(append(b, '\n'))
				f.Close()
			}
		}
		if box && line != "" || box && pending != "" {
			switch {
			case dialog:
				drawDialog()
				turnMu.Unlock()
				continue
			case dropped < envInt("FAKECLAUDE_DROP") && strings.HasPrefix(line, "📬"):
				dropped++
				if os.Getenv("FAKECLAUDE_DIALOG") != "" {
					dialog = true
					drawDialog()
				} else {
					prompt()
				}
				turnMu.Unlock()
				continue
			case stuck < envInt("FAKECLAUDE_STUCK") && strings.HasPrefix(line, "📬"):
				stuck++
				pending = line
				prompt()
				turnMu.Unlock()
				continue
			case line == "" && pending != "":
				line, pending = pending, ""
			}
		}
		next := func() string {
			turnMu.Unlock()
			l := <-lines
			turnMu.Lock()
			return l
		}
		run(line, next)
		turnMu.Unlock()
	}
}

// run acts out one line typed, then draws the prompt again.
func run(line string, next func() string) {
	switch {
	case strings.HasPrefix(line, "/limit "):
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		hook("StopFailure", map[string]any{"error": "rate_limit", "last_assistant_message": strings.TrimPrefix(line, "/limit ")})
		say("limit\r\n")
		prompt()
		return
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
				say("%s\r\n", strings.TrimSpace(string(out)))
			}
		}
		prompt()
		return
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
			say("denied: %s\r\n", d.H.Reason)
		} else {
			say("ran\r\n")
			hook("PostToolUse", map[string]any{"tool_name": "Bash"})
		}
		hook("Stop", map[string]any{"stop_hook_active": false})
		prompt()
		return
	}
	switch line {
	case "":
	case "/exit":
		hook("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
		say("bye\r\n")
		os.Exit(0)
	case "/crash":
		os.Exit(7)
	case "/clear":
		hook("SessionEnd", map[string]any{"reason": "clear"})
		session += "-cleared"
		hook("SessionStart", map[string]any{"source": "clear"})
	case "/hold":
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		hook("PreToolUse", map[string]any{"tool_name": "Bash"})
		say("holding\r\n")
		next()
		hook("PostToolUse", map[string]any{"tool_name": "Bash"})
		hook("Stop", map[string]any{"stop_hook_active": false})
	case "/ask":
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		hook("Notification", map[string]any{"notification_type": "permission_prompt",
			"message": "Claude needs your permission to use Bash"})
		say("allow Bash? \r\n")
		next()
		hook("PostToolUse", map[string]any{"tool_name": "Bash"})
		hook("Stop", map[string]any{"stop_hook_active": false})
	default:
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		say("working on: %s\r\n", line)
		hook("PreToolUse", map[string]any{"tool_name": "Bash"})
		hook("PostToolUse", map[string]any{"tool_name": "Bash"})
		say("done: %s\r\n", line)
		hook("Stop", map[string]any{"stop_hook_active": false})
	}
	prompt()
}

// say prints output, or in box mode keeps it for the next redraw above the box.
func say(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if !box {
		fmt.Print(text)
		return
	}
	for _, l := range strings.Split(strings.ReplaceAll(strings.TrimRight(text, "\r\n"), "\r\n", "\n"), "\n") {
		transcript = append(transcript, l)
	}
}

const boxTop = 30 // the row of the box's top rule in box mode (the terminal is 40 rows)

// drawDialog replaces the screen with a dialog: no input box.
func drawDialog() {
	fmt.Print("\x1b[2J\x1b[H\x1b[?25l")
	fmt.Print("Do you want to proceed?\r\n❯ 1. Yes\r\n  2. No, and tell Claude what to do differently\r\n")
}

func prompt() {
	if box {
		drawBox()
		return
	}
	fmt.Print("> ")
	if os.Getenv("FAKECLAUDE_PLACEHOLDER") != "" {
		// Dim text, then the cursor back to the start of it, as the TUI draws a placeholder.
		const hint = `Try "fix lint errors"`
		fmt.Printf("\x1b[2m%s\x1b[22m\x1b[%dD", hint, len(hint))
	}
}

// drawBox redraws the screen: the output so far above Claude Code's box at the bottom (rules, a prompt line with
// room to wrap, a footer), the cursor on the prompt line after any text left in it. The tty echoes what is typed
// there.
func drawBox() {
	rule := strings.Repeat("─", 100)
	fmt.Print("\x1b[?25l\x1b[m\x1b[2J\x1b[H")
	shown := transcript
	if len(shown) > boxTop-3 {
		shown = shown[len(shown)-(boxTop-3):]
	}
	for i, l := range shown {
		fmt.Printf("\x1b[%d;1H%s", i+1, l)
	}
	fmt.Printf("\x1b[%d;1H%s", boxTop, rule)
	fmt.Printf("\x1b[%d;1H❯\u00a0%s", boxTop+1, pending)
	if pending == "" && os.Getenv("FAKECLAUDE_PLACEHOLDER") != "" {
		fmt.Print("\x1b[2mTry \"fix lint errors\"\x1b[22m")
	}
	fmt.Printf("\x1b[%d;1H%s", boxTop+5, rule)
	fmt.Printf("\x1b[%d;1H  ? for shortcuts", boxTop+6)
	fmt.Printf("\x1b[%d;%dH\x1b[?25h", boxTop+1, 3+utf8.RuneCountInString(pending))
}

// serveInbox binds the cross-session inbox: a socket in dir, and the record Claude Code keeps of its process.
func serveInbox(dir string, frames chan<- string) {
	sock := filepath.Join(dir, fmt.Sprintf("%d.sock", os.Getpid()))
	l, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: inbox: %v\r\n", err)
		return
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	os.MkdirAll(filepath.Join(home, "sessions"), 0o700)
	rec, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "procStart": procStart(), "messagingSocketPath": sock,
		"sessionId": session})
	os.WriteFile(filepath.Join(home, "sessions", fmt.Sprintf("%d.json", os.Getpid())), rec, 0o600)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var f struct {
						Type    string `json:"type"`
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					if json.Unmarshal(sc.Bytes(), &f) != nil || f.Type != "user" {
						continue
					}
					if log := os.Getenv("FAKECLAUDE_LINES_LOG"); log != "" {
						if lf, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
							b, _ := json.Marshal("frame: " + f.Message.Content)
							lf.Write(append(b, '\n'))
							lf.Close()
						}
					}
					if os.Getenv("FAKECLAUDE_INBOX_HOLD") == "" {
						frames <- f.Message.Content
					}
				}
			}()
		}
	}()
}

// procStart is this process's start time in clock ticks since boot (/proc/self/stat, field 22).
func procStart() string {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
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
