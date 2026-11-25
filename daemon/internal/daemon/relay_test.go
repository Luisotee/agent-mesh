package daemon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/pairing"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
	"github.com/Luisotee/agent-mesh/daemon/internal/tlsutil"
)

func startWithConfig(t *testing.T, cfg daemon.Config) *harness {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(t.TempDir(), "state")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, d: d, state: cfg.StateDir, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- d.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := daemon.ReadInfo(cfg.StateDir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never wrote meshd.json")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tok, err := client.Token(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	h.device, _ = client.Dial(cfg.StateDir, tok, client.Options{})
	t.Cleanup(h.stop)
	return h
}

func TestDirectTLSListenerWithPinnedCert(t *testing.T) {
	h := startWithConfig(t, daemon.Config{
		TCP:    "127.0.0.1:0",
		Listen: "127.0.0.1:0",
	})
	info, err := daemon.ReadInfo(h.state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Listen == "" || info.CertFingerprint == "" || info.TCP == "" {
		t.Fatalf("unexpected info: %+v", info)
	}

	// Loopback --tcp remains plain HTTP.
	plainMesh := agentmeshv1connect.NewMeshServiceClient(http.DefaultClient, "http://"+info.TCP,
		connect.WithInterceptors(auth.ClientToken(h.device.Token)))
	if _, err := plainMesh.Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Fatalf("plain loopback --tcp failed: %v", err)
	}

	// --listen serves TLS with the daemon's pinned cert.
	tlsHC, err := relay.NewDirectHTTPClient(info.CertFingerprint, true)
	if err != nil {
		t.Fatal(err)
	}
	directBase := "https://" + info.Listen
	cp, err := h.device.Mesh.CreatePairing(context.Background(), connect.NewRequest(&v1.CreatePairingRequest{
		Meta: meta("direct-pair"),
		Name: "lan-phone",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cp.Msg.DirectUrl != directBase {
		t.Fatalf("DirectUrl = %q, want %q", cp.Msg.DirectUrl, directBase)
	}
	pc := agentmeshv1connect.NewPairingServiceClient(tlsHC, directBase)
	red, err := pc.RedeemPairing(context.Background(), connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatalf("direct TLS RedeemPairing: %v", err)
	}
	directMesh := agentmeshv1connect.NewMeshServiceClient(tlsHC, directBase,
		connect.WithInterceptors(auth.ClientToken(red.Msg.DeviceToken)))
	res, err := directMesh.Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{}))
	if err != nil || res.Msg.DaemonId != info.DaemonID {
		t.Fatalf("direct TLS Hello = %v, %v", res, err)
	}

	// Pin mismatch is refused.
	otherDir := t.TempDir()
	_, wrongFP, err := tlsutil.LoadOrCreate(filepath.Join(otherDir, "tls.crt"), filepath.Join(otherDir, "tls.key"), "wrong")
	if err != nil {
		t.Fatal(err)
	}
	badHC, err := relay.NewDirectHTTPClient(wrongFP, true)
	if err != nil {
		t.Fatal(err)
	}
	badMesh := agentmeshv1connect.NewMeshServiceClient(badHC, directBase,
		connect.WithInterceptors(auth.ClientToken(red.Msg.DeviceToken)))
	if _, err := badMesh.Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{})); !errors.Is(err, tlsutil.ErrPinMismatch) {
		t.Fatalf("wrong pin Hello = %v, want ErrPinMismatch", err)
	}
}

func TestDaemonThroughRelayPairingAndRevocation(t *testing.T) {
	secret := relay.NewSecret()
	rsrv, err := relay.NewServer(relay.ServerConfig{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	rhs := httptest.NewServer(rsrv.Handler())
	t.Cleanup(func() {
		rsrv.Close()
		rhs.Close()
	})
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		rcancel()
		tcpLn.Close()
	})
	go func() { _ = rsrv.ServeClientTCP(rctx, tcpLn) }()

	h := startWithConfig(t, daemon.Config{
		Relay: daemon.RelayConfig{
			URL:      rhs.URL,
			TCP:      tcpLn.Addr().String(),
			Token:    secret,
			HostName: "home-box",
		},
	})
	info, _ := daemon.ReadInfo(h.state)

	deadline := time.Now().Add(5 * time.Second)
	for !h.d.RelayOnline() || !rsrv.HostOnline(info.DaemonID) {
		if time.Now().After(deadline) {
			t.Fatal("daemon never registered with relay")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx := context.Background()
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta: meta("relay-pair"),
		Name: "remote-phone",
	}))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pairing.ParseURI(cp.Msg.Uri)
	if err != nil {
		t.Fatal(err)
	}
	if p.RelayURL != rhs.URL || p.RelayTCP != tcpLn.Addr().String() || p.RelayClientToken == "" {
		t.Fatalf("unexpected pairing URI fields: %+v", p)
	}

	relayHC, err := relay.NewTCPHTTPClient(p.RelayTCP, p.HostID, p.RelayClientToken, p.CertFingerprint, true)
	if err != nil {
		t.Fatal(err)
	}
	defer relayHC.CloseIdleConnections()

	// Pair over the relay with pinned end-to-end TLS.
	pc := agentmeshv1connect.NewPairingServiceClient(relayHC, relay.BaseURL)
	red, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: p.Token}))
	if err != nil {
		t.Fatalf("RedeemPairing over relay: %v", err)
	}

	// Call ConnectRPC and SubscribeEvents over the relay.
	rm := agentmeshv1connect.NewMeshServiceClient(relayHC, relay.BaseURL,
		connect.WithInterceptors(auth.ClientToken(red.Msg.DeviceToken)))
	hello, err := rm.Hello(ctx, connect.NewRequest(&v1.HelloRequest{}))
	if err != nil || hello.Msg.DaemonId != info.DaemonID {
		t.Fatalf("relay Hello = %v, %v", hello, err)
	}
	stream, err := rm.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if snap := recv(t, stream).GetSnapshot(); snap == nil {
		t.Fatal("expected Snapshot over relay")
	}

	// Start a run locally and see the live event arrive through the relay.
	run := startRun(t, h.device.Mesh, "r-relay")
	if ev := recv(t, stream).GetEvent(); ev.GetRunStarted().GetRun().GetId() != run.Id {
		t.Fatalf("expected RunStarted event over relay, got %v", ev)
	}

	// Revoking the device immediately drops its relayed event stream and future relayed calls fail Unauthenticated.
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{
		Meta:     meta("rev-relay"),
		DeviceId: red.Msg.DeviceId,
	})); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		closed <- stream.Err()
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("relayed stream closed without error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relayed stream was not closed on revocation")
	}
	if _, err := rm.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked device over relay = %v, want Unauthenticated", err)
	}
}
