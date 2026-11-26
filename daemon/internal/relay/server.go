package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	relayv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/relay/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
)

// Typed WebSocket close codes on the relay's control endpoint.
const (
	CloseReplaced   websocket.StatusCode = 4001
	CloseBadRequest websocket.StatusCode = 4003
	CloseShutdown   websocket.StatusCode = 4006
)

var (
	errDaemonOffline   = errors.New("daemon offline")
	errDialBackTimeout = errors.New("dial-back timed out")
	errRelayBusy       = errors.New("too many pending streams for host")
)

// ServerConfig configures a relay Server.
type ServerConfig struct {
	// Secret is the shared token that daemons authenticate with and client tokens derive from. Required.
	Secret string
	Logger *slog.Logger
	Now    func() time.Time
	// PreambleTimeout bounds how long a raw TCP client has to send its MESH1 line (default 10s).
	PreambleTimeout time.Duration
	// TicketTTL bounds how long a dial-back ticket remains valid (default 15s).
	TicketTTL time.Duration
	// DialBackTimeout bounds how long an app connection waits for the daemon to dial back (default 10s).
	DialBackTimeout time.Duration
	// MaxPendingPerHost caps simultaneous unresolved dial-back tickets per host (default 64).
	MaxPendingPerHost int
}

type pendingStream struct {
	conn net.Conn
	done chan struct{}
}

type pendingTicket struct {
	hostID    string
	expires   time.Time
	abandoned bool
	ch        chan pendingStream
}

type hostSession struct {
	id      string
	name    string
	ws      *websocket.Conn
	writeMu sync.Mutex
	closed  chan struct{}
	cancel  context.CancelFunc

	// deviceTokens: the daemon gives its devices' tokens (Register.device_tokens), and tokens holds them by
	// hash, with the time each expires on the relay's clock (zero: never). Without it, the host-wide token is
	// checked.
	deviceTokens bool
	tokensMu     sync.Mutex
	tokens       map[[sha256.Size]byte]time.Time
}

// MaxClientTokens caps the client tokens a daemon may give the relay for one host.
const MaxClientTokens = 1000

func (h *hostSession) setTokens(list []*relayv1.ClientToken, now time.Time) {
	m := make(map[[sha256.Size]byte]time.Time, len(list))
	for i, t := range list {
		if i == MaxClientTokens {
			break
		}
		if len(t.GetSha256()) != sha256.Size {
			continue
		}
		var exp time.Time
		if ttl := t.GetTtlSeconds(); ttl > 0 {
			exp = now.Add(time.Duration(ttl) * time.Second)
		}
		m[[sha256.Size]byte(t.GetSha256())] = exp
	}
	h.tokensMu.Lock()
	h.tokens = m
	h.tokensMu.Unlock()
}

func (h *hostSession) acceptsToken(token string, now time.Time) bool {
	if token == "" {
		return false
	}
	sum := TokenHash(token)
	h.tokensMu.Lock()
	exp, ok := h.tokens[sum]
	h.tokensMu.Unlock()
	return ok && (exp.IsZero() || !now.After(exp))
}

func (h *hostSession) sendFrame(ctx context.Context, f *relayv1.ControlFrame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.ws.Write(wctx, websocket.MessageBinary, b)
}

// Server is the self-hosted relay server.
type Server struct {
	cfg ServerConfig
	log *slog.Logger
	now func() time.Time
	mux *http.ServeMux

	mu       sync.Mutex
	hosts    map[string]*hostSession
	tickets  map[string]*pendingTicket
	stopping chan struct{}
	stopOnce sync.Once
}

// NewServer creates a relay Server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, errors.New("relay secret is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PreambleTimeout == 0 {
		cfg.PreambleTimeout = 10 * time.Second
	}
	if cfg.TicketTTL == 0 {
		cfg.TicketTTL = 15 * time.Second
	}
	if cfg.DialBackTimeout == 0 {
		cfg.DialBackTimeout = 10 * time.Second
	}
	if cfg.MaxPendingPerHost == 0 {
		cfg.MaxPendingPerHost = 64
	}
	s := &Server{
		cfg:      cfg,
		log:      cfg.Logger,
		now:      cfg.Now,
		mux:      http.NewServeMux(),
		hosts:    map[string]*hostSession{},
		tickets:  map[string]*pendingTicket{},
		stopping: make(chan struct{}),
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /v1/daemon/control", s.serveDaemonControl)
	s.mux.HandleFunc("GET /v1/daemon/stream", s.serveDaemonStream)
	s.mux.HandleFunc("GET /v1/client/connect", s.serveClientWS)
	return s, nil
}

// Handler returns the relay's HTTP/WebSocket handler (for --listen, typically behind TLS / reverse proxy).
func (s *Server) Handler() http.Handler { return s.mux }

// Close signals active sessions to shut down.
func (s *Server) Close() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

// HostOnline reports whether hostID currently has an active control session.
func (s *Server) HostOnline(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.hosts[hostID]
	return ok
}

// ServeClientTCP accepts raw TCP connections on l, reads the MESH1 preamble, requests a dial-back from
// the daemon, answers OK\n, and splices raw bytes without inspecting inner TLS.
func (s *Server) ServeClientTCP(ctx context.Context, l net.Listener) error {
	nl := NoDelayListener(l)
	var tempDelay time.Duration
	for {
		c, err := nl.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			select {
			case <-s.stopping:
				return nil
			default:
			}
			if !temporaryAcceptError(err) {
				return err
			}
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay = min(tempDelay*2, time.Second)
			}
			s.log.Warn("relay client TCP accept error; retrying", "err", err, "backoff", tempDelay)
			timer := time.NewTimer(tempDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-s.stopping:
				timer.Stop()
				return nil
			case <-timer.C:
			}
			continue
		}
		tempDelay = 0
		go s.HandleClientConn(ctx, c)
	}
}

// temporaryAcceptError reports whether an Accept error is worth retrying: a timeout, or running out of file
// descriptors or a connection aborted before Accept returned it.
func temporaryAcceptError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
}

// HandleClientConn handles one raw TCP client connection.
func (s *Server) HandleClientConn(ctx context.Context, rawConn net.Conn) {
	setNoDelay(rawConn)
	defer rawConn.Close()

	_ = rawConn.SetReadDeadline(time.Now().Add(s.cfg.PreambleTimeout))
	line, extra, err := readPreambleLine(rawConn, 512)
	_ = rawConn.SetReadDeadline(time.Time{})
	if err != nil {
		writeTCPAbort(rawConn, "bad_request", err.Error())
		return
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "MESH1" {
		writeTCPAbort(rawConn, "bad_request", "expected MESH1 <host_id> <token>")
		return
	}
	hostID, tok := parts[1], parts[2]
	if err := s.authorizeClient(hostID, tok); err != nil {
		if errors.Is(err, errDaemonOffline) {
			writeTCPAbort(rawConn, "offline", "daemon offline")
		} else {
			writeTCPAbort(rawConn, "auth", "invalid relay token")
		}
		return
	}

	ps, err := s.openStream(ctx, hostID)
	if err != nil {
		switch {
		case errors.Is(err, errDaemonOffline):
			writeTCPAbort(rawConn, "offline", "daemon offline")
		case errors.Is(err, errRelayBusy):
			writeTCPAbort(rawConn, "busy", "too many pending streams")
		case errors.Is(err, errDialBackTimeout):
			writeTCPAbort(rawConn, "timeout", "dial-back timed out")
		default:
			writeTCPAbort(rawConn, "unavailable", err.Error())
		}
		return
	}
	defer close(ps.done)

	_ = rawConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = rawConn.Write([]byte("OK\n"))
	_ = rawConn.SetWriteDeadline(time.Time{})
	if err != nil {
		_ = ps.conn.Close()
		return
	}

	clientConn := rawConn
	if len(extra) > 0 {
		clientConn = &prefixConn{Conn: rawConn, extra: extra}
	}
	Splice(ctx, clientConn, ps.conn)
}

var errBadClientToken = errors.New("invalid relay token")

// authorizeClient checks a client's relay token for hostID before any dial-back is asked for. A daemon that
// gives its devices' tokens is held to them; one that doesn't (an older daemon) to the host-wide token.
func (s *Server) authorizeClient(hostID, tok string) error {
	s.mu.Lock()
	sess := s.hosts[hostID]
	s.mu.Unlock()
	if sess == nil {
		// A device's token can't be judged without its host's list, so any token hears offline.
		return errDaemonOffline
	}
	if sess.deviceTokens {
		if sess.acceptsToken(tok, s.now()) {
			return nil
		}
		return errBadClientToken
	}
	if ValidClientToken(s.cfg.Secret, hostID, tok) {
		return nil
	}
	return errBadClientToken
}

func writeTCPAbort(c net.Conn, code, msg string) {
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\n", " "), "\r", " ")
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "ERR %s %s\n", code, msg)
}

// readPreambleLine reads one newline-terminated line up to maxBytes, returning any bytes that arrived after
// the newline in the same Read so pipelined bytes are preserved.
func readPreambleLine(r io.Reader, maxBytes int) (string, []byte, error) {
	buf := make([]byte, maxBytes)
	total := 0
	for total < maxBytes {
		n, err := r.Read(buf[total:])
		if n > 0 {
			searchStart := total
			total += n
			if idx := bytes.IndexByte(buf[searchStart:total], '\n'); idx >= 0 {
				nl := searchStart + idx
				line := strings.TrimRight(string(buf[:nl]), "\r")
				var extra []byte
				if nl+1 < total {
					extra = append([]byte(nil), buf[nl+1:total]...)
				}
				return line, extra, nil
			}
		}
		if err != nil {
			return "", nil, err
		}
	}
	return "", nil, errors.New("preamble line too long")
}

func (s *Server) serveDaemonControl(w http.ResponseWriter, r *http.Request) {
	if !ValidSecret(s.cfg.Secret, auth.BearerToken(r.Header)) {
		http.Error(w, "invalid relay token", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	regFrame, err := readControlFrame(hctx, ws)
	hcancel()
	if err != nil || regFrame.GetRegister() == nil || strings.TrimSpace(regFrame.GetRegister().HostId) == "" {
		_ = ws.Close(CloseBadRequest, "first frame must be Register with host_id")
		return
	}
	reg := regFrame.GetRegister()
	sess := &hostSession{
		id:           reg.HostId,
		name:         reg.HostName,
		ws:           ws,
		closed:       make(chan struct{}),
		cancel:       cancel,
		deviceTokens: reg.DeviceTokens,
	}
	if reg.DeviceTokens {
		sess.setTokens(reg.ClientTokens, s.now())
	}

	s.mu.Lock()
	old := s.hosts[sess.id]
	s.hosts[sess.id] = sess
	s.mu.Unlock()
	if old != nil {
		s.log.Info("replacing daemon control connection", "host", sess.id)
		_ = old.ws.Close(CloseReplaced, "replaced")
		old.cancel()
	}

	defer func() {
		close(sess.closed)
		s.mu.Lock()
		if s.hosts[sess.id] == sess {
			delete(s.hosts, sess.id)
		}
		s.mu.Unlock()
		s.log.Info("daemon control disconnected", "host", sess.id)
	}()

	if err := sess.sendFrame(ctx, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_Registered{Registered: &relayv1.Registered{HostId: sess.id}},
	}); err != nil {
		_ = ws.CloseNow()
		return
	}
	s.log.Info("daemon registered", "host", sess.id, "name", sess.name, "version", reg.DaemonVersion,
		"device_tokens", reg.DeviceTokens)

	go func() {
		select {
		case <-s.stopping:
			_ = ws.Close(CloseShutdown, "relay shutting down")
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		f, err := readControlFrame(ctx, ws)
		if err != nil {
			_ = ws.CloseNow()
			return
		}
		switch v := f.Frame.(type) {
		case *relayv1.ControlFrame_Ping:
			if err := sess.sendFrame(ctx, &relayv1.ControlFrame{
				Frame: &relayv1.ControlFrame_Pong{Pong: &relayv1.Pong{Seq: v.Ping.Seq}},
			}); err != nil {
				_ = ws.CloseNow()
				return
			}
		case *relayv1.ControlFrame_ClientTokens:
			if sess.deviceTokens {
				sess.setTokens(v.ClientTokens.GetTokens(), s.now())
			}
		}
	}
}

func (s *Server) serveDaemonStream(w http.ResponseWriter, r *http.Request) {
	if !ValidSecret(s.cfg.Secret, auth.BearerToken(r.Header)) {
		http.Error(w, "invalid relay token", http.StatusUnauthorized)
		return
	}
	tk := r.URL.Query().Get("ticket")
	if tk == "" {
		http.Error(w, "missing ticket", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	pt, ok := s.tickets[tk]
	if ok {
		delete(s.tickets, tk)
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "invalid or already used ticket", http.StatusNotFound)
		return
	}
	if s.now().After(pt.expires) {
		http.Error(w, "ticket expired", http.StatusGone)
		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	dc := newWSConn(ws, nil, nil)
	done := make(chan struct{})
	s.mu.Lock()
	if pt.abandoned {
		s.mu.Unlock()
		_ = dc.Close()
		return
	}
	pt.ch <- pendingStream{conn: dc, done: done}
	s.mu.Unlock()

	select {
	case <-done:
	case <-dc.done():
	case <-s.stopping:
		_ = dc.Close()
	case <-r.Context().Done():
		_ = dc.Close()
	}
}

// serveClientWS is an optional WebSocket client endpoint (/v1/client/connect?host_id=...) for future browser
// clients; native clients use ServeClientTCP directly.
func (s *Server) serveClientWS(w http.ResponseWriter, r *http.Request) {
	hostID := r.URL.Query().Get("host_id")
	if hostID == "" {
		http.Error(w, "missing host_id", http.StatusBadRequest)
		return
	}
	if err := s.authorizeClient(hostID, auth.BearerToken(r.Header)); err != nil {
		if errors.Is(err, errDaemonOffline) {
			http.Error(w, "daemon offline", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "invalid relay token", http.StatusUnauthorized)
		}
		return
	}
	ps, err := s.openStream(r.Context(), hostID)
	if err != nil {
		switch {
		case errors.Is(err, errDaemonOffline):
			http.Error(w, "daemon offline", http.StatusServiceUnavailable)
		case errors.Is(err, errRelayBusy):
			http.Error(w, "too many pending streams", http.StatusTooManyRequests)
		case errors.Is(err, errDialBackTimeout):
			http.Error(w, "dial-back timed out", http.StatusGatewayTimeout)
		default:
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
		return
	}
	defer close(ps.done)

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		_ = ps.conn.Close()
		return
	}
	cc := NewWSConn(ws, nil, nil)
	Splice(r.Context(), cc, ps.conn)
}

func (s *Server) openStream(ctx context.Context, hostID string) (pendingStream, error) {
	s.mu.Lock()
	sess := s.hosts[hostID]
	if sess == nil {
		s.mu.Unlock()
		return pendingStream{}, errDaemonOffline
	}
	now := s.now()
	pendingCount := 0
	for k, pt := range s.tickets {
		if now.After(pt.expires) {
			delete(s.tickets, k)
			continue
		}
		if pt.hostID == hostID {
			pendingCount++
		}
	}
	if pendingCount >= s.cfg.MaxPendingPerHost {
		s.mu.Unlock()
		return pendingStream{}, errRelayBusy
	}
	tk := ids.Secret("rtk_")
	streamID := ids.New("s", 10)
	pt := &pendingTicket{
		hostID:  hostID,
		expires: now.Add(s.cfg.TicketTTL),
		ch:      make(chan pendingStream, 1),
	}
	s.tickets[tk] = pt
	s.mu.Unlock()

	abandon := func() {
		s.mu.Lock()
		delete(s.tickets, tk)
		pt.abandoned = true
		s.mu.Unlock()
		// Drain if serveDaemonStream delivered just before pt.abandoned was set.
		select {
		case ps := <-pt.ch:
			_ = ps.conn.Close()
			close(ps.done)
		default:
		}
	}

	if err := sess.sendFrame(ctx, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_DialBack{
			DialBack: &relayv1.DialBack{Ticket: tk, StreamId: streamID},
		},
	}); err != nil {
		abandon()
		return pendingStream{}, errDaemonOffline
	}

	timer := time.NewTimer(s.cfg.DialBackTimeout)
	defer timer.Stop()

	select {
	case ps := <-pt.ch:
		return ps, nil
	case <-sess.closed:
		abandon()
		return pendingStream{}, errDaemonOffline
	case <-s.stopping:
		abandon()
		return pendingStream{}, errors.New("relay shutting down")
	case <-ctx.Done():
		abandon()
		return pendingStream{}, ctx.Err()
	case <-timer.C:
		abandon()
		return pendingStream{}, errDialBackTimeout
	}
}

func readControlFrame(ctx context.Context, ws *websocket.Conn) (*relayv1.ControlFrame, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		return nil, errors.New("text frame on binary control stream")
	}
	f := &relayv1.ControlFrame{}
	if err := proto.Unmarshal(data, f); err != nil {
		return nil, err
	}
	return f, nil
}

func writeControlFrame(ctx context.Context, ws *websocket.Conn, f *relayv1.ControlFrame) error {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageBinary, b)
}
