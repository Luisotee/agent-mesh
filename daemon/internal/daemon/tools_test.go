package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestToolsListAndAddReopensSameConversationWithToolsSkillsPluginsMCPAndSnapshotOff(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright", "sentry")
	claudeDir := filepath.Join(w.home, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"enabledPlugins":{"feature-dev@official":true,"code-simplifier@official":true}}`), 0o600)

	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "ui", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "inspect"})
	other := w.spawnUnder(t, asLead, "other", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "work"})
	asWorker := w.as(w.token(t, worker.Id))

	listed, err := asWorker.ListTools(ctx, connect.NewRequest(&v1.ListToolsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !listed.Msg.Lean || !slices.Contains(listed.Msg.Tools, "Workflow") || !slices.Contains(listed.Msg.Skills, "delegate") ||
		!slices.Contains(listed.Msg.Plugins, "feature-dev") || listed.Msg.ClaudeaiSkills != "anthropic-skills" ||
		!slices.Contains(listed.Msg.McpCut, "sentry") || listed.Msg.ClaudeaiConnectors != "claude.ai" {
		t.Fatalf("unexpected ListTools: %+v", listed.Msg)
	}

	// Workers cannot inspect other agents across runs or add tools to other agents.
	if _, err := asWorker.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Agent: other.Id, Names: []string{"Workflow"}})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("worker adding tools to another agent: %v", err)
	}

	addRes, err := asWorker.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{
		Names: []string{"Workflow", "delegate", "feature-dev@official", "anthropic-skills", "sentry"},
		Next:  "resume audit",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if addRes.Msg.Reopen == nil || addRes.Msg.Reopen.Why != "tools added" {
		t.Fatalf("expected tools reopen, got %+v", addRes.Msg)
	}
	back := w.waitReopened(t, worker.Id, worker.TerminalId)
	if back.SessionId != worker.SessionId || back.Model != worker.Model || back.Effort != worker.Effort {
		t.Fatalf("tools add changed session/model/effort: before=%+v after=%+v", worker, back)
	}
	if !back.ToolsAdded || !slices.Contains(back.Mcp, "sentry") || slices.Contains(back.AddedTools, "sentry") {
		t.Fatalf("agent fields after add: tools_added=%v mcp=%v added_tools=%v", back.ToolsAdded, back.Mcp, back.AddedTools)
	}

	launches := w.launches(t, back.SessionId)
	last := launches[len(launches)-1]
	joined := strings.Join(last, " ")
	deny, mcpPath, strict := leanArgs(last)
	if slices.Contains(deny, "Workflow") {
		t.Errorf("Workflow remained denied after tools add: %v", deny)
	}
	if !strings.Contains(joined, "--system-prompt-snapshot off") {
		t.Errorf("missing --system-prompt-snapshot off after adding built-in tool: %v", last)
	}
	if !strict || !slices.Equal(mcpFileServers(t, mcpPath), []string{"context7", "sentry"}) {
		t.Errorf("expected context7 + sentry in mcp.json: strict=%v servers=%v", strict, mcpFileServers(t, mcpPath))
	}
	settingsArg := flagArg(last, "--settings")
	var settingsMap map[string]any
	if err := json.Unmarshal([]byte(settingsArg), &settingsMap); err != nil {
		t.Fatalf("decode --settings: %v", err)
	}
	if skillsMap, _ := settingsMap["skillOverrides"].(map[string]any); skillsMap != nil {
		if _, stillHidden := skillsMap["delegate"]; stillHidden {
			t.Errorf("delegate still hidden in skillOverrides: %v", skillsMap)
		}
		if _, stillHidden := skillsMap["agent-mesh-daemon:delegate"]; stillHidden {
			t.Errorf("namespaced skill still hidden in skillOverrides: %v", skillsMap)
		}
	}
	if _, stillDisabled := settingsMap["syncClaudeAiSkills"]; stillDisabled {
		t.Errorf("syncClaudeAiSkills remained disabled: %v", settingsMap)
	}
	if pluginsMap, _ := settingsMap["enabledPlugins"].(map[string]any); pluginsMap != nil {
		if _, off := pluginsMap["feature-dev@official"]; off {
			t.Errorf("feature-dev@official remained disabled: %v", pluginsMap)
		}
	}

	// An INFO message instructs the agent to carry on.
	waitFor(t, "tools reopen info delivered", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Inbox(worker.Id, true)
		for _, m := range msgs {
			if m.Type == v1.MessageType_MESSAGE_TYPE_INFO && strings.Contains(m.Body, "Carry on: resume audit") && m.DeliveredAt != nil {
				return true
			}
		}
		return false
	})

	// Adding claude.ai drops --strict-mcp-config while keeping --system-prompt-snapshot off (when other holds no exclusive).
	asWorker = w.as(w.token(t, worker.Id)) // a reopen mints a new token
	if _, err := asWorker.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"claude.ai"}})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, back.Id, back.TerminalId)
	launches = w.launches(t, back.SessionId)
	last = launches[len(launches)-1]
	_, _, strict = leanArgs(last)
	if strict {
		t.Errorf("expected --strict-mcp-config dropped after claude.ai: %v", last)
	}
	if !strings.Contains(strings.Join(last, " "), "--system-prompt-snapshot off") {
		t.Errorf("expected --system-prompt-snapshot off kept after claude.ai: %v", last)
	}
}

func flagArg(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func TestExclusiveMCPLeaseAcrossSpawnAddToolsResumeSendAndRetire(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright", "sentry")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	// First spawn takes exclusive playwright.
	res1, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Repo: gitRepo(t), Slug: "browser-1", Tier: "mid", Mcp: []string{"playwright"}, Task: "drive browser",
	}))
	if err != nil {
		t.Fatal(err)
	}
	w1 := w.waitState(res1.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// ListMCPServers shows holder and marks.
	srvRes, err := asLead.ListMCPServers(ctx, connect.NewRequest(&v1.ListMCPServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var pwInfo *v1.MCPServerInfo
	for _, s := range srvRes.Msg.Servers {
		if s.Name == "playwright" {
			pwInfo = s
		}
	}
	if pwInfo == nil || !pwInfo.Exclusive || pwInfo.Always || !strings.Contains(pwInfo.HeldBy, w1.Name) {
		t.Fatalf("unexpected playwright MCPServerInfo: %+v (all=%v)", pwInfo, srvRes.Msg.Servers)
	}

	// Second spawn requesting playwright is refused naming w1.
	if _, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Repo: gitRepo(t), Slug: "browser-2", Tier: "mid", Mcp: []string{"playwright"}, Task: "also browser",
	})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), w1.Name) {
		t.Fatalf("expected FailedPrecondition naming %s, got %v", w1.Name, err)
	}

	// Second worker without playwright cannot add playwright or claude.ai while w1 holds it.
	w2 := w.spawnUnder(t, asLead, "plain", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "code"})
	asW2 := w.as(w.token(t, w2.Id))
	if _, err := asW2.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"playwright"}})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), w1.Name) {
		t.Fatalf("AddTools playwright while held: %v", err)
	}
	if _, err := asW2.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"claude.ai"}})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), w1.Name) {
		t.Fatalf("AddTools claude.ai while exclusive held: %v", err)
	}

	// Retire w1: releases lease and removes mcp.json.
	mcpPath := filepath.Join(w.state, "agents", w1.Id, "mcp.json")
	if _, err := os.Stat(mcpPath); err != nil {
		t.Fatalf("expected %s before retire: %v", mcpPath, err)
	}
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Agent: w1.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mcpPath); !os.IsNotExist(err) {
		t.Errorf("expected mcp.json deleted on retire, stat err=%v", err)
	}

	// Now w2 can take playwright via AddTools.
	if _, err := asW2.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"playwright"}})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, w2.Id, w2.TerminalId)

	// Explicit ResumeAgent --mcp playwright for stopped w1 fails naming w2.
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Agent: w1.Id, Task: "more", Mcp: []string{"playwright"},
	})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), w2.Name) {
		t.Fatalf("explicit resume --mcp playwright while held: %v", err)
	}

	// Implicit ResumeAgent drops held playwright with a note rather than failing.
	resum, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Agent: w1.Id, Task: "more without browser",
	}))
	if err != nil {
		t.Fatalf("implicit resume failed: %v", err)
	}
	if slices.Contains(resum.Msg.Agent.Mcp, "playwright") {
		t.Errorf("expected playwright dropped on implicit resume: %v", resum.Msg.Agent.Mcp)
	}
	if !strings.Contains(strings.Join(resum.Msg.Notes, " "), "playwright") || !strings.Contains(strings.Join(resum.Msg.Notes, " "), w2.Name) {
		t.Errorf("expected resume note naming dropped server & holder, got %v", resum.Msg.Notes)
	}
	w.waitReopened(t, w1.Id, "")

	// Retire w1 again after explicit Ask/Retire cycle: test SendMessage auto-resume drops held exclusive MCPs.
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Agent: w2.Id})); err != nil {
		t.Fatal(err)
	}
	// Let w1 take playwright back cleanly via AddTools, wait for reopen, then send a message to retired w2 (which still had playwright!).
	asW1 := w.as(w.token(t, w1.Id))
	if _, err := asW1.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"playwright"}})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, w1.Id, w.agent(w1.Id).TerminalId)

	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		To: w2.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task: carry on",
	})); err != nil {
		t.Fatalf("SendMessage auto-resume failed: %v", err)
	}
	w2Back := w.waitReopened(t, w2.Id, "")
	if slices.Contains(w2Back.Mcp, "playwright") {
		t.Errorf("expected SendMessage auto-resume to drop held playwright: %v", w2Back.Mcp)
	}
}

// One exclusive server, three claimants at once (two spawns, one AddTools): the single writer lets exactly one win.
func TestExclusiveMCPLeaseConcurrentClaimantsExactlyOneWins(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright", "sentry")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	plain := w.spawnUnder(t, asLead, "plain", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "code"})
	asPlain := w.as(w.token(t, plain.Id))
	repoA, repoB := gitRepo(t), gitRepo(t)

	errs := make(chan error, 3)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repoA, Slug: "b1", Tier: "mid", Mcp: []string{"playwright"}, Task: "browse"}))
		errs <- err
	}()
	go func() {
		<-start
		_, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repoB, Slug: "b2", Tier: "mid", Mcp: []string{"playwright"}, Task: "browse"}))
		errs <- err
	}()
	go func() {
		<-start
		_, err := asPlain.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"playwright"}}))
		errs <- err
	}()
	close(start)
	won, refused := 0, 0
	for i := 0; i < 3; i++ {
		err := <-errs
		switch {
		case err == nil:
			won++
		case code(err) == connect.CodeFailedPrecondition && strings.Contains(err.Error(), "one agent at a time"):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 || refused != 2 {
		t.Fatalf("exactly one claimant should win playwright: won=%d refused=%d", won, refused)
	}
}

// A server a worker's launch loads without being named (its repo's own, under `claude.mcp` `repo`) is held too:
// another agent may not take the shared one.
func TestAnExclusiveServerALaunchLoadsUnnamedIsHeld(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	// This repo's own `.mcp.json` approves playwright: every agent in it loads it, named or not.
	repo := gitRepo(t)
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(repo, ".mcp.json"), []byte(`{"mcpServers":{"playwright":{"type":"stdio","command":"playwright-cmd"}}}`), 0o600)
	os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"enableAllProjectMcpServers":true}`), 0o600)
	holder := w.spawnUnder(t, asLead, "ui", &v1.SpawnAgentRequest{Repo: repo, Tier: "mid", Task: "work"})
	if got := mcpFileServers(t, filepath.Join(w.state, "agents", holder.Id, "mcp.json")); !slices.Contains(got, "playwright") {
		t.Fatalf("the repo's server was not loaded: %v", got)
	}

	if _, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Repo: gitRepo(t), Slug: "other", Tier: "mid", Mcp: []string{"playwright"}, Task: "also browser",
	})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), holder.Name) {
		t.Fatalf("want FailedPrecondition naming %s, got %v", holder.Name, err)
	}
	srv, err := asLead.ListMCPServers(ctx, connect.NewRequest(&v1.ListMCPServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srv.Msg.Servers {
		if s.Name == "playwright" && !strings.Contains(s.HeldBy, holder.Name) {
			t.Errorf("playwright HeldBy = %q, want %s", s.HeldBy, holder.Name)
		}
	}
}

// A second agent in a repo whose own server another agent holds does not load it: the server stays out of its MCP
// file, and it is told so.
func TestAnAgentWhoseRepoLoadsAHeldExclusiveServerLeavesItOut(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright")
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	repo := gitRepo(t)
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(repo, ".mcp.json"), []byte(`{"mcpServers":{"playwright":{"type":"stdio","command":"playwright-cmd"}}}`), 0o600)
	os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"enableAllProjectMcpServers":true}`), 0o600)
	holder := w.spawnUnder(t, asLead, "ui", &v1.SpawnAgentRequest{Repo: repo, Tier: "mid", Task: "work"})
	second := w.spawnUnder(t, asLead, "docs", &v1.SpawnAgentRequest{Repo: repo, Tier: "mid", Task: "more work"})
	if got := mcpFileServers(t, filepath.Join(w.state, "agents", second.Id, "mcp.json")); slices.Contains(got, "playwright") {
		t.Errorf("the second agent loads the held server: %v", got)
	}
	msgs, err := w.d.Store().Read(context.Background()).Inbox(second.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	told := false
	for _, m := range msgs {
		told = told || (strings.Contains(m.Body, "playwright") && strings.Contains(m.Body, holder.Name))
	}
	if !told {
		t.Errorf("the second agent was not told the held server stayed out: %v", msgs)
	}
}

// A worker adding its own tools tells its parent it is waiting (PROTOCOL.md rule 10): a `wait`, not a report.
func TestAWorkersToolsAddTellsItsParentItIsWaiting(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "sentry")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "ui", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "inspect"})
	asWorker := w.as(w.token(t, worker.Id))
	waits := func() (n int) {
		msgs, err := w.d.Store().Read(ctx).Inbox(lead.Id, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.Type == v1.MessageType_MESSAGE_TYPE_WAIT && m.From.GetId() == worker.Id && strings.Contains(m.Body, "sentry") {
				n++
			}
		}
		return n
	}
	if _, err := asWorker.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"sentry"}})); err != nil {
		t.Fatal(err)
	}
	if n := waits(); n != 1 {
		t.Fatalf("waits to the parent after its own add: %d, want 1", n)
	}
	w.waitReopened(t, worker.Id, worker.TerminalId)

	// A root adding a child's tools is its own doing: the child tells nobody.
	child := w.spawnUnder(t, asLead, "api", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "work"})
	if _, err := asLead.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Agent: child.Id, Names: []string{"sentry"}})); err != nil {
		t.Fatal(err)
	}
	if n := waits(); n != 1 {
		t.Fatalf("waits after the root's add: %d, want still 1", n)
	}
}

// The holder of an exclusive server is named only within the asker's run; another run's agent is "another run".
func TestTheHolderOfAnExclusiveServerIsNamedOnlyWithinTheCallersRun(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright")
	ctx := context.Background()
	run1 := startRun(t, w.device.Mesh, "")
	_, lead1 := w.root(t, run1.Id)
	holder := w.spawnUnder(t, lead1, "browser", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Mcp: []string{"playwright"}, Task: "drive"})

	run2 := startRun(t, w.device.Mesh, "")
	_, lead2 := w.root(t, run2.Id)
	stranger := w.spawnUnder(t, lead2, "plain", &v1.SpawnAgentRequest{Repo: gitRepo(t), Tier: "mid", Task: "code"})
	asStranger := w.as(w.token(t, stranger.Id))

	_, err := asStranger.AddTools(ctx, connect.NewRequest(&v1.AddToolsRequest{Names: []string{"playwright"}}))
	if code(err) != connect.CodeFailedPrecondition || strings.Contains(err.Error(), holder.Name) || strings.Contains(err.Error(), run1.Id) ||
		!strings.Contains(err.Error(), "another run") {
		t.Errorf("a stranger's refusal names the holder or not another run: %v", err)
	}
	srv, err := asStranger.ListMCPServers(ctx, connect.NewRequest(&v1.ListMCPServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srv.Msg.Servers {
		if s.Name == "playwright" && (strings.Contains(s.HeldBy, holder.Name) || !strings.Contains(s.HeldBy, "another run")) {
			t.Errorf("a stranger sees HeldBy = %q", s.HeldBy)
		}
	}
	// Within its own run, and for the user's device, the holder is named.
	srv, err = lead1.ListMCPServers(ctx, connect.NewRequest(&v1.ListMCPServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srv.Msg.Servers {
		if s.Name == "playwright" && !strings.Contains(s.HeldBy, holder.Name) {
			t.Errorf("the holder's own run sees HeldBy = %q", s.HeldBy)
		}
	}
	srv, err = w.device.Mesh.ListMCPServers(ctx, connect.NewRequest(&v1.ListMCPServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srv.Msg.Servers {
		if s.Name == "playwright" && !strings.Contains(s.HeldBy, holder.Name) {
			t.Errorf("the device sees HeldBy = %q", s.HeldBy)
		}
	}
}
