package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
)

// A real `initialize` answer's shape (Claude Code 2.1.332, cut): the user's, a project's and a plugin's commands, a
// built-in, an MCP prompt's display name and an internal command.
const initAnswer = `{"type":"system","subtype":"hook_started"}
{"type":"control_response","response":{"subtype":"success","request_id":"mesh-commands","response":{"commands":[` +
	`{"name":"compact","description":"Free up context by summarizing the conversation so far","argumentHint":"<optional custom summarization instructions>"},` +
	`{"name":"config","description":"Set a setting by key","argumentHint":"key=value"},` +
	`{"name":"check-plan","description":"Checks a plan (user)","argumentHint":"[plan file]"},` +
	`{"name":"deploy","description":"Ship it (project)","argumentHint":"[env]"},` +
	`{"name":"agent-mesh:delegate","description":"(agent-mesh) hands work to agents","argumentHint":""},` +
	`{"name":"usage","description":"Show session cost","argumentHint":""},` +
	`{"name":"debug","description":"Enable debug logging","argumentHint":"[issue]"},` +
	`{"name":"srv:prompt (MCP)","description":"an MCP prompt","argumentHint":""},` +
	`{"name":"agents","description":"(removed) Ask Claude to create subagents","argumentHint":""},` +
	`{"name":"__remote-workflow","description":"internal","argumentHint":""}]}}}
`

func TestParseInitializeReadsTheCommandsAndTellsWhereEachComesFrom(t *testing.T) {
	got := map[string]*v1.AgentCommand{}
	for _, c := range parseInitialize([]byte(initAnswer)) {
		got[c.Name] = c
	}
	if len(got) != 7 || got["srv:prompt (MCP)"] != nil || got["__remote-workflow"] != nil {
		t.Fatalf("an MCP display name or an internal command listed: %v", got)
	}
	for name, want := range map[string][2]string{
		"compact": {"builtin", "Free up context by summarizing the conversation so far"}, "config": {"builtin", "Set a setting by key"},
		"check-plan": {"user", "Checks a plan"}, "deploy": {"project", "Ship it"},
		"agent-mesh:delegate": {"plugin", "hands work to agents"}, "debug": {"skill", "Enable debug logging"},
	} {
		if c := got[name]; c == nil || c.Source != want[0] || c.Description != want[1] {
			t.Errorf("%s: %v, want source %s and %q", name, c, want[0], want[1])
		}
	}
	if got["deploy"].ArgumentHint != "[env]" {
		t.Errorf("the argument hint: %q", got["deploy"].ArgumentHint)
	}
	if parseInitialize([]byte("not json\n")) != nil {
		t.Error("a garbled answer gave commands")
	}
}

// The probe must leave no trace: no session file, no hook (the user's, the repo's or ours), no MCP server from a
// repo's `.mcp.json`, and for a read-only agent the user's settings only.
func TestTheCommandProbeLeavesNoTrace(t *testing.T) {
	argv := commandsArgv("claude", "/share", false, `{"disableAllHooks":true}`)
	if !hasPair(argv, "--settings", `{"disableAllHooks":true}`) {
		t.Errorf("hooks not disabled: %v", argv)
	}
	for _, flag := range []string{"--no-session-persistence", "--strict-mcp-config", "-p"} {
		if !contains(argv, flag) {
			t.Errorf("%s missing: %v", flag, argv)
		}
	}
	if !hasPair(argv, "--plugin-dir", "/share") || !hasPair(argv, "--input-format", "stream-json") || contains(argv, "--setting-sources") {
		t.Errorf("a worker's probe: %v", argv)
	}
	if ro := commandsArgv("claude", "", true, `{"disableAllHooks":true}`); !hasPair(ro, "--setting-sources", "user") || contains(ro, "--plugin-dir") {
		t.Errorf("a read-only agent's probe: %v", ro)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hasPair(xs []string, k, v string) bool {
	for i := 0; i+1 < len(xs); i++ {
		if xs[i] == k && xs[i+1] == v {
			return true
		}
	}
	return false
}

// fakeProbeClaude is a shell script that answers `initialize` and counts its runs and what it was started with.
func fakeProbeClaude(t *testing.T, answer string, fail bool) (bin, runs string) {
	t.Helper()
	dir := t.TempDir()
	runs = filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho \"$@\" >> '" + runs + "'\nenv | grep '^AGENT_MESH_\\|^CLAUDECODE' >> '" + runs + ".env'\ncat >/dev/null\n"
	if fail {
		script += "echo boom >&2\nexit 1\n"
	} else {
		script += "sleep 0.2\ncat <<'EOF'\n" + answer + "EOF\n"
	}
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, runs
}

func runCount(t *testing.T, runs string) int {
	b, err := os.ReadFile(runs)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func list(t *testing.T, a *Adapter, req *adapterv1.ListCommandsRequest) map[string]*v1.AgentCommand {
	t.Helper()
	res, err := a.ListCommands(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*v1.AgentCommand{}
	for _, c := range res.Msg.Commands {
		if out[c.Name] != nil {
			t.Errorf("%s listed twice", c.Name)
		}
		out[c.Name] = c
	}
	return out
}

func TestListCommandsMergesTheProbeWithTheTerminalOnesAndKeepsTheListForAWhile(t *testing.T) {
	bin, runs := fakeProbeClaude(t, initAnswer, false)
	a := New()
	a.CommandsTTL = 300 * time.Millisecond
	req := &adapterv1.ListCommandsRequest{Cwd: t.TempDir(), Binary: bin,
		Env: map[string]string{"HOME": t.TempDir(), "PATH": os.Getenv("PATH"), "AGENT_MESH_TOKEN": "secret", "AGENT_MESH_AGENT": "a1", "AGENT_MESH_SHARE": "/nonexistent", "CLAUDECODE": "1"}}
	// Several at once (a popup opened in two windows): one probe.
	var wg sync.WaitGroup
	results := make([]map[string]*v1.AgentCommand, 4)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = list(t, a, req) }()
	}
	wg.Wait()
	for _, got := range results {
		// The probe's own: a built-in made a dialog by the table, a project's and a plugin's.
		if c := got["config"]; c == nil || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_BARE || c.Description != "Open settings" {
			t.Errorf("/config: %v", c)
		}
		if c := got["deploy"]; c == nil || c.Source != "project" || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_UNSPECIFIED {
			t.Errorf("/deploy: %v", c)
		}
		if c := got["agent-mesh:delegate"]; c == nil || c.Source != "plugin" {
			t.Errorf("/agent-mesh:delegate: %v", c)
		}
		// The terminal's own, which the probe doesn't have.
		if c := got["resume"]; c == nil || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_ALWAYS || c.Source != "builtin" {
			t.Errorf("/resume: %v", c)
		}
		if c := got["exit"]; c == nil || !c.Danger || len(c.Aliases) != 1 || c.Aliases[0] != "quit" {
			t.Errorf("/exit: %v", c)
		}
		// A cached list is not changed by the overrides of an earlier call (the aliases don't pile up).
		if c := got["usage"]; c == nil || len(c.Aliases) != 2 || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_ALWAYS {
			t.Errorf("/usage: %v", c)
		}
		if got["compact"] == nil || got["compact"].Dialog != v1.CommandDialog_COMMAND_DIALOG_UNSPECIFIED {
			t.Errorf("/compact: %v", got["compact"])
		}
	}
	if n := runCount(t, runs); n != 1 {
		t.Fatalf("%d probes for four simultaneous lists", n)
	}
	list(t, a, req)
	if n := runCount(t, runs); n != 1 {
		t.Fatalf("a second list probed again within the ttl: %d", n)
	}
	time.Sleep(350 * time.Millisecond)
	list(t, a, req)
	if n := runCount(t, runs); n != 2 {
		t.Fatalf("after the ttl: %d probes, want 2", n)
	}
	// Another worktree has its own list, and a read-only agent's differs from a worker's in the same one.
	other := proto.CloneOf(req)
	other.Cwd = t.TempDir()
	list(t, a, other)
	ro := proto.CloneOf(req)
	ro.ReadOnly = true
	list(t, a, ro)
	if n := runCount(t, runs); n != 4 {
		t.Fatalf("another worktree and a read-only agent: %d probes, want 4", n)
	}
	// The probe is nobody's agent: no identity, no token, no session variables.
	env, _ := os.ReadFile(runs + ".env")
	if strings.Contains(string(env), "AGENT_MESH_TOKEN") || strings.Contains(string(env), "AGENT_MESH_AGENT") || strings.Contains(string(env), "CLAUDECODE") {
		t.Errorf("the probe got the agent's identity: %s", env)
	}
	argv, _ := os.ReadFile(runs)
	for _, line := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if !strings.Contains(line, "--no-session-persistence") || !strings.Contains(line, "disableAllHooks") || !strings.Contains(line, "--strict-mcp-config") {
			t.Errorf("a probe that leaves traces: %s", line)
		}
	}
}

func TestAFailingProbeStillOffersTheTerminalCommandsAndIsNotRetriedAtOnce(t *testing.T) {
	bin, runs := fakeProbeClaude(t, "", true)
	a := New()
	req := &adapterv1.ListCommandsRequest{Cwd: t.TempDir(), Binary: bin, Env: map[string]string{"PATH": os.Getenv("PATH")}}
	got := list(t, a, req)
	if got["resume"] == nil || got["compact"] != nil {
		t.Errorf("without the probe: %d commands, /resume %v, /compact %v", len(got), got["resume"], got["compact"])
	}
	list(t, a, req)
	if n := runCount(t, runs); n != 1 {
		t.Errorf("a failed probe retried at every list: %d", n)
	}
}

func TestTheTerminalTableHasNoDuplicatesAndNoCommandTheProbeOwns(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range tuiCommands {
		if c.Name == "" || c.Desc == "" || seen[c.Name] {
			t.Errorf("bad or repeated entry %q", c.Name)
		}
		seen[c.Name] = true
		for _, al := range c.Aliases {
			if seen[al] {
				t.Errorf("alias %q repeats a name", al)
			}
			seen[al] = true
		}
	}
	for name := range dialogs {
		if seen[name] {
			t.Errorf("%s is in both tables", name)
		}
	}
	if len(adapter.Commands(tuiCommands)) != len(tuiCommands) {
		t.Error("Commands drops entries")
	}
}

func TestNoMoreThanTwoProbesRunAtOnce(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "live")
	// Each fake probe records how many are alive when it starts (the others' lock files), then lives 300 ms.
	script := "#!/bin/sh\nmkdir -p '" + dir + "/up'\ntouch \"" + dir + "/up/$$\"\nls '" + dir + "/up' | wc -l >> '" + log + "'\ncat >/dev/null\nsleep 0.3\nrm -f \"" + dir + "/up/$$\"\ncat <<'EOF'\n" + initAnswer + "EOF\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{} // a literal: the semaphore is made on first use
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			list(t, a, &adapterv1.ListCommandsRequest{Cwd: t.TempDir(), Binary: bin, Env: map[string]string{"PATH": os.Getenv("PATH")}})
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(log)
	lines := strings.Fields(string(b))
	if len(lines) != 6 {
		t.Fatalf("%d probes ran, want 6: %q", len(lines), b)
	}
	for _, l := range lines {
		if l != "1" && l != "2" {
			t.Errorf("%s probes alive at once, want at most 2", l)
		}
	}
}

func TestAProbeIsNotKilledWhenTheCallerThatStartedItLeaves(t *testing.T) {
	bin, runs := fakeProbeClaude(t, initAnswer, false)
	a := New()
	req := &adapterv1.ListCommandsRequest{Cwd: t.TempDir(), Binary: bin, Env: map[string]string{"PATH": os.Getenv("PATH")}}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	go func() {
		defer close(first)
		a.ListCommands(ctx, connect.NewRequest(proto.CloneOf(req)))
	}()
	time.Sleep(60 * time.Millisecond) // the probe is running (it lives 200 ms)
	cancel()
	// A second caller, which waited on the same probe, still gets the commands.
	got := list(t, a, req)
	<-first
	if got["deploy"] == nil {
		t.Errorf("the probe died with its first caller: %d commands", len(got))
	}
	if n := runCount(t, runs); n != 1 {
		t.Errorf("%d probes", n)
	}
}

func TestTheProbeHasWhatTheLeanLaunchLeavesOut(t *testing.T) {
	m := &adapterv1.ListCommandsRequest{Env: map[string]string{"HOME": t.TempDir()},
		Lean: &adapterv1.LeanLaunch{OffPlugins: []string{"pr-review-toolkit"}, HideClaudeaiSkills: true, HiddenSkills: []string{"loop"}}}
	home := m.Env["HOME"]
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"pr-review-toolkit@mp":true,"feature-dev@mp":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(probeSettings(m)), &got); err != nil {
		t.Fatal(err)
	}
	plugins, _ := got["enabledPlugins"].(map[string]any)
	if got["disableAllHooks"] != true || got["syncClaudeAiSkills"] != false || plugins["pr-review-toolkit@mp"] != false || plugins["feature-dev@mp"] != nil {
		t.Errorf("the probe's settings: %v", got)
	}
	// A skill that is only hidden from the model can still be typed: it stays in the list.
	if got["skillOverrides"] != nil {
		t.Errorf("hidden skills left out of the list: %v", got["skillOverrides"])
	}
	// Another lean policy is another list: it is not served from the first one's cache.
	if probeSettings(m) == probeSettings(&adapterv1.ListCommandsRequest{Env: m.Env}) {
		t.Error("the same settings with and without a lean policy")
	}
	var plain map[string]any
	json.Unmarshal([]byte(probeSettings(&adapterv1.ListCommandsRequest{})), &plain)
	if len(plain) != 1 || plain["disableAllHooks"] != true {
		t.Errorf("no lean, no share: %v", plain)
	}
}

func TestAResponseToAnotherRequestOrAnErrorIsNotTheCommandList(t *testing.T) {
	other := strings.Replace(initAnswer, `"request_id":"mesh-commands"`, `"request_id":"other"`, 1)
	if parseInitialize([]byte(other)) != nil {
		t.Error("another request's answer taken")
	}
	failed := strings.Replace(initAnswer, `"subtype":"success"`, `"subtype":"error"`, 1)
	if parseInitialize([]byte(failed)) != nil {
		t.Error("an error answer taken")
	}
}

func TestACallerThatLeavesWhileWaitingForAProbeSlotGivesItUpAndDoesntPoisonTheEntry(t *testing.T) {
	bin, runs := fakeProbeClaude(t, initAnswer, false)
	a := New()
	// Both slots are taken.
	a.slots() <- struct{}{}
	a.slots() <- struct{}{}
	req := &adapterv1.ListCommandsRequest{Cwd: t.TempDir(), Binary: bin, Env: map[string]string{"PATH": os.Getenv("PATH")}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := a.ListCommands(ctx, connect.NewRequest(proto.CloneOf(req)))
	if err != nil || len(res.Msg.Commands) == 0 {
		t.Fatalf("the table's commands are still offered: %v %v", res, err)
	}
	if n := runCount(t, runs); n != 0 {
		t.Fatalf("a probe ran without a slot: %d", n)
	}
	// A slot frees: the next caller probes (the entry of the one that left was not kept as a failure).
	<-a.slots()
	if got := list(t, a, req); got["deploy"] == nil {
		t.Errorf("the next caller got no probed commands: %d", len(got))
	}
}

func TestWithNoDirectoryTheTablesAnswerAndNothingIsProbed(t *testing.T) {
	bin, runs := fakeProbeClaude(t, initAnswer, false)
	got := list(t, New(), &adapterv1.ListCommandsRequest{Binary: bin, Env: map[string]string{"PATH": os.Getenv("PATH")}})
	if got["resume"] == nil || got["deploy"] != nil {
		t.Errorf("%d commands, /resume %v, /deploy %v", len(got), got["resume"], got["deploy"])
	}
	if n := runCount(t, runs); n != 0 {
		t.Errorf("a probe ran with no directory: %d", n)
	}
}
