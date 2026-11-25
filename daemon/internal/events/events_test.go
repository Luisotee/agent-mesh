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
	sub := bus.subscribe("r1", false)
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
	nonDevSub := bus.subscribe("r1", false)
	devSub := bus.subscribe("r1", true)
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
