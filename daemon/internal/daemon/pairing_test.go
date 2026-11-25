package daemon_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/pairing"
)

func pairingClient(state, hostURL string) agentmeshv1connect.PairingServiceClient {
	info, _ := daemon.ReadInfo(state)
	if hostURL == "" {
		return agentmeshv1connect.NewPairingServiceClient(client.UnixHTTP(info.Socket, time.Second), client.BaseURL)
	}
	return agentmeshv1connect.NewPairingServiceClient(http.DefaultClient, hostURL)
}

func TestPairingRedeemListAndRevoke(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	info, _ := daemon.ReadInfo(h.state)

	// Browser Origin header is still refused on PairingService.
	req, _ := http.NewRequest("POST", "http://"+info.TCP+agentmeshv1connect.PairingServiceRedeemPairingProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("PairingService with Origin: status %d, want 403", res.StatusCode)
	}

	// Local device creates a full-access pairing.
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:       meta("pair-1"),
		Name:       "my-phone",
		TtlSeconds: 600,
		DirectUrl:  "https://100.64.0.10:9443",
	}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pairing.ParseURI(cp.Msg.Uri)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.HostID != info.DaemonID || parsed.Token != cp.Msg.Token || parsed.CertFingerprint != info.CertFingerprint ||
		parsed.DirectURL != "https://100.64.0.10:9443" {
		t.Fatalf("parsed URI mismatch: %+v", parsed)
	}

	// Redeem via PairingService.
	pc := pairingClient(h.state, "")
	red, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{
		Token:      cp.Msg.Token,
		DeviceName: "Pixel 9",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if red.Msg.DeviceName != "Pixel 9" || red.Msg.Class != v1.ActorClass_ACTOR_CLASS_DEVICE || !strings.HasPrefix(red.Msg.DeviceToken, auth.TokenPrefix) {
		t.Fatalf("redeem response = %+v", red.Msg)
	}

	// Single-use: reusing the pairing token is refused.
	if _, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("reused pairing token = %v, want Unauthenticated", err)
	}

	// The CreatePairing mutation record was scrubbed on redemption so the token is not left on disk.
	if _, _, err := h.d.Store().Read(ctx).Mutation("device-local", "pair-1"); err == nil {
		t.Fatal("CreatePairing mutation record survived redemption")
	}

	// Paired device can call Hello and StartRun, but cannot manage devices.
	devClient := h.as(red.Msg.DeviceToken)
	hello, err := devClient.Hello(ctx, connect.NewRequest(&v1.HelloRequest{}))
	if err != nil || hello.Msg.Actor.GetId() != red.Msg.DeviceId || hello.Msg.Actor.GetName() != "Pixel 9" {
		t.Fatalf("paired hello = %v, %v", hello, err)
	}
	if _, err := devClient.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p-dev")})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("paired device CreatePairing = %v, want PermissionDenied", err)
	}
	if _, err := devClient.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("paired device ListDevices = %v, want PermissionDenied", err)
	}
	if _, err := devClient.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta("r-dev"), DeviceId: red.Msg.DeviceId})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("paired device RevokeDevice = %v, want PermissionDenied", err)
	}

	// Paired device opens an event stream before revocation.
	evStream, err := devClient.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer evStream.Close()
	if snap := recv(t, evStream).GetSnapshot(); snap == nil {
		t.Fatal("expected initial snapshot")
	}

	// ListDevices shows only the paired device (not device-local).
	devs, err := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
	if err != nil || len(devs.Msg.Devices) != 1 || devs.Msg.Devices[0].Id != red.Msg.DeviceId {
		t.Fatalf("ListDevices = %+v, %v", devs, err)
	}

	// Cannot revoke device-local.
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta("rev-local"), DeviceId: "device-local"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("revoke device-local = %v, want InvalidArgument", err)
	}

	// RevokeDevice invalidates the token and cancels the active event stream immediately.
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta("rev-1"), DeviceId: red.Msg.DeviceId})); err != nil {
		t.Fatal(err)
	}
	streamClosed := make(chan error, 1)
	go func() {
		for evStream.Receive() {
		}
		streamClosed <- evStream.Err()
	}()
	select {
	case err := <-streamClosed:
		if err == nil {
			t.Fatal("revoked device stream closed without error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked device active event stream was not closed")
	}
	if _, err := devClient.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked device Hello = %v, want Unauthenticated", err)
	}

	active, _ := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
	if len(active.Msg.Devices) != 0 {
		t.Errorf("revoked device still in active list: %+v", active.Msg.Devices)
	}
	all, _ := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{IncludeRevoked: true}))
	if len(all.Msg.Devices) != 1 || all.Msg.Devices[0].RevokedAt == nil {
		t.Errorf("include_revoked = %+v", all.Msg.Devices)
	}
}

func TestReadOnlyAndExpiredPairing(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	state := t.TempDir() + "/state"
	d, err := daemon.New(daemon.Config{StateDir: state, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for {
		if _, err := daemon.ReadInfo(state); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tok, err := client.Token(state)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := client.Dial(state, tok, client.Options{})
	pc := pairingClient(state, "")

	// Read-only device pairing.
	roPair, err := local.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:       meta("ro-1"),
		Name:       "viewer",
		ReadOnly:   true,
		TtlSeconds: 60,
	}))
	if err != nil {
		t.Fatal(err)
	}
	roRed, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: roPair.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	if roRed.Msg.Class != v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE || roRed.Msg.DeviceName != "viewer" {
		t.Fatalf("read-only redeem = %+v", roRed.Msg)
	}
	roConn, _ := client.Dial(state, roRed.Msg.DeviceToken, client.Options{})
	if _, err := roConn.Mesh.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Fatalf("read-only Hello: %v", err)
	}
	if _, err := roConn.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: meta("ro-run")})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("read-only StartRun = %v, want PermissionDenied", err)
	}

	// Expired pairing is refused.
	expPair, err := local.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:       meta("exp-1"),
		TtlSeconds: 30,
	}))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: expPair.Msg.Token})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("expired pairing = %v, want Unauthenticated", err)
	}
}
