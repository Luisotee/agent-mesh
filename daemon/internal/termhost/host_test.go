package termhost_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// The test binary is also the program the tests run in a PTY.
func TestMain(m *testing.M) {
	switch os.Getenv("TERMHOST_HELPER") {
	case "":
		os.Exit(m.Run())
	case "echo":
		fmt.Print("ready\r\n")
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			fmt.Printf("got:%s\r\n", in.Text())
		}
	case "da":
		in := bufio.NewReader(os.Stdin)
		fmt.Print("waiting\r\n")
		in.ReadString('\n')
		raw := exec.Command("stty", "raw", "-echo")
		raw.Stdin = os.Stdin
		raw.Run()
		fmt.Print("\x1b[c")
		got := make(chan string, 1)
		go func() {
			buf := make([]byte, 64)
			n, _ := in.Read(buf)
			got <- string(buf[:n])
		}()
		select {
		case r := <-got:
			fmt.Printf("reply:%q\r\n", r)
		case <-time.After(700 * time.Millisecond):
			fmt.Print("reply:none\r\n")
		}
		time.Sleep(200 * time.Millisecond)
	case "exit3":
		fmt.Print("bye\r\n")
		os.Exit(3)
	}
	os.Exit(0)
}

type fixture struct {
	t      *testing.T
	host   *termhost.Host
	c      *termclient.Client
	cancel context.CancelFunc
	done   chan error
}

func startHost(t *testing.T) *fixture {
	t.Helper()
	dir := shortdir.New(t)
	h, err := termhost.Open(termhost.Config{Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		KillGrace: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{t: t, host: h, cancel: cancel, done: make(chan error, 1)}
	go func() { f.done <- h.Serve(ctx) }()
	c, _, err := termclient.Ensure(context.Background(), dir, termclient.Options{Launch: termclient.LaunchExternal, Wait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.c = c
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(10 * time.Second):
			t.Error("host did not stop")
		}
	})
	return f
}

func (f *fixture) spawn(helper string) *termv1.Terminal {
	f.t.Helper()
	exe, _ := os.Executable()
	res, err := f.c.API.CreateTerminal(context.Background(), connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: []string{exe}, Env: map[string]string{"TERMHOST_HELPER": helper, "PATH": os.Getenv("PATH"), "TERM": "xterm-256color"},
		Cols: 80, Rows: 24, Labels: map[string]string{"agent": "a1"},
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Msg.Terminal
}

func (f *fixture) attach(id string, input bool, a *termv1.Attach) *termclient.Stream {
	f.t.Helper()
	url := client.BaseURL + "/term/" + id
	if input {
		url += "?input=1"
	}
	s, err := termclient.Attach(context.Background(), f.c.HTTP, url, f.c.Token, a)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	return s
}

// collector reads a stream until the output contains want.
type collector struct {
	frames []*termv1.TermFrame
	out    bytes.Buffer
	epoch  uint64
	offset uint64
}

func (col *collector) until(t *testing.T, s *termclient.Stream, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for !strings.Contains(col.out.String(), want) {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v; have %q", want, err, col.out.String())
		}
		col.add(f)
	}
}

func (col *collector) add(f *termv1.TermFrame) {
	col.frames = append(col.frames, f)
	if o := f.GetOutput(); o != nil {
		col.out.Write(o.Data)
		col.epoch, col.offset = o.Epoch, o.Offset+uint64(len(o.Data))
	}
	if sc := f.GetScreen(); sc != nil {
		col.out.Write(sc.Data)
		col.epoch, col.offset = sc.Epoch, sc.Offset
	}
}

func (col *collector) next(t *testing.T, s *termclient.Stream) *termv1.TermFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := s.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	col.add(f)
	return f
}

func TestRepaintThenLiveInputAndOutput(t *testing.T) {
	f := startHost(t)
	term := f.spawn("echo")
	s := f.attach(term.Id, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: 80, Rows: 24})
	var col collector
	att := col.next(t, s).GetAttached()
	if att == nil || !att.Input || att.TerminalId != term.Id {
		t.Fatalf("first frame %v", col.frames[0])
	}
	col.until(t, s, "ready")
	// Input sent before Ready is a replay's answers: dropped.
	ctx := context.Background()
	s.Input(ctx, []byte("early\n"))
	s.Ready(ctx)
	s.Input(ctx, []byte("late\n"))
	col.until(t, s, "got:late")
	if strings.Contains(col.out.String(), "got:early") {
		t.Errorf("input before Ready reached the program: %q", col.out.String())
	}
	var sawDone bool
	for _, fr := range col.frames {
		sawDone = sawDone || fr.GetReplayDone() != nil
	}
	if !sawDone {
		t.Error("no ReplayDone")
	}
}

func TestExactReattachReplaysAndAStaleOneIsReanchored(t *testing.T) {
	f := startHost(t)
	term := f.spawn("echo")
	ctx := context.Background()
	s := f.attach(term.Id, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: 80, Rows: 24})
	var col collector
	s.Ready(ctx)
	s.Input(ctx, []byte("one\n"))
	col.until(t, s, "got:one")
	s.Close()
	epoch, offset := col.epoch, col.offset

	// While nobody watches, the program writes more.
	if _, err := f.c.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: term.Id, Data: []byte("two\n")})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		if res.Msg.Terminals[0].Offset > offset+10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no output while detached")
		}
		time.Sleep(20 * time.Millisecond)
	}

	s2 := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_EXACT, Epoch: epoch, Offset: offset, Cols: 80, Rows: 24})
	var col2 collector
	att := col2.next(t, s2).GetAttached()
	if att.Mode != termv1.AttachMode_ATTACH_MODE_EXACT || att.Reanchored || att.Offset != offset {
		t.Fatalf("exact attach answered %v", att)
	}
	col2.until(t, s2, "got:two")
	first := col2.frames[1].GetOutput()
	if first == nil || !first.Replay || first.Offset != offset {
		t.Errorf("replay frame %v", col2.frames[1])
	}
	if strings.Contains(col2.out.String(), "got:one") {
		t.Error("exact replay resent bytes from before the offset")
	}

	s3 := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_EXACT, Epoch: epoch + 7, Offset: offset, Cols: 80, Rows: 24})
	var col3 collector
	att = col3.next(t, s3).GetAttached()
	if att.Mode != termv1.AttachMode_ATTACH_MODE_REPAINT || !att.Reanchored {
		t.Fatalf("stale epoch answered %v", att)
	}
	if sc := col3.next(t, s3).GetScreen(); sc == nil || !strings.Contains(string(sc.Data), "got:two") {
		t.Errorf("reanchored attach got no repaint with the screen: %v", sc)
	}
}

func TestTheHostAnswersQueriesOnlyWhileNoTypingClientIsAttached(t *testing.T) {
	f := startHost(t)
	ctx := context.Background()

	// A read-only watcher doesn't answer, so the host does.
	term := f.spawn("da")
	watch := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: 80, Rows: 24})
	var col collector
	col.until(t, watch, "waiting")
	f.c.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: term.Id, Data: []byte("go\n")}))
	col.until(t, watch, "reply:")
	col.until(t, watch, "\r\n")
	if !strings.Contains(col.out.String(), `reply:"\x1b[?62;`) {
		t.Errorf("host did not answer DA1: %q", col.out.String())
	}

	// A client that can type answers for itself, so the host stays quiet.
	term2 := f.spawn("da")
	typer := f.attach(term2.Id, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT, Cols: 80, Rows: 24})
	var col2 collector
	typer.Ready(ctx)
	col2.until(t, typer, "waiting")
	typer.Input(ctx, []byte("go\n"))
	col2.until(t, typer, "reply:none")
}

func TestExitIsKeptUntilAcknowledged(t *testing.T) {
	f := startHost(t)
	ctx := context.Background()
	term := f.spawn("exit3")
	deadline := time.Now().Add(5 * time.Second)
	var got *termv1.Terminal
	for got == nil || !got.Exited {
		res, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		got = res.Msg.Terminals[0]
		if time.Now().After(deadline) {
			t.Fatal("never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.ExitCode != 3 || got.Labels["agent"] != "a1" {
		t.Errorf("exited terminal %v", got)
	}
	// A watcher that connects later still learns of it.
	w, err := f.c.API.WatchTerminals(ctx, connect.NewRequest(&termv1.WatchTerminalsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Receive() || w.Msg().GetExited().GetId() != term.Id {
		t.Fatalf("watch: %v %v", w.Msg(), w.Err())
	}
	w.Close()
	// Attaching to it shows its last screen and its exit.
	s := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT})
	var col collector
	for {
		fr, err := s.Recv(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != termhost.CloseExited {
				t.Errorf("close %v", err)
			}
			break
		}
		col.add(fr)
	}
	if !strings.Contains(col.out.String(), "bye") || col.frames[len(col.frames)-1].GetExited().GetExitCode() != 3 {
		t.Errorf("frames %v", col.frames)
	}
	f.c.API.AckExit(ctx, connect.NewRequest(&termv1.AckExitRequest{Id: term.Id}))
	res, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if len(res.Msg.Terminals) != 0 {
		t.Errorf("acknowledged exit still listed: %v", res.Msg.Terminals)
	}
}

func TestKillStopsTheProcessGroup(t *testing.T) {
	f := startHost(t)
	ctx := context.Background()
	res, err := f.c.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: []string{"sh", "-c", "trap '' TERM; sleep 60 & wait"}, Env: map[string]string{"PATH": os.Getenv("PATH")}}))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.Terminal.Id
	time.Sleep(100 * time.Millisecond)
	if _, err := f.c.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		if l.Msg.Terminals[0].Exited {
			if l.Msg.Terminals[0].Signal == 0 {
				t.Errorf("exit %v, want a signal", l.Msg.Terminals[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a TERM-ignoring program was not killed after the grace period")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKillReachesGroupMembersAfterTheLeaderDied(t *testing.T) {
	f := startHost(t)
	ctx := context.Background()
	// The leader dies on TERM; its child ignores TERM and must still be killed by the escalation.
	res, err := f.c.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: []string{"sh", "-c", `sh -c 'trap "" TERM; exec sleep 60' & exec sleep 60`},
		Env:  map[string]string{"PATH": os.Getenv("PATH")}}))
	if err != nil {
		t.Fatal(err)
	}
	term := res.Msg.Terminal
	time.Sleep(200 * time.Millisecond)
	tty, err := proc.TTY(int(term.Pid))
	if err != nil || tty == 0 {
		t.Fatalf("tty %d, %v", tty, err)
	}
	if _, err := f.c.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: term.Id})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for proc.GroupAlive(int(term.Pid), tty) {
		if time.Now().After(deadline) {
			t.Fatal("a TERM-ignoring group member survived its leader")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestOneHostPerDirAndTokensRequired(t *testing.T) {
	f := startHost(t)
	if _, err := termhost.Open(termhost.Config{Dir: f.host.Paths().Dir}); err == nil || !strings.Contains(err.Error(), "another mesh-term") {
		t.Errorf("second host: %v", err)
	}
	if _, err := termclient.Attach(context.Background(), f.c.HTTP, client.BaseURL+"/term/x", "amt_wrong", &termv1.Attach{}); err == nil {
		t.Error("a stream opened with a wrong token")
	}
	hc := client.UnixHTTP(f.host.Paths().Socket(), time.Second)
	req, _ := newHelloRequest()
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("Hello without a token: %d", res.StatusCode)
	}
}

func TestResizeFollowsTheSmallestClientAndMovesTheEpoch(t *testing.T) {
	f := startHost(t)
	term := f.spawn("echo")
	ctx := context.Background()
	a := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: 100, Rows: 30})
	var ca collector
	first := ca.next(t, a).GetAttached()
	b := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: 60, Rows: 40})
	var cb collector
	second := cb.next(t, b).GetAttached()
	if second.Cols != 60 || second.Rows != 30 {
		t.Errorf("size %dx%d, want the smallest 60x30", second.Cols, second.Rows)
	}
	if second.Epoch <= first.Epoch {
		t.Errorf("epoch %d after a resize, was %d", second.Epoch, first.Epoch)
	}
	b.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		if l.Msg.Terminals[0].Cols == 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("size did not grow back when the small client left")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSizesAreClampedAndOnlyATyperMayResize(t *testing.T) {
	f := startHost(t)
	term := f.spawn("echo")
	ctx := context.Background()
	// A viewer asking for a huge screen gets the cap, not an allocation of 60000 x 60000 cells.
	viewer := f.attach(term.Id, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: 60000, Rows: 60000})
	var cv collector
	at := cv.next(t, viewer).GetAttached()
	if at.Cols != 500 || at.Rows != 300 {
		t.Errorf("attached at %dx%d, want the cap 500x300", at.Cols, at.Rows)
	}
	// It may not resize at all; a typer may, within the limits.
	resize := func(s *termclient.Stream, cols, rows uint32) {
		if err := s.Send(ctx, &termv1.TermFrame{Frame: &termv1.TermFrame_Resize{Resize: &termv1.Resize{Cols: cols, Rows: rows}}}); err != nil {
			t.Fatal(err)
		}
	}
	size := func() (uint32, uint32) {
		l, _ := f.c.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return l.Msg.Terminals[0].Cols, l.Msg.Terminals[0].Rows
	}
	resize(viewer, 1, 1)
	typer := f.attach(term.Id, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: 200, Rows: 50})
	var ct collector
	ct.next(t, typer)
	if cols, rows := size(); cols != 200 || rows != 50 {
		t.Errorf("size %dx%d after a viewer's resize to 1x1 and a typer at 200x50", cols, rows)
	}
	resize(typer, 1, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cols, rows := size(); cols == 20 && rows == 5 {
			break
		}
		if time.Now().After(deadline) {
			cols, rows := size()
			t.Fatalf("a typer's 1x1 resize gave %dx%d, want the floor 20x5", cols, rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newHelloRequest() (*http.Request, error) {
	req, err := http.NewRequest("POST", "http://meshd"+termv1connect.TermHostServiceHelloProcedure, strings.NewReader("{}"))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}
