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
//	              PreCompact, SubagentStop, then SessionStart (compact) under the same session id, and then a
//	              compact_boundary line appended to the session's transcript under $HOME/.claude/projects, as the
//	              real one writes it (after its hook; $FAKECLAUDE_BOUNDARY_DELAY, a duration, delays the write: a
//	              process stopped before it never writes it); with $FAKECLAUDE_COMPACT=none it only prints "Not
//	              enough messages" and fires nothing
//	/hold         UserPromptSubmit and PreToolUse, then waits for a line (a long turn); then PostToolUse and
//	              Stop. With $FAKECLAUDE_ESC set, a lone Esc instead ends the turn with no Stop hook, as
//	              interrupting the real one does
//	/exit         SessionEnd, then exits 0
//	/crash        exits 7 without any hook
//	/limit <text> UserPromptSubmit, then StopFailure (rate_limit, <text> as the message): a usage limit
//	/ratelimits <json>
//	              invokes settings.statusLine.command with rate_limits JSON on stdin
//	/bash <cmd>   UserPromptSubmit, PreToolUse (Bash, <cmd>), then PostToolUse and Stop; "denied: <reason>" is
//	              printed instead of the tool's, when a PreToolUse hook printed a permission denial
//
// With $FAKECLAUDE_ESC set the terminal leaves canonical mode, so a lone Esc reaches it as a key: it interrupts a
// /hold turn (no Stop hook, as Claude Code fires none for an interrupted turn) and does nothing between turns.
//
// Like Claude Code, it runs the hooks of the project's `.claude/settings.json` and `.claude/settings.local.json`
// (under its working directory) along with the --settings ones, unless `--setting-sources` is given and leaves
// `project` and `local` out: a test of what an untrusted PR head can run puts a hook there.
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
// With $FAKECLAUDE_TRANSCRIPT set each plain prompt's turn is written to the session's transcript under
// $HOME/.claude/projects, in the shapes Claude Code writes: the chat view's source.
//
// With $FAKECLAUDE_PLACEHOLDER set it draws Claude Code's empty-box placeholder after its prompt, as the real one
// does at a fresh pane: text that looks like a draft to anything reading the screen.
//
// With $FAKECLAUDE_SUGGESTION set (and the box) it draws that text as the box's ghost text after each turn that ran a prompt, the
// way Claude Code draws its suggested next prompt (dim, the first character inverted as it is with the terminal focused).
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
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
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
	// sources is the --setting-sources list ("" = the flag is absent: user, project and local).
	sources string
	cwd, _  = os.Getwd()

	box = os.Getenv("FAKECLAUDE_BOX") != ""
	// suggestion is the text of $FAKECLAUDE_SUGGESTION once a turn has been recorded (box mode): Claude's predicted next prompt.
	suggestion string
	transcript []string // what the box mode shows above the box
	pending    string   // text left in the box (a line whose Enter wasn't taken)
	dialog     bool     // the screen shows a dialog, not the box
	turnMu     sync.Mutex
)

// answerInitialize is `claude -p --input-format stream-json` asked for its commands (the SDK's `initialize`
// control request, which the daemon's command list sends): one control response with a fixed list, or the JSON in
// FAKECLAUDE_COMMANDS, then the process ends at the end of its input. FAKECLAUDE_PROBE_FAIL makes it fail.
func answerInitialize() {
	if os.Getenv("FAKECLAUDE_PROBE_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "fakeclaude: initialize failed")
		os.Exit(1)
	}
	commands := os.Getenv("FAKECLAUDE_COMMANDS")
	if commands == "" {
		commands = `[{"name":"compact","description":"Free up context by summarizing the conversation so far","argumentHint":"<optional custom summarization instructions>"},` +
			`{"name":"model","description":"Set the AI model for Claude Code","argumentHint":"<model>"},` +
			`{"name":"context","description":"Show current context usage","argumentHint":""},` +
			`{"name":"config","description":"Set a setting by key","argumentHint":"key=value"},` +
			`{"name":"deploy","description":"Ship it (project)","argumentHint":"[env]"},` +
			`{"name":"delegate","description":"hands work to agents","argumentHint":""},` +
			`{"name":"agent-mesh:delegate","description":"(agent-mesh) hands work to agents","argumentHint":""}]`
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var req struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.Type != "control_request" || req.Request.Subtype != "initialize" {
			continue
		}
		fmt.Printf(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"commands":%s}}}`+"\n", req.RequestID, commands)
	}
}

func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

func main() {
	args := os.Args[1:]
	if box {
		// A resize redraws the box where the bottom of the screen now is, unless a dialog is on it.
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		go func() {
			for range winch {
				// Twice: the host resizes its own screen after the terminal, and what was drawn before that is lost.
				for i := 0; i < 2; i++ {
					turnMu.Lock()
					if !dialog {
						drawBox()
					}
					turnMu.Unlock()
					time.Sleep(200 * time.Millisecond)
				}
			}
		}()
	}
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
	source, printMode, streamIn := "startup", false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p":
			printMode = true
		case "--input-format":
			i++
			streamIn = args[i] == "stream-json"
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
		case "--setting-sources":
			i++
			sources = args[i]
		case "--model", "--effort":
			i++
		case "--disallowedTools":
			// Several values, up to the next flag.
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		}
	}
	if printMode && streamIn {
		answerInitialize()
		return
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
	// The inbox is bound before anything is drawn: a daemon that takes the terminal for settled once its output
	// is quiet must find the socket there, or it types the doorbell (a race a loaded host lost one run in thirty).
	frames := make(chan string, 8)
	if dir := os.Getenv("FAKECLAUDE_INBOX"); dir != "" {
		serveInbox(dir, frames)
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
	if os.Getenv("FAKECLAUDE_ESC") != "" {
		go readKeys(lines)
	} else {
		go readLines(lines)
	}
	if os.Getenv("FAKECLAUDE_SPIN") != "" {
		go func() {
			for {
				time.Sleep(150 * time.Millisecond)
				fmt.Print("\x1b7\x1b[1;118H.\x1b8")
			}
		}()
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
		if raw == "\x1b" {
			// Esc with no turn to interrupt: nothing happens.
			turnMu.Unlock()
			continue
		}
		line := strings.TrimSpace(strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(raw))
		if log := os.Getenv("FAKECLAUDE_INPUT_LOG"); log != "" && line != "" {
			if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.WriteString(line + "\n")
				f.Close()
			}
		}
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
	case line == "/bg":
		// A background command, as Claude Code starts one: `bash -c` that sources a shell snapshot, a child of this
		// process that outlives the turn. It ends when the file FAKECLAUDE_BG_STOP exists.
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		stop := os.Getenv("FAKECLAUDE_BG_STOP")
		job := exec.Command("bash", "-c", "source ~/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && "+
			"while [ ! -e '"+stop+"' ]; do sleep 0.2; done")
		if err := job.Start(); err == nil {
			go job.Wait()
		}
		say("background command started\r\n")
		hook("Stop", map[string]any{"stop_hook_active": false})
		prompt()
		return
	case strings.HasPrefix(line, "/perm "):
		// `/perm <Tool> <command or path>`: a tool call that needs a permission prompt. The PermissionRequest hook
		// answers it (allow, deny with a reason) or leaves it to the person at the terminal (shown as `permission: pane`).
		tool, arg, _ := strings.Cut(strings.TrimPrefix(line, "/perm "), " ")
		hook("UserPromptSubmit", map[string]any{"prompt": line})
		input := map[string]any{"command": arg}
		if tool == "Edit" || tool == "Write" {
			input = map[string]any{"file_path": arg, "new_string": "SECRET FILE CONTENTS"}
		}
		out := hook("PermissionRequest", map[string]any{"tool_name": tool, "tool_input": input})
		var d struct {
			H struct {
				Decision struct {
					Behavior string `json:"behavior"`
					Message  string `json:"message"`
				} `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		switch _ = json.Unmarshal(out, &d); d.H.Decision.Behavior {
		case "allow":
			say("permission: allow\r\nran\r\n")
			hook("PostToolUse", map[string]any{"tool_name": tool})
		case "deny":
			say("permission: deny %s\r\n", d.H.Decision.Message)
		default:
			say("permission: pane\r\n")
		}
		hook("Stop", map[string]any{"stop_hook_active": false})
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
	if strings.HasPrefix(line, "/compact ") {
		compact(strings.TrimPrefix(line, "/compact "))
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
		say("holding\r\n")
		if next() == "\x1b" {
			// Interrupted (Esc, with $FAKECLAUDE_ESC): Claude Code fires no Stop hook for a turn the user interrupts.
			say("Interrupted by user\r\n")
			break
		}
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
		recordTurn(line)
		suggestion = os.Getenv("FAKECLAUDE_SUGGESTION")
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

// boxTopRow is the row of the box's top rule in box mode: ten rows above the bottom of the terminal as it is now
// (30 when it can't be read: the 40 rows meshd starts agents with). An app attached at another size changes it, and the
// real CLI redraws for that, so the fake does too (SIGWINCH, in main).
func boxTopRow() int {
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Row >= 12 {
		return int(ws.Row) - 10
	}
	return 30
}

// ruleWidth is the width of the box's rules: 100, or the terminal's when it is narrower (a rule that wraps is two
// lines, and no box reads right then).
func ruleWidth() int {
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 && int(ws.Col) < 100 {
		return int(ws.Col)
	}
	return 100
}

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
	rule := strings.Repeat("─", ruleWidth())
	boxTop := boxTopRow()
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
	if pending == "" && suggestion != "" {
		// After a turn that did work, as Claude Code draws its predicted next prompt: dim, and with its terminal
		// focused the first character inverted (the cursor on it). A fresh start has none.
		r := []rune(suggestion)
		fmt.Printf("\x1b[7m%s\x1b[27m\x1b[2m%s\x1b[22m", string(r[:1]), string(r[1:]))
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
	cwd, _ := os.Getwd()
	rec, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "procStart": procStart(), "messagingSocketPath": sock,
		"sessionId": session, "cwd": cwd, "status": "idle"})
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
	groups := conf.Hooks[event]
	for _, file := range []struct{ name, source string }{{"settings.json", "project"}, {"settings.local.json", "local"}} {
		src, f := file.source, file.name
		if sources != "" && !slices.Contains(strings.Split(sources, ","), src) {
			continue
		}
		var proj settings
		if b, err := os.ReadFile(filepath.Join(cwd, ".claude", f)); err == nil && json.Unmarshal(b, &proj) == nil {
			groups = append(groups, proj.Hooks[event]...)
		}
	}
	for _, group := range groups {
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
	hook("SubagentStop", map[string]any{})
	hook("SessionStart", map[string]any{"source": "compact"})
	// Claude Code 2.1.332 fires SessionStart (compact) before the compacted transcript is on disk (live: the
	// boundary landed 1 to 2 s later, and a process stopped in between left the conversation whole).
	if d, err := time.ParseDuration(os.Getenv("FAKECLAUDE_BOUNDARY_DELAY")); err == nil {
		time.Sleep(d)
	}
	boundary()
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

var recorded int

// recordTurn appends a turn to the session's transcript, in the shapes Claude Code writes (a user line, an assistant
// line with text and a tool call, the tool's result, the closing text), when $FAKECLAUDE_TRANSCRIPT is set: the file
// the chat view reads. Off by default, as a transcript that exists changes what compaction tests see (`boundary`).
func recordTurn(prompt string) {
	home := os.Getenv("HOME")
	if os.Getenv("FAKECLAUDE_TRANSCRIPT") == "" || home == "" || session == "" {
		return
	}
	dir := filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(cwd, "-"))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, session+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	recorded++
	id := fmt.Sprintf("%s-%d", session, recorded)
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	line := func(kind, uuid string, content any) {
		b, _ := json.Marshal(map[string]any{"type": kind, "uuid": uuid, "timestamp": now(), "sessionId": session, "isSidechain": false,
			"message": map[string]any{"role": kind, "content": content}})
		f.Write(append(b, '\n'))
	}
	if prompt == "/context" {
		// A local command: its name as typed, then what it printed (no model turn), as Claude Code records it.
		line("user", id+"-u", "<command-name>/context</command-name>\n<command-message>context</command-message>\n<command-args></command-args>")
		line("user", id+"-o", "<local-command-stdout>\x1b[1mContext\x1b[22m 12k/200k tokens</local-command-stdout>")
		return
	}
	line("user", id+"-u", prompt)
	line("assistant", id+"-a1", []map[string]any{
		{"type": "text", "text": "Working on: " + prompt},
		{"type": "tool_use", "id": "toolu_" + id, "name": "Bash", "input": map[string]any{"command": "echo " + prompt, "description": "say it"}}})
	line("user", id+"-r", []map[string]any{{"type": "tool_result", "tool_use_id": "toolu_" + id, "content": prompt}})
	line("assistant", id+"-a2", []map[string]any{{"type": "text", "text": "Done: " + prompt}})
}

// readLines sends each line typed, as the terminal's line discipline hands them over (canonical mode). A bracketed
// paste of several lines is one prompt, as the real CLI takes it.
func readLines(lines chan<- string) {
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		l := in.Text()
		for strings.Contains(l, "\x1b[200~") && !strings.Contains(l, "\x1b[201~") && in.Scan() {
			l += "\n" + in.Text()
		}
		lines <- l
	}
	close(lines)
}

// readKeys is readLines for $FAKECLAUDE_ESC: the terminal leaves canonical mode (echo stays on) so a lone Esc
// reaches the fake as it reaches the real CLI, sent as the line "\x1b" (an interrupt); everything else is gathered
// into lines as before.
func readKeys(lines chan<- string) {
	fd := int(os.Stdin.Fd())
	if t, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
		t.Lflag &^= unix.ICANON
		t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
		unix.IoctlSetTermios(fd, unix.TCSETS, t)
	}
	buf := make([]byte, 4096)
	var line []byte
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			close(lines)
			return
		}
		chunk := buf[:n]
		if len(line) == 0 && n == 1 && chunk[0] == 0x1b {
			lines <- "\x1b"
			continue
		}
		for _, b := range chunk {
			switch {
			case (b == '\r' || b == '\n') && bytes.Contains(line, []byte("\x1b[200~")) && !bytes.Contains(line, []byte("\x1b[201~")):
				line = append(line, '\n') // inside a paste: one prompt of several lines
			case b == '\r' || b == '\n':
				lines <- string(line)
				line = line[:0]
			case b == 0x15:
				line = line[:0] // Ctrl-U
			case b == 0x7f:
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			default:
				line = append(line, b)
			}
		}
	}
}
