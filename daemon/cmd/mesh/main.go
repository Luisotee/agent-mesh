// Command mesh is agent-mesh's CLI for agents and users. Its commands
// are the action registry's, derived from the schemas at run time (internal/cli); only `hook` (what a CLI's
// hooks run), `version`, `whoami` and `dir` are written by hand.
package main

import (
	"fmt"
	"os"

	"github.com/Luisotee/agent-mesh/daemon/internal/cli"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
)

// Version is mesh's version.
const Version = "0.1.0-dev"

func main() {
	env := cli.OSEnv()
	os.Exit(cli.Main(os.Args[1:], env, map[string]func(cli.Env, []string) int{
		"hook":       func(_ cli.Env, a []string) int { return runHook(a) },
		"statusline": runStatusLine,
		"whoami":     cli.Whoami,
		"dir":        cli.Dir,
		"version":    func(cli.Env, []string) int { fmt.Println("mesh", Version); return 0 },
	}))
}

func runStatusLine(env cli.Env, args []string) int {
	var state string
	for i := 0; i < len(args); i++ {
		if args[i] == "--state" && i+1 < len(args) {
			state = args[i+1]
			i++
		}
	}
	if state == "" {
		state = cli.DefaultState(env)
	}
	_ = hook.RunStatusLine(hook.StatusLineOptions{
		StateDir: state,
		Stdin:    env.Stdin,
		Stdout:   env.Stdout,
		Env:      env.Getenv,
	})
	return 0
}

// runHook always exits 0: a hook that fails must not disturb the agent's CLI. Failures go to the state dir's
// hook.log. With $AGENT_MESH_HOOK_CAPTURE set it also saves each payload there. For a PreToolUse with
// deny flags, a denial JSON is printed to stdout; otherwise nothing is printed. --check-only checks those flags
// and reports nothing (a session that isn't the agent's).
func runHook(args []string) int {
	var kind, state, agent, event string
	var denyPaths []string
	var denyPublish, checkOnly bool
	rest := args
	if len(rest) > 0 {
		kind, rest = rest[0], rest[1:]
	}
	for len(rest) > 0 {
		switch {
		case rest[0] == "--state" && len(rest) > 1:
			state, rest = rest[1], rest[2:]
		case rest[0] == "--agent" && len(rest) > 1:
			agent, rest = rest[1], rest[2:]
		case rest[0] == "--deny-path" && len(rest) > 1:
			denyPaths, rest = append(denyPaths, rest[1]), rest[2:]
		case rest[0] == "--deny-publish":
			denyPublish, rest = true, rest[1:]
		case rest[0] == "--check-only":
			checkOnly, rest = true, rest[1:]
		default:
			event, rest = rest[0], rest[1:]
		}
	}
	if state == "" || agent == "" || kind == "" || event == "" {
		fmt.Fprintln(os.Stderr, "mesh hook: need <kind> --state <dir> --agent <id> <event>")
		return 0
	}
	cwd, _ := os.Getwd()
	if err := hook.Run(hook.Options{StateDir: state, AgentID: agent, Kind: kind, Event: event,
		Stdin: os.Stdin, Stdout: os.Stdout, Cwd: cwd, DenyPaths: denyPaths, DenyPublish: denyPublish, CheckOnly: checkOnly,
		CaptureDir: os.Getenv("AGENT_MESH_HOOK_CAPTURE")}); err != nil {
		hook.Log(state, "agent %s %s %s: %v", agent, kind, event, err)
	}
	return 0
}
