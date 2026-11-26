package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"github.com/charmbracelet/x/term"
	"github.com/coder/websocket"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

const detachByte = 0x1d // Ctrl-]

var errDetached = errors.New("detached")

// reopen asks the daemon for a fresh way in to the same agent: an attach path is a one-use ticket, so a re-attach
// after the daemon restarted (which forgets its tickets) needs a new one. nil: the attach is not retried.
type reopen func(ctx context.Context) (*v1.OpenAgentResponse, error)

func runOpen(ctx context.Context, conn *client.Conn, res *v1.OpenAgentResponse, env Env, redo reopen) error {
	switch res.Mode {
	case v1.OpenMode_OPEN_MODE_ATTACH:
		return attachOpen(ctx, conn, res, env, redo)
	case v1.OpenMode_OPEN_MODE_EXEC:
		return execOpen(res, env)
	default:
		return fmt.Errorf("unexpected open mode %v", res.Mode)
	}
}

func openAgentLabel(res *v1.OpenAgentResponse) string {
	if name := res.GetAgent().GetName(); name != "" {
		return name
	}
	return res.GetAgent().GetId()
}

func execOpen(res *v1.OpenAgentResponse, env Env) error {
	if len(res.Argv) == 0 {
		return errors.New("daemon returned an empty command")
	}
	name := openAgentLabel(res)
	if res.Branch != "" {
		fmt.Fprintf(env.Stderr, "Reopening %s (%s) on branch %s\n", name, res.GetAgent().GetKind(), res.Branch)
	} else {
		fmt.Fprintf(env.Stderr, "Reopening %s (%s)\n", name, res.GetAgent().GetKind())
	}
	for _, note := range res.Notes {
		fmt.Fprintln(env.Stderr, note)
	}
	environFn := env.Environ
	if environFn == nil {
		environFn = os.Environ
	}
	childEnv := buildStandaloneEnv(environFn(), res.Env)
	execFn := env.Exec
	if execFn == nil {
		execFn = defaultExec
	}
	return execFn(res.Argv, childEnv, res.Cwd)
}

func buildStandaloneEnv(hostEnv []string, overrides map[string]string) []string {
	drop := make(map[string]bool, len(claude.SessionVars)+len(agy.SessionVars))
	for _, k := range claude.SessionVars {
		drop[k] = true
	}
	for _, k := range agy.SessionVars {
		drop[k] = true
	}
	merged := map[string]string{}
	for _, kv := range hostEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "AGENT_MESH_") || strings.HasPrefix(k, "MESH_") || drop[k] {
			continue
		}
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func defaultExec(argv, env []string, dir string) error {
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(bin, argv, env)
}

// How a dropped attach is retried (variables so tests can shorten them): the first retry after reconnectStart,
// the wait doubling up to reconnectMax, for reconnectWindow in all.
var (
	reconnectStart  = 250 * time.Millisecond
	reconnectMax    = 5 * time.Second
	reconnectWindow = 2 * time.Minute
)

// transportError is a stream that ended without the host saying so (the daemon went away): worth a re-attach.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// openSession is the stream the keyboard and window-size pumps send to: nil while the daemon is away, when what
// is typed is dropped (only Ctrl-] still counts).
type openSession struct {
	mu     sync.Mutex
	stream *termclient.Stream
}

func (s *openSession) set(st *termclient.Stream) {
	s.mu.Lock()
	s.stream = st
	s.mu.Unlock()
}

func (s *openSession) current() *termclient.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

func attachOpen(ctx context.Context, conn *client.Conn, res *v1.OpenAgentResponse, env Env, redo reopen) error {
	inFile, inOK := env.Stdin.(*os.File)
	outFile, outOK := env.Stdout.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(inFile.Fd()) {
		return fmt.Errorf("cannot attach to %s: stdin/stdout must be a terminal", openAgentLabel(res))
	}
	size := func() (int, int) {
		cols, rows, err := term.GetSize(outFile.Fd())
		if err != nil || cols <= 0 || rows <= 0 {
			if c2, r2, err2 := term.GetSize(inFile.Fd()); err2 == nil && c2 > 0 && r2 > 0 {
				return c2, r2
			}
			return 80, 24
		}
		return cols, rows
	}
	name := openAgentLabel(res)
	attach := func(ctx context.Context, path string) (*termclient.Stream, error) {
		cols, rows := size()
		return termclient.Attach(ctx, conn.HTTP, client.BaseURL+path, conn.Token, &termv1.Attach{
			Mode:   termv1.AttachMode_ATTACH_MODE_REPAINT,
			Cols:   uint32(cols),
			Rows:   uint32(rows),
			Redraw: true,
		})
	}

	fmt.Fprintf(env.Stderr, "Attached to %s (Ctrl-] to detach)\n", name)
	attachCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := attach(attachCtx, res.TerminalPath)
	if err != nil {
		return err
	}
	sess := &openSession{}
	sess.set(stream)
	defer func() {
		if st := sess.current(); st != nil {
			st.Close()
		}
	}()

	oldState, err := term.MakeRaw(inFile.Fd())
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = term.Restore(inFile.Fd(), oldState)
		}
	}
	defer restore()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-attachCtx.Done():
				return
			case <-winch:
				c, r, gerr := term.GetSize(outFile.Fd())
				if st := sess.current(); gerr == nil && c > 0 && r > 0 && st != nil {
					_ = st.Send(attachCtx, &termv1.TermFrame{
						Frame: &termv1.TermFrame_Resize{Resize: &termv1.Resize{Cols: uint32(c), Rows: uint32(r)}},
					})
				}
			}
		}
	}()

	// The keyboard, until Ctrl-] (0x1d): it goes to whichever stream is current, and is dropped while there is none.
	stop := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := inFile.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				idx := bytes.IndexByte(chunk, detachByte)
				if idx >= 0 {
					chunk = chunk[:idx]
				}
				if st := sess.current(); st != nil && len(chunk) > 0 {
					_ = st.Input(attachCtx, chunk)
				}
				if idx >= 0 {
					stop <- errDetached
					return
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					stop <- errDetached
				} else {
					stop <- rerr
				}
				return
			}
		}
	}()

	for {
		out := make(chan error, 1)
		go func(st *termclient.Stream) { out <- pumpOutput(attachCtx, st, outFile, name, env, restore) }(stream)
		var perr error
		select {
		case perr = <-out:
		case perr = <-stop:
		}
		sess.set(nil)
		_ = stream.Close()
		var te *transportError
		if !errors.As(perr, &te) || redo == nil {
			restore()
			if errors.Is(perr, errDetached) {
				fmt.Fprintln(env.Stderr, "\n[detached]")
				return nil
			}
			return perr
		}
		// The daemon went away (an upgrade, a restart): the terminal stays raw and one status line says so, then
		// the attach is retried until the daemon is back, a repaint at the current size redrawing the screen.
		fmt.Fprint(env.Stderr, "\r\n[meshd went away, reconnecting… Ctrl-] to give up]\r\n")
		next, err := reconnect(attachCtx, redo, attach, stop, te)
		if err != nil {
			restore()
			if errors.Is(err, errDetached) {
				fmt.Fprintln(env.Stderr, "\n[detached]")
				return nil
			}
			return err
		}
		stream = next
		sess.set(stream)
	}
}

// reconnect re-attaches after the daemon went away: a new way in from redo (a fresh ticket), then the stream, with
// the wait between tries doubling from reconnectStart to reconnectMax, for reconnectWindow. Ctrl-] (stop) ends it.
func reconnect(ctx context.Context, redo reopen, attach func(context.Context, string) (*termclient.Stream, error),
	stop <-chan error, last error) (*termclient.Stream, error) {
	deadline := time.Now().Add(reconnectWindow)
	delay := reconnectStart
	for {
		select {
		case <-time.After(delay):
		case err := <-stop:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("meshd did not come back within %s (last: %v)", reconnectWindow, last)
		}
		delay = min(delay*2, reconnectMax)
		res, err := redo(ctx)
		if err != nil {
			if !transient(err) {
				return nil, err
			}
			last = err
			continue
		}
		if res.Mode != v1.OpenMode_OPEN_MODE_ATTACH {
			return nil, fmt.Errorf("the agent can no longer be attached to (the daemon says %v)", res.Mode)
		}
		st, err := attach(ctx, res.TerminalPath)
		if err != nil {
			last = err
			continue
		}
		return st, nil
	}
}

// transient reports whether a failed call is worth another try while the daemon comes back: it could not be
// reached or answered, rather than saying no (the agent is gone, the token refused).
func transient(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return true
	}
	switch ce.Code() {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeUnknown, connect.CodeInternal, connect.CodeAborted:
		return true
	}
	return false
}

// pumpOutput writes the terminal's output to stdout until the stream ends. A normal end (the agent exited, the
// host closed the terminal, a clean close) is nil; a stream that dropped without one is a *transportError.
func pumpOutput(ctx context.Context, stream *termclient.Stream, outFile *os.File, name string, env Env, restore func()) error {
	for {
		f, rerr := stream.Recv(ctx)
		if rerr != nil {
			if status := websocket.CloseStatus(rerr); status == websocket.StatusNormalClosure {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &transportError{rerr}
		}
		switch v := f.Frame.(type) {
		case *termv1.TermFrame_Output:
			if _, werr := outFile.Write(v.Output.Data); werr != nil {
				return werr
			}
		case *termv1.TermFrame_Screen:
			if _, werr := outFile.Write(v.Screen.Data); werr != nil {
				return werr
			}
		case *termv1.TermFrame_ReplayDone:
			if serr := stream.Ready(ctx); serr != nil {
				return &transportError{serr}
			}
		case *termv1.TermFrame_Exited:
			restore()
			fmt.Fprintf(env.Stderr, "\n[%s exited (%d)]\n", name, v.Exited.ExitCode)
			return nil
		case *termv1.TermFrame_Closing:
			restore()
			if v.Closing.Reason != "" {
				fmt.Fprintf(env.Stderr, "\n[%s closed: %s]\n", name, v.Closing.Reason)
			} else {
				fmt.Fprintf(env.Stderr, "\n[%s closed]\n", name)
			}
			return nil
		}
	}
}
