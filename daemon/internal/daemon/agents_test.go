package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/cleanenv"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// bins holds mesh-term, mesh and the fake claude, built once for the package's tests.
var bins string

func TestMain(m *testing.M) {
	cleanenv.Agent()
	shortdir.Sweep(3 * time.Hour) // what a killed or timed-out run left: its cleanups never ran
	dir, err := os.MkdirTemp("", "meshd-bins-")
	if err != nil {
		panic(err)
	}
	for out, pkg := range map[string]string{"mesh-term": "./cmd/mesh-term", "mesh": "./cmd/mesh",
		"claude": "./internal/testutil/fakeclaude", "agy": "./internal/testutil/fakeagy", "opencode": "./internal/testutil/fakeopencode", "copilot": "./internal/testutil/fakecopilot"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, out), pkg)
		cmd.Dir = "../.."
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

type agentsHarness struct {
	*harness
	home string
}

// agentConfig is the daemon's agent settings for a test. Its memory guard reads files of its own, with plenty of memory and
// no pressure: the host's /proc/pressure/memory and /proc/meminfo are the machine's (several gates at once push its
// pressure over `memory_pressure_max` and refused 121 spawns at once), and a test of the guard points the paths at its own
// numbers with a tweak.
func agentConfig(t *testing.T, home string) daemon.AgentConfig {
	procDir, err := shortdir.Make("proc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(procDir) })
	mem := writeProc(t, procDir, "meminfo", meminfoText(24000, 4000, 32000, 4000))
	psi := writeProc(t, procDir, "pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	return daemon.AgentConfig{
		MeminfoPath: mem, PressurePath: psi,
		TermLaunch: termclient.LaunchDetached, TermBinary: filepath.Join(bins, "mesh-term"),
		TermArgs: []string{"--max-lifetime", "3m"}, Mesh: filepath.Join(bins, "mesh"),
		Binaries:      map[string]string{"claude": filepath.Join(bins, "claude"), "agy": filepath.Join(bins, "agy"), "opencode": filepath.Join(bins, "opencode")},
		LookPath:      func(string) (string, bool) { return "", false },
		Env:           map[string]string{"HOME": home, "PATH": os.Getenv("PATH")},
		DeliveryDelay: 20 * time.Millisecond, SpoolEvery: 100 * time.Millisecond, TypingQuiet: 1500 * time.Millisecond,
		ReadySettle: 150 * time.Millisecond,
		Sys: &daemon.SystemOps{
			ProcTable:   func() ([]proc.Info, error) { return nil, nil },
			TaskIDs:     func(pid int) []int { return []int{pid} },
			MachineCPUs: func() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} },
			GetAffinity: func(int) ([]int, error) { return []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, nil },
			SetAffinity: func(int, []int) error { return nil },
			SignalProc:  func(int, uint64, syscall.Signal) error { return nil },
			ProcAlive:   func(int, uint64) bool { return false },
			RunScope: func(context.Context, []string) error {
				return fmt.Errorf("systemd scopes are disabled in tests")
			},
		},
	}
}

func startWithAgents(t *testing.T, state, home string, tweak ...func(*daemon.AgentConfig)) *agentsHarness {
	t.Helper()
	cfg := agentConfig(t, home)
	for _, f := range tweak {
		f(&cfg)
	}
	d, err := daemon.New(daemon.Config{StateDir: state, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: cfg})
	if err != nil {
		t.Fatal(err)
	}
	// Registered before serve's own cleanup, so it runs after the daemon stopped: the detached mesh-term keeps
	// writing into the state dir until it exits, and t.TempDir's cleanup must not race it.
	t.Cleanup(func() { stopTermHost(t, state) })
	return &agentsHarness{harness: serve(t, d, state), home: home}
}

// serve runs an already made daemon like start does.
func serve(t *testing.T, d *daemon.Daemon, state string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, d: d, state: state, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- d.Serve(ctx) }()
	waitFor(t, "meshd.json", func() bool {
		info, err := daemon.ReadInfo(state)
		return err == nil && info.InstanceID != ""
	})
	tok, _ := client.Token(state)
	h.device, _ = client.Dial(state, tok, client.Options{})
	t.Cleanup(h.stop)
	return h
}

// stopTermHost ends the detached mesh-term of a state dir, as its owner would, and waits until it is gone: it
// writes into the state dir until then.
func stopTermHost(t *testing.T, state string) {
	b, err := os.ReadFile(termhost.Paths{Dir: statedir.Paths{Dir: state}.Term()}.Info())
	if err != nil {
		return
	}
	var info termhost.Info
	json.Unmarshal(b, &info)
	ticks, err := proc.StartTicks(info.PID)
	if err != nil {
		return
	}
	proc.Signal(info.PID, ticks, syscall.SIGTERM)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if now, err := proc.StartTicks(info.PID); err != nil || now != ticks {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("mesh-term %d did not exit", info.PID)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, 15*time.Second, what, cond)
}

// waitForWithin is waitFor with its own limit, for a wait that holds a daemon timeout of its own (a compaction's).
func waitForWithin(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	waitForWithinSaying(t, limit, what, cond, nil)
}

// waitForWithinSaying is waitForWithin that adds what `say` returns to the failure: the state a flake that is seen once
// in a gate and never alone needs to be told apart from the next one.
func waitForWithinSaying(t *testing.T, limit time.Duration, what string, cond func() bool, say func() string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			if say != nil {
				t.Fatalf("timed out waiting for %s: %s", what, say())
			}
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop-api")
	os.MkdirAll(dir, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func (h *agentsHarness) agent(id string) *v1.Agent {
	h.t.Helper()
	res, err := h.device.Mesh.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{Agent: id}))
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Msg.Agent
}

func (h *agentsHarness) waitState(id string, want v1.AgentState) *v1.Agent {
	h.t.Helper()
	var a *v1.Agent
	waitFor(h.t, "state "+want.String(), func() bool {
		a = h.agent(id)
		return a.State == want
	})
	return a
}

func (h *agentsHarness) spawn(run, repo, slug, task string) *v1.Agent {
	h.t.Helper()
	res, err := h.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		Meta: meta("spawn-" + slug), RunId: run, Repo: repo, Slug: slug, Task: task, Tier: "mid"}))
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Msg.Agent
}

// stateChanges collects the agent's state transitions from an event stream.
func stateChanges(t *testing.T, s *connect.ServerStreamForClient[v1.SubscribeEventsResponse], agent string, until v1.AgentState, n int) []v1.AgentState {
	t.Helper()
	var got []v1.AgentState
	seen := 0
	for {
		ev := recv(t, s).GetEvent()
		if ch := ev.GetAgentStateChanged(); ch != nil && ev.AgentId == agent {
			got = append(got, ch.To)
			if ch.To == until {
				seen++
				if seen == n {
					return got
				}
			}
		}
	}
}

func TestSpawnRunsTheAgentAndItsHooksDriveItsState(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	stream, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id}))
	defer stream.Close()
	recv(t, stream) // snapshot

	repo := gitRepo(t)
	a := h.spawn(run.Id, repo, "fix-typo", "fix the typo in README")
	if a.Name != "shop-api-fix-typo" || a.Branch != "mesh/"+run.Id+"/fix-typo" || a.TerminalId == "" || a.SessionId == "" {
		t.Fatalf("spawned %v", a)
	}
	if out, err := exec.Command("git", "-C", a.Worktree, "branch", "--show-current").Output(); err != nil ||
		strings.TrimSpace(string(out)) != a.Branch {
		t.Fatalf("worktree branch %q, %v", out, err)
	}
	// SessionStart makes it idle; the task's doorbell then starts a turn, and Stop ends it.
	got := stateChanges(t, stream, a.Id, v1.AgentState_AGENT_STATE_IDLE, 2)
	want := []v1.AgentState{v1.AgentState_AGENT_STATE_IDLE, v1.AgentState_AGENT_STATE_WORKING, v1.AgentState_AGENT_STATE_IDLE}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("states %v, want %v", got, want)
	}
	// A retried spawn returns the same agent and starts nothing.
	again := h.spawn(run.Id, repo, "fix-typo", "")
	if again.Id != a.Id {
		t.Errorf("a retried spawn made %s", again.Id)
	}
	// The agent reads its task with its own token (the one its hooks use).
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: state}.AgentToken(a.Id))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := h.as(tok).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{}))
	if err != nil || len(inbox.Msg.Messages) != 1 || inbox.Msg.Messages[0].DeliveredAt == nil ||
		inbox.Msg.Messages[0].Type != v1.MessageType_MESSAGE_TYPE_TASK {
		t.Fatalf("inbox %v, %v", inbox, err)
	}

	// The agent doesn't inherit an ignored SIGHUP from the host: it must die when its terminal hangs up.
	tc, _ := termclient.New(statedir.Paths{Dir: state}.Term())
	list, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", list.Msg.Terminals[0].Pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if mask, ok := strings.CutPrefix(line, "SigIgn:"); ok {
			var bits uint64
			fmt.Sscanf(strings.TrimSpace(mask), "%x", &bits)
			if bits&1 != 0 {
				t.Errorf("the agent ignores SIGHUP (SigIgn %s)", strings.TrimSpace(mask))
			}
		}
	}

	// While a user who can type is typing, a doorbell waits; it rings once they stop.
	ictx, icancel := context.WithTimeout(ctx, 10*time.Second)
	user := h.openTerminal(ictx, a, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	for {
		f, err := user.Recv(ictx)
		if err != nil {
			t.Fatal(err)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	user.Ready(ictx)
	waitFor(t, "the user counted as attached", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return l.Msg.Terminals[0].InputClients == 1
	})
	user.Input(ictx, []byte("x\x7f")) // a keystroke, taken back: the user is typing
	waitFor(t, "the keystroke counted", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return l.Msg.Terminals[0].LastClientInput != nil
	})
	if _, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "fyi"})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if peek, _ := h.as(tok).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Peek: true})); len(peek.Msg.Messages) != 1 ||
		peek.Msg.Messages[0].DeliveredAt != nil {
		t.Errorf("the doorbell rang into a user's typing: %v", peek.Msg.Messages)
	}
	// Still attached but quiet: the doorbell rings.
	waitFor(t, "the doorbell once the user stopped typing", func() bool {
		peek, _ := h.as(tok).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Peek: true}))
		return len(peek.Msg.Messages) == 1 && peek.Msg.Messages[0].DeliveredAt != nil
	})
	user.Close()
	icancel()
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

	// A permission prompt is needs-input, and answering it clears it.
	h.typeInto(a, "/ask\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_NEEDS_INPUT)
	h.typeInto(a, "y\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

	// A worker can't start another worker, but can start a reviewer.
	worker := h.as(tok)
	if _, err := worker.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repo, Slug: "more",
		Role: v1.AgentRole_AGENT_ROLE_WORKER})); code(err) != connect.CodePermissionDenied {
		t.Errorf("worker starting a worker: %v", err)
	}
	rev, err := worker.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repo, Slug: "review",
		Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Tier: "mid"}))
	if err != nil || rev.Msg.Agent.ParentId != a.Id || rev.Msg.Agent.RunId != run.Id {
		t.Fatalf("worker starting a reviewer: %v, %v", rev, err)
	}

	// Stopping the reviewer records its exit and revokes its token.
	if _, err := worker.StopAgent(ctx, connect.NewRequest(&v1.StopAgentRequest{Agent: rev.Msg.Agent.Id})); err != nil {
		t.Fatal(err)
	}
	h.waitState(rev.Msg.Agent.Id, v1.AgentState_AGENT_STATE_EXITED)
	revTok, _ := fsutil.ReadSecret(statedir.Paths{Dir: state}.AgentToken(rev.Msg.Agent.Id))
	if _, err := h.as(revTok).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("an exited agent's token still works: %v", err)
	}
	// The reviewer left with no report, so the daemon told its parent (a message that rings the parent's doorbell
	// and starts a turn): the crash is typed once that turn has ended, not into it.
	var told *v1.Message
	waitFor(t, "the parent told of the reviewer's exit, and rung", func() bool {
		peek, _ := h.as(tok).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Peek: true}))
		for _, m := range peek.Msg.Messages {
			if strings.HasPrefix(m.Body, "[mesh alert exited "+rev.Msg.Agent.Id+"]") && m.DeliveredAt != nil {
				told = m
			}
		}
		return told != nil
	})
	waitFor(t, "the turn the alert started to end", func() bool {
		cur := h.agent(a.Id)
		return cur.State == v1.AgentState_AGENT_STATE_IDLE && cur.LastActivity != nil &&
			cur.LastActivity.AsTime().After(told.DeliveredAt.AsTime())
	})
	// A crash is recorded with its code.
	h.typeInto(a, "/crash\r")
	if got := h.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED); got.ExitCode != 7 {
		t.Errorf("exit code %d", got.ExitCode)
	}
}

// typeInto types as the user would, through the daemon's terminal proxy.
func (h *agentsHarness) typeInto(a *v1.Agent, text string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := h.openTerminal(ctx, a, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	defer s.Close()
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			h.t.Fatal(err)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	s.Ready(ctx)
	if err := s.Input(ctx, []byte(text)); err != nil {
		h.t.Fatal(err)
	}
	// The host drops a closed stream's client a moment after the close: until it has, the terminal counts as having someone
	// typing in it (`InputClients`), which holds a prompt as "typing" instead of reading the box. Wait for it, so what the
	// test does next sees the terminal as the user left it.
	s.Close()
	tc, err := termclient.New(statedir.Paths{Dir: h.state}.Term())
	if err != nil {
		h.t.Fatal(err)
	}
	waitFor(h.t, "the typist's stream dropped by the host", func() bool {
		t := h.terminalOf(ctx, tc, a)
		return t == nil || t.InputClients == 0
	})
}

// terminalOf is the host's own view of an agent's terminal (nil when it lists none or the host can't be asked): what the
// daemon's "someone is typing" reads, observed without changing anything.
func (h *agentsHarness) terminalOf(ctx context.Context, tc *termclient.Client, a *v1.Agent) *termv1.Terminal {
	l, err := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return nil
	}
	for _, t := range l.Msg.Terminals {
		if t.Id == a.TerminalId {
			return t
		}
	}
	return nil
}

func (h *agentsHarness) openTerminal(ctx context.Context, a *v1.Agent, input bool, att *termv1.Attach) *termclient.Stream {
	h.t.Helper()
	res, err := h.device.Mesh.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Agent: a.Id, Input: input}))
	if err != nil {
		h.t.Fatal(err)
	}
	if res.Msg.Input != input {
		h.t.Fatalf("input granted %v", res.Msg.Input)
	}
	s, err := termclient.Attach(ctx, h.device.HTTP, client.BaseURL+res.Msg.Path, "", att)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestTheTerminalSurvivesADaemonRestartAndSpooledHooksApply(t *testing.T) {
	state := shortdir.State(t)
	home := t.TempDir()
	h := startWithAgents(t, state, home)
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "keep", "")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	before := h.agent(a.Id)

	// Read the terminal up to now, and remember where we were.
	s := h.openTerminal(ctx, a, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT})
	var epoch, offset uint64
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if att := f.GetAttached(); att != nil {
			epoch = att.Epoch
		}
		if d := f.GetReplayDone(); d != nil {
			offset = d.Offset
			break
		}
	}
	s.Close()
	evs, _ := h.d.Store().Read(ctx).EventsAfter(0, "", 1000)
	cursor := &v1.Cursor{Generation: evs[0].Generation, Seq: evs[len(evs)-1].Seq}

	h.stop()
	// With the daemon down, the agent gets input straight from the host; its hooks spool.
	tc, err := termclient.New(statedir.Paths{Dir: state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
		Data: []byte("while you were out\r")})); err != nil {
		t.Fatal(err)
	}
	spool := statedir.Paths{Dir: state}.Spool(a.Id)
	waitFor(t, "four spooled hooks", func() bool {
		files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
		return len(files) >= 4
	})

	h2 := startWithAgents(t, state, home)
	after := h2.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if after.TerminalId != before.TerminalId {
		t.Errorf("terminal %s, was %s", after.TerminalId, before.TerminalId)
	}
	waitFor(t, "the spool drained", func() bool {
		files, _ := filepath.Glob(filepath.Join(spool, "*.json"))
		return len(files) == 0
	})
	list, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if len(list.Msg.Terminals) != 1 || list.Msg.Terminals[0].Exited {
		t.Fatalf("terminals after restart %v", list.Msg.Terminals)
	}

	// The event stream resumes from the old cursor and shows the adoption and the spooled turn.
	es, _ := h2.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Cursor: cursor}))
	defer es.Close()
	if recv(t, es).GetResumed() == nil {
		t.Fatal("not resumed")
	}
	var adopted, working, idle bool
	for !(adopted && working && idle) {
		ev := recv(t, es).GetEvent()
		adopted = adopted || ev.GetAgentAdopted() != nil
		if ch := ev.GetAgentStateChanged(); ch != nil {
			working = working || ch.To == v1.AgentState_AGENT_STATE_WORKING
			idle = idle || (working && ch.To == v1.AgentState_AGENT_STATE_IDLE)
		}
	}

	// An exact reattach from before the restart gets exactly what was written since.
	s2 := h2.openTerminal(ctx, a, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_EXACT, Epoch: epoch, Offset: offset})
	defer s2.Close()
	var replay strings.Builder
	for {
		f, err := s2.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if att := f.GetAttached(); att != nil && (att.Reanchored || att.Offset != offset) {
			t.Fatalf("not an exact reattach: %v", att)
		}
		if o := f.GetOutput(); o != nil {
			replay.Write(o.Data)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	if !strings.Contains(replay.String(), "done: while you were out") {
		t.Errorf("replay %q", replay.String())
	}
}

func TestAnAgentWhoseTerminalVanishedIsMarkedLost(t *testing.T) {
	state := shortdir.State(t)
	home := t.TempDir()
	h := startWithAgents(t, state, home)
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "gone", "")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	// What a restart brings back by itself is restart_test.go's: here the agent stays lost.
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta(""), Key: "restart.resume", Value: val(t, false)})); err != nil {
		t.Fatal(err)
	}
	h.stop()
	// The terminal host dies with the daemon down (a reboot): its terminals are gone.
	stopTermHost(t, state)
	waitFor(t, "the host to stop", func() bool {
		return !fsutil.Held(termhost.Paths{Dir: statedir.Paths{Dir: state}.Term()}.Lock())
	})
	h2 := startWithAgents(t, state, home)
	t.Cleanup(func() { stopTermHost(t, state) })
	got := h2.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	evs, _ := h2.d.Store().Read(context.Background()).EventsAfter(0, run.Id, 1000)
	lost := false
	for _, ev := range evs {
		lost = lost || ev.GetAgentExited().GetReason() == "lost"
	}
	if !lost || got.ExitCode != -1 {
		t.Errorf("exit %d, lost %v", got.ExitCode, lost)
	}
	// The stored form tells it from a stop whose process was already gone (also exit code -1): a client reloading it must.
	if got.StateDetail != "lost" {
		t.Errorf("stored detail %q, want lost", got.StateDetail)
	}
}

func TestAStopRecordedForAProcessAlreadyGoneIsStoredAsStopped(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "halt", "")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if err := h.d.RecordStopped(context.Background(), a.Id); err != nil {
		t.Fatal(err)
	}
	got, err := h.d.Store().Read(context.Background()).GetAgent(a.Id)
	if err != nil || got.State != v1.AgentState_AGENT_STATE_EXITED || got.ExitCode != -1 || got.StateDetail != "stopped" {
		t.Errorf("stopped agent %v, %v: want exited, -1, detail stopped", got, err)
	}
}

func TestACLIThatExitsAtOnceIsRecordedExited(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) { c.Binaries["claude"] = "/bin/false" })
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "quick", "")
	got := h.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if got.ExitCode != 1 || got.TerminalId == "" {
		t.Errorf("exited agent %v", got)
	}
}

func TestAFailedLaunchFreesItsSlug(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) { c.Binaries["claude"] = "/nonexistent/claude" })
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	repo := gitRepo(t)
	for i := 0; i < 2; i++ {
		_, err := h.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
			Meta: meta("same"), RunId: run.Id, Repo: repo, Slug: "fix", Tier: "mid"}))
		if code(err) != connect.CodeFailedPrecondition {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "mesh/*").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("branches left: %s", out)
	}
	agents, _ := h.device.Mesh.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id}))
	if len(agents.Msg.Agents) != 0 {
		t.Errorf("agents left: %v", agents.Msg.Agents)
	}
	if _, err := h.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("no repo: %v", err)
	}
}

func TestAdoptAcknowledgesTheTerminalOfAnAgentAlreadyRecordedExited(t *testing.T) {
	// The host's watch replays exited terminals, which acknowledges this one too, so this passes either way...
	adoptAcknowledgesAnExitedTerminal(t)
}

func TestOnlyAdoptAcknowledgesTheTerminalOfAnAgentAlreadyRecordedExited(t *testing.T) {
	// ...and with the watch left out, only adopt can.
	adoptAcknowledgesAnExitedTerminal(t, func(c *daemon.AgentConfig) { c.NoTerminalWatch = true })
}

func adoptAcknowledgesAnExitedTerminal(t *testing.T, tweak ...func(*daemon.AgentConfig)) {
	t.Helper()
	state := shortdir.State(t)
	home := t.TempDir()
	h := startWithAgents(t, state, home)
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "gone", "")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	h.stop()

	// The crash this stands for: the exit was committed, but the daemon died before it acknowledged the host.
	tc, err := termclient.New(statedir.Paths{Dir: state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: a.TerminalId, Signal: int32(syscall.SIGKILL)})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the host to hold the exited terminal", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return len(l.Msg.Terminals) == 1 && l.Msg.Terminals[0].Exited
	})
	st, err := store.Open(statedir.Paths{Dir: state}.DB())
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, nil, "", func(tx *store.Tx) error {
		ag, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		ag.State = v1.AgentState_AGENT_STATE_EXITED
		return tx.PutAgent(ag)
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}

	startWithAgents(t, state, home, tweak...)
	waitFor(t, "the host to forget the exited terminal", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return len(l.Msg.Terminals) == 0
	})
}

// The tool an agent's hooks report changes no state, so it writes no event; a live subscriber gets it as an
// ephemeral frame, and a resumed one gets no replay of it.
func TestToolActivityReachesLiveSubscribersAsEphemeralFrames(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	stream, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id, Activity: true}))
	defer stream.Close()
	// One that doesn't ask for activity (`mesh events`) is told state changes only.
	quiet, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id}))
	defer quiet.Close()
	recv(t, quiet) // snapshot
	gen := recv(t, stream).GetSnapshot().Cursor.Generation
	a := h.spawn(run.Id, gitRepo(t), "tool", "do a thing")

	var got *v1.AgentActivity
	for got == nil {
		f := recv(t, stream)
		if act := f.GetActivity(); act != nil && act.AgentId == a.Id {
			got = act
		}
	}
	if got.RunId != run.Id || got.Detail != "Bash" || got.Time == nil {
		t.Errorf("activity %v", got)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	for !recvIdle(t, quiet) {
	}

	// A subscriber that resumes from the start sees the state events, and no activity frame.
	again, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id, Activity: true, Cursor: &v1.Cursor{Generation: gen, Seq: 0}}))
	defer again.Close()
	for i := 0; i < 5; i++ {
		if recv(t, again).GetActivity() != nil {
			t.Fatal("a resumed stream replayed an activity frame")
		}
	}
}

// AgentSpawned is written before the terminal exists, so the stream says which terminal it is once the launch recorded
// it: an app learns it from the event, not from the next listing of the agents.
func TestALaunchTellsTheStreamWhichTerminalTheAgentGot(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	stream, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id}))
	defer stream.Close()
	recv(t, stream) // snapshot
	a := h.spawn(run.Id, gitRepo(t), "term", "")

	var got string
	for got == "" {
		ev := recv(t, stream).GetEvent()
		if ad := ev.GetAgentAdopted(); ad != nil && ev.AgentId == a.Id {
			got = ad.TerminalId
			if !ad.Launched {
				t.Error("a launch's event doesn't say it was a launch")
			}
		}
	}
	if want := h.agent(a.Id).TerminalId; got == "" || got != want {
		t.Errorf("the stream said terminal %q, the agent has %q", got, want)
	}
}

// recvIdle reads a frame of a stream that wants no activity: it must not be one, and it reports whether the frame
// was the agent's move to IDLE.
func recvIdle(t *testing.T, stream *connect.ServerStreamForClient[v1.SubscribeEventsResponse]) bool {
	t.Helper()
	f := recv(t, stream)
	if f.GetActivity() != nil {
		t.Fatal("a stream that didn't ask for activity got a frame")
	}
	sc := f.GetEvent().GetAgentStateChanged()
	return sc != nil && sc.To == v1.AgentState_AGENT_STATE_IDLE
}

func TestHelloNamesTheCLIsTheHostCanLaunch(t *testing.T) {
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) {
		c.Binaries["agy"] = "/nonexistent/agy"
		c.LookPath = func(name string) (string, bool) { return "/usr/bin/" + name, name == "copilot" }
	})
	t.Cleanup(func() { stopTermHost(t, state) })
	res, err := h.device.Mesh.Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{}))
	// claude and opencode by their configured binaries, copilot through the lookup, agy's binary is not there.
	if want := []string{"claude", "opencode", "copilot"}; err != nil || !slices.Equal(res.Msg.Clis, want) {
		t.Errorf("clis %v, %v: want %v", res.Msg.GetClis(), err, want)
	}
	if !slices.Contains(res.Msg.GetCapabilities(), "clis") {
		t.Errorf("capabilities %v lack clis: an empty list would read as an older daemon's", res.Msg.GetCapabilities())
	}
}
