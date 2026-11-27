// Package e2e runs the real binaries together: meshd, mesh-term and mesh, with a fake claude in the PTY.
// It is the milestone's done-when: spawn, hooks, state through the CLI and the event stream, and a daemon
// SIGKILLed and restarted with the terminal intact.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/pairing"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/cleanenv"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

var bins string

func TestMain(m *testing.M) {
	cleanenv.Agent()
	dir, err := os.MkdirTemp("", "meshd-e2e-bins-")
	if err != nil {
		panic(err)
	}
	for out, pkg := range map[string]string{"meshd": "./cmd/meshd", "mesh-term": "./cmd/mesh-term",
		"mesh": "./cmd/mesh", "mesh-relay": "./cmd/mesh-relay", "claude": "./internal/testutil/fakeclaude",
		"agy": "./internal/testutil/fakeagy"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, out), pkg)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n%s", pkg, err, b)
			os.Exit(1)
		}
	}
	bins = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type world struct {
	t     *testing.T
	state string
	home  string
	meshd *exec.Cmd
	log   *lockedBuffer
	// extra are more meshd flags.
	extra []string
}

// env is the isolated environment every process of the test gets: its own HOME, nothing of the caller's
// agent-mesh, Claude or herdr identity.
func (w *world) env() []string {
	env := []string{"HOME=" + w.home, "PATH=" + os.Getenv("PATH"), client.StateEnv + "=" + w.state}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	return env
}

// lockedBuffer is meshd's output: the process writes it while a failing wait prints it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (w *world) startMeshd() {
	w.t.Helper()
	w.log = &lockedBuffer{}
	// A `mesh` that is not ours, first on the PATH meshd is told to give agents: the agent must still get ours, which
	// meshd puts ahead.
	decoy := filepath.Join(w.home, "decoy")
	os.MkdirAll(decoy, 0o755)
	if err := os.WriteFile(filepath.Join(decoy, "mesh"), []byte("#!/bin/sh\necho the wrong mesh >&2\nexit 99\n"), 0o755); err != nil {
		w.t.Fatal(err)
	}
	// The share folder meshd is given reaches the agents' environment ("protocol" in it is what makes it one).
	share := filepath.Join(w.home, "share")
	os.MkdirAll(filepath.Join(share, "protocol"), 0o755)
	cmd := exec.Command(filepath.Join(bins, "meshd"), "--state", w.state, "--term-launch", "detached", "--share", share,
		"--max-lifetime", "3m", "--cli", "claude="+filepath.Join(bins, "claude"),
		"--cli", "agy="+filepath.Join(bins, "agy"),
		"--agent-env", "HOME="+w.home, "--agent-env", "PATH="+decoy+":"+os.Getenv("PATH"),
		"--agent-env", "FAKECLAUDE_ENV_LOG="+filepath.Join(w.home, "claude-env.log"))
	cmd.Args = append(cmd.Args, w.extra...)
	cmd.Env = w.env()
	cmd.Stdout, cmd.Stderr = w.log, w.log
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	w.meshd = cmd
	w.waitFor("meshd up", func() bool {
		info, err := statedir.ReadInfo(w.state)
		return err == nil && info.PID == cmd.Process.Pid
	})
}

func (w *world) killMeshd() {
	w.meshd.Process.Signal(syscall.SIGKILL)
	w.meshd.Wait()
}

func (w *world) stopAll() {
	if w.meshd != nil && w.meshd.ProcessState == nil {
		w.meshd.Process.Signal(syscall.SIGTERM)
		w.meshd.Wait()
	}
	b, err := os.ReadFile(termhost.Paths{Dir: statedir.Paths{Dir: w.state}.Term()}.Info())
	if err != nil {
		return
	}
	var info termhost.Info
	json.Unmarshal(b, &info)
	if ticks, err := proc.StartTicks(info.PID); err == nil {
		proc.Signal(info.PID, ticks, syscall.SIGTERM)
	}
}

func (w *world) waitFor(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s\nmeshd log:\n%s", what, w.log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ctl runs mesh and decodes its JSON output.
func (w *world) ctl(args ...string) map[string]any {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(bins, "mesh"), args...)
	cmd.Env = w.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		w.t.Fatalf("mesh %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		w.t.Fatalf("mesh %s printed %q: %v", strings.Join(args, " "), out, err)
	}
	return m
}

func get(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func str(m any, path ...string) string {
	s, _ := get(m, path...).(string)
	return s
}

func (w *world) agentState(id string) string {
	return str(w.ctl("agent", "--agent", id), "agent", "state")
}

// follower runs `mesh events` and collects its lines.
type follower struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	lines []map[string]any
	done  chan struct{}
}

func (w *world) follow(args ...string) *follower {
	w.t.Helper()
	f := &follower{done: make(chan struct{})}
	f.cmd = exec.Command(filepath.Join(bins, "mesh"), append([]string{"events"}, args...)...)
	f.cmd.Env = w.env()
	out, _ := f.cmd.StdoutPipe()
	f.cmd.Stderr = io.Discard
	if err := f.cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	go func() {
		defer close(f.done)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				f.mu.Lock()
				f.lines = append(f.lines, m)
				f.mu.Unlock()
			}
		}
		f.cmd.Wait()
	}()
	w.t.Cleanup(func() { f.cmd.Process.Kill(); <-f.done })
	return f
}

func (f *follower) snapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.lines...)
}

// states lists an agent's state changes the follower has seen.
func (f *follower) states(agent string) []string {
	var out []string
	for _, l := range f.snapshot() {
		if str(l, "event", "agentId") == agent {
			if to := str(l, "event", "agentStateChanged", "to"); to != "" {
				out = append(out, strings.TrimPrefix(to, "AGENT_STATE_"))
			}
		}
	}
	return out
}

// cursor is the position after the last event the follower saw.
func (f *follower) cursor() (string, string) {
	var gen, seq string
	for _, l := range f.snapshot() {
		if ev, ok := get(l, "event").(map[string]any); ok {
			gen, seq = str(ev, "generation"), str(ev, "seq")
		}
	}
	return gen, seq
}

func hasSeq(states []string, want ...string) bool {
	i := 0
	for _, s := range states {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

// text runs mesh and returns what it printed (for the commands whose default output is text).
func (w *world) text(args ...string) string {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(bins, "mesh"), args...)
	cmd.Env = w.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		w.t.Fatalf("mesh %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// The policy file of bin/mesh is imported at meshd's first start, `mesh import-settings` applies it again, and
// `mesh model` shows what a spawn would run (with the setting the file brought).
func TestPolicyFileAndModelEndToEnd(t *testing.T) {
	root := shortdir.New(t)
	w := &world{t: t, state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.stopAll)
	policy := filepath.Join(w.home, "agent-mesh.json")
	body := `{"repos_dir": ["~/projects"], "compact": {"claude": 250000}, "side_panes": 3,
		"tiers": {"agy": {"mid": {"model": ["flash"], "effort": "low"}}}}`
	if err := os.WriteFile(policy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	w.extra = []string{"--policy-file", policy}
	w.startMeshd()
	got := w.text("settings", "--prefix", "compact.claude")
	if !strings.Contains(got, "compact.claude = 250000   (set; default 300000)") {
		t.Errorf("settings: %s", got)
	}
	if got := w.text("settings", "--prefix", "repos.dir"); !strings.Contains(got, `repos.dir = ["~/projects"]`) {
		t.Errorf("repos.dir: %s", got)
	}
	if got := w.text("model", "--kind", "agy", "--tier", "mid"); !strings.HasPrefix(got, "flash low\n  flash\n") {
		t.Errorf("model: %q", got)
	}
	if got := w.text("model", "--tier", "light"); !strings.HasPrefix(got, "haiku medium\n") {
		t.Errorf("model: %q", got)
	}
	// Nothing is left to import; a file edited since then is applied on demand, and never over what is set.
	if got := w.text("import-settings"); !strings.Contains(got, "imported 0 setting(s)") || !strings.Contains(got, "left alone: side_panes: not used by the daemon") {
		t.Errorf("import-settings: %s", got)
	}
	os.WriteFile(policy, []byte(`{"compact": {"claude": 1, "agy": 5}}`), 0o644)
	got = w.text("import-settings", "--from", policy)
	if !strings.Contains(got, "imported 1 setting(s)") || !strings.Contains(got, "compact.agy = 5") || !strings.Contains(got, "compact.claude: already set") {
		t.Errorf("import-settings --from: %s", got)
	}
	if got := w.text("import-settings", "--from", policy, "--overwrite", "--dry-run"); !strings.Contains(got, "would import 1 setting(s)") || !strings.Contains(got, "compact.claude = 1") {
		t.Errorf("dry run: %s", got)
	}
}

func TestEndToEnd(t *testing.T) {
	root := shortdir.New(t)
	w := &world{t: t, state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.stopAll)
	repo := filepath.Join(root, "shop-api")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, w.env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	w.startMeshd()
	hello := w.ctl("hello")
	if str(hello, "actor", "class") != "ACTOR_CLASS_DEVICE" {
		t.Fatalf("hello %v", hello)
	}
	run := str(w.ctl("start-run", "--title", "e2e"), "run", "id")
	events := w.follow("--run-id", run)

	// A setting: set through the global --json (before the command), listed, seen on the run's stream (it is
	// global, so a run's subscriber gets it), then reset. The stream is up once its snapshot arrived: an event
	// committed before that is in no later frame (a client lists settings after a snapshot).
	w.waitFor("the stream's snapshot", func() bool { return len(events.snapshot()) > 0 })
	set := w.ctl("--json", `{"key":"caps.agents","value":3}`, "set")
	if get(set, "setting", "value") != 3.0 || get(set, "setting", "isSet") != true || get(set, "setting", "default") != "auto" {
		t.Fatalf("set %v", set)
	}
	if got := w.ctl("settings", "--format", "json", "--prefix", "caps."); len(get(got, "settings").([]any)) != 2 {
		t.Fatalf("settings %v", got)
	}
	w.waitFor("the setting on the stream", func() bool {
		for _, l := range events.snapshot() {
			if str(l, "event", "settingChanged", "key") == "caps.agents" && get(l, "event", "settingChanged", "value") == 3.0 {
				return true
			}
		}
		return false
	})
	if reset := w.ctl("unset", "--key", "caps.agents"); get(reset, "setting", "value") != "auto" || get(reset, "setting", "isSet") == true {
		t.Fatalf("unset %v", reset)
	}

	// Spawn a Claude agent (the fake) in a worktree of the repo.
	spawned := w.ctl("spawn", "--run-id", run, "--repo", repo, "--slug", "fix", "--kind", "claude", "--role", "worker", "--tier", "mid")
	agent := str(spawned, "agent", "id")
	worktree, branch := str(spawned, "agent", "worktree"), str(spawned, "agent", "branch")
	if branch != "mesh/"+run+"/fix" {
		t.Fatalf("branch %q", branch)
	}
	if out, err := exec.Command("git", "-C", worktree, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(out)) != branch {
		t.Fatalf("worktree %s on %q, %v", worktree, out, err)
	}
	// Its SessionStart hook makes it idle, as the CLI's list shows.
	w.waitFor("idle in `mesh agents`", func() bool {
		for _, a := range get(w.ctl("agents", "--run-id", run), "agents").([]any) {
			if str(a, "id") == agent && str(a, "state") == "AGENT_STATE_IDLE" {
				return true
			}
		}
		return false
	})

	// A message rings its doorbell; the fake works on it, and the stream shows working, then idle.
	w.ctl("send", "--to", agent, "--type", "task", "--body", "fix the typo")
	w.waitFor("working then idle on the stream", func() bool { return hasSeq(events.states(agent), "IDLE", "WORKING", "IDLE") })
	if got := w.agentState(agent); got != "AGENT_STATE_IDLE" {
		t.Fatalf("state %s", got)
	}
	before := w.ctl("agent", "--agent", agent)
	terminal := str(before, "agent", "terminalId")

	// Remember the terminal's position through the daemon, as a client would.
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil || len(list.Msg.Terminals) != 1 {
		t.Fatalf("terminals %v, %v", list, err)
	}
	pid, epoch, offset := list.Msg.Terminals[0].Pid, list.Msg.Terminals[0].Epoch, list.Msg.Terminals[0].Offset
	gen, seq := events.cursor()

	// SIGKILL the daemon. The terminal host and the agent go on; the agent's hooks spool.
	w.killMeshd()
	<-events.done
	if _, err := tc.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: terminal,
		Data: []byte("while meshd was down\r")})); err != nil {
		t.Fatal(err)
	}
	spool := statedir.Paths{Dir: w.state}.Spool(agent)
	w.waitFor("a hook to spool", func() bool {
		files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
		return len(files) > 0
	})

	// Restart: the same terminal and process are adopted, and the spooled hooks apply.
	w.startMeshd()
	w.waitFor("the spool drained", func() bool {
		files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
		return len(files) == 0
	})
	// The spool can be empty between two hooks of the turn: wait for its end.
	var after map[string]any
	w.waitFor("idle after the restart", func() bool {
		after = w.ctl("agent", "--agent", agent)
		return str(after, "agent", "state") == "AGENT_STATE_IDLE"
	})
	if str(after, "agent", "terminalId") != terminal {
		t.Fatalf("after restart %v", after)
	}
	list, _ = tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if len(list.Msg.Terminals) != 1 || list.Msg.Terminals[0].Pid != pid || list.Msg.Terminals[0].Exited {
		t.Fatalf("terminal after restart %v (pid was %d)", list.Msg.Terminals, pid)
	}

	// The stream resumes from the cursor it had: the adoption and the spooled turn follow.
	resumed := w.follow("--run-id", run, "--cursor", fmt.Sprintf(`{"generation":%q,"seq":%s}`, gen, seq))
	w.waitFor("the resumed stream", func() bool {
		lines := resumed.snapshot()
		if len(lines) == 0 || get(lines[0], "resumed") == nil {
			return false
		}
		adopted := false
		for _, l := range lines {
			adopted = adopted || get(l, "event", "agentAdopted") != nil
		}
		return adopted && hasSeq(resumed.states(agent), "WORKING", "IDLE")
	})

	// An exact reattach through the daemon gets the bytes written while it was down.
	open := w.ctl("open-terminal", "--agent", agent)
	// The device token from the state dir, never this process's own AGENT_MESH_TOKEN (an agent running the tests).
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: w.state}.DeviceToken())
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := client.Dial(w.state, tok, client.Options{})
	s, err := termclient.Attach(ctx, conn.HTTP, client.BaseURL+str(open, "path"), "",
		&termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_EXACT, Epoch: epoch, Offset: offset})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var replay strings.Builder
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		f, err := s.Recv(rctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if a := f.GetAttached(); a != nil && a.Reanchored {
			t.Fatalf("reanchored: %v", a)
		}
		if o := f.GetOutput(); o != nil {
			replay.Write(o.Data)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	if !strings.Contains(replay.String(), "done: while meshd was down") {
		t.Errorf("replay %q", replay.String())
	}

	// The agent reads its message as itself.
	agentTok, _ := os.ReadFile(statedir.Paths{Dir: w.state}.AgentToken(agent))
	cmd := exec.Command(filepath.Join(bins, "mesh"), "inbox")
	cmd.Env = append(w.env(), client.TokenEnv+"="+strings.TrimSpace(string(agentTok)))
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "fix the typo") {
		t.Errorf("inbox %s, %v", out, err)
	}

	// In the agent's own environment (the fake claude logged it at launch) a bare `mesh` is ours, not the decoy
	// ahead of it in the PATH meshd was given, and it knows who it is, its run folder, and its parent.
	agentEnv := lastEnvLine(t, filepath.Join(w.home, "claude-env.log"))
	meshBin := lookPath(agentEnv, "mesh")
	if meshBin != filepath.Join(bins, "mesh") {
		t.Errorf("a bare `mesh` in the agent resolves to %q, want %q (PATH %s)", meshBin, filepath.Join(bins, "mesh"), envOf(agentEnv, "PATH"))
	}
	if got, want := envOf(agentEnv, "AGENT_MESH_SHARE"), filepath.Join(w.home, "share"); got != want {
		t.Errorf("AGENT_MESH_SHARE = %q, want %q", got, want)
	}
	asAgent := func(args ...string) []byte {
		cmd := exec.Command(meshBin, args...)
		cmd.Env = agentEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mesh %v as the agent: %v\n%s", args, err, out)
		}
		return out
	}
	runDir := statedir.Paths{Dir: w.state}.RunDir(run)
	if got := strings.TrimSpace(string(asAgent("dir"))); got != runDir {
		t.Errorf("mesh dir = %q, want %q", got, runDir)
	}
	for _, f := range statedir.RunFolders {
		if st, err := os.Stat(filepath.Join(runDir, f)); err != nil || !st.IsDir() {
			t.Errorf("run folder %s: %v", f, err)
		}
	}
	var who map[string]any
	if err := json.Unmarshal(asAgent("whoami"), &who); err != nil {
		t.Fatal(err)
	}
	if str(who, "role") != "worker" || str(who, "name") != "shop-api-fix" || str(who, "run") != run || str(who, "run_dir") != runDir {
		t.Errorf("whoami %v", who)
	}
	// It writes under its run folder and reports with `mesh send parent`, the positional form. A device started
	// this agent, so it has no parent: that refusal is the daemon reading `parent` as the recipient.
	report := filepath.Join(runDir, "reports", "shop-api-fix.md")
	os.WriteFile(report, []byte("done"), 0o600)
	send := exec.Command(meshBin, "send", "parent", "--type", "info", "--body", "@"+report)
	send.Env = agentEnv
	if out, err := send.CombinedOutput(); err == nil || !strings.Contains(string(out), "no parent agent") {
		t.Errorf("mesh send parent: %v\n%s", err, out)
	}

	// A model change reopens the same conversation: a switch to another tier, then a stop and a resume for a
	// task it plans (the strong tier, under the deny list).
	session := str(after, "agent", "sessionId")
	w.ctl("switch", "--agent", agent, "--tier", "light", "--why", "e2e")
	var moved map[string]any
	w.waitFor("the switch's reopen", func() bool {
		moved = w.ctl("agent", "--agent", agent)
		return str(moved, "agent", "model") == "haiku" && str(moved, "agent", "terminalId") != terminal &&
			str(moved, "agent", "state") == "AGENT_STATE_IDLE"
	})
	if str(moved, "agent", "sessionId") != session {
		t.Fatalf("the switch started another conversation: %v", moved)
	}
	w.ctl("stop", "--agent", agent)
	w.waitFor("stopped", func() bool { return w.agentState(agent) == "AGENT_STATE_EXITED" })
	w.ctl("resume", "--agent", agent, "--task", "plan the next fix", "--plan", "--tier", "strong")
	w.waitFor("the resume", func() bool {
		a := w.ctl("agent", "--agent", agent)
		return str(a, "agent", "model") == "opus" && str(a, "agent", "phase") == "AGENT_PHASE_PLANNING" &&
			get(a, "agent", "denyEdits") == true && str(a, "agent", "state") == "AGENT_STATE_IDLE" &&
			str(a, "agent", "sessionId") == session
	})

	// Retire keeps the conversation; a message to a retired agent resumes it; stopping the run retires what is
	// left and closes it.
	w.ctl("retire", "--agent", agent)
	w.waitFor("retired", func() bool { return w.agentState(agent) == "AGENT_STATE_RETIRED" })
	w.ctl("send", "--to", agent, "--type", "task", "--body", "one more thing")
	w.waitFor("resumed by the message", func() bool {
		a := w.ctl("agent", "--agent", agent)
		return str(a, "agent", "state") == "AGENT_STATE_IDLE" && str(a, "agent", "sessionId") == session
	})
	stopped := w.ctl("stop-run", "--run-id", run)
	if str(stopped, "run", "status") != "RUN_STATUS_CLOSED" || w.agentState(agent) != "AGENT_STATE_RETIRED" {
		t.Fatalf("stop-run %v", stopped)
	}
}

func TestAgyEndToEnd(t *testing.T) {
	root := shortdir.New(t)
	w := &world{t: t, state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.stopAll)
	repo := filepath.Join(root, "shop-api")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, w.env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	w.startMeshd()
	run := str(w.ctl("start-run", "--title", "agy-e2e"), "run", "id")
	events := w.follow("--run-id", run)
	w.waitFor("snapshot", func() bool { return len(events.snapshot()) > 0 })

	spawned := w.ctl("spawn", "--run-id", run, "--repo", repo, "--slug", "fix", "--kind", "agy", "--role", "worker", "--tier", "mid")
	agent := str(spawned, "agent", "id")
	worktree := str(spawned, "agent", "worktree")
	w.waitFor("agy idle from settled output", func() bool { return w.agentState(agent) == "AGENT_STATE_IDLE" })

	if out, err := exec.Command("git", "-C", worktree, "status", "--porcelain").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("agy hooks.json dirtied worktree: %q (%v)", out, err)
	}

	w.ctl("send", "--to", agent, "--type", "task", "--body", "check README")
	w.waitFor("agy working then idle", func() bool { return hasSeq(events.states(agent), "IDLE", "WORKING", "IDLE") })
	before := w.ctl("agent", "--agent", agent)
	terminal := str(before, "agent", "terminalId")
	session := str(before, "agent", "sessionId")
	if session == "" {
		t.Fatal("expected conversationId recorded after first agy turn")
	}

	// Switch to light tier reopens the same conversation on flash.
	w.ctl("switch", "--agent", agent, "--tier", "light", "--why", "e2e")
	w.waitFor("agy switch reopen", func() bool {
		a := w.ctl("agent", "--agent", agent)
		return str(a, "agent", "model") == "flash" && str(a, "agent", "terminalId") != terminal &&
			str(a, "agent", "state") == "AGENT_STATE_IDLE" && str(a, "agent", "sessionId") == session
	})

	stopped := w.ctl("stop-run", "--run-id", run)
	if str(stopped, "run", "status") != "RUN_STATUS_CLOSED" || w.agentState(agent) != "AGENT_STATE_RETIRED" {
		t.Fatalf("stop-run %v", stopped)
	}
}

func TestRelayAndPairingEndToEnd(t *testing.T) {
	root := shortdir.New(t)
	w := &world{t: t, state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.stopAll)

	repo := filepath.Join(root, "shop-api")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, w.env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	// Start the real mesh-relay binary on loopback.
	secret := relay.NewSecret()
	secretFile := filepath.Join(root, "relay.token")
	if err := fsutil.WriteAtomic(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reserve free loopback ports for the relay's HTTP and raw TCP listeners, then close before starting mesh-relay.
	pickAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	}
	relayHTTPAddr := pickAddr()
	relayTCPAddr := pickAddr()
	relayLog := &lockedBuffer{}
	rcmd := exec.Command(filepath.Join(bins, "mesh-relay"),
		"--listen", relayHTTPAddr,
		"--client-listen", relayTCPAddr,
		"--token-file", secretFile,
		"--max-lifetime", "3m",
	)
	rcmd.Env = w.env()
	rcmd.Stdout, rcmd.Stderr = relayLog, relayLog
	if err := rcmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rcmd.Process != nil && rcmd.ProcessState == nil {
			_ = rcmd.Process.Signal(syscall.SIGTERM)
			_ = rcmd.Wait()
		}
	})
	w.waitFor("mesh-relay /healthz", func() bool {
		resp, err := http.Get("http://" + relayHTTPAddr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// Start real meshd wired to mesh-relay + opt-in --listen TLS.
	w.log = &lockedBuffer{}
	cmd := exec.Command(filepath.Join(bins, "meshd"),
		"--state", w.state,
		"--term-launch", "detached",
		"--max-lifetime", "3m",
		"--cli", "claude="+filepath.Join(bins, "claude"),
		"--listen", "127.0.0.1:0",
		"--relay-url", "http://"+relayHTTPAddr,
		"--relay-tcp", relayTCPAddr,
		"--relay-token-file", secretFile,
		"--host-name", "e2e-host",
		"--agent-env", "HOME="+w.home,
		"--agent-env", "PATH="+os.Getenv("PATH"),
	)
	cmd.Env = w.env()
	cmd.Stdout, cmd.Stderr = w.log, w.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.meshd = cmd
	w.waitFor("meshd up", func() bool {
		info, err := statedir.ReadInfo(w.state)
		return err == nil && info.PID == cmd.Process.Pid && info.Listen != "" && info.CertFingerprint != ""
	})

	// Create a pairing via real mesh pair.
	pairOut := w.ctl("pair", "--name", "e2e-phone", "--format", "json")
	p, err := pairing.ParseURI(str(pairOut, "uri"))
	if err != nil {
		t.Fatalf("ParseURI(%q): %v", str(pairOut, "uri"), err)
	}

	// Wait until meshd has registered with mesh-relay, then redeem and call APIs through the relay binary.
	var red *connect.Response[v1.RedeemPairingResponse]
	relayHC, err := relay.NewTCPHTTPClient(p.RelayTCP, p.HostID, p.RelayClientToken, p.CertFingerprint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer relayHC.CloseIdleConnections()
	pc := agentmeshv1connect.NewPairingServiceClient(relayHC, relay.BaseURL)
	w.waitFor("pairing redeemed over relay", func() bool {
		r, err := pc.RedeemPairing(context.Background(), connect.NewRequest(&v1.RedeemPairingRequest{Token: p.Token}))
		if err != nil {
			return false
		}
		red = r
		return true
	})

	// Spawn a fake claude locally and attach to its terminal WebSocket *through mesh-relay*!
	run := str(w.ctl("start-run", "--title", "relay-e2e"), "run", "id")
	spawned := w.ctl("spawn", "--run-id", run, "--repo", repo, "--slug", "fix", "--kind", "claude", "--role", "worker", "--tier", "mid")
	agentID := str(spawned, "agent", "id")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rm := agentmeshv1connect.NewMeshServiceClient(relayHC, relay.BaseURL,
		connect.WithInterceptors(auth.ClientToken(red.Msg.DeviceToken)))
	open, err := rm.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Agent: agentID, Input: true}))
	if err != nil {
		t.Fatalf("OpenTerminal over relay: %v", err)
	}
	wsHC, err := relay.NewTCPHTTPClient(p.RelayTCP, p.HostID, p.RelayClientToken, p.CertFingerprint, false)
	if err != nil {
		t.Fatal(err)
	}
	defer wsHC.CloseIdleConnections()
	s, err := termclient.Attach(ctx, wsHC, relay.BaseURL+open.Msg.Path, "",
		&termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Attach terminal over relay: %v", err)
	}
	var gotAttached, gotReplayDone bool
	for !gotReplayDone {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("terminal Recv over relay: %v", err)
		}
		if f.GetAttached() != nil {
			gotAttached = true
		}
		if f.GetReplayDone() != nil {
			gotReplayDone = true
		}
	}
	s.Close()
	if !gotAttached {
		t.Fatal("missing Attached frame over relay")
	}

	// Verify direct TLS route also works against --listen.
	directHC, err := relay.NewDirectHTTPClient(p.CertFingerprint, true)
	if err != nil {
		t.Fatal(err)
	}
	dm := agentmeshv1connect.NewMeshServiceClient(directHC, p.DirectURL,
		connect.WithInterceptors(auth.ClientToken(red.Msg.DeviceToken)))
	if _, err := dm.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Fatalf("direct TLS Hello: %v", err)
	}

	// Revoke via mesh revoke-device and verify remote client loses access.
	w.ctl("revoke-device", "--device-id", red.Msg.DeviceId)
	if _, err := dm.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked device Hello = %v, want Unauthenticated", err)
	}
}

// lastEnvLine is the environment the fake claude logged at its newest launch.
func lastEnvLine(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var env []string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func envOf(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// lookPath finds a command the way the agent's shell would: by that environment's PATH, not the test's.
func lookPath(env []string, name string) string {
	for _, dir := range filepath.SplitList(envOf(env, "PATH")) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// An attached `mesh open` survives meshd being killed and started again (an install, a drop-in change): it prints one
// status line, asks the new meshd for a fresh way in, repaints, and shows what the agent says from then on.
func TestMeshOpenReattachesAfterMeshdRestarts(t *testing.T) {
	root := shortdir.New(t)
	w := &world{t: t, state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.stopAll)
	repo := filepath.Join(root, "shop-api")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, w.env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	w.startMeshd()
	run := str(w.ctl("start-run", "--title", "open"), "run", "id")
	agent := str(w.ctl("spawn", "--run-id", run, "--repo", repo, "--slug", "watched", "--kind", "claude", "--role", "worker", "--tier", "mid"), "agent", "id")
	w.waitFor("idle", func() bool { return w.agentState(agent) == "AGENT_STATE_IDLE" })
	terminal := str(w.ctl("agent", "--agent", agent), "agent", "terminalId")

	cmd := exec.Command(filepath.Join(bins, "mesh"), "open", agent)
	cmd.Env = w.env()
	master, err := pty.Start(cmd)
	if err != nil {
		t.Skip("no pty: ", err)
	}
	defer master.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	var seen lockedBuffer
	go io.Copy(&seen, master)
	waitSeen := func(what, want string) {
		t.Helper()
		w.waitFor(what, func() bool { return strings.Contains(seen.String(), want) })
	}
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if _, err := tc.API.WriteTerminal(context.Background(), connect.NewRequest(&termv1.WriteTerminalRequest{Id: terminal, Data: []byte(text)})); err != nil {
			t.Fatal(err)
		}
	}
	waitSeen("the attach", "Attached to")
	write("before the restart\r")
	waitSeen("output before the restart", "before the restart")

	w.killMeshd()
	waitSeen("the status line", "meshd went away, reconnecting")
	w.startMeshd()
	write("after the restart\r")
	waitSeen("output after the restart", "after the restart")
	if n := strings.Count(seen.String(), "meshd went away, reconnecting"); n != 1 {
		t.Errorf("%d status lines:\n%s", n, seen.String())
	}
	select {
	case err := <-exited:
		t.Fatalf("mesh open ended: %v\n%s", err, seen.String())
	default:
	}
	master.Write([]byte{0x1d}) // Ctrl-]
	select {
	case err := <-exited:
		if err != nil || !strings.Contains(seen.String(), "[detached]") {
			t.Errorf("detach: %v\n%s", err, seen.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Ctrl-] did not detach")
	}
}
