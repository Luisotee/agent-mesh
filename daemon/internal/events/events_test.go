package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestSlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	s.OnCommit = bus.Publish

	first := make(chan struct{})
	block := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		n := 0
		done <- bus.Serve(context.Background(), &v1.SubscribeEventsRequest{}, true, nil, func(*v1.SubscribeEventsResponse) error {
			if n == 0 {
				close(first)
			}
			n++
			<-block // never reads past the snapshot
			return nil
		})
	}()
	<-first
	// Publishing more than the buffer must not block the writer.
	wrote := make(chan struct{})
	go func() {
		evs := make([]*v1.Event, LiveBuffer+10)
		for i := range evs {
			evs[i] = &v1.Event{Seq: int64(i + 1)}
		}
		bus.Publish(evs)
		close(wrote)
	}()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	close(block)
	select {
	case err := <-done:
		if connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("Serve = %v, want ResourceExhausted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slow subscriber was not dropped")
	}
	if bus.Subscribers() != 0 {
		t.Errorf("%d subscribers left", bus.Subscribers())
	}
}

func TestOtherRunsDoNotFillAFilteredSubscriber(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	sub := bus.subscribe("r1", false, false, false)
	evs := make([]*v1.Event, LiveBuffer+10)
	for i := range evs {
		evs[i] = &v1.Event{Seq: int64(i + 1), RunId: "r2"}
	}
	bus.Publish(evs)
	bus.Publish([]*v1.Event{{Seq: 9999, RunId: "r1"}})
	if sub.dropped || len(sub.ch) != 1 {
		t.Errorf("dropped %v, %d queued", sub.dropped, len(sub.ch))
	}
}

func TestDeviceEventsFilteredFromNonDeviceSubscribers(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	s.OnCommit = bus.Publish

	ctx := context.Background()
	if err := s.Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Status: v1.RunStatus_RUN_STATUS_OPEN}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{
			Payload: &v1.Event_DevicePaired{DevicePaired: &v1.DevicePaired{Device: &v1.Device{Id: "dev1", Name: "Pixel"}}},
		}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{
			Payload: &v1.Event_DeviceRevoked{DeviceRevoked: &v1.DeviceRevoked{DeviceId: "dev1", Name: "Pixel"}},
		}); err != nil {
			return err
		}
		_, err := tx.AppendEvent(&v1.Event{
			RunId:   "r1",
			Payload: &v1.Event_RunStarted{RunStarted: &v1.RunStarted{Run: &v1.Run{Id: "r1"}}},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Non-device subscriber resuming from cursor 0 skips DevicePaired/DeviceRevoked and receives RunStarted.
	collect := func(deviceEvents bool, want int) []*v1.Event {
		t.Helper()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var got []*v1.Event
		err := bus.Serve(cctx, &v1.SubscribeEventsRequest{
			RunId:  "r1",
			Cursor: &v1.Cursor{Generation: s.Generation(), Seq: 0},
		}, deviceEvents, nil, func(resp *v1.SubscribeEventsResponse) error {
			if ev := resp.GetEvent(); ev != nil {
				got = append(got, ev)
				if len(got) == want {
					cancel()
				}
			}
			return nil
		})
		if err != nil && err != context.Canceled {
			t.Fatalf("Serve(deviceEvents=%v): %v", deviceEvents, err)
		}
		return got
	}

	nonDev := collect(false, 1)
	if len(nonDev) != 1 || nonDev[0].GetRunStarted() == nil {
		t.Fatalf("non-device backlog = %+v, want only RunStarted", nonDev)
	}
	dev := collect(true, 3)
	if len(dev) != 3 || dev[0].GetDevicePaired() == nil || dev[1].GetDeviceRevoked() == nil || dev[2].GetRunStarted() == nil {
		t.Fatalf("device backlog = %+v, want DevicePaired, DeviceRevoked, RunStarted", dev)
	}

	// Live publishing also drops device events for non-device subscribers.
	nonDevSub := bus.subscribe("r1", false, false, false)
	devSub := bus.subscribe("r1", true, false, false)
	bus.Publish([]*v1.Event{
		{Seq: 100, Payload: &v1.Event_DevicePaired{DevicePaired: &v1.DevicePaired{Device: &v1.Device{Id: "dev2"}}}},
		{Seq: 101, Payload: &v1.Event_DeviceRevoked{DeviceRevoked: &v1.DeviceRevoked{DeviceId: "dev2"}}},
	})
	if len(nonDevSub.ch) != 0 {
		t.Fatalf("non-device live subscriber received %d device events", len(nonDevSub.ch))
	}
	if len(devSub.ch) != 2 {
		t.Fatalf("device live subscriber received %d events, want 2", len(devSub.ch))
	}
}

func TestSubscribeRunReceivesLiveRunEventsAndUnsubscribesCleanly(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	ch, unsub := bus.SubscribeRun("r1", false)
	defer unsub()
	if bus.Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", bus.Subscribers())
	}
	bus.Publish([]*v1.Event{
		{Seq: 1, RunId: "r2"},
		{Seq: 2, RunId: "r1"},
	})
	select {
	case ev := <-ch:
		if ev.Seq != 2 {
			t.Fatalf("got seq %d, want 2", ev.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for r1 event")
	}
	unsub()
	if bus.Subscribers() != 0 {
		t.Fatalf("subscribers after unsub = %d, want 0", bus.Subscribers())
	}
}

func TestActivityFramesAreLiveOnlyScopedAndNeverDropASubscriber(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	mine := bus.subscribe("r1", false, true, false)
	other := bus.subscribe("r2", false, true, false)
	none := bus.subscribe("", false, false, false)
	// Past the buffer: the frames are skipped and the subscriber stays.
	for i := 0; i < ActivityBuffer+10; i++ {
		bus.PublishActivity(&v1.AgentActivity{RunId: "r1", AgentId: "a1", Detail: "Bash"})
	}
	if mine.dropped || len(mine.act) != ActivityBuffer {
		t.Errorf("dropped %v, %d queued, want %d", mine.dropped, len(mine.act), ActivityBuffer)
	}
	if len(other.act) != 0 || none.act != nil {
		t.Errorf("another run's subscriber got %d frames, one that wants none has a channel: %v", len(other.act), none.act)
	}
	if bus.Subscribers() != 3 {
		t.Errorf("%d subscribers, want 3", bus.Subscribers())
	}
}

// An activity frame that was published before a state event is told before it: with both ready, select would pick
// either, and a frame after the state event that followed it shows a tool that has stopped running.
func TestAnActivityFrameIsToldBeforeTheEventPublishedAfterIt(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	s.OnCommit = bus.Publish
	write := func(ctx context.Context) {
		t.Helper()
		if err := s.Write(ctx, nil, "", func(tx *store.Tx) error {
			_, err := tx.AppendEvent(&v1.Event{RunId: "r1", Payload: &v1.Event_RunStarted{RunStarted: &v1.RunStarted{}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 30; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		held, release := make(chan struct{}), make(chan struct{})
		got := make(chan string, 16)
		done := make(chan error, 1)
		go func() {
			n := 0
			done <- bus.Serve(ctx, &v1.SubscribeEventsRequest{Activity: true}, true, nil, func(f *v1.SubscribeEventsResponse) error {
				switch {
				case f.GetActivity() != nil:
					got <- "activity"
				case f.GetEvent() != nil:
					got <- "event"
					if n++; n == 1 {
						close(held)
						<-release // both are ready when the stream looks again
					}
				}
				return nil
			})
		}()
		// Past the snapshot the stream is live: an event takes it into its send, where it waits.
		for ready := false; !ready; {
			write(ctx)
			select {
			case <-held:
				ready = true
			case <-time.After(20 * time.Millisecond):
			}
		}
		for len(got) > 0 {
			<-got
		}
		bus.PublishActivity(&v1.AgentActivity{RunId: "r1", AgentId: "a1", Detail: "Bash"})
		write(ctx)
		close(release)
		for _, want := range []string{"activity", "event"} {
			select {
			case x := <-got:
				if x != want {
					t.Fatalf("round %d: told %s, want %s", round, x, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: %s never told", round, want)
			}
		}
		cancel()
		<-done
	}
}

// Frames that queued while the snapshot or backlog was being sent are older than the events just sent: they are dropped.
func TestActivityFramesQueuedDuringTheSnapshotAreDropped(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	s.OnCommit = bus.Publish
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held, release := make(chan struct{}), make(chan struct{})
	got := make(chan string, 64)
	go bus.Serve(ctx, &v1.SubscribeEventsRequest{Activity: true}, true, nil, func(f *v1.SubscribeEventsResponse) error {
		if f.GetSnapshot() != nil {
			close(held)
			<-release
			return nil
		}
		if a := f.GetActivity(); a != nil {
			got <- a.Detail
		}
		return nil
	})
	<-held
	bus.PublishActivity(&v1.AgentActivity{RunId: "r1", AgentId: "a1", Detail: "old"})
	close(release)
	// Live frames after the snapshot are told (published until one is, as the stream may not have looked yet), and the
	// one queued during the snapshot is not among them.
	for told := false; !told; {
		bus.PublishActivity(&v1.AgentActivity{RunId: "r1", AgentId: "a1", Detail: "new"})
		select {
		case d := <-got:
			if d != "new" {
				t.Fatalf("told the frame queued during the snapshot: %s", d)
			}
			told = true
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// An `OpenView` asked for right after an app got its snapshot is told to it, however soon it comes: the app can
// act on the snapshot before the stream's loop has run (the frames that queued during the snapshot are drained
// there), and a view dropped there left a caller waiting on a toast that never came.
func TestAViewPublishedWhileTheSnapshotIsBeingSentIsNotDropped(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bus := New(s)
	s.OnCommit = bus.Publish
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	go bus.ServeFor(ctx, &v1.SubscribeEventsRequest{Activity: true}, true, true, "", nil, func(f *v1.SubscribeEventsResponse) error {
		if f.GetSnapshot() != nil {
			// What the app does with its snapshot, before the server's next line runs.
			bus.PublishView(&v1.ViewRequest{View: "messages", Target: "a1"})
		}
		if v := f.GetView(); v != nil {
			got <- v.Target
		}
		return nil
	})
	select {
	case target := <-got:
		if target != "a1" {
			t.Errorf("view target %q", target)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the view published while the snapshot was sent was dropped")
	}
}

func TestVisibleToShowsAViewerOnlyItsOwnTextBearingEvents(t *testing.T) {
	msg := func(from, to string) *v1.Event {
		return &v1.Event{Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: &v1.Message{
			From: &v1.Actor{Id: from}, ToId: to, Body: "text"}}}}
	}
	prompt := func(agent string) *v1.Event {
		return &v1.Event{Payload: &v1.Event_PromptQueued{PromptQueued: &v1.PromptQueued{Prompt: &v1.QueuedPrompt{AgentId: agent, Text: "typed"}}}}
	}
	held := func(agent string) *v1.Event {
		return &v1.Event{AgentId: agent, Payload: &v1.Event_DoorbellHeld{DoorbellHeld: &v1.DoorbellHeld{Draft: "draft"}}}
	}
	state := &v1.Event{AgentId: "other", Payload: &v1.Event_AgentStateChanged{AgentStateChanged: &v1.AgentStateChanged{}}}
	for _, c := range []struct {
		name   string
		viewer string
		ev     *v1.Event
		want   bool
	}{
		{"unrestricted sees a message between others", "", msg("a", "b"), true},
		{"a message to the viewer", "me", msg("a", "me"), true},
		{"a message from the viewer", "me", msg("me", "b"), true},
		{"a message between others", "me", msg("a", "b"), false},
		{"a prompt for the viewer", "me", prompt("me"), true},
		{"a prompt for another agent", "me", prompt("b"), false},
		{"a held draft of another agent", "me", held("b"), false},
		{"its own held draft", "me", held("me"), true},
		{"a state change of another agent is metadata", "me", state, true},
	} {
		if got := VisibleTo(c.viewer, c.ev); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
