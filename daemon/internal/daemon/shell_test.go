package daemon_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// shellWorld is a host with a real terminal host and a project its repos.dir lists.
type shellWorld struct {
	*agentsHarness
	state, project string
}

func startShells(t *testing.T) *shellWorld {
	t.Helper()
	t.Setenv("SHELL", "/bin/bash") // the tests type bash syntax, whatever shell the developer uses
	state := shortdir.State(t)
	h := startWithAgents(t, state, t.TempDir())
	project := gitRepo(t)
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta("dir"), Key: "repos.dir",
		Value: val(t, []any{filepath.Dir(project)})})); err != nil {
		t.Fatal(err)
	}
	return &shellWorld{agentsHarness: h, state: state, project: project}
}

func (w *shellWorld) open(t *testing.T, id string) (*v1.Shell, error) {
	t.Helper()
	res, err := w.device.Mesh.OpenShell(context.Background(), connect.NewRequest(&v1.OpenShellRequest{Meta: meta(id), Repo: w.project}))
	if err != nil {
		return nil, err
	}
	return res.Msg.Shell, nil
}

// attach opens a shell's stream with a fresh ticket and reads it until what was typed shows (or the stream ends).
func (w *shellWorld) attach(t *testing.T, c *client.Conn, id string, input bool) (*termclient.Stream, bool) {
	t.Helper()
	res, err := c.Mesh.OpenTerminal(context.Background(), connect.NewRequest(&v1.OpenTerminalRequest{Shell: id, Input: input}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Msg.Path, "/v1/shell/"+id+"?ticket=") {
		t.Fatalf("path %q", res.Msg.Path)
	}
	s, err := termclient.Attach(context.Background(), c.HTTP, client.BaseURL+res.Msg.Path, "", &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, res.Msg.Input
}

func readUntil(t *testing.T, s *termclient.Stream, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var all strings.Builder
	for !strings.Contains(all.String(), want) {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v; have %q", want, err, all.String())
		}
		if o := f.GetOutput(); o != nil {
			all.Write(o.Data)
		}
		if sc := f.GetScreen(); sc != nil {
			all.Write(sc.Data)
		}
	}
	return all.String()
}

func (w *shellWorld) list(t *testing.T) []*v1.Shell {
	t.Helper()
	res, err := w.device.Mesh.ListShells(context.Background(), connect.NewRequest(&v1.ListShellsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Shells
}

func TestAShellRunsInTheProjectWithoutTheDaemonsSecrets(t *testing.T) {
	w := startShells(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-do-not-leak")
	t.Setenv("AGENT_MESH_TOKEN", "tok-do-not-leak")
	sh, err := w.open(t, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Id == "" || sh.Repo != w.project || sh.Exited {
		t.Fatalf("shell %v", sh)
	}
	if l := w.list(t); len(l) != 1 || l[0].Id != sh.Id {
		t.Fatalf("list %v", l)
	}
	s, input := w.attach(t, w.device, sh.Id, true)
	if !input {
		t.Fatal("a device with the terminal.input right got no input")
	}
	s.Ready(context.Background())
	s.Input(context.Background(), []byte("echo cwd=$(pwd) key=[$ANTHROPIC_API_KEY] tok=[$AGENT_MESH_TOKEN] home=[$HOME] n=$(env | grep -c AGENT_MESH); echo END-$((40+2))\n"))
	out := readUntil(t, s, "END-42")
	if !strings.Contains(out, "cwd="+w.project) || !strings.Contains(out, "key=[]") || !strings.Contains(out, "tok=[]") || !strings.Contains(out, "n=0\r\nEND-42") {
		t.Errorf("a shell sees the daemon's secrets or the wrong folder: %q", out)
	}
	if !strings.Contains(out, "home=["+w.home+"]") {
		t.Errorf("HOME: %q", out)
	}
	// An opening replayed with the same mutation id is the same shell, not a second.
	if again, err := w.open(t, "s1"); err != nil || again.Id != sh.Id {
		t.Errorf("a replayed open: %v, %v", again, err)
	}
	if l := w.list(t); len(l) != 1 {
		t.Errorf("a replay made %d shells", len(l))
	}
}

func TestOnlyAProjectTheHostListsOpensAShell(t *testing.T) {
	w := startShells(t)
	other := gitRepo(t) // a git repo outside repos.dir that no agent used
	_, err := w.device.Mesh.OpenShell(context.Background(), connect.NewRequest(&v1.OpenShellRequest{Meta: meta("x"), Repo: other}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unlisted folder: %v", err)
	}
	_, err = w.device.Mesh.OpenShell(context.Background(), connect.NewRequest(&v1.OpenShellRequest{Meta: meta("y"), Repo: ""}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no project: %v", err)
	}
}

// An agent (a root or a worker) opens no shell and gets no ticket for one; a read-only device may look at a shell, not type.
func TestAgentsAreRefusedShellsAndAReadOnlyDeviceGetsNoInput(t *testing.T) {
	w := startShells(t)
	ctx := context.Background()
	sh, err := w.open(t, "s1")
	if err != nil {
		t.Fatal(err)
	}
	run := startRun(t, w.device.Mesh, "r1")
	rootTok := w.seedAgent(run.Id, "a-root", "shop-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	workTok := w.seedAgent(run.Id, "a-w1", "shop-fix", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	for name, tok := range map[string]string{"root": rootTok, "worker": workTok} {
		c := w.as(tok)
		if _, err := c.OpenShell(ctx, connect.NewRequest(&v1.OpenShellRequest{Meta: meta("a-" + name), Repo: w.project})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s OpenShell: %v", name, err)
		}
		if _, err := c.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Shell: sh.Id, Input: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s OpenTerminal{shell}: %v", name, err)
		}
		if _, err := c.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("c-" + name), Id: sh.Id})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s CloseShell: %v", name, err)
		}
	}
	if l := w.list(t); len(l) != 1 {
		t.Fatalf("a refused call changed the shells: %v", l)
	}

	cp, err := w.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{DirectUrl: testDirectURL, Meta: meta("ro"), Name: "viewer", ReadOnly: true, TtlSeconds: 60}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(w.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	ro, _ := client.Dial(w.state, red.Msg.DeviceToken, client.Options{})
	if _, err := ro.Mesh.OpenShell(ctx, connect.NewRequest(&v1.OpenShellRequest{Meta: meta("ro-open"), Repo: w.project})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("read-only OpenShell: %v", err)
	}
	if _, err := ro.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("ro-close"), Id: sh.Id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("read-only CloseShell: %v", err)
	}
	if l, err := ro.Mesh.ListShells(ctx, connect.NewRequest(&v1.ListShellsRequest{})); err != nil || len(l.Msg.Shells) != 1 {
		t.Errorf("read-only ListShells: %v %v", l, err)
	}
	s, input := w.attach(t, ro, sh.Id, true)
	if input {
		t.Error("a read-only device was granted input to a shell")
	}
	// Whatever it types is dropped: the stream is a view.
	s.Ready(ctx)
	s.Input(ctx, []byte("touch "+filepath.Join(w.project, "typed-by-viewer")+"\n"))
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(w.project, "typed-by-viewer")); err == nil {
		t.Error("a read-only device's input reached the shell")
	}
}

func TestAShellTicketOpensOnlyItsShellAndAClosedOneIsRefused(t *testing.T) {
	w := startShells(t)
	ctx := context.Background()
	a, err := w.open(t, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.open(t, "b")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) int {
		resp, err := w.device.HTTP.Get(client.BaseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	tk := func(shell string) string {
		res, err := w.device.Mesh.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Shell: shell}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Ticket
	}
	// A shell's ticket is no good for another shell, nor for the agent route.
	if c := get("/v1/shell/" + b.Id + "?ticket=" + tk(a.Id)); c != http.StatusUnauthorized {
		t.Errorf("a's ticket at b: %d", c)
	}
	if c := get("/v1/term/" + a.Id + "?ticket=" + tk(a.Id)); c != http.StatusUnauthorized {
		t.Errorf("a shell's ticket on the agent route: %d", c)
	}
	// Both ways at once, or neither, is refused.
	if _, err := w.device.Mesh.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Shell: a.Id, Agent: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("agent and shell: %v", err)
	}
	if _, err := w.device.Mesh.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Shell: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown shell: %v", err)
	}
	// A ticket issued before the shell was closed stops working, and the shell is gone from the list.
	old := tk(a.Id)
	if _, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("close-a"), Id: a.Id})); err != nil {
		t.Fatal(err)
	}
	if c := get("/v1/shell/" + a.Id + "?ticket=" + old); c != http.StatusUnauthorized {
		t.Errorf("a closed shell's ticket: %d", c)
	}
	waitFor(t, "the closed shell forgotten", func() bool {
		l := w.list(t)
		return len(l) == 1 && l[0].Id == b.Id
	})
	if _, err := w.device.Mesh.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Shell: a.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a ticket for a closed shell: %v", err)
	}
	if _, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("close-a2"), Id: a.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("closing it again: %v", err)
	}
	// An agent's terminal is not a shell: CloseShell never kills one.
	run := startRun(t, w.device.Mesh, "r1")
	ag := w.spawn(run.Id, w.project, "keep", "")
	if _, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("close-agent"), Id: ag.TerminalId})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("CloseShell on an agent's terminal: %v", err)
	}
}

// A shell that exits is forgotten by the host (acknowledged), and the cap counts only the live ones.
func TestAShellThatExitsIsForgottenAndTheCapCountsLiveShellsOnly(t *testing.T) {
	w := startShells(t)
	ctx := context.Background()
	sh, err := w.open(t, "s1")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := w.attach(t, w.device, sh.Id, true)
	s.Ready(ctx)
	s.Input(ctx, []byte("exit 3\n"))
	waitFor(t, "the exited shell forgotten", func() bool { return len(w.list(t)) == 0 })
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{})); len(l.Msg.Terminals) != 0 {
		t.Errorf("the host still holds the exited shell: %v", l.Msg.Terminals)
	}

	// 20 live shells fill the host; a 21st is refused; one closed (or exited) makes room.
	var ids []string
	for i := 0; i < 20; i++ {
		sh, err := w.open(t, "fill-"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("shell %d: %v", i, err)
		}
		ids = append(ids, sh.Id)
	}
	if _, err := w.open(t, "one-too-many"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("the 21st shell: %v", err)
	}
	if _, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("room"), Id: ids[0]})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "room for another", func() bool {
		_, err := w.open(t, "after-close")
		return err == nil
	})
}

// The shells live in the terminal host: a daemon restart finds them, and one that died while the daemon was down is forgotten.
func TestShellsSurviveADaemonRestart(t *testing.T) {
	w := startShells(t)
	ctx := context.Background()
	alive, err := w.open(t, "alive")
	if err != nil {
		t.Fatal(err)
	}
	dying, err := w.open(t, "dying")
	if err != nil {
		t.Fatal(err)
	}
	w.stop()
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: dying.Id, Data: []byte("exit 0\r")})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the shell exited", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		for _, x := range l.Msg.Terminals {
			if x.Id == dying.Id && x.Exited {
				return true
			}
		}
		return false
	})
	h2 := startWithAgents(t, w.state, w.home)
	w2 := &shellWorld{agentsHarness: h2, state: w.state, project: w.project}
	waitFor(t, "the dead shell forgotten and the live one kept", func() bool {
		l := w2.list(t)
		return len(l) == 1 && l[0].Id == alive.Id
	})
	s, _ := w2.attach(t, h2.device, alive.Id, true)
	s.Ready(ctx)
	s.Input(ctx, []byte("echo STILL-$((20+1))\n"))
	readUntil(t, s, "STILL-21")
}

// Ending a shell is like closing a terminal window: its background jobs get the hangup, so a server started with `&` does not
// outlive the shell holding its port; a closed shell's retry is not "no such shell". The close here comes as soon as the job's
// line has been answered, and a bash that gets SIGHUP then can exit without passing it on (the job was found alive under
// init in about a third of the runs, more under load), so the terminal host hangs the shell's session up itself
// (proc.SignalSession), before it signals the shell.
func TestEndingAShellHangsUpItsBackgroundJobs(t *testing.T) {
	w := startShells(t)
	ctx := context.Background()
	sh, err := w.open(t, "s1")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := w.attach(t, w.device, sh.Id, true)
	s.Ready(ctx)
	pidFile := filepath.Join(t.TempDir(), "job.pid")
	s.Input(ctx, []byte("sleep 4242 & echo $! > "+pidFile+"; echo JOB-$((40+2))\n"))
	readUntil(t, s, "JOB-42")
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 1 {
		t.Fatalf("job pid %q", raw)
	}
	// Whatever the test finds, it leaves no `sleep 4242` behind: the job is killed at the end if it is still our sleep.
	t.Cleanup(func() {
		if cmd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && strings.HasPrefix(string(cmd), "sleep\x004242") {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	// Gone, or a zombie waiting for its parent to reap it (a stopped process is not a running job).
	alive := func() bool {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		return err == nil && !strings.Contains(string(st), ") Z ")
	}
	if !alive() {
		t.Fatal("the background job is not running")
	}
	close := func(id string) error {
		_, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("close-1"), Id: id}))
		return err
	}
	if err := close(sh.Id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the shell's background job hung up", func() bool { return !alive() })
	waitFor(t, "the shell forgotten", func() bool { return len(w.list(t)) == 0 })
	// The same request again (the bridge retries a call that got no answer) is the stored answer, not NotFound.
	if err := close(sh.Id); err != nil {
		t.Errorf("a replayed close: %v", err)
	}
}

// An app (a device's subscriber that asked for activity) is told when a shell opens and when it ends, by CloseShell or by its
// own exit; a subscriber that is no app is told nothing.
func TestAnAppIsToldWhenAShellOpensAndEnds(t *testing.T) {
	w := startShells(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Activity: true}))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*connect.ServerStreamForClient[v1.SubscribeEventsResponse]{app, plain} {
		if !s.Receive() || s.Msg().GetSnapshot() == nil {
			t.Fatalf("no snapshot: %v", s.Err())
		}
	}
	// The stream is live once an event every subscriber gets arrives.
	live := func(s *connect.ServerStreamForClient[v1.SubscribeEventsResponse]) {
		t.Helper()
		for s.Receive() {
			if s.Msg().GetEvent() != nil {
				return
			}
			if s.Msg().GetShell() != nil {
				t.Fatalf("a shell frame before any shell: %v", s.Msg())
			}
		}
		t.Fatalf("the stream ended: %v", s.Err())
	}
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "claude.lean", Value: val(t, false)})); err != nil {
		t.Fatal(err)
	}
	live(app)
	live(plain)

	next := func() *v1.ShellChange {
		t.Helper()
		for app.Receive() {
			if c := app.Msg().GetShell(); c != nil {
				return c
			}
		}
		t.Fatalf("no shell frame: %v", app.Err())
		return nil
	}
	a, err := w.open(t, "frame-a")
	if err != nil {
		t.Fatal(err)
	}
	if c := next(); c.Closed || c.Shell.Id != a.Id || c.Shell.Repo == "" {
		t.Errorf("opening: %v", c)
	}
	// A replay of the open tells nobody twice (the next frame below is the close of the first shell).
	if _, err := w.open(t, "frame-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.CloseShell(ctx, connect.NewRequest(&v1.CloseShellRequest{Meta: meta("c-a"), Id: a.Id})); err != nil {
		t.Fatal(err)
	}
	if c := next(); !c.Closed || c.Shell.Id != a.Id {
		t.Errorf("closing: %v", c)
	}
	// A shell that exits by itself is told too.
	b, err := w.open(t, "frame-b")
	if err != nil {
		t.Fatal(err)
	}
	if c := next(); c.Closed || c.Shell.Id != b.Id {
		t.Errorf("opening b: %v", c)
	}
	s, _ := w.attach(t, w.device, b.Id, true)
	s.Ready(ctx)
	s.Input(ctx, []byte("exit 0\n"))
	if c := next(); !c.Closed || c.Shell.Id != b.Id {
		t.Errorf("b's own exit: %v", c)
	}
	// The subscriber that is no app saw none of it: a later event is the next thing it gets.
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "claude.lean", Value: val(t, true)})); err != nil {
		t.Fatal(err)
	}
	for plain.Receive() {
		if plain.Msg().GetShell() != nil {
			t.Fatalf("a subscriber that is no app got %v", plain.Msg())
		}
		if plain.Msg().GetEvent() != nil {
			return
		}
	}
	t.Fatalf("the stream ended: %v", plain.Err())
}
