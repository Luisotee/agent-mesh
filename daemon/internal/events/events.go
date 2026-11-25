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
	ch      chan *v1.Event
	runID   string // only this run's events, when set
	dropped bool
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
			if s.runID != "" && ev.RunId != s.runID {
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

func (b *Bus) subscribe(runID string) *sub {
	s := &sub{ch: make(chan *v1.Event, LiveBuffer), runID: runID}
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

// Serve runs one subscription until ctx ends, the subscriber falls behind (ErrSlow, as ResourceExhausted), or
// stopping closes (then it sends End first).
func (b *Bus) Serve(ctx context.Context, req *v1.SubscribeEventsRequest, stopping <-chan struct{},
	send func(*v1.SubscribeEventsResponse) error) error {
	// Live events first, so nothing committed while the backlog is read is missed; duplicates are skipped by seq.
	live := b.subscribe(req.RunId)
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
				if err := send(eventFrame(ev)); err != nil {
					return err
				}
				sent = ev.Seq
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopping:
			return send(&v1.SubscribeEventsResponse{Frame: &v1.SubscribeEventsResponse_End{
				End: &v1.End{Reason: "daemon shutting down"}}})
		case ev, ok := <-live.ch:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted, ErrSlow)
			}
			if ev.Seq <= sent || (req.RunId != "" && ev.RunId != req.RunId) {
				continue
			}
			if err := send(eventFrame(ev)); err != nil {
				return err
			}
			sent = ev.Seq
		}
	}
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
