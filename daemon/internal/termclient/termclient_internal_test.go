package termclient

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

// A host that accepts the connection and never answers (a loaded machine, a host that is stopped) must not hold
// Ensure past its wait: each probe is bounded, so the caller gets an error instead of waiting for its context.
func TestEnsureGivesUpOnAHostThatAcceptsAndStaysSilent(t *testing.T) {
	old := helloTimeout
	helloTimeout = 100 * time.Millisecond
	t.Cleanup(func() { helloTimeout = old })
	dir, err := os.MkdirTemp("/tmp", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", termhost.Paths{Dir: dir}.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // held open, never read or answered
		}
	}()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := Ensure(context.Background(), dir, Options{Launch: LaunchExternal, Wait: 300 * time.Millisecond})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Ensure answered for a host that never replied")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("Ensure took %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ensure is stuck on a host that accepts and never answers")
	}
}
