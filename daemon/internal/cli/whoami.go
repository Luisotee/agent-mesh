package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// Whoami prints who the caller is to the daemon, as JSON: its role, name and id, its run and run folder, and for
// an agent its parent. It is `mesh whoami`: an agent reads its identity here instead of the environment.
func Whoami(env Env, args []string) int {
	fs := flag.NewFlagSet("mesh whoami", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	state := fs.String("state", DefaultState(env), "the daemon's state dir (or $"+client.StateEnv+", else "+DefaultStateDir+" when meshd runs there)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(env.Stderr, "mesh whoami: takes no arguments")
		return ExitUsage
	}
	if *state == "" {
		fmt.Fprintln(env.Stderr, "mesh: give --state or $"+client.StateEnv)
		return ExitUsage
	}
	conn, err := dialAs(env, *state)
	if err != nil {
		fmt.Fprintln(env.Stderr, "mesh:", err)
		return ExitError
	}
	ctx := context.Background()
	hello, err := conn.Mesh.Hello(ctx, connect.NewRequest(&v1.HelloRequest{}))
	if err != nil {
		fmt.Fprintln(env.Stderr, "mesh whoami:", err)
		return ExitError
	}
	actor := hello.Msg.Actor
	out := map[string]any{
		"role": strings.ToLower(strings.TrimPrefix(actor.GetClass().String(), "ACTOR_CLASS_")),
		"id":   actor.GetId(), "name": actor.GetName(), "daemon": hello.Msg.DaemonId, "state": *state,
	}
	if run := actor.GetRunId(); run != "" {
		out["run"] = run
		out["run_dir"] = statedir.Paths{Dir: *state}.RunDir(run)
		// The agent's parent, when it has one (a root's is the user: none).
		if ag, err := conn.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: actor.GetId()})); err == nil {
			if pid := ag.Msg.Agent.GetParentId(); pid != "" {
				out["parent"] = pid
				if p, err := conn.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: pid})); err == nil {
					out["parent"] = p.Msg.Agent.GetName()
				}
			}
		}
	}
	return printJSON(env.Stdout, out)
}

// Dir prints the folder of a run, where its agents write plans, reports and tasks: `$(mesh dir)/plans/<name>.md`.
// An agent's is its own run's; anyone else names one with --run.
func Dir(env Env, args []string) int {
	fs := flag.NewFlagSet("mesh dir", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	state := fs.String("state", DefaultState(env), "the daemon's state dir (or $"+client.StateEnv+", else "+DefaultStateDir+" when meshd runs there)")
	run := fs.String("run", "", "the run (default: your own)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(env.Stderr, "mesh dir: takes no arguments")
		return ExitUsage
	}
	if *run == "" {
		if d := env.Getenv(client.RunDirEnv); d != "" {
			fmt.Fprintln(env.Stdout, d)
			return ExitOK
		}
		fmt.Fprintln(env.Stderr, "mesh dir: not in a run: give --run")
		return ExitUsage
	}
	if *state == "" {
		fmt.Fprintln(env.Stderr, "mesh: give --state or $"+client.StateEnv)
		return ExitUsage
	}
	fmt.Fprintln(env.Stdout, statedir.Paths{Dir: *state}.RunDir(*run))
	return ExitOK
}

func printJSON(w io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ExitError
	}
	fmt.Fprintf(w, "%s\n", b)
	return ExitOK
}
