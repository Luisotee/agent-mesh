package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DialTCPNoDelay dials a TCP address with TCP_NODELAY explicitly enabled.
func DialTCPNoDelay(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	setNoDelay(c)
	return c, nil
}

// NoDelayListener wraps l so every accepted TCP connection has TCP_NODELAY enabled.
func NoDelayListener(l net.Listener) net.Listener {
	return noDelayListener{Listener: l}
}

type noDelayListener struct{ net.Listener }

func (l noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	setNoDelay(c)
	return c, nil
}

func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// wsAddr implements net.Addr for a WebSocket stream.
type wsAddr string

func (a wsAddr) Network() string { return "websocket" }
func (a wsAddr) String() string  { return string(a) }

// wsStreamConn adapts a binary *websocket.Conn to net.Conn without tying Read deadlines to the underlying
// WebSocket connection. coder/websocket.NetConn closes the WebSocket if a Read deadline fires during an
// active read, which breaks net/http keep-alive idle probes (SetReadDeadline(aLongTimeAgo)).
type wsStreamConn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	local  net.Addr
	remote net.Addr

	inCh     chan []byte
	inClosed chan struct{}
	readErr  error

	readMu       sync.Mutex
	buf          []byte
	deadlineMu   sync.Mutex
	readDeadline time.Time
	readWake     chan struct{}
	writeDead    time.Time

	closeOnce sync.Once
}

// NewWSConn wraps a binary WebSocket connection as a deadline-safe net.Conn.
func NewWSConn(ws *websocket.Conn, local, remote net.Addr) net.Conn {
	ws.SetReadLimit(-1)
	if local == nil {
		local = wsAddr("websocket/local")
	}
	if remote == nil {
		remote = wsAddr("websocket/remote")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &wsStreamConn{
		ws:       ws,
		ctx:      ctx,
		cancel:   cancel,
		local:    local,
		remote:   remote,
		inCh:     make(chan []byte, 64),
		inClosed: make(chan struct{}),
		readWake: make(chan struct{}, 1),
	}
	go c.readLoop()
	return c
}

func (c *wsStreamConn) readLoop() {
	defer close(c.inClosed)
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				c.readErr = io.EOF
			default:
				c.readErr = err
			}
			return
		}
		if typ != websocket.MessageBinary {
			_ = c.ws.Close(websocket.StatusUnsupportedData, "binary frames only")
			c.readErr = errors.New("unexpected text frame on binary stream")
			return
		}
		if len(data) == 0 {
			continue
		}
		select {
		case c.inCh <- data:
		case <-c.ctx.Done():
			c.readErr = net.ErrClosed
			return
		}
	}
}

func (c *wsStreamConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	for {
		c.deadlineMu.Lock()
		dl := c.readDeadline
		c.deadlineMu.Unlock()

		var timer *time.Timer
		var timerC <-chan time.Time
		if !dl.IsZero() {
			rem := time.Until(dl)
			if rem <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(rem)
			timerC = timer.C
		}

		// Non-blocking fast path so queued data takes precedence over a simultaneous close.
		select {
		case data := <-c.inCh:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, data)
			if n < len(data) {
				c.buf = data[n:]
			}
			return n, nil
		default:
		}

		select {
		case data := <-c.inCh:
			if timer != nil {
				timer.Stop()
			}
			n := copy(p, data)
			if n < len(data) {
				c.buf = data[n:]
			}
			return n, nil
		case <-c.inClosed:
			if timer != nil {
				timer.Stop()
			}
			// Drain any remaining buffered chunk before reporting EOF/error.
			select {
			case data := <-c.inCh:
				n := copy(p, data)
				if n < len(data) {
					c.buf = data[n:]
				}
				return n, nil
			default:
			}
			if c.readErr != nil {
				return 0, c.readErr
			}
			return 0, io.EOF
		case <-timerC:
			return 0, os.ErrDeadlineExceeded
		case <-c.readWake:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *wsStreamConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.deadlineMu.Lock()
	dl := c.writeDead
	c.deadlineMu.Unlock()

	wctx := c.ctx
	var cancel context.CancelFunc
	if !dl.IsZero() {
		if time.Now().After(dl) {
			return 0, os.ErrDeadlineExceeded
		}
		wctx, cancel = context.WithDeadline(c.ctx, dl)
		defer cancel()
	}
	if err := c.ws.Write(wctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsStreamConn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	})
	return nil
}

func (c *wsStreamConn) LocalAddr() net.Addr  { return c.local }
func (c *wsStreamConn) RemoteAddr() net.Addr { return c.remote }

func (c *wsStreamConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *wsStreamConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = t
	c.deadlineMu.Unlock()
	select {
	case c.readWake <- struct{}{}:
	default:
	}
	return nil
}

func (c *wsStreamConn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.writeDead = t
	c.deadlineMu.Unlock()
	return nil
}

// Splice copies bytes bidirectionally between a and b until either side closes or ctx is canceled, then
// closes both connections and waits for both copy goroutines to finish.
func Splice(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		buf := make([]byte, 32*1024)
		_, _ = io.CopyBuffer(dst, src, buf)
		// Close both sides when one direction ends so the reverse io.Copy unblocks cleanly over WS/TLS.
		_ = dst.Close()
		_ = src.Close()
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)

	stop := context.AfterFunc(ctx, func() {
		_ = a.Close()
		_ = b.Close()
	})
	defer stop()

	<-done
	<-done
}

// prefixConn replays bytes read alongside the handshake preamble before delegating to the underlying Conn.
type prefixConn struct {
	net.Conn
	extra []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.extra) > 0 {
		n := copy(b, p.extra)
		p.extra = p.extra[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}
