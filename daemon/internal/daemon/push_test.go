package daemon_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/webpush"
)

// fakePush is a push service (what ntfy is to UnifiedPush) on loopback: it answers with the statuses it is given,
// in turn (then 201), and keeps what it was sent.
type fakePush struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	got      [][]byte
	headers  []http.Header
	arrived  chan struct{}
}

func newFakePush(t *testing.T, statuses ...int) *fakePush {
	f := &fakePush{statuses: statuses, arrived: make(chan struct{}, 64)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		code := http.StatusCreated
		if len(f.statuses) > 0 {
			code, f.statuses = f.statuses[0], f.statuses[1:]
		}
		if code < 300 {
			f.got = append(f.got, body)
			f.headers = append(f.headers, r.Header.Clone())
		}
		f.mu.Unlock()
		w.WriteHeader(code)
		f.arrived <- struct{}{}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakePush) delivered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// phone is a paired device with its WebPush keys.
type phone struct {
	id   string
	c    agentmeshv1connect.MeshServiceClient
	key  *ecdh.PrivateKey
	auth []byte
}

func pairPhone(t *testing.T, h *harness, name string, readOnly bool) *phone {
	t.Helper()
	ctx := context.Background()
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta(""), Name: name, ReadOnly: readOnly}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &phone{id: red.Msg.DeviceId, c: h.as(red.Msg.DeviceToken), key: k, auth: auth}
}

func (p *phone) register(t *testing.T, endpoint string) *v1.PushRegistration {
	t.Helper()
	res, err := p.c.RegisterPush(context.Background(), connect.NewRequest(&v1.RegisterPushRequest{Meta: meta(""), Endpoint: endpoint,
		P256Dh: base64.RawURLEncoding.EncodeToString(p.key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(p.auth)}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Registration
}

type payload struct {
	V      int    `json:"v"`
	Daemon string `json:"daemon"`
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Agent  string `json:"agent"`
	Tab    string `json:"tab"`
	At     int64  `json:"at"`
}

func (p *phone) open(t *testing.T, body []byte) payload {
	t.Helper()
	pt, err := webpush.Decrypt(body, p.key, p.auth)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var out payload
	if err := json.Unmarshal(pt, &out); err != nil {
		t.Fatalf("payload %q: %v", pt, err)
	}
	return out
}

func fastPush(cfg *daemon.Config) {
	cfg.Push.Retry = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
}

func TestPushCarriesTheNoticeEncryptedToEachPhone(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t)
	info, _ := daemon.ReadInfo(h.state)
	ph := pairPhone(t, h, "pixel", false)
	reg := ph.register(t, srv.URL+"/upAbc?up=1")
	if reg.DeviceId != ph.id || reg.EndpointHost == "" || reg.LastOkAt != nil {
		t.Fatalf("registration %+v", reg)
	}
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "p-root", "shop-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w := h.seedAgent(run.Id, "p-w1", "shop-fix", "p-root", v1.AgentRole_AGENT_ROLE_WORKER)
	send := func(tok string, typ v1.MessageType, body string) {
		t.Helper()
		readInbox(t, h.as(tok))
		if _, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: typ, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send(w, v1.MessageType_MESSAGE_TYPE_PLAN, "# Plan\n- **do** `x`")
	waitFor(t, "the plan's push", func() bool { return srv.delivered() == 1 })
	srv.mu.Lock()
	body, hdr := srv.got[0], srv.headers[0]
	srv.mu.Unlock()
	got := ph.open(t, body)
	if got.V != 1 || got.Daemon != info.DaemonID || got.Kind != "plan" || got.Title != "shop-fix sent a plan" || got.Body != "Plan do x" ||
		got.Agent != "p-w1" || got.Tab != "messages" || got.Seq == 0 || got.At == 0 {
		t.Fatalf("payload %+v", got)
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("Urgency") != "high" || hdr.Get("TTL") != "86400" || len(hdr.Get("Topic")) != 22 {
		t.Fatalf("headers %v", hdr)
	}

	// A message the user sent, a kind the host doesn't notify, and notifications off push nothing.
	if _, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "p-root", Type: v1.MessageType_MESSAGE_TYPE_QUESTION, Body: "from me"})); err != nil {
		t.Fatal(err)
	}
	setPushSetting(t, h, "app.notify.events", []any{"question"})
	send(w, v1.MessageType_MESSAGE_TYPE_DONE, "done")
	setPushSetting(t, h, "app.notify.enabled", false)
	send(w, v1.MessageType_MESSAGE_TYPE_QUESTION, "which db?")
	setPushSetting(t, h, "app.notify.enabled", true)
	send(w, v1.MessageType_MESSAGE_TYPE_QUESTION, "and now?")
	waitFor(t, "the question's push", func() bool { return srv.delivered() == 2 })
	srv.mu.Lock()
	body = srv.got[1]
	srv.mu.Unlock()
	if got := ph.open(t, body); got.Kind != "question" || got.Body != "and now?" {
		t.Fatalf("second push %+v (an earlier one leaked through?)", got)
	}

	// The registration shows on `mesh devices`, with the last success.
	devs, err := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(devs.Msg.Devices) != 1 || devs.Msg.Devices[0].Push.GetLastOkAt() == nil {
		t.Fatalf("devices %+v", devs.Msg.Devices)
	}
}

func setPushSetting(t *testing.T, h *harness, key string, v any) {
	t.Helper()
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: key, Value: val(t, v)})); err != nil {
		t.Fatal(err)
	}
}

func TestPushRetriesThenForgetsAGoneEndpoint(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t, 503, 429, http.StatusCreated, http.StatusGone)
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, srv.URL+"/up1")

	// 503 then 429 are retried; the third try lands.
	if _, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("")})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("test push against a 503: %v", err)
	}
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "g-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w := h.seedAgent(run.Id, "g-w1", "fix", "g-root", v1.AgentRole_AGENT_ROLE_WORKER)
	readInbox(t, h.as(w))
	if _, err := h.as(w).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_FAILED, Body: "broke"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the retried push", func() bool { return srv.delivered() == 1 })
	srv.mu.Lock()
	if u := srv.headers[0].Get("Urgency"); u != "normal" {
		t.Errorf("a failure's urgency is %q", u)
	}
	srv.mu.Unlock()

	// 410: the endpoint is gone, and the registration with it.
	if _, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("test push against a 410: %v", err)
	}
	if _, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("after the 410 the registration should be gone: %v", err)
	}
}

func TestPushRegistrationsBelongToTheirDevice(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t)
	a := pairPhone(t, h, "phone-a", false)
	ro := pairPhone(t, h, "watch-only", true)

	// A read-only device registers its own endpoint and gets its test push.
	ro.register(t, srv.URL+"/ro")
	if _, err := ro.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("")})); err != nil {
		t.Fatalf("read-only test push: %v", err)
	}
	srv.mu.Lock()
	if got := ro.open(t, srv.got[0]); got.Kind != "test" {
		t.Fatalf("test payload %+v", got)
	}
	srv.mu.Unlock()
	// No device touches another's; the host's own device may.
	for _, c := range []agentmeshv1connect.MeshServiceClient{a.c, ro.c} {
		if _, err := c.UnregisterPush(ctx, connect.NewRequest(&v1.UnregisterPushRequest{Meta: meta(""), DeviceId: ro.id})); c == a.c && connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("another device unregistered it: %v", err)
		}
	}
	ro.register(t, srv.URL+"/ro")
	if _, err := a.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta(""), DeviceId: ro.id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("another device pushed to it: %v", err)
	}
	if res, err := h.device.Mesh.UnregisterPush(ctx, connect.NewRequest(&v1.UnregisterPushRequest{Meta: meta(""), DeviceId: ro.id})); err != nil || !res.Msg.Removed {
		t.Fatalf("host device unregister: %v %v", res, err)
	}

	// The same endpoint registered by a new pairing moves to it (no double pushes); revoking deletes it.
	a.register(t, srv.URL+"/shared")
	b := pairPhone(t, h, "phone-a-again", false)
	b.register(t, srv.URL+"/shared")
	if _, err := a.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("")})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("the old device kept the moved endpoint: %v", err)
	}
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta(""), DeviceId: b.id})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.device.Mesh.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta(""), DeviceId: b.id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("a revoked device's endpoint survived: %v", err)
	}

	// Bad endpoints and keys are refused.
	for _, req := range []*v1.RegisterPushRequest{
		{Endpoint: "http://ntfy.example.com/up", P256Dh: base64.RawURLEncoding.EncodeToString(a.key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(a.auth)},
		{Endpoint: srv.URL + "/x", P256Dh: "BAAA", Auth: base64.RawURLEncoding.EncodeToString(a.auth)},
		{Endpoint: srv.URL + "/x", P256Dh: base64.RawURLEncoding.EncodeToString(a.key.PublicKey().Bytes()), Auth: "AAAA"},
	} {
		req.Meta = meta("")
		if _, err := a.c.RegisterPush(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", req.Endpoint, err)
		}
	}
}

func TestPushReportsALaunchThatFailedAndDeletedItsAgent(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t)
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, srv.URL+"/up")
	run := startRun(t, h.device.Mesh, "")
	a := &v1.Agent{Id: "lf-w1", RunId: run.Id, Name: "shop-fix"}
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: run.Id, AgentId: a.Id, Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: a}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// As failLaunch does: the exit and the agent's deletion in one transaction.
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		if _, err := tx.AppendEvent(&v1.Event{RunId: run.Id, AgentId: a.Id, Payload: &v1.Event_AgentExited{AgentExited: &v1.AgentExited{ExitCode: -1, Reason: "launch failed: no CLI"}}}); err != nil {
			return err
		}
		return tx.DeleteAgent(a.Id)
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed launch's push", func() bool { return srv.delivered() == 1 })
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := ph.open(t, srv.got[0]); got.Kind != "failed" || got.Title != "shop-fix exited with code -1" {
		t.Fatalf("payload %+v", got)
	}
}

func TestTestPushIsRecordedOnlyOnceTaken(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t, http.StatusBadRequest)
	srv.Config.Handler = withBody(srv.Config.Handler, "internal secret reply")
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, srv.URL+"/up")
	test := func() error {
		_, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("test-1")}))
		return err
	}
	// A 400: the device sees the status, never the service's reply; the endpoint isn't counted as failing.
	err := test()
	if connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), "secret") {
		t.Fatalf("first: %v", err)
	}
	devs, _ := h.device.Mesh.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
	if p := devs.Msg.Devices[0].Push; p.GetLastError() == "" || p.GetFailingSince() != nil {
		t.Fatalf("after a rejection: %+v", p)
	}
	// The retry with the same mutation id really sends; a retry of that success doesn't send again.
	if err := test(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := test(); err != nil || srv.delivered() != 1 {
		t.Fatalf("replay: %v, %d delivered", err, srv.delivered())
	}
}

// withBody makes a handler write a body after its status.
func withBody(h http.Handler, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(bodyWriter{w, body}, r)
	})
}

type bodyWriter struct {
	http.ResponseWriter
	body string
}

func (b bodyWriter) WriteHeader(code int) {
	b.ResponseWriter.WriteHeader(code)
	_, _ = io.WriteString(b.ResponseWriter, b.body)
}

func TestNoPushGoesToARevokedDevice(t *testing.T) {
	h := startWith(t, "", func(cfg *daemon.Config) { cfg.Push.Retry = []time.Duration{300 * time.Millisecond} })
	ctx := context.Background()
	srv := newFakePush(t, http.StatusServiceUnavailable)
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, srv.URL+"/up")
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "rv-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w := h.seedAgent(run.Id, "rv-w1", "fix", "rv-root", v1.AgentRole_AGENT_ROLE_WORKER)
	readInbox(t, h.as(w))
	if _, err := h.as(w).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "p"})); err != nil {
		t.Fatal(err)
	}
	<-srv.arrived // the 503; the retry waits 300 ms
	if _, err := h.device.Mesh.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta(""), DeviceId: ph.id})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	select {
	case <-srv.arrived:
		t.Fatal("the retry went to a revoked device")
	default:
	}
}

func TestPushToANewEndpointDoesNotWaitForTheOldOnesRetries(t *testing.T) {
	h := startWith(t, "", func(cfg *daemon.Config) { cfg.Push.Retry = []time.Duration{time.Hour} })
	ctx := context.Background()
	old := newFakePush(t, http.StatusServiceUnavailable)
	moved := newFakePush(t)
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, old.URL+"/up")
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "mv-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w := h.seedAgent(run.Id, "mv-w1", "fix", "mv-root", v1.AgentRole_AGENT_ROLE_WORKER)
	readInbox(t, h.as(w))
	send := func(body string) {
		t.Helper()
		if _, err := h.as(w).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send("first")
	<-old.arrived // the 503; its retry waits an hour
	ph.register(t, moved.URL+"/up")
	send("second")
	waitFor(t, "the push to the new endpoint", func() bool { return moved.delivered() == 1 })
	moved.mu.Lock()
	got := ph.open(t, moved.got[0])
	moved.mu.Unlock()
	if !strings.Contains(got.Body, "second") {
		t.Fatalf("the new endpoint got %+v", got)
	}
}

func TestTestPushSendsNothingForAnExpiredOrReusedRequest(t *testing.T) {
	h := startWith(t, "", fastPush)
	ctx := context.Background()
	srv := newFakePush(t)
	ph := pairPhone(t, h, "pixel", false)
	ph.register(t, srv.URL+"/up")
	past := &v1.RequestMeta{MutationId: "late", ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))}
	if _, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: past})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expired: %v", err)
	}
	if _, err := ph.c.UnregisterPush(ctx, connect.NewRequest(&v1.UnregisterPushRequest{Meta: meta("used")})); err != nil {
		t.Fatal(err)
	}
	ph.register(t, srv.URL+"/up")
	if _, err := ph.c.TestPush(ctx, connect.NewRequest(&v1.TestPushRequest{Meta: meta("used")})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("reused mutation id: %v", err)
	}
	if n := srv.delivered(); n != 0 {
		t.Fatalf("%d pushes sent", n)
	}
}
