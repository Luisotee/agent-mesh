package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/notice"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/webpush"
)

// Push notifications (README, "Push"): every notice the apps would post (internal/notice) goes, encrypted for the
// device (RFC 8291), to each paired device's UnifiedPush endpoint, so a phone gets it with its app closed.

// PushConfig tunes the push sender; the zero value is the default. Tests shorten the retries.
type PushConfig struct {
	// Retry is the wait before each retry of a push the service couldn't take yet (429, 5xx, a network error).
	Retry []time.Duration
	// Client posts the pushes; nil is webpush.NewClient (15 s timeout, no redirects, public addresses only unless
	// push.allow_private_endpoints).
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
	keys     [32]byte // the keys it was encrypted for (keyPrint)
	msg      webpush.Message
}

// keyPrint identifies a registration's WebPush keys.
func keyPrint(reg store.PushRecord) [32]byte {
	return sha256.Sum256(append(append([]byte{}, reg.P256dh...), reg.Auth...))
}

// onPushLeave is a test seam: it runs when a sender's endpoint stopped matching, before it decides to go.
var onPushLeave func()

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

	// pending counts the batches enqueued and not yet dispatched (for tests: the notify settings are read when a batch
	// is dispatched, so a test that changes them waits for this to reach zero first).
	pending atomic.Int64

	// started and live count senders, for tests; wg waits for them at shutdown.
	started, live atomic.Int64
	wg            sync.WaitGroup

	// spawned holds the agents spawned lately, as their AgentSpawned events said: a launch that fails deletes its
	// agent in the transaction that reports the exit, so the store no longer has it when the exit is pushed.
	// Only the dispatcher touches it.
	spawned map[string]*v1.Agent
	// runs is what decide remembers of each run (a lead's finish, told once). Only the dispatcher touches it.
	runs map[string]*runState
}

// maxSpawned bounds the spawned cache; past it the cache starts over (an agent launched long ago is in the store).
const maxSpawned = 1000

func newPusher(d *Daemon, cfg PushConfig) *pusher {
	p := &pusher{d: d, client: cfg.Client, retry: cfg.Retry, in: make(chan []*v1.Event, pushInbox), senders: map[senderKey]*pushSender{}, spawned: map[string]*v1.Agent{}}
	if p.client == nil {
		p.client = webpush.NewClient(p.allowPrivate)
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
		if _, spawn := e.Payload.(*v1.Event_AgentSpawned); spawn || relevant(e) {
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		return
	}
	p.pending.Add(1)
	select {
	case p.in <- keep:
	default:
		p.pending.Add(-1)
		p.d.log.Warn("push: dispatcher behind; events not pushed", "events", len(keep))
	}
}

// run dispatches until ctx ends, then returns once every sender has stopped.
func (p *pusher) run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			// Past the lock, queue sees ctx ended and starts no sender: every wg.Add happened before.
			p.mu.Lock()
			p.mu.Unlock()
			p.wg.Wait()
			return
		case evs := <-p.in:
			p.dispatch(ctx, evs)
			p.pending.Add(-1)
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
		ns := p.decide(r, e, lookup)
		if _, exit := e.Payload.(*v1.Event_AgentExited); exit {
			delete(p.spawned, e.AgentId)
		}
		for _, n := range ns {
			p.announce(ctx, e, n)
		}
	}
}

// relevant reports whether decide may make a notice of this event.
func relevant(e *v1.Event) bool {
	switch e.Payload.(type) {
	case *v1.Event_AgentStateChanged, *v1.Event_MessageSent, *v1.Event_AgentExited, *v1.Event_PullRequestChanged, *v1.Event_PermissionChanged:
		return true
	}
	return false
}

// announce stores a notice as a UserNotice event, then pushes it to the phones that take its kind, when it is for the user.
func (p *pusher) announce(ctx context.Context, e *v1.Event, n *notice.Notice) {
	seq, err := p.d.record(ctx, e.RunId, n)
	if err != nil {
		p.d.log.Warn("notice: storing", "err", err)
	}
	if !n.ForUser {
		return
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
	// A device that chose its own list is pushed by it; one that didn't, by the host's.
	var to []store.PushRecord
	for _, reg := range regs {
		if pushedKind(reg, n.Kind, kinds) {
			to = append(to, reg)
		}
	}
	if len(to) == 0 {
		return
	}
	at := p.d.now()
	if e.Time != nil {
		at = e.Time.AsTime()
	}
	tab := n.Tab
	if tab == notice.TabChat {
		tab = notice.TabMessages
	}
	p.send(to, pushPayload{V: 1, Daemon: p.d.daemonID, Seq: seq, // the UserNotice event's: the phone's live stream brings the same event, and it drops the repeat by this number
		Kind: n.Kind, Title: n.Title, Body: n.Body,
		Agent: n.AgentID, Tab: tab, At: at.UnixMilli()}, n.Waiting())
}

// pushedKind reports whether a registration is pushed a notice of this kind: by its own list when it chose one, else by the
// host's (hostKinds).
func pushedKind(reg store.PushRecord, kind string, hostKinds map[string]bool) bool {
	if len(reg.Events) > 0 {
		return slices.Contains(reg.Events, kind)
	}
	return hostKinds[kind]
}

// allowPrivate reads push.allow_private_endpoints, at each dial.
func (p *pusher) allowPrivate() bool {
	v, err := p.d.Setting(p.d.store.Read(context.Background()), "push.allow_private_endpoints")
	b, _ := v.(bool)
	return err == nil && b
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
		p.queue(reg.DeviceID, pushJob{endpoint: reg.Endpoint, keys: keyPrint(reg), msg: m})
	}
}

// queue hands a job to its endpoint's sender, starting it on first use; a full queue drops its oldest job.
func (p *pusher) queue(device string, j pushJob) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil || p.ctx.Err() != nil {
		return // not started yet, or shutting down
	}
	k := senderKey{device, j.endpoint}
	s := p.senders[k]
	if s == nil {
		p.started.Add(1)
		p.wg.Add(1)
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
	p.live.Add(1)
	defer p.wg.Done()
	defer p.live.Add(-1)
	leave := func() {
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
			// Stopped (stop took it out already) or shutting down: what it holds is dropped.
			leave()
			return
		case j := <-s.ch:
			if p.deliver(ctx, k.device, j) {
				continue
			}
			// The registration has another endpoint or none (revoked, unregistered, gone), or new keys: that job is
			// dropped. If the device registered this same endpoint again meanwhile, the rest is still for it, in
			// order; else what is still queued here goes with the sender.
			if onPushLeave != nil {
				onPushLeave()
			}
			if ctx.Err() == nil && p.running() && p.registered(ctx, k.device, k.endpoint) {
				continue
			}
			leave()
			return
		}
	}
}

// running reports whether the pusher runs and isn't shutting down.
func (p *pusher) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctx != nil && p.ctx.Err() == nil
}

// current reports whether the device still has the job's endpoint and keys, so nothing goes to a revoked or
// replaced endpoint, nor under keys the phone no longer has.
func (p *pusher) current(ctx context.Context, device string, j pushJob) bool {
	reg, err := p.d.store.Read(ctx).Push(device)
	return err == nil && reg.Endpoint == j.endpoint && keyPrint(reg) == j.keys
}

// registered reports whether the device's registration has this endpoint (whatever its keys).
func (p *pusher) registered(ctx context.Context, device, endpoint string) bool {
	reg, err := p.d.store.Read(ctx).Push(device)
	return err == nil && reg.Endpoint == endpoint
}

// deliver sends one job, with its retries. It returns false when the device's registration is gone.
func (p *pusher) deliver(ctx context.Context, device string, j pushJob) bool {
	for attempt := 0; ; attempt++ {
		if !p.current(ctx, device, j) {
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
	if err := requireHostDevice(ctx, "act on another device's push endpoint"); err != nil {
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
	events, err := pushEvents(req.Msg.Events)
	if err != nil {
		return nil, err
	}
	out := &v1.RegisterPushResponse{}
	rekeyed := false
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRegisterPushProcedure, out, func(t *store.Tx) error {
		old, err := t.Push(device)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		rekeyed = err == nil && old.Endpoint == endpoint && (!bytes.Equal(old.P256dh, pub) || !bytes.Equal(old.Auth, secret))
		if err := t.PutPush(device, endpoint, pub, secret, events); err != nil {
			return err
		}
		reg, err := t.Push(device)
		if err != nil {
			return err
		}
		out.Registration = reg.Proto()
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A replay changed nothing (it may be an old endpoint's): the senders stay. New keys for the same endpoint stop
	// its sender too: what it holds was encrypted for the old ones, which the phone can no longer read.
	switch {
	case replayed:
	case rekeyed:
		d.push.stop(device, "")
	default:
		d.push.stop(device, endpoint)
	}
	return connect.NewResponse(out), nil
}

// pushEvents checks the kinds a device chose to be pushed against the ones app.notify.events knows, and drops a repeat:
// an unknown kind is refused (a typo would silently push nothing), and none means follow the host's list.
func pushEvents(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, k := range in {
		k = strings.TrimSpace(k)
		if !slices.Contains(settings.NotifyEvents, k) {
			return nil, invalid("unknown event %q: one of %s", k, strings.Join(settings.NotifyEvents, ", "))
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

// UnregisterPush removes a device's push endpoint.
func (d *Daemon) UnregisterPush(ctx context.Context, req *connect.Request[v1.UnregisterPushRequest]) (*connect.Response[v1.UnregisterPushResponse], error) {
	device, err := pushTarget(ctx, req.Msg.DeviceId)
	if err != nil {
		return nil, err
	}
	out := &v1.UnregisterPushResponse{}
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceUnregisterPushProcedure, out, func(t *store.Tx) error {
		var err error
		out.Removed, err = t.DeletePush(device, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	if !replayed {
		d.push.stop(device, "")
	}
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
	// The post finishes (within the client's 15 s) even if the caller goes, cancelled or past its deadline, as a
	// push once sent can't be taken back: its outcome is settled and a push the service took is recorded below,
	// so a retry gets the answer instead of pushing twice.
	wctx := context.WithoutCancel(ctx)
	res := webpush.Post(wctx, d.push.client, reg.Endpoint, m)
	gone := ctx.Err()
	d.push.settle(wctx, device, reg.Endpoint, res)
	switch {
	case res.Outcome == webpush.Delivered:
	case res.Outcome == webpush.Gone:
		d.push.stop(device, "") // a sender waiting out a retry for it stops now
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the push service says the endpoint is gone (removed): turn push off and on in the app"))
	case gone != nil:
		return nil, callerGone(gone)
	default:
		return nil, connect.NewError(connect.CodeUnavailable, res.Err)
	}
	// Recorded even if the request expired during the post (its expiry was checked before it).
	meta := proto.CloneOf(req.Msg.Meta)
	if meta != nil {
		meta.ExpiresAt = nil
	}
	out := &v1.TestPushResponse{}
	if _, err := d.write(wctx, meta, agentmeshv1connect.MeshServiceTestPushProcedure, out, func(t *store.Tx) error {
		r, err := t.Push(device)
		if err == nil {
			out.Registration = r.Proto()
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if gone != nil {
		return nil, callerGone(gone)
	}
	return connect.NewResponse(out), nil
}

// callerGone is the answer to a caller whose context ended.
func callerGone(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeCanceled, err)
}
