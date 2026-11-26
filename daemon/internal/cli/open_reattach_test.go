package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/creack/pty"
	"google.golang.org/protobuf/proto"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
)

// fakeHost serves terminal streams the way the daemon does: one WebSocket per attach, the client's first frame the
// Attach. serve is called with the attach and the socket, and returns when that stream is over.
type fakeHost struct {
	srv      *httptest.Server
	mu       sync.Mutex
	attaches []*termv1.Attach
	serve    func(n int, c *websocket.Conn)
}

func newFakeHost(t *testing.T, serve func(n int, c *websocket.Conn)) (*fakeHost, *client.Conn) {
	t.Helper()
	h := &fakeHost{serve: serve}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var f termv1.TermFrame
		if proto.Unmarshal(data, &f) != nil || f.GetAttach() == nil {
			c.CloseNow()
			return
		}
		h.mu.Lock()
		h.attaches = append(h.attaches, f.GetAttach())
		n := len(h.attaches)
		h.mu.Unlock()
		h.serve(n, c)
	}))
	t.Cleanup(h.srv.Close)
	addr := h.srv.Listener.Addr().String()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}}
	return h, &client.Conn{HTTP: hc, Token: "t"}
}

func send(t *testing.T, c *websocket.Conn, f *termv1.TermFrame) {
	t.Helper()
	b, err := proto.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

func output(s string) *termv1.TermFrame {
	return &termv1.TermFrame{Frame: &termv1.TermFrame_Output{Output: &termv1.Output{Data: []byte(s)}}}
}

func screen(s string) *termv1.TermFrame {
	return &termv1.TermFrame{Frame: &termv1.TermFrame_Screen{Screen: &termv1.Screen{Data: []byte(s)}}}
}

func exited() *termv1.TermFrame {
	return &termv1.TermFrame{Frame: &termv1.TermFrame_Exited{Exited: &termv1.Exited{}}}
}

// outputOf reads a pty master until it holds want, or fails.
func readUntil(t *testing.T, master io.Reader, want string) string {
	t.Helper()
	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			got.Write(buf[:n])
			if strings.Contains(got.String(), want) || err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q in the terminal output: %q", want, got.String())
	}
	if !strings.Contains(got.String(), want) {
		t.Fatalf("no %q in the terminal output: %q", want, got.String())
	}
	return got.String()
}

func shortRetries(t *testing.T) {
	t.Helper()
	s, m, w := reconnectStart, reconnectMax, reconnectWindow
	reconnectStart, reconnectMax, reconnectWindow = 10*time.Millisecond, 40*time.Millisecond, 5*time.Second
	t.Cleanup(func() { reconnectStart, reconnectMax, reconnectWindow = s, m, w })
}

// The daemon going away mid-attach doesn't end `mesh open`: the terminal stays raw, one status line says so, the
// attach is retried with a fresh way in (the ticket is single-use) until the daemon answers, the screen is repainted
// at the current size, and output goes on.
func TestOpenReattachesAfterTheDaemonWentAway(t *testing.T) {
	shortRetries(t)
	master, slave, err := pty.Open()
	if err != nil {
		t.Skip("no pty: ", err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	host, conn := newFakeHost(t, func(n int, c *websocket.Conn) {
		switch n {
		case 1:
			send(t, c, screen("first screen"))
			c.CloseNow() // the daemon is killed: no close frame
		default:
			send(t, c, screen("repainted screen"))
			send(t, c, output(" and more output"))
			send(t, c, exited())
			c.Close(websocket.StatusNormalClosure, "")
		}
	})
	var mu sync.Mutex
	redos, down := 0, 2 // the first two re-asks find the daemon still down
	redo := func(ctx context.Context) (*v1.OpenAgentResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		redos++
		if redos <= down {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
		}
		return &v1.OpenAgentResponse{Mode: v1.OpenMode_OPEN_MODE_ATTACH, TerminalPath: "/v1/term/a1?ticket=new"}, nil
	}
	var errOut bytes.Buffer
	env := Env{Stdin: slave, Stdout: slave, Stderr: &errOut}
	res := &v1.OpenAgentResponse{Agent: &v1.Agent{Name: "repo-fix"}, Mode: v1.OpenMode_OPEN_MODE_ATTACH, TerminalPath: "/v1/term/a1?ticket=old"}
	errc := make(chan error, 1)
	go func() { errc <- runOpen(context.Background(), conn, res, env, redo) }()
	got := readUntil(t, master, "and more output")
	if !strings.Contains(got, "first screen") || !strings.Contains(got, "repainted screen") {
		t.Errorf("output %q", got)
	}
	if err := <-errc; err != nil {
		t.Fatalf("runOpen: %v", err)
	}
	if n := strings.Count(errOut.String(), "meshd went away, reconnecting"); n != 1 {
		t.Errorf("%d status lines:\n%s", n, errOut.String())
	}
	if !strings.Contains(errOut.String(), "repo-fix exited") {
		t.Errorf("stderr %q", errOut.String())
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.attaches) != 2 || redos != 3 {
		t.Fatalf("%d attaches, %d re-asks", len(host.attaches), redos)
	}
	a := host.attaches[1]
	if a.Mode != termv1.AttachMode_ATTACH_MODE_REPAINT || !a.Redraw || a.Cols != 100 || a.Rows != 30 {
		t.Errorf("the second attach: %v", a)
	}
}

// Ctrl-] still detaches while the daemon is away, and after the window the command gives up with a clear error.
func TestOpenGivesUpOrDetachesWhileTheDaemonIsAway(t *testing.T) {
	shortRetries(t)
	run := func(t *testing.T, redo reopen) (error, string, io.Writer) {
		master, slave, err := pty.Open()
		if err != nil {
			t.Skip("no pty: ", err)
		}
		t.Cleanup(func() { master.Close(); slave.Close() })
		_, conn := newFakeHost(t, func(n int, c *websocket.Conn) {
			send(t, c, screen("hello"))
			c.CloseNow()
		})
		var errOut bytes.Buffer
		res := &v1.OpenAgentResponse{Agent: &v1.Agent{Name: "w"}, Mode: v1.OpenMode_OPEN_MODE_ATTACH, TerminalPath: "/v1/term/a1?ticket=t"}
		errc := make(chan error, 1)
		go func() {
			errc <- runOpen(context.Background(), conn, res, Env{Stdin: slave, Stdout: slave, Stderr: &errOut}, redo)
		}()
		readUntil(t, master, "hello")
		return func() error {
			select {
			case err := <-errc:
				return err
			case <-time.After(10 * time.Second):
				t.Fatal("runOpen did not return")
				return nil
			}
		}(), errOut.String(), master
	}
	down := func(ctx context.Context) (*v1.OpenAgentResponse, error) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	}
	t.Run("the window runs out", func(t *testing.T) {
		reconnectWindow = 150 * time.Millisecond
		err, _, _ := run(t, down)
		if err == nil || !strings.Contains(err.Error(), "did not come back within") || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("a refusal is not retried", func(t *testing.T) {
		err, _, _ := run(t, func(ctx context.Context) (*v1.OpenAgentResponse, error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no agent"))
		})
		if err == nil || !strings.Contains(err.Error(), "no agent") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("Ctrl-] detaches", func(t *testing.T) {
		reconnectWindow = time.Minute
		master, slave, err := pty.Open()
		if err != nil {
			t.Skip("no pty: ", err)
		}
		defer master.Close()
		defer slave.Close()
		_, conn := newFakeHost(t, func(n int, c *websocket.Conn) {
			send(t, c, screen("hello"))
			c.CloseNow()
		})
		var errOut bytes.Buffer
		res := &v1.OpenAgentResponse{Agent: &v1.Agent{Name: "w"}, Mode: v1.OpenMode_OPEN_MODE_ATTACH, TerminalPath: "/v1/term/a1?ticket=t"}
		errc := make(chan error, 1)
		go func() {
			errc <- runOpen(context.Background(), conn, res, Env{Stdin: slave, Stdout: slave, Stderr: &errOut}, down)
		}()
		readUntil(t, master, "hello")
		time.Sleep(100 * time.Millisecond) // it is retrying now
		master.Write([]byte{detachByte})
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("detach: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Ctrl-] did not detach while reconnecting")
		}
		if !strings.Contains(errOut.String(), "[detached]") {
			t.Errorf("stderr %q", errOut.String())
		}
	})
}
