// Package termhost is mesh-term: the process that owns the agents' PTYs, separate from the daemon so a daemon
// restart leaves every terminal running. It keeps each terminal's output in a ring buffer (epoch + offset, for
// exact replay) and in a terminal emulator (for repaints and for answering terminal queries while no client
// is attached), and serves the daemon a control API and one binary WebSocket per terminal.
package termhost

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"
	"google.golang.org/protobuf/types/known/timestamppb"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

// Version is mesh-term's version.
const Version = "0.1.0-dev"

// Defaults.
const (
	DefaultRingSize = 4 << 20
	DefaultCols     = 120
	DefaultRows     = 40
	clientQueue     = 256
	outputChunk     = 64 << 10
	markEvery       = time.Second
)

// Paths of a terminal host's files in its dir.
type Paths struct{ Dir string }

func (p Paths) Lock() string   { return filepath.Join(p.Dir, "term.lock") }
func (p Paths) Socket() string { return filepath.Join(p.Dir, "term.sock") }
func (p Paths) Info() string   { return filepath.Join(p.Dir, "term.json") }
func (p Paths) Token() string  { return filepath.Join(p.Dir, "term.token") }
func (p Paths) Log() string    { return filepath.Join(p.Dir, "mesh-term.log") }

// Info is term.json.
type Info struct {
	Socket     string    `json:"socket"`
	PID        int       `json:"pid"`
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
	Version    string    `json:"version"`
}

// Config is how a host starts.
type Config struct {
	Dir       string
	RingSize  int
	KillGrace time.Duration
	Logger    *slog.Logger
	// NewScreen makes each terminal's emulator; tests may replace it.
	NewScreen NewScreenFunc
}

// Host is a running terminal host.
type Host struct {
	termv1connect.UnimplementedTermHostServiceHandler

	cfg        Config
	paths      Paths
	log        *slog.Logger
	lock       *fsutil.Lock
	token      string
	instanceID string
	startedAt  time.Time

	mu       sync.Mutex
	terms    map[string]*Terminal
	watchers map[chan *termv1.WatchTerminalsResponse]struct{}
	stopping chan struct{}
}

// Open takes the host's lock (one host per dir) and makes its token on first start.
func Open(cfg Config) (*Host, error) {
	if cfg.Dir == "" {
		return nil, errors.New("no dir given")
	}
	if cfg.RingSize == 0 {
		cfg.RingSize = DefaultRingSize
	}
	if cfg.KillGrace == 0 {
		cfg.KillGrace = 3 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.NewScreen == nil {
		cfg.NewScreen = NewScreen
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	h := &Host{cfg: cfg, paths: Paths{dir}, log: cfg.Logger, instanceID: ids.New("ti", 10),
		terms: map[string]*Terminal{}, watchers: map[chan *termv1.WatchTerminalsResponse]struct{}{},
		stopping: make(chan struct{})}
	if h.lock, err = fsutil.TryLock(h.paths.Lock()); err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			return nil, fmt.Errorf("another mesh-term is running on %s", dir)
		}
		return nil, err
	}
	if h.token, err = EnsureToken(h.paths); err != nil {
		h.lock.Release()
		return nil, err
	}
	return h, nil
}

// EnsureToken returns the host's token, making it if missing. The daemon reads the same file.
func EnsureToken(p Paths) (string, error) {
	tok, err := fsutil.ReadSecret(p.Token())
	if errors.Is(err, os.ErrNotExist) {
		tok = auth.NewToken()
		return tok, fsutil.WriteAtomic(p.Token(), []byte(tok+"\n"), 0o600)
	}
	return tok, err
}

// Serve listens on the host's socket and serves until ctx ends. Then it hangs up every terminal.
func (h *Host) Serve(ctx context.Context) error {
	defer h.lock.Release()
	h.startedAt = time.Now()
	sock := h.paths.Socket()
	// We hold the lock: a socket left here belongs to a dead host. Never remove one without the lock.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return err
	}
	info, _ := json.MarshalIndent(Info{Socket: sock, PID: os.Getpid(), InstanceID: h.instanceID,
		StartedAt: h.startedAt, Version: Version}, "", "  ")
	if err := fsutil.WriteAtomic(h.paths.Info(), info, 0o600); err != nil {
		l.Close()
		return err
	}
	mux := http.NewServeMux()
	path, handler := termv1connect.NewTermHostServiceHandler(h, connect.WithInterceptors(h.interceptor()))
	mux.Handle(path, handler)
	mux.HandleFunc("/term/{id}", h.serveWS)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	h.log.Info("mesh-term serving", "socket", sock, "instance", h.instanceID)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	close(h.stopping)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	h.hangUpAll()
	os.Remove(h.paths.Info())
	os.Remove(sock)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return serveErr
}

// Token is the host's token.
func (h *Host) Token() string { return h.token }

// Paths are the host's files.
func (h *Host) Paths() Paths { return h.paths }

func (h *Host) authorized(header http.Header) bool {
	tok := auth.BearerToken(header)
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(h.token)) == 1
}

func (h *Host) interceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !h.authorized(req.Header()) {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bad host token"))
			}
			return next(ctx, req)
		}
	})
}

// hangUpAll ends every terminal when the host stops: SIGHUP to each process group, then the PTYs close.
func (h *Host) hangUpAll() {
	h.mu.Lock()
	terms := make([]*Terminal, 0, len(h.terms))
	for _, t := range h.terms {
		terms = append(terms, t)
	}
	h.mu.Unlock()
	for _, t := range terms {
		t.closeClients("shutdown")
		if !t.isExited() {
			proc.SignalGroup(t.pid, t.startTicks, t.tty, syscall.SIGHUP)
		}
	}
	for _, t := range terms {
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
			proc.SignalGroup(t.pid, t.startTicks, t.tty, syscall.SIGKILL)
		}
		t.ptmx.Close()
	}
}

func (h *Host) get(id string) *Terminal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.terms[id]
}

func (h *Host) broadcast(f *termv1.WatchTerminalsResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.watchers {
		select {
		case w <- f:
		default:
			// A watcher this far behind is dropped; it reconnects and gets the unacked exits again.
			delete(h.watchers, w)
			close(w)
		}
	}
}

// ── control API ──

// Hello identifies this host instance.
func (h *Host) Hello(context.Context, *connect.Request[termv1.HelloRequest]) (*connect.Response[termv1.HelloResponse], error) {
	return connect.NewResponse(&termv1.HelloResponse{InstanceId: h.instanceID, StartedAt: timestamppb.New(h.startedAt),
		Pid: int64(os.Getpid()), Version: Version}), nil
}

// CreateTerminal starts a program in a new PTY, in its own session and process group.
func (h *Host) CreateTerminal(_ context.Context, req *connect.Request[termv1.CreateTerminalRequest]) (*connect.Response[termv1.CreateTerminalResponse], error) {
	m := req.Msg
	if len(m.Argv) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no argv"))
	}
	cols, rows := uint16(m.Cols), uint16(m.Rows)
	if cols == 0 {
		cols = DefaultCols
	}
	if rows == 0 {
		rows = DefaultRows
	}
	cmd := exec.Command(m.Argv[0], m.Argv[1:]...)
	cmd.Dir = m.Cwd
	cmd.Env = make([]string, 0, len(m.Env))
	keys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+m.Env[k])
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("starting %s: %w", m.Argv[0], err))
	}
	// Without its start time the terminal could never be signalled safely: refuse it.
	ticks, err := proc.StartTicks(cmd.Process.Pid)
	tty, terr := proc.TTY(cmd.Process.Pid)
	if err = errors.Join(err, terr); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		ptmx.Close()
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("identifying the new process: %w", err))
	}
	t := &Terminal{h: h, id: ids.New("t", 10), argv: m.Argv, cwd: m.Cwd, labels: m.Labels, cmd: cmd, ptmx: ptmx,
		pid: cmd.Process.Pid, startTicks: ticks, tty: tty, startedAt: time.Now(), ring: NewRing(h.cfg.RingSize),
		epoch: 1, cols: cols, rows: rows, replies: make(chan []byte, 64),
		clients: map[*client]struct{}{}, done: make(chan struct{}), readDone: make(chan struct{})}
	t.screen = h.cfg.NewScreen(int(cols), int(rows), t.reply)
	h.mu.Lock()
	h.terms[t.id] = t
	h.mu.Unlock()
	go t.readLoop()
	go t.waitLoop()
	go t.replyLoop()
	h.log.Info("terminal started", "id", t.id, "pid", t.pid, "argv0", m.Argv[0])
	return connect.NewResponse(&termv1.CreateTerminalResponse{Terminal: t.proto()}), nil
}

// ListTerminals lists every terminal, exited ones the daemon hasn't acknowledged included.
func (h *Host) ListTerminals(context.Context, *connect.Request[termv1.ListTerminalsRequest]) (*connect.Response[termv1.ListTerminalsResponse], error) {
	h.mu.Lock()
	terms := make([]*Terminal, 0, len(h.terms))
	for _, t := range h.terms {
		terms = append(terms, t)
	}
	h.mu.Unlock()
	sort.Slice(terms, func(i, j int) bool { return terms[i].startedAt.Before(terms[j].startedAt) })
	out := &termv1.ListTerminalsResponse{}
	for _, t := range terms {
		out.Terminals = append(out.Terminals, t.proto())
	}
	return connect.NewResponse(out), nil
}

// KillTerminal signals a terminal's process group, checking its start time first. Signal 0 is SIGTERM, then
// SIGKILL after the grace period if it is still running.
func (h *Host) KillTerminal(_ context.Context, req *connect.Request[termv1.KillTerminalRequest]) (*connect.Response[termv1.KillTerminalResponse], error) {
	t := h.get(req.Msg.Id)
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no terminal %s", req.Msg.Id))
	}
	sig := syscall.Signal(req.Msg.Signal)
	if t.isExited() {
		// The leader is gone, but members of its group may still hold the PTY.
		if sig == 0 {
			sig = syscall.SIGKILL
		}
		proc.SignalGroup(t.pid, t.startTicks, t.tty, sig)
		return connect.NewResponse(&termv1.KillTerminalResponse{}), nil
	}
	if sig == 0 {
		sig = syscall.SIGTERM
		go func() {
			select {
			case <-t.done:
			case <-time.After(h.cfg.KillGrace):
				proc.SignalGroup(t.pid, t.startTicks, t.tty, syscall.SIGKILL)
			}
		}()
	}
	if err := proc.SignalGroup(t.pid, t.startTicks, t.tty, sig); err != nil && !errors.Is(err, proc.ErrGone) {
		return nil, err
	}
	return connect.NewResponse(&termv1.KillTerminalResponse{}), nil
}

// WriteTerminal types into a terminal for the daemon (a doorbell).
func (h *Host) WriteTerminal(_ context.Context, req *connect.Request[termv1.WriteTerminalRequest]) (*connect.Response[termv1.WriteTerminalResponse], error) {
	t := h.get(req.Msg.Id)
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no terminal %s", req.Msg.Id))
	}
	if t.isExited() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("terminal %s exited", t.id))
	}
	if _, err := t.ptmx.Write(req.Msg.Data); err != nil {
		return nil, err
	}
	return connect.NewResponse(&termv1.WriteTerminalResponse{}), nil
}

// AckExit forgets an exited terminal.
func (h *Host) AckExit(_ context.Context, req *connect.Request[termv1.AckExitRequest]) (*connect.Response[termv1.AckExitResponse], error) {
	h.mu.Lock()
	t := h.terms[req.Msg.Id]
	if t != nil && t.isExited() {
		delete(h.terms, t.id)
	}
	h.mu.Unlock()
	if t == nil {
		return connect.NewResponse(&termv1.AckExitResponse{}), nil
	}
	if !t.isExited() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("terminal %s is running", t.id))
	}
	// Close the PTY (its fd would leak otherwise), let the reader finish, then the screen it writes to.
	t.ptmx.Close()
	select {
	case <-t.readDone:
	case <-time.After(2 * time.Second):
	}
	t.mu.Lock()
	t.screen.Close()
	t.mu.Unlock()
	return connect.NewResponse(&termv1.AckExitResponse{}), nil
}

// WatchTerminals streams exits (first those not yet acknowledged) and output marks.
func (h *Host) WatchTerminals(ctx context.Context, _ *connect.Request[termv1.WatchTerminalsRequest],
	stream *connect.ServerStream[termv1.WatchTerminalsResponse]) error {
	w := make(chan *termv1.WatchTerminalsResponse, 1024)
	h.mu.Lock()
	h.watchers[w] = struct{}{}
	var pending []*termv1.Terminal
	for _, t := range h.terms {
		if t.isExited() {
			pending = append(pending, t.proto())
		}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if _, ok := h.watchers[w]; ok {
			delete(h.watchers, w)
			close(w)
		}
		h.mu.Unlock()
	}()
	for _, t := range pending {
		if err := stream.Send(&termv1.WatchTerminalsResponse{Frame: &termv1.WatchTerminalsResponse_Exited{Exited: t}}); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.stopping:
			return nil
		case f, ok := <-w:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted, errors.New("watcher fell behind"))
			}
			if err := stream.Send(f); err != nil {
				return err
			}
		}
	}
}

// ── terminals ──

// Terminal is one PTY and its program.
type Terminal struct {
	h          *Host
	id         string
	argv       []string
	cwd        string
	labels     map[string]string
	cmd        *exec.Cmd
	ptmx       *os.File
	pid        int
	startTicks uint64
	tty        uint64 // the PTY's device number, which the group's members share
	startedAt  time.Time
	replies    chan []byte
	done       chan struct{} // closed once the program exited and its output was read
	readDone   chan struct{}

	// inputClients counts attached clients that may type and are ready; read without mu by reply.
	inputClients atomic.Int32

	mu         sync.Mutex
	ring       *Ring
	screen     Screen
	epoch      uint64
	epochAt    uint64 // the output offset where the current epoch starts
	cols, rows uint16
	clients    map[*client]struct{}
	exited     bool
	exitCode   int32
	signal     int32
	exitedAt   time.Time
	lastOutput time.Time
	lastInput  time.Time // newest input frame from a client with input rights
	lastMark   time.Time
}

func (t *Terminal) isExited() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exited
}

func (t *Terminal) proto() *termv1.Terminal {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := &termv1.Terminal{Id: t.id, Argv: t.argv, Cwd: t.cwd, Pid: int64(t.pid), PidStartTicks: t.startTicks,
		Epoch: t.epoch, Offset: t.ring.End(), Cols: uint32(t.cols), Rows: uint32(t.rows), Exited: t.exited,
		ExitCode: t.exitCode, Signal: t.signal, StartedAt: timestamppb.New(t.startedAt), Labels: t.labels,
		InputClients: uint32(t.inputClients.Load())}
	if t.exited {
		p.ExitedAt = timestamppb.New(t.exitedAt)
	}
	if !t.lastOutput.IsZero() {
		p.LastOutput = timestamppb.New(t.lastOutput)
	}
	if !t.lastInput.IsZero() {
		p.LastClientInput = timestamppb.New(t.lastInput)
	}
	return p
}

// readLoop drains the PTY into the ring, the screen and the attached clients. It never waits for a client.
func (t *Terminal) readLoop() {
	defer close(t.readDone)
	buf := make([]byte, 32<<10)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			t.output(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (t *Terminal) output(p []byte) {
	data := append([]byte(nil), p...)
	now := time.Now()
	t.mu.Lock()
	off := t.ring.End()
	t.ring.Append(data)
	t.screen.Write(data)
	frame := &termv1.TermFrame{Frame: &termv1.TermFrame_Output{Output: &termv1.Output{Epoch: t.epoch, Offset: off, Data: data}}}
	for c := range t.clients {
		t.sendLocked(c, frame)
	}
	t.lastOutput = now
	mark := now.Sub(t.lastMark) >= markEvery
	if mark {
		t.lastMark = now
	}
	end := t.ring.End()
	t.mu.Unlock()
	if mark {
		t.h.broadcast(&termv1.WatchTerminalsResponse{Frame: &termv1.WatchTerminalsResponse_Output{
			Output: &termv1.OutputMark{Id: t.id, Offset: end, Time: timestamppb.New(now)}}})
	}
}

// reply takes the emulator's answers to terminal queries. They go to the program only while no client that
// can type is attached: an attached terminal answers for itself. It never blocks: the emulator calls it while
// the terminal's lock is held, and a program not reading its input must not stall the host. A full queue
// drops the answer.
func (t *Terminal) reply(b []byte) {
	if t.inputClients.Load() != 0 {
		return
	}
	select {
	case t.replies <- b:
	default:
	}
}

func (t *Terminal) replyLoop() {
	for {
		select {
		case b := <-t.replies:
			t.ptmx.Write(b)
		case <-t.done:
			return
		}
	}
}

func (t *Terminal) waitLoop() {
	err := t.cmd.Wait()
	// Read what the program wrote before exiting; a child that keeps the PTY open must not hold the exit up.
	select {
	case <-t.readDone:
	case <-time.After(time.Second):
	}
	var code, sig int32
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig = int32(ws.Signal())
			code = -1
		} else {
			code = int32(ee.ExitCode())
		}
	} else if err != nil {
		code = -1
	}
	t.mu.Lock()
	t.exited, t.exitCode, t.signal, t.exitedAt = true, code, sig, time.Now()
	frame := &termv1.TermFrame{Frame: &termv1.TermFrame_Exited{Exited: &termv1.Exited{ExitCode: code, Signal: sig}}}
	for c := range t.clients {
		t.sendLocked(c, frame)
		t.dropLocked(c, "exited")
	}
	t.mu.Unlock()
	close(t.done)
	t.h.log.Info("terminal exited", "id", t.id, "code", code, "signal", sig)
	t.h.broadcast(&termv1.WatchTerminalsResponse{Frame: &termv1.WatchTerminalsResponse_Exited{Exited: t.proto()}})
}

func (t *Terminal) closeClients(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for c := range t.clients {
		t.dropLocked(c, reason)
	}
}

// resizeLocked sets the PTY to the smallest attached client's size. A change is a sync boundary: the epoch
// moves, so an exact reattach from before it gets a repaint instead, and every attached client but `except` is
// told (`Resized`). It reports whether the size changed.
func (t *Terminal) resizeLocked(except *client) bool {
	var cols, rows uint16
	for c := range t.clients {
		if c.cols == 0 || c.rows == 0 {
			continue
		}
		if cols == 0 || c.cols < cols {
			cols = c.cols
		}
		if rows == 0 || c.rows < rows {
			rows = c.rows
		}
	}
	if cols == 0 || (cols == t.cols && rows == t.rows) {
		return false
	}
	t.setSizeLocked(cols, rows, except)
	return true
}

// setSizeLocked moves the terminal to a size, in a new epoch, and queues a Resized for every attached client but `except`,
// after the output of the old epoch (the same queue) and before that of the new: from the offset it names, the output is in
// the new epoch. A client of the old kind ignores it and finds out as it always did, by the epoch of the next output.
func (t *Terminal) setSizeLocked(cols, rows uint16, except *client) {
	t.cols, t.rows = cols, rows
	t.epoch++
	t.epochAt = t.ring.End()
	t.screen.Resize(int(cols), int(rows))
	if !t.exited {
		pty.Setsize(t.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
	f := frameOf(&termv1.Resized{Epoch: t.epoch, Offset: t.epochAt, Cols: uint32(cols), Rows: uint32(rows)})
	for c := range t.clients {
		if c != except {
			t.sendLocked(c, f)
		}
	}
}

// redraw makes the program repaint: shrink the width by one, then restore it. Ink TUIs redraw only on a shrink.
func (t *Terminal) redraw() {
	t.mu.Lock()
	cols, rows := t.cols, t.rows
	if t.exited || cols < 2 {
		t.mu.Unlock()
		return
	}
	t.setSizeLocked(cols-1, rows, nil)
	t.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	t.mu.Lock()
	if !t.exited && t.cols == cols-1 && t.rows == rows {
		t.setSizeLocked(cols, rows, nil)
	}
	t.mu.Unlock()
}
