// Package e2e runs the real binaries together: meshd, mesh-term and meshctl, with a fake claude in the PTY.
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

var bins string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "meshd-e2e-bins-")
	if err != nil {
		panic(err)
	}
	for out, pkg := range map[string]string{"meshd": "./cmd/meshd", "mesh-term": "./cmd/mesh-term",
		"meshctl": "./cmd/meshctl", "claude": "./internal/testutil/fakeclaude"} {
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
	cmd := exec.Command(filepath.Join(bins, "meshd"), "--state", w.state, "--term-launch", "detached",
		"--max-lifetime", "3m", "--cli", "claude="+filepath.Join(bins, "claude"),
		"--agent-env", "HOME="+w.home, "--agent-env", "PATH="+os.Getenv("PATH"))
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

// ctl runs meshctl and decodes its JSON output.
func (w *world) ctl(args ...string) map[string]any {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(bins, "meshctl"), args...)
	cmd.Env = w.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		w.t.Fatalf("meshctl %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		w.t.Fatalf("meshctl %s printed %q: %v", strings.Join(args, " "), out, err)
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

// follower runs `meshctl events` and collects its lines.
type follower struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	lines []map[string]any
	done  chan struct{}
}

func (w *world) follow(args ...string) *follower {
	w.t.Helper()
	f := &follower{done: make(chan struct{})}
	f.cmd = exec.Command(filepath.Join(bins, "meshctl"), append([]string{"events"}, args...)...)
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

func TestEndToEnd(t *testing.T) {
	root := t.TempDir()
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
	if get(set, "setting", "value") != 3.0 || get(set, "setting", "isSet") != true || get(set, "setting", "default") != 8.0 {
		t.Fatalf("set %v", set)
	}
	if got := w.ctl("settings", "--prefix", "caps."); len(get(got, "settings").([]any)) != 1 {
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
	if reset := w.ctl("unset", "--key", "caps.agents"); get(reset, "setting", "value") != 8.0 || get(reset, "setting", "isSet") == true {
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
	w.waitFor("idle in `meshctl agents`", func() bool {
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
	cmd := exec.Command(filepath.Join(bins, "meshctl"), "inbox")
	cmd.Env = append(w.env(), client.TokenEnv+"="+strings.TrimSpace(string(agentTok)))
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "fix the typo") {
		t.Errorf("inbox %s, %v", out, err)
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
}
