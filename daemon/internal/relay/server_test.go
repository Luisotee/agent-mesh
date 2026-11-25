package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	relayv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/relay/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

func startTestRelay(t *testing.T, secret string) (*Server, *httptest.Server, string) {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Secret:          secret,
		TicketTTL:       5 * time.Second,
		DialBackTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.Close()
		hs.Close()
	})
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		tcpLn.Close()
	})
	go func() { _ = srv.ServeClientTCP(ctx, tcpLn) }()
	return srv, hs, tcpLn.Addr().String()
}

func startDaemonEchoTLS(t *testing.T, relayURL, secret, hostID string) (*Connector, string) {
	t.Helper()
	dir := t.TempDir()
	tlsCfg, fp, err := tlsutil.LoadOrCreate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), hostID)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewConnector(ConnectorConfig{
		RelayURL:          relayURL,
		Secret:            secret,
		HostID:            hostID,
		HostName:          "devbox",
		DaemonVersion:     "test",
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = conn.Run(ctx) }()

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	hs := &http.Server{
		TLSConfig: tlsCfg,
		Protocols: protocols,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "hello %s %s", r.URL.Path, r.Proto)
		}),
	}
	go func() { _ = hs.ServeTLS(conn.Listener(), "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	return conn, fp
}

func waitOnline(t *testing.T, s *Server, hostID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !s.HostOnline(hostID) {
		if time.Now().After(deadline) {
			t.Fatalf("host %s never came online", hostID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRelayDialBackE2ETLSAndKeepAlive(t *testing.T) {
	secret := NewSecret()
	srv, hs, tcpAddr := startTestRelay(t, secret)
	_, fp := startDaemonEchoTLS(t, hs.URL, secret, "h-1")
	waitOnline(t, srv, "h-1")

	clientTok := ClientToken(secret, "h-1")
	for _, http2 := range []bool{false, true} {
		hc, err := NewTCPHTTPClient(tcpAddr, "h-1", clientTok, fp, http2)
		if err != nil {
			t.Fatal(err)
		}
		// Multiple sequential requests exercise HTTP keep-alive and deadline resets over spliced WS.
		for i := 0; i < 3; i++ {
			res, err := hc.Get(fmt.Sprintf("https://daemon.invalid/ping/%d", i))
			if err != nil {
				t.Fatalf("http2=%v req %d: %v", http2, i, err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if !strings.HasPrefix(string(b), fmt.Sprintf("hello /ping/%d", i)) {
				t.Fatalf("http2=%v got %q", http2, b)
			}
		}
		hc.CloseIdleConnections()
	}

	// Optional WS client endpoint also carries pinned TLS end to end.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsTLS, err := tlsutil.PinnedConfig(fp, "http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	_ = wsTLS
	wsHC := &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return DialRelayWS(ctx, http.DefaultClient, hs.URL, "h-1", clientTok, fp, "http/1.1")
			},
		},
	}
	res, err := wsHC.Get("https://daemon.invalid/ws-path")
	if err != nil {
		t.Fatalf("ws route: %v", err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(string(b), "hello /ws-path") {
		t.Fatalf("ws route got %q", b)
	}
	_ = ctx
}

func TestRelayRejectsPinMismatchAndOfflineAndBadAuth(t *testing.T) {
	secret := NewSecret()
	srv, hs, tcpAddr := startTestRelay(t, secret)
	_, fp := startDaemonEchoTLS(t, hs.URL, secret, "h-1")
	waitOnline(t, srv, "h-1")

	otherDir := t.TempDir()
	_, wrongFP, err := tlsutil.LoadOrCreate(filepath.Join(otherDir, "tls.crt"), filepath.Join(otherDir, "tls.key"), "other")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTok := ClientToken(secret, "h-1")

	// Pin mismatch fails at TLS handshake.
	if _, err := DialRelayTCP(ctx, tcpAddr, "h-1", clientTok, wrongFP, "h2"); !errors.Is(err, tlsutil.ErrPinMismatch) {
		t.Fatalf("wrong pin: got %v, want ErrPinMismatch", err)
	}
	// Wrong client token fails at relay preamble before dial-back.
	if _, err := DialRelayTCP(ctx, tcpAddr, "h-1", "amr_wrong", fp, "h2"); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong token: got %v, want ErrAuth", err)
	}
	// Cross-host token reuse fails.
	if _, err := DialRelayTCP(ctx, tcpAddr, "h-1", ClientToken(secret, "h-2"), fp, "h2"); !errors.Is(err, ErrAuth) {
		t.Fatalf("cross-host token: got %v, want ErrAuth", err)
	}
	// Offline daemon fails fast.
	if _, err := DialRelayTCP(ctx, tcpAddr, "h-offline", ClientToken(secret, "h-offline"), fp, "h2"); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline host: got %v, want ErrOffline", err)
	}
}

func TestRelayTicketIsSingleUseAndHostBound(t *testing.T) {
	secret := NewSecret()
	srv, hs, _ := startTestRelay(t, secret)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect a fake daemon control WS manually so we receive the DialBack ticket directly.
	wsBase, _ := toWSBase(hs.URL)
	hdr := http.Header{"Authorization": {"Bearer " + secret}}
	ctrlWS, _, err := websocket.Dial(ctx, wsBase+"/v1/daemon/control", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlWS.CloseNow()
	if err := writeControlFrame(ctx, ctrlWS, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_Register{Register: &relayv1.Register{HostId: "h-ticket"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ack, err := readControlFrame(ctx, ctrlWS); err != nil || ack.GetRegistered() == nil {
		t.Fatalf("ack = %v, %v", ack, err)
	}

	// Open a stream from openStream in background; read the DialBack ticket from control WS.
	type result struct {
		ps  pendingStream
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		ps, err := srv.openStream(ctx, "h-ticket")
		resCh <- result{ps, err}
	}()

	dbFrame, err := readControlFrame(ctx, ctrlWS)
	if err != nil {
		t.Fatal(err)
	}
	ticket := dbFrame.GetDialBack().GetTicket()
	if ticket == "" {
		t.Fatal("empty dial-back ticket")
	}

	// First use succeeds.
	stream1, resp1, err := websocket.Dial(ctx, wsBase+"/v1/daemon/stream?ticket="+ticket, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("first ticket dial failed: %v (resp=%v)", err, resp1)
	}
	defer stream1.CloseNow()

	r := <-resCh
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer func() {
		_ = r.ps.conn.Close()
		close(r.ps.done)
	}()

	// Reusing the exact same ticket is refused.
	if stream2, resp2, err := websocket.Dial(ctx, wsBase+"/v1/daemon/stream?ticket="+ticket, &websocket.DialOptions{HTTPHeader: hdr}); err == nil {
		stream2.CloseNow()
		t.Fatal("reused ticket succeeded")
	} else if resp2 == nil || resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("reused ticket status = %v, err = %v; want 404", resp2, err)
	}
}

func TestRelayReconnectReplacesOldControlConnection(t *testing.T) {
	secret := NewSecret()
	srv, hs, tcpAddr := startTestRelay(t, secret)
	conn1, fp1 := startDaemonEchoTLS(t, hs.URL, secret, "h-dup")
	waitOnline(t, srv, "h-dup")

	// Start a second Connector for the same host_id: the first gets CloseReplaced and stops cleanly.
	conn2, fp2 := startDaemonEchoTLS(t, hs.URL, secret, "h-dup")
	deadline := time.Now().Add(5 * time.Second)
	for conn1.Online() || !conn2.Online() {
		if time.Now().After(deadline) {
			t.Fatalf("expected conn1 offline (%v) and conn2 online (%v)", conn1.Online(), conn2.Online())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = fp1

	// Traffic now routes to conn2 (verified by conn2's cert fingerprint).
	hc, err := NewTCPHTTPClient(tcpAddr, "h-dup", ClientToken(secret, "h-dup"), fp2, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := hc.Get("https://daemon.invalid/after-replace")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(string(b), "hello /after-replace") {
		t.Fatalf("got %q", b)
	}
}

func TestRelayConnectorStopsOnBadToken(t *testing.T) {
	secret := NewSecret()
	_, hs, _ := startTestRelay(t, secret)

	conn, err := NewConnector(ConnectorConfig{
		RelayURL: hs.URL,
		Secret:   "amr_wrong",
		HostID:   "h-bad",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = conn.Run(ctx)
	if !errors.Is(err, errControlAuthFailed) {
		t.Fatalf("Run = %v, want errControlAuthFailed", err)
	}
}

func TestRelayPreambleReadPreservesPipelinedBytes(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		_, _ = c1.Write([]byte("MESH1 h1 tok1\nEXTRA"))
	}()
	line, extra, err := readPreambleLine(c2, 512)
	if err != nil {
		t.Fatal(err)
	}
	if line != "MESH1 h1 tok1" || string(extra) != "EXTRA" {
		t.Fatalf("got line=%q extra=%q", line, extra)
	}
	_ = tls.RecordHeaderError{}
}

func TestRelayRawTCPTimeoutAndSlowDialBack(t *testing.T) {
	secret := NewSecret()
	// Use a past clock for s.now() to verify socket deadlines use time.Now(), not the injected clock.
	fixedNow := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	srv, err := NewServer(ServerConfig{
		Secret:          secret,
		Now:             func() time.Time { return fixedNow },
		PreambleTimeout: 120 * time.Millisecond,
		TicketTTL:       5 * time.Second,
		DialBackTimeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.Close()
		hs.Close()
	})
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		tcpLn.Close()
	})
	go func() { _ = srv.ServeClientTCP(ctx, tcpLn) }()

	// Connect a fake daemon control WS so we control whether and when it dials back.
	wsBase, _ := toWSBase(hs.URL)
	hdr := http.Header{"Authorization": {"Bearer " + secret}}
	ctrlWS, _, err := websocket.Dial(ctx, wsBase+"/v1/daemon/control", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlWS.CloseNow()
	if err := writeControlFrame(ctx, ctrlWS, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_Register{Register: &relayv1.Register{HostId: "h-slow"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ack, err := readControlFrame(ctx, ctrlWS); err != nil || ack.GetRegistered() == nil {
		t.Fatalf("ack = %v, %v", ack, err)
	}

	dir := t.TempDir()
	_, fp, err := tlsutil.LoadOrCreate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "h-slow")
	if err != nil {
		t.Fatal(err)
	}
	clientTok := ClientToken(secret, "h-slow")

	// 1. Daemon ignores DialBack -> relay must deliver ERR timeout (not fail on an expired preamble deadline).
	dctx, dcancel := context.WithTimeout(ctx, 3*time.Second)
	defer dcancel()
	if _, err := DialRelayTCP(dctx, tcpLn.Addr().String(), "h-slow", clientTok, fp, "http/1.1"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("dial-back timeout = %v, want ErrTimeout", err)
	}
	// Drain the ignored DialBack frame from control WS.
	if f, err := readControlFrame(dctx, ctrlWS); err != nil || f.GetDialBack() == nil {
		t.Fatalf("expected DialBack frame, got %v, %v", f, err)
	}

	// 2. Slow preamble (70ms) + slow dial-back (90ms) = 160ms > PreambleTimeout (120ms), but each is within
	// its own timeout -> OK\n must still be delivered and spliced cleanly.
	raw, err := DialTCPNoDelay(dctx, "tcp", tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	time.Sleep(70 * time.Millisecond)
	if _, err := fmt.Fprintf(raw, "MESH1 h-slow %s\n", clientTok); err != nil {
		t.Fatal(err)
	}
	dbFrame, err := readControlFrame(dctx, ctrlWS)
	if err != nil || dbFrame.GetDialBack() == nil {
		t.Fatalf("expected second DialBack, got %v, %v", dbFrame, err)
	}
	time.Sleep(90 * time.Millisecond)
	streamWS, _, err := websocket.Dial(dctx, wsBase+"/v1/daemon/stream?ticket="+dbFrame.GetDialBack().Ticket,
		&websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("slow dial-back stream dial failed: %v", err)
	}
	defer streamWS.CloseNow()

	line, _, err := readPreambleLine(raw, 512)
	if err != nil || line != "OK" {
		t.Fatalf("slow dial-back reply = %q, %v; want OK", line, err)
	}
}

func TestRelayDialBackRaceWithTimeoutDoesNotLeak(t *testing.T) {
	secret := NewSecret()
	srv, hs, _ := startTestRelay(t, secret)
	wsBase, _ := toWSBase(hs.URL)
	hdr := http.Header{"Authorization": {"Bearer " + secret}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Case 1: ticket abandoned between claim and channel send (pt.abandoned == true) ->
	// serveDaemonStream closes the WebSocket and returns immediately without leaking.
	pt := &pendingTicket{
		hostID:    "h-race",
		expires:   time.Now().Add(time.Minute),
		abandoned: true,
		ch:        make(chan pendingStream, 1),
	}
	srv.mu.Lock()
	srv.tickets["rtk_abandoned"] = pt
	srv.mu.Unlock()

	ws, _, err := websocket.Dial(ctx, wsBase+"/v1/daemon/stream?ticket=rtk_abandoned", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial abandoned ticket: %v", err)
	}
	defer ws.CloseNow()
	// Because serveDaemonStream closes dc immediately on pt.abandoned, ws.Read must return promptly.
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	if _, _, err := ws.Read(rctx); err == nil {
		t.Fatal("expected abandoned dial-back WebSocket to be closed")
	}

	// Case 2: serveDaemonStream delivered to pt.ch, then openStream's ctx was already canceled ->
	// abandon() drains pt.ch, closes the conn and closes done so serveDaemonStream exits cleanly.
	srv2, err := NewServer(ServerConfig{
		Secret:          secret,
		TicketTTL:       5 * time.Second,
		DialBackTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs2 := httptest.NewServer(srv2.Handler())
	defer func() {
		srv2.Close()
		hs2.Close()
	}()
	wsBase2, _ := toWSBase(hs2.URL)
	ctrlWS, _, err := websocket.Dial(ctx, wsBase2+"/v1/daemon/control", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlWS.CloseNow()
	if err := writeControlFrame(ctx, ctrlWS, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_Register{Register: &relayv1.Register{HostId: "h-race2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if ack, err := readControlFrame(ctx, ctrlWS); err != nil || ack.GetRegistered() == nil {
		t.Fatalf("ack = %v, %v", ack, err)
	}

	openCtx, openCancel := context.WithCancel(ctx)
	openDone := make(chan error, 1)
	go func() {
		ps, err := srv2.openStream(openCtx, "h-race2")
		if err == nil {
			_ = ps.conn.Close()
			close(ps.done)
		}
		openDone <- err
	}()
	dbFrame, err := readControlFrame(ctx, ctrlWS)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel client and race dial-back simultaneously; neither side may hang.
	openCancel()
	streamWS, _, dialErr := websocket.Dial(ctx, wsBase2+"/v1/daemon/stream?ticket="+dbFrame.GetDialBack().Ticket,
		&websocket.DialOptions{HTTPHeader: hdr})
	if dialErr == nil {
		defer streamWS.CloseNow()
		rctx2, rcancel2 := context.WithTimeout(ctx, 2*time.Second)
		defer rcancel2()
		if _, _, err := streamWS.Read(rctx2); err == nil {
			t.Fatal("raced dial-back stream was not closed after client cancel")
		}
	}
	select {
	case <-openDone:
	case <-time.After(2 * time.Second):
		t.Fatal("openStream did not return after cancel")
	}
}

type flakyListener struct {
	mu      sync.Mutex
	errs    []error
	conns   chan net.Conn
	closed  chan struct{}
	closeOp sync.Once
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	select {
	case c := <-f.conns:
		return c, nil
	case <-f.closed:
		return nil, net.ErrClosed
	}
}

func (f *flakyListener) Close() error {
	f.closeOp.Do(func() { close(f.closed) })
	return nil
}

func (f *flakyListener) Addr() net.Addr { return wsAddr("flaky") }

func TestServeClientTCPRetriesTransientAcceptErrors(t *testing.T) {
	secret := NewSecret()
	srv, err := NewServer(ServerConfig{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	fl := &flakyListener{
		errs:   []error{syscall.EMFILE, syscall.ECONNABORTED},
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
	defer fl.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ServeClientTCP(ctx, fl) }()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	fl.conns <- serverConn

	tok := ClientToken(secret, "h-after-emfile")
	if _, err := fmt.Fprintf(clientConn, "MESH1 h-after-emfile %s\n", tok); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _, err := readPreambleLine(clientConn, 512)
	if err != nil {
		t.Fatalf("failed to read reply after transient Accept errors: %v", err)
	}
	if !strings.HasPrefix(line, "ERR offline ") {
		t.Fatalf("got %q, want ERR offline ...", line)
	}

	cancel()
	fl.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeClientTCP returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeClientTCP did not stop on close")
	}
}

type brokenListener struct {
	flakyListener
	err error
}

func (b *brokenListener) Accept() (net.Conn, error) { return nil, b.err }

func TestServeClientTCPReturnsOnPermanentAcceptError(t *testing.T) {
	srv, err := NewServer(ServerConfig{Secret: NewSecret()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	want := errors.New("listener is broken")
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ServeClientTCP(context.Background(), &brokenListener{err: want}) }()
	select {
	case err := <-serveDone:
		if !errors.Is(err, want) {
			t.Fatalf("ServeClientTCP returned %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeClientTCP kept retrying a permanent Accept error")
	}
}
