package relay

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	relayv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/relay/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

// noPin is a well-formed fingerprint no certificate has: a refused client never gets as far as TLS.
var noPin = "sha256:" + strings.Repeat("0", 64)

func tokenEntry(tok string, ttl int64) *relayv1.ClientToken {
	h := TokenHash(tok)
	return &relayv1.ClientToken{Sha256: h[:], TtlSeconds: ttl}
}

// fakeControl registers hostID on the relay by hand and counts the DialBack frames it gets, so a test can tell
// that a refused client never made the relay ask for a dial-back.
type fakeControl struct {
	ws        *websocket.Conn
	dialBacks atomic.Int32
}

func dialFakeControl(t *testing.T, ctx context.Context, relayURL, secret string, reg *relayv1.Register) *fakeControl {
	t.Helper()
	wsBase, _ := toWSBase(relayURL)
	ws, _, err := websocket.Dial(ctx, wsBase+"/v1/daemon/control", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	if err := writeControlFrame(ctx, ws, &relayv1.ControlFrame{Frame: &relayv1.ControlFrame_Register{Register: reg}}); err != nil {
		t.Fatal(err)
	}
	if f, err := readControlFrame(ctx, ws); err != nil || f.GetRegistered() == nil {
		t.Fatalf("registration: %v %v", f, err)
	}
	fc := &fakeControl{ws: ws}
	go func() {
		for {
			f, err := readControlFrame(context.Background(), ws)
			if err != nil {
				return
			}
			if f.GetDialBack() != nil {
				fc.dialBacks.Add(1)
			}
		}
	}()
	return fc
}

func (fc *fakeControl) send(t *testing.T, ctx context.Context, list ...*relayv1.ClientToken) {
	t.Helper()
	if err := writeControlFrame(ctx, fc.ws, &relayv1.ControlFrame{
		Frame: &relayv1.ControlFrame_ClientTokens{ClientTokens: &relayv1.ClientTokens{Tokens: list}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayDeviceTokensGateThePreamble(t *testing.T) {
	secret := NewSecret()
	srv, hs, tcpAddr := startTestRelay(t, secret)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	devA := DeviceToken(secret, "h-1", "dev-a")
	devB := DeviceToken(secret, "h-1", "dev-b")
	pending := DeviceToken(secret, "h-1", "dev-pending")
	expired := DeviceToken(secret, "h-1", "dev-expired")
	fc := dialFakeControl(t, ctx, hs.URL, secret, &relayv1.Register{
		HostId: "h-1", DeviceTokens: true,
		ClientTokens: []*relayv1.ClientToken{
			tokenEntry(devA, 0), tokenEntry(devB, 0),
			tokenEntry(pending, 3600),
			tokenEntry(expired, 1),
		},
	})
	waitOnline(t, srv, "h-1")

	if err := srv.authorizeClient("h-1", devA); err != nil {
		t.Fatalf("device a: %v", err)
	}
	if err := srv.authorizeClient("h-1", pending); err != nil {
		t.Fatalf("open pairing: %v", err)
	}
	for name, tok := range map[string]string{
		"host-wide token": ClientToken(secret, "h-1"),
		"other host's":    DeviceToken(secret, "h-2", "dev-a"),
		"empty":           "",
	} {
		if err := srv.authorizeClient("h-1", tok); !errors.Is(err, errBadClientToken) {
			t.Errorf("%s: got %v, want a refusal", name, err)
		}
	}

	// A pairing's lifetime runs on the relay's clock from when the relay got it.
	srv.mu.Lock()
	sess := srv.hosts["h-1"]
	srv.mu.Unlock()
	if !sess.acceptsToken(expired, time.Now()) || sess.acceptsToken(expired, time.Now().Add(2*time.Second)) {
		t.Fatal("a 1 s pairing token: want accepted now and refused 2 s later")
	}
	if !sess.acceptsToken(pending, time.Now().Add(59*time.Minute)) || !sess.acceptsToken(devA, time.Now().Add(1000*time.Hour)) {
		t.Fatal("an hour's pairing or a device's token expired early")
	}

	// Revoking b: the daemon sends the new set, and b is refused at the preamble with no dial-back asked for.
	fc.send(t, ctx, tokenEntry(devA, 0))
	deadline := time.Now().Add(5 * time.Second)
	for srv.authorizeClient("h-1", devB) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the new set never applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := DialRelayTCP(ctx, tcpAddr, "h-1", devB, noPin, "h2"); !errors.Is(err, ErrAuth) {
		t.Fatalf("revoked device over TCP: got %v, want ErrAuth", err)
	}
	if _, err := DialRelayWS(ctx, http.DefaultClient, hs.URL, "h-1", devB, noPin, "http/1.1"); err == nil {
		t.Fatal("revoked device over the WebSocket endpoint: connected")
	}
	time.Sleep(100 * time.Millisecond)
	if n := fc.dialBacks.Load(); n != 0 {
		t.Fatalf("the relay asked for %d dial-backs for refused clients", n)
	}

	// An offline host: any token hears offline.
	if err := srv.authorizeClient("h-gone", devA); !errors.Is(err, errDaemonOffline) {
		t.Fatalf("offline host: got %v", err)
	}
}

func TestRelayOldDaemonKeepsTheHostToken(t *testing.T) {
	secret := NewSecret()
	srv, hs, _ := startTestRelay(t, secret)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fc := dialFakeControl(t, ctx, hs.URL, secret, &relayv1.Register{HostId: "h-old"})
	waitOnline(t, srv, "h-old")
	if err := srv.authorizeClient("h-old", ClientToken(secret, "h-old")); err != nil {
		t.Fatalf("host-wide token from an old daemon: %v", err)
	}
	// A ClientTokens frame from a daemon that didn't register with device tokens changes nothing.
	fc.send(t, ctx, tokenEntry(DeviceToken(secret, "h-old", "d"), 0))
	time.Sleep(50 * time.Millisecond)
	if err := srv.authorizeClient("h-old", ClientToken(secret, "h-old")); err != nil {
		t.Fatalf("after a stray ClientTokens: %v", err)
	}
	if err := srv.authorizeClient("h-old", DeviceToken(secret, "h-old", "d")); err == nil {
		t.Fatal("a device token was accepted for a host-token daemon")
	}
}

// The connector sends its set at registration and again after TokensChanged; a revoked device then can't reach
// the daemon, and a kept one still does, over pinned TLS.
func TestConnectorPushesClientTokens(t *testing.T) {
	secret := NewSecret()
	srv, hs, tcpAddr := startTestRelay(t, secret)
	devA := DeviceToken(secret, "h-1", "dev-a")
	devB := DeviceToken(secret, "h-1", "dev-b")
	var mu sync.Mutex
	current := []string{devA, devB}
	failFirst := true // the first read fails: the connector must not register with an empty set
	dir := t.TempDir()
	tlsCfg, fp, err := tlsutil.LoadOrCreate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "h-1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewConnector(ConnectorConfig{
		RelayURL: hs.URL, Secret: secret, HostID: "h-1",
		HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: time.Second,
		ClientTokens: func() ([]*relayv1.ClientToken, error) {
			mu.Lock()
			defer mu.Unlock()
			if failFirst {
				failFirst = false
				return nil, errors.New("database is locked")
			}
			var out []*relayv1.ClientToken
			for _, tok := range current {
				out = append(out, tokenEntry(tok, 0))
			}
			return out, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = conn.Run(ctx) }()
	hsrv := &http.Server{TLSConfig: tlsCfg, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = hsrv.ServeTLS(conn.Listener(), "", "") }()
	t.Cleanup(func() { _ = hsrv.Close() })
	waitOnline(t, srv, "h-1")

	get := func(tok string) error {
		hc, err := NewTCPHTTPClient(tcpAddr, "h-1", tok, fp, false)
		if err != nil {
			return err
		}
		defer hc.CloseIdleConnections()
		res, err := hc.Get("https://daemon.invalid/")
		if err != nil {
			return err
		}
		res.Body.Close()
		return nil
	}
	if err := get(devB); err != nil {
		t.Fatalf("device b before revoke: %v", err)
	}
	if err := get(ClientToken(secret, "h-1")); err == nil {
		t.Fatal("the host-wide token reached a daemon that gives device tokens")
	}

	mu.Lock()
	current = []string{devA}
	mu.Unlock()
	conn.TokensChanged()
	deadline := time.Now().Add(5 * time.Second)
	for srv.authorizeClient("h-1", devB) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the revoke never reached the relay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := get(devB); !errors.Is(err, ErrAuth) {
		t.Fatalf("device b after revoke: got %v, want ErrAuth", err)
	}
	if err := get(devA); err != nil {
		t.Fatalf("device a after b's revoke: %v", err)
	}
}
