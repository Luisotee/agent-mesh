// Package events serves the event log as a resumable stream: a subscriber gives its cursor and gets the events
// after it, then live ones, or a snapshot when the cursor can't be resumed. A subscriber that falls behind is
// dropped and resumes from its cursor; the log never waits for it.
package events

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// LiveBuffer is how many live events a subscriber may be behind before it is dropped.
const LiveBuffer = 1024

// ActivityBuffer is how many activity frames a subscriber may be behind before newer ones are skipped.
const ActivityBuffer = 256

const backlogPage = 500

// ErrSlow ends the stream of a subscriber that fell behind; it resumes from its cursor.
var ErrSlow = errors.New("subscriber fell behind: resume from your cursor")

// Bus fans committed events out to subscribers.
type Bus struct {
	store *store.Store
	mu    sync.Mutex
	subs  map[*sub]struct{}
	// AgentView, when set, adjusts an agent before a snapshot shows it (derived state).
	AgentView func(*v1.Agent) *v1.Agent
}

type sub struct {
	ch           chan *v1.Event
	runID        string // only this run's events, when set
	deviceEvents bool   // whether host device-management events are delivered
	dropped      bool
	// act carries ephemeral activity frames, for a subscriber served by Serve (nil: it wants none).
	act chan *v1.AgentActivity
	// views carries `OpenView` requests, for an app: a device's subscriber that also asked for activity (nil otherwise).
	views chan *v1.ViewRequest
}

// New returns a bus over a store. Wire Publish to store.OnCommit.
func New(s *store.Store) *Bus {
	return &Bus{store: s, subs: map[*sub]struct{}{}}
}

// Publish hands committed events to every subscriber without blocking; one whose buffer is full is dropped.
func (b *Bus) Publish(evs []*v1.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		for _, ev := range evs {
			if !inScope(s.runID, s.deviceEvents, ev) {
				continue
			}
			select {
			case s.ch <- ev:
			default:
				s.dropped = true
				close(s.ch)
				delete(b.subs, s)
			}
			if s.dropped {
				break
			}
		}
	}
}

// PublishActivity hands an ephemeral activity frame to the live subscribers of its run, without blocking. It is
// never stored or replayed, and a subscriber whose buffer is full misses it: a lost frame is not worth dropping
// the stream for, and the next one (or the next state event) says it again.
func (b *Bus) PublishActivity(a *v1.AgentActivity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if s.act == nil || !(s.runID == "" || a.RunId == s.runID) {
			continue
		}
		select {
		case s.act <- a:
		default:
		}
	}
}

// PublishView hands an `OpenView` request to the apps connected now (a device's live subscribers that asked for
// activity), whatever run they watch, and says how many it reached. It is never stored or replayed: a view asked for
// while no app was connected is not opened later.
func (b *Bus) PublishView(v *v1.ViewRequest) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for s := range b.subs {
		if s.views == nil {
			continue
		}
		select {
		case s.views <- v:
			n++
		default:
		}
	}
	return n
}

func (b *Bus) subscribe(runID string, deviceEvents, activity, device bool) *sub {
	s := &sub{ch: make(chan *v1.Event, LiveBuffer), runID: runID, deviceEvents: deviceEvents}
	if activity {
		s.act = make(chan *v1.AgentActivity, ActivityBuffer)
		if device {
			s.views = make(chan *v1.ViewRequest, 8)
		}
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Bus) unsubscribe(s *sub) {
	b.mu.Lock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		close(s.ch)
	}
	b.mu.Unlock()
}

// Subscribers is the number of live subscribers.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// SubscribeRun returns a channel receiving live events for runID (or all runs when empty) and an unsubscribe
// function. If the subscriber falls behind LiveBuffer, the channel closes.
func (b *Bus) SubscribeRun(runID string, deviceEvents bool) (<-chan *v1.Event, func()) {
	s := b.subscribe(runID, deviceEvents, false, false)
	return s.ch, func() { b.unsubscribe(s) }
}

// Serve runs one subscription until ctx ends, the subscriber falls behind (ErrSlow, as ResourceExhausted), or
// stopping closes (then it sends End first). When deviceEvents is false, host device-management events
// (DevicePaired, DeviceRevoked) are filtered out of both resumed backlogs and live streams.
func (b *Bus) Serve(ctx context.Context, req *v1.SubscribeEventsRequest, deviceEvents bool, stopping <-chan struct{},
	send func(*v1.SubscribeEventsResponse) error) error {
	return b.ServeFor(ctx, req, deviceEvents, false, "", stopping, send)
}

// ServeFor is Serve for a subscriber that may be a device (an app): it also gets the `OpenView` requests. A
// non-empty viewer is a subscriber that sees only its own text-bearing events (VisibleTo).
func (b *Bus) ServeFor(ctx context.Context, req *v1.SubscribeEventsRequest, deviceEvents, device bool, viewer string, stopping <-chan struct{},
	send func(*v1.SubscribeEventsResponse) error) error {
	// Live events first, so nothing committed while the backlog is read is missed; duplicates are skipped by seq.
	live := b.subscribe(req.RunId, deviceEvents, req.Activity, device)
	defer b.unsubscribe(live)

	gen := b.store.Generation()
	last, err := b.store.Read(ctx).LastSeq()
	if err != nil {
		return err
	}
	var sent int64
	if c := req.Cursor; c != nil && c.Generation == gen && c.Seq <= last && c.Seq >= 0 {
		if err := send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_Resumed{
			Resumed: &v1.Resumed{From: c}}}); err != nil {
			return err
		}
		sent = c.Seq
		for {
			evs, err := b.store.Read(ctx).EventsAfter(sent, req.RunId, backlogPage)
			if err != nil {
				return err
			}
			for _, ev := range evs {
				sent = ev.Seq
				if !inScope(req.RunId, deviceEvents, ev) || !VisibleTo(viewer, ev) {
					continue
				}
				if err := send(eventFrame(ev)); err != nil {
					return err
				}
			}
			if len(evs) < backlogPage {
				break
			}
		}
		// A filtered backlog may end before the log does; live events after this point are what count.
		if sent < last {
			sent = last
		}
	} else {
		snap, err := b.snapshot(ctx, req.RunId)
		if err != nil {
			return err
		}
		if err := send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_Snapshot{Snapshot: snap}}); err != nil {
			return err
		}
		sent = snap.Cursor.Seq
	}

	// What queued while the backlog or snapshot was sent is older than events the client has just applied. Not the
	// views: an `OpenView` is no state the snapshot covers, and one asked for just after the client got its snapshot
	// (it can act before this loop runs) is a view the app must open, not one to drop.
	for n := len(live.act); n > 0; n-- {
		<-live.act
	}
	for {
		// The frames queued now are told before the event after them: they were published first (an activity frame
		// goes out as its write commits, a state event as the next write does), and select alone picks a ready
		// channel at random. Only what is queued now, so frames arriving faster than they are sent can't hold events off.
	frames:
		for n := len(live.act); n > 0; n-- {
			select {
			case a := <-live.act:
				if err := send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_Activity{Activity: a}}); err != nil {
					return err
				}
			default:
				break frames
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopping:
			return send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_End{
				End: &v1.End{Reason: "daemon shutting down"}}})
		case a := <-live.act:
			if err := send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_Activity{Activity: a}}); err != nil {
				return err
			}
		case v := <-live.views: // nil channel for a subscriber that is no app: never ready
			if err := send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_View{View: v}}); err != nil {
				return err
			}
		case ev, ok := <-live.ch:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted, ErrSlow)
			}
			if ev.Seq <= sent || !inScope(req.RunId, deviceEvents, ev) {
				continue
			}
			sent = ev.Seq
			if !VisibleTo(viewer, ev) {
				continue
			}
			if err := send(eventFrame(ev)); err != nil {
				return err
			}
		}
	}
}

// IsDeviceEvent reports whether ev is a host device-management event (DevicePaired or DeviceRevoked).
func IsDeviceEvent(ev *v1.Event) bool {
	if ev == nil {
		return false
	}
	switch ev.Payload.(type) {
	case *v1.Event_DevicePaired, *v1.Event_DeviceRevoked:
		return true
	}
	return false
}

// VisibleTo reports whether a viewer (an agent id; empty: no restriction) may see ev. The events that carry text
// someone wrote to someone else (a message, a prompt the user typed, a draft held in the user's input box) are
// the viewer's only when they are its own; every other event (a state, a spawn, a PR) is run-wide metadata. A run is
// otherwise one trust domain: the host restricts only the agents that are read-only and short-lived, an asker or a
// reviewer, which may run on a third-party model (`restrictedViewer` in the daemon).
func VisibleTo(viewer string, ev *v1.Event) bool {
	if viewer == "" || ev == nil {
		return true
	}
	switch p := ev.Payload.(type) {
	case *v1.Event_MessageSent:
		m := p.MessageSent.GetMessage()
		return m.GetFrom().GetId() == viewer || m.GetToId() == viewer
	case *v1.Event_PromptQueued:
		return p.PromptQueued.GetPrompt().GetAgentId() == viewer
	case *v1.Event_DoorbellHeld, *v1.Event_RootResetRequested, *v1.Event_RootResetFinished, *v1.Event_AgentReopenRequested,
		*v1.Event_AgentReopened, *v1.Event_AgentReopenDropped:
		// A held draft; a root's own notes (what it does next, its focus, its transcript); the `--why` of an approve
		// or a resume: all free text the agent's parent wrote, no one else's to read.
		return ev.AgentId == viewer
	}
	return true
}

// inScope reports whether a subscriber of runID (empty: every run) gets ev. Device-management events are
// delivered only when deviceEvents is true; other run-less events (a setting changed) reach every subscriber.
func inScope(runID string, deviceEvents bool, ev *v1.Event) bool {
	if IsDeviceEvent(ev) && !deviceEvents {
		return false
	}
	return runID == "" || ev.RunId == "" || ev.RunId == runID
}

func eventFrame(ev *v1.Event) *v1.SubscribeEventsResponse {
	return &v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_Event{Event: ev}}
}

func (b *Bus) snapshot(ctx context.Context, runID string) (*v1.Snapshot, error) {
	snap := &v1.Snapshot{}
	err := b.store.Snapshot(ctx, func(r store.Reader) error {
		seq, err := r.LastSeq()
		if err != nil {
			return err
		}
		snap.Cursor = &v1.Cursor{Generation: b.store.Generation(), Seq: seq}
		if runID != "" {
			run, err := r.GetRun(runID)
			if err != nil {
				return err
			}
			snap.Runs = []*v1.Run{run}
		} else if snap.Runs, err = r.ListRuns(false); err != nil {
			return err
		}
		agents, err := r.ListAgents(runID)
		if err != nil {
			return err
		}
		for _, a := range agents {
			if b.AgentView != nil {
				a = b.AgentView(a)
			}
			snap.Agents = append(snap.Agents, a)
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no run "+runID))
	}
	return snap, err
}
