// Package termclient is the daemon's side of the terminal host: its control API, its terminal streams, and
// starting a host when none answers.
package termclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

// Client talks to one terminal host.
type Client struct {
	Paths termhost.Paths
	Token string
	HTTP  *http.Client
	API   termv1connect.TermHostServiceClient
}

// New makes a client for the host in dir, reading its token.
func New(dir string) (*Client, error) {
	p := termhost.Paths{Dir: dir}
	tok, err := termhost.EnsureToken(p)
	if err != nil {
		return nil, err
	}
	hc := client.UnixHTTP(p.Socket(), 2*time.Second)
	return &Client{Paths: p, Token: tok, HTTP: hc,
		API: termv1connect.NewTermHostServiceClient(hc, client.BaseURL,
			connect.WithInterceptors(auth.ClientToken(tok)))}, nil
}

// Hello asks the host who it is.
func (c *Client) Hello(ctx context.Context) (*termv1.HelloResponse, error) {
	res, err := c.API.Hello(ctx, connect.NewRequest(&termv1.HelloRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Launch is how the daemon gets a host running when none answers.
type Launch string

const (
	// LaunchDetached starts mesh-term itself, in its own session, so the daemon's exit doesn't take it along
	// (dev and tests; a machine without systemd).
	LaunchDetached Launch = "detached"
	// LaunchSystemd starts the agent-mesh-term user unit, which keeps the terminals out of the daemon's
	// cgroup (production).
	LaunchSystemd Launch = "systemd"
	// LaunchExternal only waits for a host something else starts.
	LaunchExternal Launch = "external"
)

// Options of Ensure.
type Options struct {
	Launch Launch
	// Binary is mesh-term's path, for LaunchDetached.
	Binary string
	// Unit is the systemd user unit, for LaunchSystemd.
	Unit string
	// Args are passed on to a detached mesh-term (tests pass --max-lifetime).
	Args []string
	Wait time.Duration
}

// SystemdArgv is the command that starts the host's user unit.
func SystemdArgv(unit string) []string {
	if unit == "" {
		unit = "agent-mesh-term.service"
	}
	return []string{"systemctl", "--user", "start", unit}
}

// helloTimeout bounds one probe of the host in Ensure. The client's own 2 s is for dialling only: a host that
// accepted the connection and never answers (starting, stopped, a loaded machine) would otherwise block the
// probe for as long as its context lives, and Ensure's own deadline is only looked at between probes.
var helloTimeout = 3 * time.Second

// probe is one Hello, bounded by helloTimeout.
func (c *Client) probe(ctx context.Context) (*termv1.HelloResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	return c.Hello(ctx)
}

// Ensure returns a client for a running host in dir, starting one as opts says if none answers. It gives up after
// opts.Wait (10 s by default) plus one probe, whether the host refuses connections or accepts and stays silent.
func Ensure(ctx context.Context, dir string, opts Options) (*Client, *termv1.HelloResponse, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	c, err := New(dir)
	if err != nil {
		return nil, nil, err
	}
	if hello, err := c.probe(ctx); err == nil {
		return c, hello, nil
	}
	if opts.Wait == 0 {
		opts.Wait = 10 * time.Second
	}
	switch opts.Launch {
	case LaunchDetached:
		if err := startDetached(dir, opts); err != nil {
			return nil, nil, err
		}
	case LaunchSystemd:
		argv := SystemdArgv(opts.Unit)
		sctx, cancel := context.WithTimeout(ctx, opts.Wait) // `systemctl start` of a wedged unit waits as long as the unit does
		defer cancel()
		if out, err := exec.CommandContext(sctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
			return nil, nil, fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, out)
		}
	case LaunchExternal:
	default:
		return nil, nil, fmt.Errorf("unknown terminal-host launch %q", opts.Launch)
	}
	deadline := time.Now().Add(opts.Wait)
	for {
		hello, err := c.probe(ctx)
		if err == nil {
			return c, hello, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("no terminal host answered in %s: %w", dir, err)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func startDetached(dir string, opts Options) error {
	if opts.Binary == "" {
		return errors.New("no mesh-term binary given")
	}
	logf, err := os.OpenFile(termhost.Paths{Dir: dir}.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(opts.Binary, append([]string{"--dir", dir}, opts.Args...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Not our child to wait for in the long run: reap it if it exits while we live, and let it outlive us.
	go cmd.Wait()
	return nil
}

// ── terminal streams ──

// Stream is one attached terminal stream.
type Stream struct {
	conn *websocket.Conn
}

// Attach opens a terminal's stream on the host (path "/term/<id>") or through the daemon's proxy (the path
// OpenTerminal returned), and sends the Attach frame.
func Attach(ctx context.Context, hc *http.Client, url, bearer string, a *termv1.Attach) (*Stream, error) {
	h := http.Header{}
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(16 << 20)
	s := &Stream{conn: conn}
	if err := s.Send(ctx, &termv1.TermFrame{Frame: &termv1.TermFrame_Attach{Attach: a}}); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return s, nil
}

// Send sends a frame.
func (s *Stream) Send(ctx context.Context, f *termv1.TermFrame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageBinary, b)
}

// Input types into the terminal (after Ready).
func (s *Stream) Input(ctx context.Context, data []byte) error {
	return s.Send(ctx, &termv1.TermFrame{Frame: &termv1.TermFrame_Input{Input: &termv1.Input{Data: data}}})
}

// Ready says the replay is done, so the host accepts input.
func (s *Stream) Ready(ctx context.Context) error {
	return s.Send(ctx, &termv1.TermFrame{Frame: &termv1.TermFrame_Ready{Ready: &termv1.Ready{}}})
}

// Recv reads the next frame. A close carries the host's code (websocket.CloseStatus).
func (s *Stream) Recv(ctx context.Context) (*termv1.TermFrame, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	f := &termv1.TermFrame{}
	return f, proto.Unmarshal(data, f)
}

// Close closes the stream.
func (s *Stream) Close() error { return s.conn.Close(websocket.StatusNormalClosure, "") }
