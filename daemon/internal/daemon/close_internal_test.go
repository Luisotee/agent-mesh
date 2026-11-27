package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
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
