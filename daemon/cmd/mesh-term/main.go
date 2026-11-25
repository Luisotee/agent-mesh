// Command mesh-term is agent-mesh's terminal host: it owns the agents' PTYs so they survive a daemon restart.
// The daemon starts it (or systemd does) and is its only client.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

func main() {
	dir := flag.String("dir", "", "the host's dir (the daemon's <state>/term); required")
	maxLife := flag.Duration("max-lifetime", 0, "exit after this long (tests: no orphan survives a killed test)")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "mesh-term: give --dir")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// A daemon that exits must not take us along: ignore the hangup of a closed session.
	signal.Ignore(syscall.SIGHUP)
	if *maxLife > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *maxLife)
		defer cancel()
	}
	h, err := termhost.Open(termhost.Config{Dir: *dir, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh-term:", err)
		os.Exit(1)
	}
	if err := h.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "mesh-term:", err)
		os.Exit(1)
	}
}
