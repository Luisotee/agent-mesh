package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/notice"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/webpush"
)

// Push notifications (README, "Push"): every notice the apps would post (internal/notice) goes, encrypted for the
// device (RFC 8291), to each paired device's UnifiedPush endpoint, so a phone gets it with its app closed.

// PushConfig tunes the push sender; the zero value is the default. Tests shorten the retries.
type PushConfig struct {
	// Retry is the wait before each retry of a push the service couldn't take yet (429, 5xx, a network error).
	Retry []time.Duration
	// Client posts the pushes; nil is webpush.NewClient (15 s timeout, no redirects).
	Client *http.Client
}

var defaultPushRetry = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

const (
	// pushTTL is how long the push service keeps a push for a phone that is offline: a plan still waits tomorrow.
	pushTTL = 24 * time.Hour
	// pushFailingMax: a registration whose every push has failed this long is dropped at its next failure.
	pushFailingMax = 7 * 24 * time.Hour
	// pushMaxRetryAfter caps a service's Retry-After.
	pushMaxRetryAfter = 10 * time.Minute
	// pushDeviceQueue is how many pushes may wait per device; the oldest goes first when it is full.
	pushDeviceQueue = 32
	// pushInbox is how many committed batches may wait for the dispatcher; more are dropped (with a log line).
	pushInbox = 256
)

// pushPayload is the plaintext of a push, read by the app (android/.../notify/PushPayload.kt).
type pushPayload struct {
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

type pushJob struct {
	endpoint string
	msg      webpush.Message
}

// senderKey is one sender's: a device's endpoint. A device that moves to a new endpoint gets a new sender, so
// pushes for it never wait behind, or get dropped with, the old endpoint's retries.
type senderKey struct{ device, endpoint string }

type pushSender struct {
	ch     chan pushJob
	cancel context.CancelFunc
}

type pusher struct {
	d      *Daemon
	client *http.Client
	retry  []time.Duration
	in     chan []*v1.Event

	mu      sync.Mutex
	ctx     context.Context
	senders map[senderKey]*pushSender

	// spawned holds the agents spawned lately, as their AgentSpawned events said: a launch that fails deletes its
	// agent in the transaction that reports the exit, so the store no longer has it when the exit is pushed.
	// Only the dispatcher touches it.
	spawned map[string]*v1.Agent
}

// maxSpawned bounds the spawned cache; past it the cache starts over (an agent launched long ago is in the store).
const maxSpawned = 1000

func newPusher(d *Daemon, cfg PushConfig) *pusher {
	p := &pusher{d: d, client: cfg.Client, retry: cfg.Retry, in: make(chan []*v1.Event, pushInbox), senders: map[senderKey]*pushSender{}, spawned: map[string]*v1.Agent{}}
	if p.client == nil {
		p.client = webpush.NewClient()
	}
	if p.retry == nil {
		p.retry = defaultPushRetry
	}
	return p
}

// enqueue takes a committed transaction's events. It runs inside store.OnCommit, under the store's write lock,
// so it only filters and hands over, never blocks or reads.
func (p *pusher) enqueue(evs []*v1.Event) {
	var keep []*v1.Event
	for _, e := range evs {
		if _, spawn := e.Payload.(*v1.Event_AgentSpawned); spawn || notice.Relevant(e) {
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		return
	}
	select {
	case p.in <- keep:
	default:
		p.d.log.Warn("push: dispatcher behind; events not pushed", "events", len(keep))
	}
}

// run dispatches until ctx ends.
func (p *pusher) run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-p.in:
			p.dispatch(ctx, evs)
		}
	}
}

func (p *pusher) dispatch(ctx context.Context, evs []*v1.Event) {
	for _, e := range evs {
		if s, ok := e.Payload.(*v1.Event_AgentSpawned); ok && s.AgentSpawned.GetAgent() != nil {
			if len(p.spawned) >= maxSpawned {
				p.spawned = map[string]*v1.Agent{}
			}
			p.spawned[s.AgentSpawned.Agent.Id] = s.AgentSpawned.Agent
		}
	}
	r := p.d.store.Read(ctx)
	regs, err := r.ListPush()
	if err != nil || len(regs) == 0 {
		if err != nil {
			p.d.log.Warn("push: reading endpoints", "err", err)
		}
		return
	}
	kinds, ok := p.notifyKinds(r)
	if !ok {
		return
	}
	agents := map[string]*v1.Agent{}
	lookup := func(id string) *v1.Agent {
		if a, seen := agents[id]; seen {
			return a
		}
		a, err := r.GetAgent(id)
		if err != nil {
			a = p.spawned[id]
		}
		agents[id] = a
		return a
	}
	for _, e := range evs {
		n := notice.For(e, lookup)
		if _, exit := e.Payload.(*v1.Event_AgentExited); exit {
			delete(p.spawned, e.AgentId)
		}
		if n == nil || !kinds[n.Kind] {
			continue
		}
		at := p.d.now()
		if e.Time != nil {
			at = e.Time.AsTime()
		}
		p.send(regs, pushPayload{V: 1, Daemon: p.d.daemonID, Seq: e.Seq, Kind: n.Kind, Title: n.Title, Body: n.Body,
			Agent: n.AgentID, Tab: n.Tab, At: at.UnixMilli()}, n.Waiting())
	}
}

// notifyKinds reads the host's app.notify.enabled and app.notify.events: what every client of this host notifies.
func (p *pusher) notifyKinds(r store.Reader) (map[string]bool, bool) {
	on, err := p.d.Setting(r, "app.notify.enabled")
	if err != nil {
		p.d.log.Warn("push: reading app.notify.enabled", "err", err)
		return nil, false
	}
	if b, _ := on.(bool); !b {
		return nil, false
	}
	evs, err := p.d.Setting(r, "app.notify.events")
	if err != nil {
		p.d.log.Warn("push: reading app.notify.events", "err", err)
		return nil, false
	}
	kinds := map[string]bool{}
	list, _ := evs.([]any)
	for _, k := range list {
		if s, ok := k.(string); ok {
			kinds[s] = true
		}
	}
	return kinds, true
}

// pushMessage encrypts a payload for one registration.
func (p *pusher) pushMessage(reg store.PushRecord, pl pushPayload, urgent bool) (webpush.Message, error) {
	plain, err := json.Marshal(pl)
	if err != nil {
		return webpush.Message{}, err
	}
	body, err := webpush.Encrypt(plain, reg.P256dh, reg.Auth)
	if err != nil {
		return webpush.Message{}, err
	}
	m := webpush.Message{Body: body, TTL: pushTTL, Urgency: "normal"}
	if urgent {
		m.Urgency = "high"
	}
	if pl.Agent != "" {
		// One waiting push per agent at the push service, as the phone keeps one notification per agent.
		sum := sha256.Sum256([]byte(pl.Daemon + "/" + pl.Agent))
		m.Topic = base64.RawURLEncoding.EncodeToString(sum[:16])
	}
	return m, nil
}

func (p *pusher) send(regs []store.PushRecord, pl pushPayload, urgent bool) {
	for _, reg := range regs {
		m, err := p.pushMessage(reg, pl, urgent)
		if err != nil {
			p.d.log.Warn("push: encrypting", "device", reg.DeviceID, "err", err)
			continue
		}
		p.queue(reg.DeviceID, pushJob{endpoint: reg.Endpoint, msg: m})
	}
}

// queue hands a job to its endpoint's sender, starting it on first use; a full queue drops its oldest job.
func (p *pusher) queue(device string, j pushJob) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := senderKey{device, j.endpoint}
	s := p.senders[k]
	if s == nil {
		if p.ctx == nil {
			return
		}
		ctx, cancel := context.WithCancel(p.ctx)
		s = &pushSender{ch: make(chan pushJob, pushDeviceQueue), cancel: cancel}
		p.senders[k] = s
		go p.sender(ctx, k, s)
	}
	for {
		select {
		case s.ch <- j:
			return
		default:
			select {
			case <-s.ch:
				p.d.log.Warn("push: queue full; oldest push dropped", "device", device)
			default:
			}
		}
	}
}

// stop ends a device's senders, but the one for keep: what they still hold is dropped, and a push waiting out a
// retry stops waiting. RegisterPush (keep is the new endpoint), UnregisterPush and RevokeDevice call it.
func (p *pusher) stop(device, keep string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, s := range p.senders {
		if k.device == device && k.endpoint != keep {
			s.cancel()
			delete(p.senders, k)
		}
	}
}

// sender delivers one endpoint's pushes in order, retrying each per p.retry.
func (p *pusher) sender(ctx context.Context, k senderKey, s *pushSender) {
	done := func() {
		s.cancel()
		p.mu.Lock()
		if p.senders[k] == s {
			delete(p.senders, k)
		}
		p.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			done()
			return
		case j := <-s.ch:
			if !p.deliver(ctx, k.device, j) {
				// The registration has another endpoint or none (revoked, unregistered, gone): what is still
				// queued here is for this endpoint, so it is dropped with the sender.
				done()
				return
			}
		}
	}
}

// current reports whether the device still has this endpoint, so nothing goes to a revoked or replaced one.
func (p *pusher) current(ctx context.Context, device, endpoint string) bool {
	reg, err := p.d.store.Read(ctx).Push(device)
	return err == nil && reg.Endpoint == endpoint
}

// deliver sends one job, with its retries. It returns false when the device's registration is gone.
func (p *pusher) deliver(ctx context.Context, device string, j pushJob) bool {
	for attempt := 0; ; attempt++ {
		if !p.current(ctx, device, j.endpoint) {
			return false
		}
		res := webpush.Post(ctx, p.client, j.endpoint, j.msg)
		if ctx.Err() != nil {
			return true
		}
		if !p.settle(ctx, device, j.endpoint, res) {
			return false
		}
		if res.Outcome != webpush.Retry || attempt >= len(p.retry) {
			return true
		}
		wait := p.retry[attempt]
		if res.RetryAfter > wait {
			wait = min(res.RetryAfter, pushMaxRetryAfter)
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(wait):
		}
	}
}

// settle records a push's outcome. It returns false when the registration is gone (deleted now or before).
func (p *pusher) settle(ctx context.Context, device, endpoint string, res webpush.Result) bool {
	host := endpointHost(endpoint)
	alive := true
	err := p.d.store.Write(ctx, meshActor, "", func(t *store.Tx) error {
		reg, err := t.Push(device)
		if errors.Is(err, store.ErrNotFound) || (err == nil && reg.Endpoint != endpoint) {
			alive = false
			return nil
		}
		if err != nil {
			return err
		}
		switch res.Outcome {
		case webpush.Delivered:
			return t.MarkPush(device, endpoint, true, "")
		case webpush.Gone:
			alive = false
			p.d.log.Info("push: endpoint gone; removed", "device", device, "host", host, "status", res.Status)
			_, err := t.DeletePush(device, endpoint)
			return err
		}
		if res.Outcome == webpush.Rejected {
			// That push will never be taken, but the endpoint may be fine: record why, don't count it as failing.
			p.d.log.Warn("push: rejected", "device", device, "host", host, "status", res.Status, "err", res.Err, "detail", res.Detail)
			return t.NotePush(device, endpoint, res.Err.Error())
		}
		if !reg.FailingSince.IsZero() && t.Now().Sub(reg.FailingSince) > pushFailingMax {
			alive = false
			p.d.log.Info("push: endpoint failing for a week; removed", "device", device, "host", host, "err", res.Err)
			_, err := t.DeletePush(device, endpoint)
			return err
		}
		p.d.log.Warn("push: failed", "device", device, "host", host, "outcome", res.Outcome.String(), "err", res.Err, "detail", res.Detail)
		return t.MarkPush(device, endpoint, false, res.Err.Error())
	})
	if err != nil {
		p.d.log.Warn("push: recording outcome", "device", device, "err", err)
	}
	return alive
}

func endpointHost(endpoint string) string {
	if u, err := webpush.ValidEndpoint(endpoint); err == nil {
		return u.Host
	}
	return ""
}

// pushTarget is the device a push action acts on: the caller itself, or another (the host's own device only).
func pushTarget(ctx context.Context, deviceID string) (string, error) {
	actor := auth.ActorFrom(ctx)
	if actor == nil {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("no actor"))
	}
	id := strings.TrimSpace(deviceID)
	if id == "" || id == actor.Id {
		return actor.Id, nil
	}
	if err := requireHostDevice(ctx); err != nil {
		return "", denied("a device may act only on its own push endpoint")
	}
	return id, nil
}

// RegisterPush stores the calling device's push endpoint.
func (d *Daemon) RegisterPush(ctx context.Context, req *connect.Request[v1.RegisterPushRequest]) (*connect.Response[v1.RegisterPushResponse], error) {
	device, err := pushTarget(ctx, "")
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(req.Msg.Endpoint)
	if _, err := webpush.ValidEndpoint(endpoint); err != nil {
		return nil, invalid("%v", err)
	}
	pub, secret, err := webpush.ParseKeys(req.Msg.P256Dh, req.Msg.Auth)
	if err != nil {
		return nil, invalid("%v", err)
	}
	out := &v1.RegisterPushResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRegisterPushProcedure, out, func(t *store.Tx) error {
		if err := t.PutPush(device, endpoint, pub, secret); err != nil {
			return err
		}
		reg, err := t.Push(device)
		if err != nil {
			return err
		}
		out.Registration = reg.Proto()
		return nil
	}); err != nil {
		return nil, err
	}
	d.push.stop(device, endpoint)
	return connect.NewResponse(out), nil
}

// UnregisterPush removes a device's push endpoint.
func (d *Daemon) UnregisterPush(ctx context.Context, req *connect.Request[v1.UnregisterPushRequest]) (*connect.Response[v1.UnregisterPushResponse], error) {
	device, err := pushTarget(ctx, req.Msg.DeviceId)
	if err != nil {
		return nil, err
	}
	out := &v1.UnregisterPushResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceUnregisterPushProcedure, out, func(t *store.Tx) error {
		out.Removed, err = t.DeletePush(device, "")
		return err
	}); err != nil {
		return nil, err
	}
	d.push.stop(device, "")
	return connect.NewResponse(out), nil
}

// TestPush sends one test notification at once (no retry) and answers with how the push service took it. Its
// mutation is recorded only once the push service took the push: a retry of a failed call sends again (a call
// that failed recorded nothing), and a retry of one that worked gets its answer without a second push.
func (d *Daemon) TestPush(ctx context.Context, req *connect.Request[v1.TestPushRequest]) (*connect.Response[v1.TestPushResponse], error) {
	device, err := pushTarget(ctx, req.Msg.DeviceId)
	if err != nil {
		return nil, err
	}
	// What d.write checks, checked before anything is sent: a replay of a test that worked gets its answer, and a
	// mutation id used for something else or a request past its expiry sends nothing.
	actor := auth.ActorFrom(ctx)
	if mid := req.Msg.Meta.GetMutationId(); mid != "" {
		proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid)
		switch {
		case err == nil && proc != agentmeshv1connect.MeshServiceTestPushProcedure:
			return nil, reusedMutation(mid, proc)
		case err == nil:
			out := &v1.TestPushResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	if err := expired(req.Msg.Meta, d.now()); err != nil {
		return nil, err
	}
	reg, err := d.store.Read(ctx).Push(device)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("device %s has no push endpoint: turn push on in the app", device)
	}
	if err != nil {
		return nil, err
	}
	m, err := d.push.pushMessage(reg, pushPayload{V: 1, Daemon: d.daemonID, Kind: "test", Title: "agent-mesh",
		Body: "Push notifications from " + d.hostName + " work.", At: d.now().UnixMilli()}, true)
	if err != nil {
		return nil, err
	}
	res := webpush.Post(ctx, d.push.client, reg.Endpoint, m)
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	d.push.settle(ctx, device, reg.Endpoint, res)
	switch res.Outcome {
	case webpush.Delivered:
	case webpush.Gone:
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the push service says the endpoint is gone (removed): turn push off and on in the app"))
	default:
		return nil, connect.NewError(connect.CodeUnavailable, res.Err)
	}
	out := &v1.TestPushResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceTestPushProcedure, out, func(t *store.Tx) error {
		r, err := t.Push(device)
		if err == nil {
			out.Registration = r.Proto()
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
