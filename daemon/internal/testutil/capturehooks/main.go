// Command capturehooks drives one real Claude Code through a running meshd and keeps the raw hook payloads it
// fires, scrubbed, as fixtures for the Claude adapter (internal/adapter/claude/testdata/<version>/). It is run
// by hand through daemon/scripts/capture-claude-hooks.sh, never by a test: it uses the account of whoever runs
// it, for a handful of short turns on the light tier.
//
// The scenarios, in order: startup; a prompt that makes it read a file (UserPromptSubmit, PreToolUse,
// PostToolUse, Stop); a prompt that makes it write one without permission (Notification, then the tool and
// Stop after the prompt is accepted); /compact; /clear; a reopen (`claude --resume`, which shows whether the
// session id survives it); /exit. Which files belong to which scenario is in sequence.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

type step struct {
	Scenario string   `json:"scenario"`
	Files    []string `json:"files"`
}

type sequence struct {
	ClaudeVersion string `json:"claude_version"`
	Steps         []step `json:"steps"`
}

func main() {
	state := flag.String("state", os.Getenv(client.StateEnv), "the running meshd's state dir")
	capture := flag.String("capture", "", "the dir meshd's agents capture raw payloads in (AGENT_MESH_HOOK_CAPTURE)")
	repo := flag.String("repo", "", "a git repository to run the agent in")
	out := flag.String("out", "", "the testdata dir: fixtures go to <out>/<claude version>/")
	claude := flag.String("claude", "claude", "the claude binary, for its version")
	timeout := flag.Duration("step-timeout", 3*time.Minute, "how long one scenario may take")
	flag.Parse()
	if *state == "" || *capture == "" || *repo == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "capturehooks: need --state, --capture, --repo and --out")
		os.Exit(2)
	}
	if err := run(*state, *capture, *repo, *out, *claude, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "capturehooks:", err)
		os.Exit(1)
	}
}

type driver struct {
	ctx     context.Context
	conn    *client.Conn
	capture string
	timeout time.Duration
	agent   string
}

func run(state, capture, repo, out, claude string, timeout time.Duration) error {
	vout, err := exec.Command(claude, "--version").Output()
	if err != nil {
		return fmt.Errorf("claude --version: %w", err)
	}
	version := strings.Fields(string(vout))[0]
	tok, err := client.Token(state)
	if err != nil {
		return err
	}
	conn, err := client.Dial(state, tok, client.Options{})
	if err != nil {
		return err
	}
	d := &driver{ctx: context.Background(), conn: conn, capture: capture, timeout: timeout}
	runRes, err := conn.Mesh.StartRun(d.ctx, connect.NewRequest(&v1.StartRunRequest{Meta: &v1.RequestMeta{MutationId: "capture-run"}, Title: "capture hooks"}))
	if err != nil {
		return err
	}
	// The light tier: Haiku at medium effort, the cheapest the adapter launches.
	sp, err := conn.Mesh.SpawnAgent(d.ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: &v1.RequestMeta{MutationId: "capture-agent"},
		RunId: runRes.Msg.Run.Id, Repo: repo, Slug: "capture", Tier: "light"}))
	if err != nil {
		return err
	}
	d.agent = sp.Msg.Agent.Id
	seq := sequence{ClaudeVersion: version}
	record := func(scenario string, from int) {
		files := d.captured()
		seq.Steps = append(seq.Steps, step{Scenario: scenario, Files: files[from:]})
	}
	mark := 0
	next := func(scenario string) {
		record(scenario, mark)
		mark = len(d.captured())
		fmt.Printf("== %s: %d hooks so far\n", scenario, mark)
	}

	// Startup: the trust dialog of a folder claude hasn't seen blocks it (its default is "No, exit"): choose "Yes,
	// I trust this folder" if it shows. A worktree of a run is always such a folder.
	if err := d.waitState(v1.AgentState_AGENT_STATE_IDLE, "startup", func() {
		if !strings.Contains(d.screen(), "I trust this folder") {
			return // slow, not stuck at the dialog: no stray keys
		}
		d.type_("\x1b[B")
		time.Sleep(300 * time.Millisecond)
		d.type_("\r")
	}); err != nil {
		return err
	}
	next("startup")

	if err := d.prompt("Read the file README.md and answer with only its first line."); err != nil {
		return err
	}
	if err := d.waitState(v1.AgentState_AGENT_STATE_IDLE, "the read turn", nil); err != nil {
		return err
	}
	next("a turn with a tool call")

	// No --dangerously-skip-permissions: writing a file asks.
	if err := d.prompt("Create a file named hello.txt in this directory containing only the word hi, using the Write tool."); err != nil {
		return err
	}
	if err := d.waitState(v1.AgentState_AGENT_STATE_NEEDS_INPUT, "the permission prompt", nil); err != nil {
		return err
	}
	next("a permission prompt")
	d.type_("\r") // the default answer is yes
	if err := d.waitState(v1.AgentState_AGENT_STATE_IDLE, "the write turn", nil); err != nil {
		return err
	}
	next("the prompt accepted, the turn ending")

	base := d.hooks("SessionStart")
	if err := d.prompt("/compact"); err != nil {
		return err
	}
	if err := d.waitHooks("SessionStart", base+1, "/compact"); err != nil {
		return err
	}
	if err := d.waitState(v1.AgentState_AGENT_STATE_IDLE, "/compact", nil); err != nil {
		return err
	}
	next("/compact")

	base = d.hooks("SessionStart")
	if err := d.prompt("/clear"); err != nil {
		return err
	}
	if err := d.waitHooks("SessionStart", base+1, "/clear"); err != nil {
		return err
	}
	next("/clear")

	// A reopen: the same conversation, resumed. The ids before and after tell whether the session id survives.
	base = d.hooks("SessionStart")
	prompts := d.hooks("UserPromptSubmit")
	if _, err := conn.Mesh.SwitchAgent(d.ctx, connect.NewRequest(&v1.SwitchAgentRequest{Meta: &v1.RequestMeta{MutationId: "capture-switch"},
		Agent: d.agent, Effort: "low"})); err != nil {
		return err
	}
	if err := d.waitHooks("SessionStart", base+1, "the reopen"); err != nil {
		return err
	}
	// The message the switch stored rings in the new process: a turn, in which it reads its inbox, which asks.
	if err := d.waitHooks("UserPromptSubmit", prompts+1, "the reopened session's first turn"); err != nil {
		return err
	}
	if err := d.settle("the reopened session"); err != nil {
		return err
	}
	next("a reopen (claude --resume)")
	if err := d.prompt("/exit"); err != nil {
		return err
	}
	if err := d.waitState(v1.AgentState_AGENT_STATE_EXITED, "/exit", nil); err != nil {
		return err
	}
	next("/exit")
	return d.write(out, version, repo, seq)
}

// settle waits for the agent's turn to end, accepting each permission prompt it stops at.
func (d *driver) settle(what string) error {
	deadline := time.Now().Add(d.timeout)
	for {
		st, _, err := d.agentState()
		if err != nil {
			return err
		}
		switch {
		case st == v1.AgentState_AGENT_STATE_IDLE:
			return nil
		case st == v1.AgentState_AGENT_STATE_NEEDS_INPUT:
			time.Sleep(time.Second)
			d.type_("\r")
			time.Sleep(2 * time.Second)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the agent is %s after %s\n%s", what, st, d.timeout, d.screen())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// captured lists the raw payload files so far, in the order they fired.
func (d *driver) captured() []string {
	files, _ := filepath.Glob(filepath.Join(d.capture, "*.json"))
	sort.Strings(files)
	for i, f := range files {
		files[i] = filepath.Base(f)
	}
	return files
}

var sessionRE = regexp.MustCompile(`"session_id"\s*:\s*"([^"]+)"`)

// sessionIDs lists the session id of each SessionStart so far.
func (d *driver) sessionIDs() []string {
	var ids []string
	for _, f := range d.captured() {
		if !strings.HasSuffix(f, "-SessionStart.json") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(d.capture, f))
		if m := sessionRE.FindSubmatch(b); m != nil {
			ids = append(ids, string(m[1]))
		}
	}
	return ids
}

// hooks counts the captured payloads of an event.
func (d *driver) hooks(event string) int {
	n := 0
	for _, f := range d.captured() {
		if strings.HasSuffix(f, "-"+event+".json") {
			n++
		}
	}
	return n
}

// waitHooks waits until at least n payloads of an event were captured.
func (d *driver) waitHooks(event string, n int, what string) error {
	deadline := time.Now().Add(d.timeout)
	for d.hooks(event) < n {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: no %s hook within %s\n%s", what, event, d.timeout, d.screen())
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func (d *driver) agentState() (v1.AgentState, *v1.Agent, error) {
	res, err := d.conn.Mesh.GetAgent(d.ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: d.agent}))
	if err != nil {
		return 0, nil, err
	}
	return res.Msg.Agent.State, res.Msg.Agent, nil
}

// waitState waits for the agent's state, as its hooks drove it. stuck, when set, is tried once after 20 s without
// progress (a dialog waiting for Enter).
func (d *driver) waitState(want v1.AgentState, what string, stuck func()) error {
	deadline := time.Now().Add(d.timeout)
	tried := time.Now().Add(20 * time.Second)
	for {
		st, _, err := d.agentState()
		if err != nil {
			return err
		}
		if st == want {
			return nil
		}
		if stuck != nil && time.Now().After(tried) {
			// Again every 20 s, not once: the dialog may only be drawn after a slow start. The callback looks at
			// the screen first and sends nothing when there is no dialog.
			fmt.Printf("   %s: still %s after 20 s; screen:\n%s\n   looking for a dialog\n", what, st, d.screen())
			stuck()
			tried = time.Now().Add(20 * time.Second)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the agent is %s, wanted %s, after %s\n%s", what, st, want, d.timeout, d.screen())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// prompt types a line into the agent's terminal and submits it. The text and the Enter go separately, or the
// TUI takes them for a paste and Enter for a newline.
func (d *driver) prompt(text string) error {
	d.type_(text)
	time.Sleep(500 * time.Millisecond)
	d.type_("\r")
	return nil
}

func (d *driver) type_(text string) {
	if text == "" {
		return
	}
	_, a, err := d.agentState()
	if err != nil || a.TerminalId == "" {
		return
	}
	res, err := d.conn.Mesh.OpenTerminal(d.ctx, connect.NewRequest(&v1.OpenTerminalRequest{Agent: d.agent, Input: true}))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open-terminal:", err)
		return
	}
	s, err := termclient.Attach(d.ctx, d.conn.HTTP, client.BaseURL+res.Msg.Path, "", &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "attach:", err)
		return
	}
	defer s.Close()
	for {
		f, err := s.Recv(d.ctx)
		if err != nil {
			return
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	s.Ready(d.ctx)
	s.Input(d.ctx, []byte(text))
	time.Sleep(200 * time.Millisecond)
}

// screen is what the agent's terminal shows now, for a failure message.
func (d *driver) screen() string {
	res, err := d.conn.Mesh.OpenTerminal(d.ctx, connect.NewRequest(&v1.OpenTerminalRequest{Agent: d.agent}))
	if err != nil {
		return "(no terminal: " + err.Error() + ")"
	}
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	s, err := termclient.Attach(ctx, d.conn.HTTP, client.BaseURL+res.Msg.Path, "", &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT})
	if err != nil {
		return "(no terminal: " + err.Error() + ")"
	}
	defer s.Close()
	var sb strings.Builder
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			break
		}
		if sc := f.GetScreen(); sc != nil {
			sb.Write(sc.Data)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	return sb.String()
}

// write scrubs the captured payloads and saves them under <out>/<version>/, numbered in the order they fired.
func (d *driver) write(out, version, repo string, seq sequence) error {
	files := d.captured()
	home, _ := os.UserHomeDir()
	repoReal, _ := filepath.EvalSymlinks(repo)
	root := filepath.Dir(repo) // the script makes the repo, the state dir and the capture dir under one folder
	rootReal, _ := filepath.EvalSymlinks(root)
	scrubber := newScrubber(home, rootReal, repoReal)
	scrubber.alsoPath(repo, "/work/repo")
	scrubber.alsoPath(root, "/work")
	if homeReal, err := filepath.EvalSymlinks(home); err == nil && homeReal != home {
		scrubber.alsoPath(homeReal, "/home/user") // a home that is a symlink: payloads may carry either spelling
	}
	dir := filepath.Join(out, version)
	// Everything is scrubbed in memory first: a payload the scrubber refuses must leave the committed fixtures as
	// they were, not half of them.
	renamed := map[string]string{}
	cleaned := map[string][]byte{}
	for i, f := range files {
		event := strings.TrimSuffix(f[strings.Index(f, "-")+1:], ".json")
		b, err := os.ReadFile(filepath.Join(d.capture, f))
		if err != nil {
			return err
		}
		clean, err := scrubber.scrub(b)
		if err != nil {
			return fmt.Errorf("%s: %w (nothing was written)", f, err)
		}
		name := fmt.Sprintf("%02d-%s.json", i+1, event)
		renamed[f] = name
		cleaned[name] = clean
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, clean := range cleaned {
		if err := os.WriteFile(filepath.Join(dir, name), clean, 0o644); err != nil {
			return err
		}
	}
	for i := range seq.Steps {
		for j, f := range seq.Steps[i].Files {
			seq.Steps[i].Files[j] = renamed[f]
		}
	}
	b, _ := json.MarshalIndent(seq, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "sequence.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d payloads to %s\n", len(files), dir)
	return nil
}
