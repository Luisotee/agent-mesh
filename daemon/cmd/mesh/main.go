// Command mesh is agent-mesh's CLI for agents and users (renamed `mesh` at the switchover). Its commands
// are the action registry's, derived from the schemas at run time (internal/cli); only `hook` (what a CLI's
// hooks run) and `version` are written by hand.
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
	os.Exit(cli.Main(os.Args[1:], env, map[string]func([]string) int{
		"hook":    runHook,
		"whoami":  func(a []string) int { return cli.Whoami(env, a) },
		"dir":     func(a []string) int { return cli.Dir(env, a) },
		"version": func([]string) int { fmt.Println("mesh", Version); return 0 },
	}))
}

// runHook always exits 0: a hook that fails must not disturb the agent's CLI. Failures go to the state dir's
// hook.log. With $AGENT_MESH_HOOK_CAPTURE set it also saves each payload there. For agy PreToolUse with
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
