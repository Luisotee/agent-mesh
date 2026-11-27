package daemon_test

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/pairing"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
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

	// The CreatePairing mutation record is kept as a tombstone with secrets cleared so retrying the same
	// mutation_id replays the scrubbed response instead of minting a fresh pairing token.
	_, rawMut, err := h.d.Store().Read(ctx).Mutation("device-local", "pair-1")
	if err != nil {
		t.Fatalf("CreatePairing mutation tombstone missing after redemption: %v", err)
	}
	if strings.Contains(string(rawMut), cp.Msg.Token) {
		t.Fatal("CreatePairing mutation record still contains pairing token after redemption")
	}
	var scrubbedMut v1.CreatePairingResponse
	if err := proto.Unmarshal(rawMut, &scrubbedMut); err != nil {
		t.Fatal(err)
	}
	if scrubbedMut.PairingId != cp.Msg.PairingId || scrubbedMut.Token != "" || scrubbedMut.Uri != "" {
		t.Fatalf("scrubbed mutation = %+v", &scrubbedMut)
	}
	// A retry after redemption has no token or URI left to return: it is refused, not answered with an empty link.
	_, err = h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:       meta("pair-1"),
		Name:       "my-phone",
		TtlSeconds: 600,
	}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "pairing already redeemed") {
		t.Fatalf("retried CreatePairing after redemption = %v, want FailedPrecondition pairing already redeemed", err)
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

// A paired device, full-access or read-only, unpairs itself with RevokeSelf; the host's own device can't.
func TestRevokeSelf(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	pc := pairingClient(h.state, "")
	pair := func(name string, readOnly bool) *v1.RedeemPairingResponse {
		t.Helper()
		cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("pair-" + name), Name: name, ReadOnly: readOnly, TtlSeconds: 600}))
		if err != nil {
			t.Fatal(err)
		}
		red, err := pc.RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token, DeviceName: name}))
		if err != nil {
			t.Fatal(err)
		}
		return red.Msg
	}
	phone, viewer, other := pair("phone", false), pair("viewer", true), pair("other", false)

	// The host's own device can't revoke itself.
	if _, err := h.device.Mesh.RevokeSelf(ctx, connect.NewRequest(&v1.RevokeSelfRequest{Meta: meta("self-local")})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("device-local RevokeSelf = %v, want InvalidArgument", err)
	}

	phoneClient := h.as(phone.DeviceToken)
	stream, err := phoneClient.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if recv(t, stream).GetSnapshot() == nil {
		t.Fatal("expected initial snapshot")
	}
	res, err := phoneClient.RevokeSelf(ctx, connect.NewRequest(&v1.RevokeSelfRequest{Meta: meta("self-1")}))
	if err != nil || res.Msg.Device.GetId() != phone.DeviceId || res.Msg.Device.RevokedAt == nil {
		t.Fatalf("RevokeSelf = %+v, %v", res, err)
	}
	// Its own stream closes and its token is dead.
	closed := make(chan error, 1)
	go func() {
		for stream.Receive() {
		}
		closed <- stream.Err()
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("the device's stream closed without error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the device's own stream was not closed")
	}
	if _, err := phoneClient.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("Hello after RevokeSelf = %v, want Unauthenticated", err)
	}

	// A read-only device may unpair itself too.
	if _, err := h.as(viewer.DeviceToken).RevokeSelf(ctx, connect.NewRequest(&v1.RevokeSelfRequest{Meta: meta("self-2")})); err != nil {
		t.Fatalf("read-only RevokeSelf = %v", err)
	}
	// It reached no other device: the third still works, and the host lists exactly it as active.
	if _, err := h.as(other.DeviceToken).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Fatalf("the other device after two RevokeSelf = %v", err)
	}
	active, err := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
	if err != nil || len(active.Msg.Devices) != 1 || active.Msg.Devices[0].Id != other.DeviceId {
		t.Fatalf("active devices = %+v, %v", active, err)
	}
}

func TestReadOnlyAndExpiredPairing(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	state := shortdir.State(t)
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

func TestCreatePairingRelayOverrideAndTTLOverflow(t *testing.T) {
	secret := relay.NewSecret()
	h := startWithConfig(t, daemon.Config{
		Relay: daemon.RelayConfig{
			URL:   "https://relay-a.example",
			TCP:   "relay-a.example:8443",
			Token: secret,
		},
	})
	ctx := context.Background()

	// Huge ttl_seconds must be rejected instead of wrapping negative via time.Duration multiplication.
	for _, badTTL := range []int64{-1, 86401, 9223372037, math.MaxInt64} {
		if _, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
			Meta:       meta("bad-ttl"),
			TtlSeconds: badTTL,
		})); code(err) != connect.CodeInvalidArgument {
			t.Fatalf("TtlSeconds=%d: got %v, want InvalidArgument", badTTL, err)
		}
	}

	// Default uses configured relay URL, TCP, and derived client token.
	defRes, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta: meta("def-relay"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if defRes.Msg.RelayUrl != "https://relay-a.example" || defRes.Msg.RelayTcp != "relay-a.example:8443" || defRes.Msg.RelayClientToken == "" {
		t.Fatalf("default relay pairing = %+v", defRes.Msg)
	}

	// A differently written URL for the configured relay keeps its TCP address and token.
	sameRes, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:     meta("same-relay"),
		RelayUrl: "https://Relay-A.example:443/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if sameRes.Msg.RelayTcp != "relay-a.example:8443" || sameRes.Msg.RelayClientToken == "" {
		t.Fatalf("equivalent relay override dropped the relay's TCP/token: %+v", sameRes.Msg)
	}

	// Overriding relay_url to a different relay must not attach the configured relay's TCP address or token.
	ovrRes, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta:     meta("ovr-relay"),
		RelayUrl: "https://relay-b.example",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if ovrRes.Msg.RelayUrl != "https://relay-b.example" || ovrRes.Msg.RelayTcp != "" || ovrRes.Msg.RelayClientToken != "" {
		t.Fatalf("overridden relay pairing mixed relays: %+v", ovrRes.Msg)
	}
	parsed, err := pairing.ParseURI(ovrRes.Msg.Uri)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RelayURL != "https://relay-b.example" || parsed.RelayTCP != "" || parsed.RelayClientToken != "" {
		t.Fatalf("overridden pairing URI mixed relays: %+v", parsed)
	}
}

func TestRevokeDeviceInvalidatesPendingTerminalTicketAndSubscribeRace(t *testing.T) {
	home := t.TempDir()
	state := shortdir.State(t)
	h := startWithAgents(t, state, home)
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()

	repo := gitRepo(t)
	run := startRun(t, h.device.Mesh, "term-revoke")
	ag := h.spawn(run.Id, repo, "w1", "hi")

	// Pair a remote device and obtain a terminal ticket BEFORE revocation.
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta: meta("pair-term"), Name: "stolen-phone",
	}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{
		Token: cp.Msg.Token,
	}))
	if err != nil {
		t.Fatal(err)
	}
	devClient := h.as(red.Msg.DeviceToken)
	tkt, err := devClient.OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{
		Agent: ag.Id, Input: true,
	}))
	if err != nil {
		t.Fatalf("OpenTerminal before revoke: %v", err)
	}

	// Revoke the device before it redeems the terminal ticket.
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{
		Meta: meta("rev-term"), DeviceId: red.Msg.DeviceId,
	})); err != nil {
		t.Fatal(err)
	}

	// Attempting to open the terminal with the pre-revocation ticket must fail with 401.
	info, err := daemon.ReadInfo(h.state)
	if err != nil {
		t.Fatal(err)
	}
	unixHC := client.UnixHTTP(info.Socket, 3*time.Second)
	res, err := unixHC.Get(client.BaseURL + tkt.Msg.Path)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v1/term with revoked device's ticket: status %d, want 401", res.StatusCode)
	}

	// Re-check after trackStream also rejects a SubscribeEvents call whose actor was revoked after auth.
	revActor := &v1.Actor{Id: red.Msg.DeviceId, Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Name: "stolen-phone"}
	if err := h.d.SubscribeEvents(auth.WithActor(ctx, revActor), connect.NewRequest(&v1.SubscribeEventsRequest{}), nil); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("SubscribeEvents for revoked actor after auth = %v, want Unauthenticated", err)
	}
}

func TestDeviceEventsNotSentToRunAgents(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "r-scope")

	workerTok := auth.NewToken()
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutToken(auth.Hash(workerTok), store.TokenRecord{
			Class:     v1.ActorClass_ACTOR_CLASS_WORKER,
			SubjectID: "a-worker-1",
			Name:      "worker-1",
			RunID:     run.Id,
		})
	}); err != nil {
		t.Fatal(err)
	}
	workerClient := h.as(workerTok)

	devStream, err := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer devStream.Close()
	if recv(t, devStream).GetSnapshot() == nil {
		t.Fatal("device expected snapshot")
	}

	workerStream, err := workerClient.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer workerStream.Close()
	workerSnap := recv(t, workerStream).GetSnapshot()
	if workerSnap == nil {
		t.Fatal("worker expected snapshot")
	}

	// Pair and revoke a device, then close the worker's run.
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
		Meta: meta("pair-ev"), Name: "secret-phone",
	}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{
		Token: cp.Msg.Token,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{
		Meta: meta("rev-ev"), DeviceId: red.Msg.DeviceId,
	})); err != nil {
		t.Fatal(err)
	}
	// Emit a run-scoped event after the device events so we can assert the worker skips straight to it.
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		_, err := tx.AppendEvent(&v1.Event{
			RunId:   run.Id,
			Payload: &v1.Event_MessageDelivered{MessageDelivered: &v1.MessageDelivered{MessageId: "m-marker"}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Host device receives DevicePaired, DeviceRevoked, and MessageDelivered.
	ev := recv(t, devStream).GetEvent()
	if ev.GetDevicePaired().GetDevice().GetId() != red.Msg.DeviceId {
		t.Fatalf("device stream 1 = %v, want DevicePaired", ev)
	}
	// The event says which link the device came through, so a client can match them exactly.
	if got := ev.GetDevicePaired().GetPairingId(); got == "" || got != cp.Msg.PairingId {
		t.Fatalf("DevicePaired.pairing_id = %q, want the id CreatePairing returned (%q)", got, cp.Msg.PairingId)
	}
	if ev := recv(t, devStream).GetEvent(); ev.GetDeviceRevoked().GetDeviceId() != red.Msg.DeviceId {
		t.Fatalf("device stream 2 = %v, want DeviceRevoked", ev)
	}
	if ev := recv(t, devStream).GetEvent(); ev.GetMessageDelivered().GetMessageId() != "m-marker" {
		t.Fatalf("device stream 3 = %v, want MessageDelivered", ev)
	}

	// Worker's live stream skips DevicePaired/DeviceRevoked and gets MessageDelivered directly.
	if ev := recv(t, workerStream).GetEvent(); ev.GetMessageDelivered().GetMessageId() != "m-marker" {
		t.Fatalf("worker live stream = %v, want MessageDelivered (no device events)", ev)
	}

	// Worker resuming from the pre-pairing cursor also skips DevicePaired/DeviceRevoked.
	resumed, err := workerClient.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{
		Cursor: workerSnap.Cursor,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if recv(t, resumed).GetResumed() == nil {
		t.Fatal("worker expected Resumed frame")
	}
	if ev := recv(t, resumed).GetEvent(); ev.GetMessageDelivered().GetMessageId() != "m-marker" {
		t.Fatalf("worker resumed stream = %v, want MessageDelivered (no device events)", ev)
	}
}

func TestDeviceEventsOnlyToHostDevice(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "r-dev-scope")

	pair := func(id string, readOnly bool) *v1.RedeemPairingResponse {
		cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{
			Meta: meta("pair-" + id), Name: id, ReadOnly: readOnly,
		}))
		if err != nil {
			t.Fatal(err)
		}
		red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
		if err != nil {
			t.Fatal(err)
		}
		return red.Msg
	}
	full, ro := pair("full", false), pair("ro", true)

	subscribe := func(tok string) *connect.ServerStreamForClient[v1.SubscribeEventsResponse] {
		st, err := h.as(tok).SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if recv(t, st).GetSnapshot() == nil {
			t.Fatal("expected snapshot")
		}
		return st
	}
	fullStream, roStream := subscribe(full.DeviceToken), subscribe(ro.DeviceToken)

	// Pairing a third device and revoking it, then a run event as a marker.
	third := pair("third", false)
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{
		Meta: meta("rev-third"), DeviceId: third.DeviceId,
	})); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		_, err := tx.AppendEvent(&v1.Event{
			RunId:   run.Id,
			Payload: &v1.Event_MessageDelivered{MessageDelivered: &v1.MessageDelivered{MessageId: "m-marker"}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]*connect.ServerStreamForClient[v1.SubscribeEventsResponse]{"device": fullStream, "read-only device": roStream} {
		if ev := recv(t, st).GetEvent(); ev.GetMessageDelivered().GetMessageId() != "m-marker" {
			t.Fatalf("%s stream got %v first, want MessageDelivered (no device events)", name, ev)
		}
	}
}

func TestSameRelayURL(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://relay.example.com", "https://relay.example.com", true},
		{"https://relay.example.com/", "https://relay.example.com", true},
		{"https://Relay.Example.com", "https://relay.example.com", true},
		{"HTTPS://relay.example.com:443", "https://relay.example.com", true},
		{"http://relay.example.com:80", "http://relay.example.com", true},
		{"https://relay.example.com:8443", "https://relay.example.com", false},
		{"http://relay.example.com", "https://relay.example.com", false},
		{"https://relay-b.example.com", "https://relay.example.com", false},
		{"https://relay.example.com/a", "https://relay.example.com/b", false},
		{"not a url", "not a url", false},
	} {
		if got := daemon.SameRelayURL(c.a, c.b); got != c.want {
			t.Errorf("SameRelayURL(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTerminalTicketStoreErrorIs503(t *testing.T) {
	home := t.TempDir()
	state := shortdir.State(t)
	h := startWithAgents(t, state, home)
	t.Cleanup(func() { stopTermHost(t, state) })
	ctx := context.Background()

	repo := gitRepo(t)
	run := startRun(t, h.device.Mesh, "term-503")
	ag := h.spawn(run.Id, repo, "w1", "hi")
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("pair-503"), Name: "phone"}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	tkt, err := h.as(red.Msg.DeviceToken).OpenTerminal(ctx, connect.NewRequest(&v1.OpenTerminalRequest{Agent: ag.Id, Input: true}))
	if err != nil {
		t.Fatal(err)
	}
	info, err := daemon.ReadInfo(h.state)
	if err != nil {
		t.Fatal(err)
	}
	// A failing store is "cannot verify", not "revoked": the client must not drop its credentials.
	if err := h.d.Store().Close(); err != nil {
		t.Fatal(err)
	}
	res, err := client.UnixHTTP(info.Socket, 3*time.Second).Get(client.BaseURL + tkt.Msg.Path)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/term with the store closed: status %d, want 503", res.StatusCode)
	}
}
