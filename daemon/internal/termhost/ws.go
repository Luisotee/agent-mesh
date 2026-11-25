package termhost

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
)

// WebSocket close codes of the terminal stream.
const (
	CloseExited     websocket.StatusCode = 4000
	CloseSlow       websocket.StatusCode = 4001
	CloseShutdown   websocket.StatusCode = 4002
	CloseBadRequest websocket.StatusCode = 4003
)

func closeCode(reason string) websocket.StatusCode {
	switch reason {
	case "exited":
		return CloseExited
	case "slow":
		return CloseSlow
	case "shutdown":
		return CloseShutdown
	}
	return CloseBadRequest
}

// client is one attached WebSocket.
type client struct {
	out   chan *termv1.TermFrame
	input bool
	ready bool // set under the terminal's mu once the client sent Ready
	cols  uint16
	rows  uint16

	once   sync.Once
	gone   chan struct{}
	reason string
}

// sendLocked queues a frame for a client without waiting; a client whose queue is full is dropped, and
// reattaches by offset.
func (t *Terminal) sendLocked(c *client, f *termv1.TermFrame) {
	select {
	case <-c.gone:
		return
	default:
	}
	select {
	case c.out <- f:
	default:
		t.dropLocked(c, "slow")
	}
}

func (t *Terminal) dropLocked(c *client, reason string) {
	if _, ok := t.clients[c]; ok {
		delete(t.clients, c)
		if c.input && c.ready {
			t.inputClients.Add(-1)
		}
	}
	c.once.Do(func() {
		c.reason = reason
		close(c.gone)
	})
}

func frameOf(f any) *termv1.TermFrame {
	switch v := f.(type) {
	case *termv1.Attached:
		return &termv1.TermFrame{Frame: &termv1.TermFrame_Attached{Attached: v}}
	case *termv1.Output:
		return &termv1.TermFrame{Frame: &termv1.TermFrame_Output{Output: v}}
	case *termv1.Screen:
		return &termv1.TermFrame{Frame: &termv1.TermFrame_Screen{Screen: v}}
	case *termv1.ReplayDone:
		return &termv1.TermFrame{Frame: &termv1.TermFrame_ReplayDone{ReplayDone: v}}
	case *termv1.Exited:
		return &termv1.TermFrame{Frame: &termv1.TermFrame_Exited{Exited: v}}
	}
	panic("unknown frame")
}

// attach registers a client and queues what it needs before live output: Attached, then the replay (exact)
// or the preamble and repaint, then ReplayDone, all under the lock so no live output slips in between.
func (t *Terminal) attach(c *client, a *termv1.Attach) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c.cols, c.rows = clampSize(a.Cols, a.Rows)
	t.clients[c] = struct{}{}
	t.resizeLocked()

	mode := a.Mode
	if mode == termv1.AttachMode_ATTACH_MODE_UNSPECIFIED {
		mode = termv1.AttachMode_ATTACH_MODE_REPAINT
	}
	end := t.ring.End()
	var replay []byte
	reanchored := false
	if mode == termv1.AttachMode_ATTACH_MODE_EXACT {
		data, ok := t.ring.Since(a.Offset)
		if ok && a.Epoch == t.epoch {
			replay = data
		} else {
			mode, reanchored = termv1.AttachMode_ATTACH_MODE_REPAINT, true
		}
	}
	start := end
	if mode == termv1.AttachMode_ATTACH_MODE_EXACT {
		start = a.Offset
	}
	t.sendLocked(c, frameOf(&termv1.Attached{TerminalId: t.id, Mode: mode, Reanchored: reanchored, Epoch: t.epoch,
		Offset: start, Cols: uint32(t.cols), Rows: uint32(t.rows), Input: c.input}))
	switch mode {
	case termv1.AttachMode_ATTACH_MODE_EXACT:
		for off := 0; off < len(replay); off += outputChunk {
			chunk := replay[off:min(off+outputChunk, len(replay))]
			t.sendLocked(c, frameOf(&termv1.Output{Epoch: t.epoch, Offset: start + uint64(off), Data: chunk, Replay: true}))
		}
	case termv1.AttachMode_ATTACH_MODE_TAIL:
		t.sendLocked(c, frameOf(&termv1.Screen{Epoch: t.epoch, Offset: end, Data: t.screen.Preamble()}))
	default:
		t.sendLocked(c, frameOf(&termv1.Screen{Epoch: t.epoch, Offset: end, Data: t.screen.Repaint()}))
	}
	t.sendLocked(c, frameOf(&termv1.ReplayDone{Offset: end}))
	if t.exited {
		t.sendLocked(c, frameOf(&termv1.Exited{ExitCode: t.exitCode, Signal: t.signal}))
		t.dropLocked(c, "exited")
	}
}

func (t *Terminal) detach(c *client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropLocked(c, "detached")
	if len(t.clients) > 0 {
		t.resizeLocked()
	}
}

func (t *Terminal) ready(c *client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.clients[c]; !ok || c.ready {
		return
	}
	c.ready = true
	if c.input {
		t.inputClients.Add(1)
	}
}

// The terminal sizes a client may ask for. A size of 0 is "unknown" and counts for nothing; anything else is
// clamped, so a client can't make the host allocate a screen of 60000 x 60000 cells.
const (
	minCols, maxCols = 20, 500
	minRows, maxRows = 5, 300
)

func clampSize(cols, rows uint32) (uint16, uint16) {
	if cols == 0 || rows == 0 {
		return 0, 0
	}
	return uint16(min(max(cols, minCols), maxCols)), uint16(min(max(rows, minRows), maxRows))
}

// resize applies a client's new size. Only a client with input rights may resize: a viewer's size was set at
// attach, and a ticket for watching must not shrink every other client's terminal.
func (t *Terminal) resize(c *client, cols, rows uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.clients[c]; !ok || !c.input {
		return
	}
	c.cols, c.rows = clampSize(cols, rows)
	t.resizeLocked()
}

func (t *Terminal) canType(c *client) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return c.input && c.ready && !t.exited
}

// serveWS is one client's terminal stream. The daemon is the only caller (with the host token) and says
// whether the client may type (input=1).
func (h *Host) serveWS(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r.Header) {
		http.Error(w, "bad host token", http.StatusUnauthorized)
		return
	}
	t := h.get(r.PathValue("id"))
	if t == nil {
		http.Error(w, "no such terminal", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	actx, acancel := context.WithTimeout(ctx, 10*time.Second)
	first, err := readFrame(actx, conn)
	acancel()
	if err != nil || first.GetAttach() == nil {
		conn.Close(CloseBadRequest, "the first frame must be Attach")
		return
	}
	c := &client{out: make(chan *termv1.TermFrame, clientQueue), input: r.URL.Query().Get("input") == "1",
		gone: make(chan struct{})}
	t.attach(c, first.GetAttach())
	if first.GetAttach().Redraw {
		go t.redraw()
	}
	defer t.detach(c)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer cancel()
		for {
			select {
			case f := <-c.out:
				if err := writeFrame(ctx, conn, f); err != nil {
					return
				}
				continue
			default:
			}
			select {
			case f := <-c.out:
				if err := writeFrame(ctx, conn, f); err != nil {
					return
				}
			case <-c.gone:
				// An exited terminal's client gets what was queued (the Exited frame) first; a slow one doesn't.
				for c.reason == "exited" {
					select {
					case f := <-c.out:
						if writeFrame(ctx, conn, f) != nil {
							return
						}
						continue
					default:
					}
					break
				}
				conn.Close(closeCode(c.reason), c.reason)
				return
			case <-ctx.Done():
				return
			case <-h.stopping:
				conn.Close(CloseShutdown, "shutdown")
				return
			}
		}
	}()

	for {
		f, err := readFrame(ctx, conn)
		if err != nil {
			break
		}
		switch v := f.Frame.(type) {
		case *termv1.TermFrame_Ready:
			t.ready(c)
		case *termv1.TermFrame_Input:
			// Input before Ready is the client answering replayed queries: dropped.
			if t.canType(c) {
				t.mu.Lock()
				t.lastInput = time.Now()
				t.mu.Unlock()
				t.ptmx.Write(v.Input.Data)
			}
		case *termv1.TermFrame_Resize:
			t.resize(c, v.Resize.Cols, v.Resize.Rows)
		}
	}
	cancel()
	<-writerDone
	conn.CloseNow()
}

func readFrame(ctx context.Context, conn *websocket.Conn) (*termv1.TermFrame, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errors.New("text frame on a binary stream")
	}
	f := &termv1.TermFrame{}
	return f, proto.Unmarshal(data, f)
}

func writeFrame(ctx context.Context, conn *websocket.Conn, f *termv1.TermFrame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageBinary, b)
}
