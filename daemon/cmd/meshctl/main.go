// Command meshctl is agent-mesh's CLI for agents and users (renamed `mesh` at the switchover). This build has
// only its hand-written commands: `hook`, which a CLI's hooks run, and `version`.
package main

import (
	"fmt"
	"os"

	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
)

// Version is meshctl's version.
const Version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: meshctl hook <kind> --state <dir> --agent <id> <event> | meshctl version")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("meshctl", Version)
		return 0
	case "hook":
		return runHook(args[1:])
	}
	fmt.Fprintf(os.Stderr, "meshctl: unknown command %q\n", args[0])
	return 2
}

// runHook always exits 0: a hook that fails must not disturb the agent's CLI. Failures go to the state dir's
// hook.log.
func runHook(args []string) int {
	var kind, state, agent, event string
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
		default:
			event, rest = rest[0], rest[1:]
		}
	}
	if state == "" || agent == "" || kind == "" || event == "" {
		fmt.Fprintln(os.Stderr, "meshctl hook: need <kind> --state <dir> --agent <id> <event>")
		return 0
	}
	if err := hook.Run(hook.Options{StateDir: state, AgentID: agent, Kind: kind, Event: event, Stdin: os.Stdin}); err != nil {
		hook.Log(state, "agent %s %s %s: %v", agent, kind, event, err)
	}
	return 0
}
