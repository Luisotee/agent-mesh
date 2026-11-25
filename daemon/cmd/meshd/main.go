// Command meshd is agent-mesh's host daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	self, _ := os.Executable()
	here := filepath.Dir(self)
	state := flag.String("state", os.Getenv(client.StateEnv), "state dir (or $"+client.StateEnv+"); required")
	tcp := flag.String("tcp", "", "also listen on this loopback address, e.g. 127.0.0.1:0")
	maxLife := flag.Duration("max-lifetime", 0, "exit after this long, and start mesh-term with the same "+
		"(tests: no orphan survives a killed test)")
	termLaunch := flag.String("term-launch", string(termclient.LaunchDetached),
		"how to start the terminal host when none answers: detached, systemd, external, or none (no agents)")
	termBinary := flag.String("term-binary", filepath.Join(here, "mesh-term"), "mesh-term, for --term-launch detached")
	termUnit := flag.String("term-unit", "agent-mesh-term.service", "the user unit, for --term-launch systemd")
	meshctl := flag.String("meshctl", filepath.Join(here, "meshctl"), "the CLI the agents' hooks run")
	worktrees := flag.String("worktrees", "", "where agents' worktrees go (default <state>/worktrees)")
	skip := flag.Bool("skip-permissions", false, "start agents with their CLI's permission prompts off")
	var bins, envs multi
	flag.Var(&bins, "cli", "kind=path of an agent CLI, e.g. claude=/usr/local/bin/claude (repeatable)")
	flag.Var(&envs, "agent-env", "KEY=VALUE added to every agent's environment (repeatable)")
	flag.Parse()
	if *state == "" {
		fmt.Fprintln(os.Stderr, "meshd: give --state or $"+client.StateEnv)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	agents := daemon.AgentConfig{TermBinary: *termBinary, TermUnit: *termUnit, Meshctl: *meshctl,
		Worktrees: *worktrees, SkipPermissions: *skip, Binaries: map[string]string{}, Env: map[string]string{}}
	if *termLaunch != "none" {
		agents.TermLaunch = termclient.Launch(*termLaunch)
	}
	if *maxLife > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *maxLife)
		defer cancel()
		agents.TermArgs = []string{"--max-lifetime", maxLife.String()}
	}
	for _, b := range bins {
		k, v, ok := strings.Cut(b, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "meshd: --cli %q: want kind=path\n", b)
			os.Exit(2)
		}
		agents.Binaries[k] = v
	}
	for _, e := range envs {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "meshd: --agent-env %q: want KEY=VALUE\n", e)
			os.Exit(2)
		}
		agents.Env[k] = v
	}
	d, err := daemon.New(daemon.Config{StateDir: *state, TCP: *tcp, Logger: log, Now: time.Now, Agents: agents})
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
	if err := d.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "meshd:", err)
		os.Exit(1)
	}
}
