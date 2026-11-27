package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// close() waits for every goroutine started with goBG, and returns once they saw the daemon stop; none starts
// after it began, so none can read a store that is closed or write a state dir that is gone.
func TestCloseWaitsForBackgroundWorkAndReturnsOnceItSawTheStop(t *testing.T) {
	d, err := New(Config{StateDir: filepath.Join(t.TempDir(), "s"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var sawStop, finished = make(chan struct{}), make(chan struct{})
	if !d.goBG(func() {
		<-d.stopping
		close(sawStop)
		time.Sleep(50 * time.Millisecond) // work that ends after the stop
		close(finished)
	}) {
		t.Fatal("goBG refused before close")
	}
	closed := make(chan struct{})
	go func() { d.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close() hangs on a goroutine that ends at the stop")
	}
	select {
	case <-finished:
	default:
		t.Fatal("close() returned before the background work ended")
	}
	if d.goBG(func() { t.Error("ran after close") }) {
		t.Fatal("goBG accepted work after close")
	}
}

// A doorbell run (deliverSoon) is in bgWG and ends with the daemon's stop, so a poll that rings one can't outlive
// the store it reads.
func TestStopCtxEndsWithTheDaemon(t *testing.T) {
	d := &Daemon{stopping: make(chan struct{})}
	ctx, cancel := d.stopCtx()
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("ended before the stop")
	}
	close(d.stopping)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stopCtx did not end at the stop")
	}
}

// A context made after the stop is already ended: nothing that runs on it starts its work.
func TestStopCtxIsEndedWhenTheDaemonAlreadyStopped(t *testing.T) {
	d := &Daemon{stopping: make(chan struct{})}
	close(d.stopping)
	ctx, cancel := d.stopCtx()
	defer cancel()
	if ctx.Err() == nil {
		t.Fatal("a context made after the stop is not ended")
	}
}

// A retire that finishes during shutdown sweeps nothing: after close() no goroutine starts (goBG refuses) and a
// sweep on an ended context returns before it reads the store, which is closed by then.
func TestASweepAfterCloseReadsNoStore(t *testing.T) {
	var logs bytes.Buffer
	d, err := New(Config{StateDir: filepath.Join(t.TempDir(), "s"), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	d.close()
	agents := []*v1.Agent{{Id: "a1", RunId: "r1", Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: "/nonexistent/wt"}}
	d.sweepAsync(agents)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.sweepWorktrees(ctx, agents)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(logs.String(), "reading run agents") {
		t.Fatalf("a sweep read the closed store: %s", logs.String())
	}
}
