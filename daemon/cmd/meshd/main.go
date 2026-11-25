// Command meshd is agent-mesh's host daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func main() {
	state := flag.String("state", os.Getenv(client.StateEnv), "state dir (or $"+client.StateEnv+"); required")
	tcp := flag.String("tcp", "", "also listen on this loopback address, e.g. 127.0.0.1:0")
	maxLife := flag.Duration("max-lifetime", 0, "exit after this long (tests: no orphan survives a killed test)")
	flag.Parse()
	if *state == "" {
		fmt.Fprintln(os.Stderr, "meshd: give --state or $"+client.StateEnv)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *maxLife > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *maxLife)
		defer cancel()
	}
	d, err := daemon.New(daemon.Config{StateDir: *state, TCP: *tcp, Logger: log, Now: time.Now})
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
	if err := d.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
}
