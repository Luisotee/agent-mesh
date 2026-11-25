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
		done <- bus.Serve(context.Background(), &v1.SubscribeEventsRequest{}, nil, func(*v1.SubscribeEventsResponse) error {
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
