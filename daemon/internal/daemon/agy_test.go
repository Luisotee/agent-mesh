package daemon_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	_ "modernc.org/sqlite"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func (w *reopenWorld) agyTurn(t *testing.T, a *v1.Agent, line string) *v1.Agent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	es, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: a.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	recv(t, es) // snapshot
	w.typeInto(a, line+"\r")
	var working bool
	for {
		ev := recv(t, es).GetEvent()
		if ch := ev.GetAgentStateChanged(); ch != nil && ev.AgentId == a.Id {
			if ch.To == v1.AgentState_AGENT_STATE_WORKING {
				working = true
			} else if working && ch.To == v1.AgentState_AGENT_STATE_IDLE {
				return w.agent(a.Id)
			}
		}
	}
}

func (w *reopenWorld) agyRoot(t *testing.T, run string) (*v1.Agent, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run, Repo: gitRepo(t), Slug: "agy-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
		Kind: "agy", Tier: "strong",
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	return res.Msg.Agent, w.as(w.token(t, res.Msg.Agent.Id))
}

func TestAgyEveryReportReachesTheRootAndRingsItsDoorbell(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.agyRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "reporter", &v1.SpawnAgentRequest{Kind: "agy"})

	// A wait is not a report: stored for the root to read, it rings no doorbell.
	wait := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_WAIT)
	if wait.ToId != lead.Id {
		t.Fatalf("a wait went to %s, not the root", wait.ToId)
	}
	time.Sleep(time.Second)
	if screen := w.screen(t, lead); contains(screen, "📬") {
		t.Fatalf("the agy root's screen shows a doorbell for a wait:\n%s", screen)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED, v1.MessageType_MESSAGE_TYPE_INFO} {
		m := w.sendToParent(t, worker, typ)
		if m.ToId != lead.Id {
			t.Fatalf("a %s went to %s, not the root", typ, m.ToId)
		}
		waitFor(t, "the agy root's doorbell for a "+typ.String(), func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
		w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
}

func TestAgyReportSentWhileRootIsWorkingRingsAtNextIdle(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.agyRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "early", &v1.SpawnAgentRequest{Kind: "agy"})

	w.typeInto(lead, "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	m := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	time.Sleep(1500 * time.Millisecond)
	if !w.deliveredAt(t, lead.Id, m.Id).IsZero() {
		t.Fatal("a doorbell was typed into the agy root's working turn")
	}
	if peek, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true})); len(peek.Msg.Messages) != 1 {
		t.Fatalf("the report is not in the agy root's inbox: %v", peek)
	}
	w.typeInto(lead, "go on\r")
	waitFor(t, "the doorbell at the agy root's next idle", func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestAgyPlanFromWorkerResumedIntoPlanningReachesTheRoot(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.agyRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "again", &v1.SpawnAgentRequest{Kind: "agy"})

	// First task records agy's conversationId on the worker via SessionStart/PreInvocation.
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "first"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "worker session id recorded", func() bool {
		a := w.agent(worker.Id)
		return a.SessionId != "" && a.State == v1.AgentState_AGENT_STATE_IDLE
	})
	w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "first report rung", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Undelivered(lead.Id)
		return len(msgs) == 0
	})
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)

	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: worker.Id, Task: "plan second feature", Plan: true, Tier: "strong",
	})); err != nil {
		t.Fatal(err)
	}
	back := w.waitReopened(t, worker.Id, "")
	if back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !back.DenyEdits {
		t.Fatalf("resumed as %v", back)
	}
	plan := w.sendToParent(t, back, v1.MessageType_MESSAGE_TYPE_PLAN)
	waitFor(t, "the plan reaching the root", func() bool { return !w.deliveredAt(t, lead.Id, plan.Id).IsZero() })

	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.Model != "hydrogen" {
		t.Fatalf("approving agy plan: %v, %v", ap, err)
	}
	waitFor(t, "approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
	after := w.agent(worker.Id)
	if after.DenyEdits || after.SessionId != back.SessionId {
		t.Fatalf("expected DenyEdits lifted and same sessionId, got %v", after)
	}
}

func TestAgyPlaceholderNeverHoldsADoorbellAndTypingDoes(t *testing.T) {
	typingQuiet := 3 * time.Second
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Env["FAKEAGY_PLACEHOLDER"] = "1"
		c.TypingQuiet = typingQuiet
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.agyRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fresh", &v1.SpawnAgentRequest{Kind: "agy"})

	screen := w.screen(t, worker)
	if want := `Auto mode: edits & safe commands auto-approved`; !contains(screen, want) {
		t.Fatalf("fakeagy drew no placeholder: %q", screen)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_TASK, v1.MessageType_MESSAGE_TYPE_INFO} {
		sent := time.Now()
		res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: typ, Body: "hello"}))
		if err != nil {
			t.Fatal(err)
		}
		var rang time.Time
		waitFor(t, "doorbell over agy placeholder", func() bool {
			rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
			return !rang.IsZero()
		})
		if held := rang.Sub(sent); held > typingQuiet/2 {
			t.Errorf("a %s was held %s by agy placeholder", typ, held)
		}
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	user := w.attach(t, worker, true)
	defer user.Close()
	user.Input(ctx, []byte("x\x7f"))
	waitFor(t, "keystroke counted", func() bool { return w.lastClientInput(t, worker) })
	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "while typing"}))
	if err != nil {
		t.Fatal(err)
	}
	typed := w.lastClientInputAt(t, worker)
	var rang time.Time
	waitFor(t, "doorbell after user stopped typing", func() bool {
		rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
		return !rang.IsZero()
	})
	if held := rang.Sub(typed); held < typingQuiet-200*time.Millisecond {
		t.Errorf("the doorbell rang %s after a keystroke, inside the %s a typing user holds it", held, typingQuiet)
	}
}

func TestAgy429LimitSetsLimitedStateAndSwitchNextReopensSameConversation(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	worker, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "lim", Kind: "agy", Tier: "strong",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(worker.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	oldTerm := a.TerminalId

	// Trigger a normal turn first so conversationId is set, then hit /limit.
	a = w.agyTurn(t, a, "hi")
	convID := a.SessionId
	if convID == "" {
		t.Fatal("expected conversationId after first turn")
	}

	w.typeInto(a, "/limit\r")
	a = w.waitState(a.Id, v1.AgentState_AGENT_STATE_LIMITED)
	if !strings.Contains(a.StateDetail, "RESOURCE_EXHAUSTED") {
		t.Errorf("expected RESOURCE_EXHAUSTED detail, got %q", a.StateDetail)
	}

	// SwitchAgent --next moves from neon -> opus in the same conversation.
	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{
		Agent: a.Id, Next: true, Why: "429",
	}))
	if err != nil {
		t.Fatalf("switch --next from LIMITED: %v", err)
	}
	if sw.Msg.Reopen.Model != "opus" {
		t.Fatalf("expected next model opus, got %q", sw.Msg.Reopen.Model)
	}
	reopened := w.waitReopened(t, a.Id, oldTerm)
	if reopened.SessionId != convID || reopened.Model != "opus" {
		t.Fatalf("expected same session %s on opus, got %v", convID, reopened)
	}
	waitFor(t, "quota info message delivered", func() bool { return w.delivered(t, a.Id, sw.Msg.Message.Id) })
}

// A repo's tracked .agents/hooks.json: a reviewer (maybe of an untrusted PR head) never runs it, a worker on the
// repo keeps it in force beside the mesh's hooks. Neither dirties git status, and nothing the daemon writes hides
// a file in the user's own checkout.
func TestAgyTrackedRepoHooksNeutralizedForReviewersAndMergedForWorkers(t *testing.T) {
	for _, c := range []struct {
		role    v1.AgentRole
		repoRan bool
	}{{v1.AgentRole_AGENT_ROLE_REVIEWER, false}, {v1.AgentRole_AGENT_ROLE_WORKER, true}} {
		t.Run(c.role.String(), func(t *testing.T) {
			w := startReopen(t)
			ctx := context.Background()
			run := startRun(t, w.device.Mesh, "")

			repo := gitRepo(t)
			sentinel := filepath.Join(t.TempDir(), "repo-hook-ran")
			if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
				t.Fatal(err)
			}
			own := `{"repo":{"PreInvocation":[{"type":"command","command":"touch ` + sentinel + `"}]}}`
			if err := os.WriteFile(filepath.Join(repo, ".agents", "hooks.json"), []byte(own), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"add", ".agents/hooks.json"},
				{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "the repo's own hooks"},
			} {
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}

			sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
				RunId: run.Id, Repo: repo, Slug: "hooks", Kind: "agy", Tier: "light", Role: c.role,
			}))
			if err != nil {
				t.Fatal(err)
			}
			a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
			a = w.agyTurn(t, a, "hello") // the mesh's hooks ran: the turn was seen
			if _, err := os.Stat(sentinel); (err == nil) != c.repoRan {
				t.Fatalf("the repo's own hook ran: %v, want %v", err == nil, c.repoRan)
			}
			if out, err := exec.Command("git", "-C", a.Worktree, "status", "--porcelain").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
				t.Fatalf("worktree dirtied by .agents/hooks.json: %q (%v)", out, err)
			}
			stopped, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{RunId: run.Id}))
			if err != nil || len(stopped.Msg.Worktrees) != 1 || !stopped.Msg.Worktrees[0].WorktreeRemoved {
				t.Fatalf("StopRun worktree removal failed: %+v (%v)", stopped.Msg, err)
			}
		})
	}
}

// The untracked hooks file is ignored in the agent's worktree alone: the repo's shared info/exclude is left
// alone, so a .agents/hooks.json the user makes in their own checkout still shows.
func TestAgyHooksIgnoredOnlyInTheAgentsWorktree(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "ign", Kind: "agy", Tier: "light",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := os.Stat(filepath.Join(a.Worktree, ".agents", "hooks.json")); err != nil {
		t.Fatalf("no hooks file: %v", err)
	}
	if out, err := exec.Command("git", "-C", a.Worktree, "status", "--porcelain", "--untracked-files=all").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the agent's worktree shows the daemon's files: %q (%v)", out, err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agents", "hooks.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil || !strings.Contains(string(out), ".agents/hooks.json") {
		t.Fatalf("the user's own .agents/hooks.json is hidden in their checkout: %q (%v)", out, err)
	}
}

func TestAgyPlanningAndReadOnlyEnforcePreToolUseDenyAndLiftOnApprove(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "plan-agy", Kind: "agy", Tier: "strong", Plan: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// While planning, /edit and /push are blocked by PreToolUse.
	a = w.agyTurn(t, a, "/edit")
	if _, err := os.Stat(filepath.Join(a.Worktree, "blocked.txt")); err == nil {
		t.Fatal("write_file created blocked.txt while planning!")
	}
	a = w.agyTurn(t, a, "/push")
	if !strings.Contains(w.screen(t, a), "git push denied") {
		t.Fatalf("expected git push denied on screen: %q", w.screen(t, a))
	}

	// Under a root, send plan, approve it, and verify /edit succeeds afterwards.
	_, asLead := w.agyRoot(t, run.Id)
	w2 := w.spawnUnder(t, asLead, "exec-agy", &v1.SpawnAgentRequest{Kind: "agy", Plan: true, Tier: "strong"})
	w2 = w.agyTurn(t, w2, "/edit")
	if _, err := os.Stat(filepath.Join(w2.Worktree, "blocked.txt")); err == nil {
		t.Fatal("w2 created blocked.txt while planning")
	}
	oldTerm := w2.TerminalId
	w.sendToParent(t, w2, v1.MessageType_MESSAGE_TYPE_PLAN)
	es, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	recv(t, es) // snapshot
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: w2.Id, Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	w2 = w.waitReopened(t, w2.Id, oldTerm)
	// Wait for the "approved" doorbell turn to complete before typing /edit.
	var delivered, working bool
	for !(delivered && working && w.agent(w2.Id).State == v1.AgentState_AGENT_STATE_IDLE) {
		ev := recv(t, es).GetEvent()
		if md := ev.GetMessageDelivered(); md != nil && md.MessageId == ap.Msg.Message.Id {
			delivered = true
		}
		if ch := ev.GetAgentStateChanged(); ch != nil && ev.AgentId == w2.Id {
			if ch.To == v1.AgentState_AGENT_STATE_WORKING {
				working = true
			} else if delivered && working && ch.To == v1.AgentState_AGENT_STATE_IDLE {
				break
			}
		}
	}
	w2 = w.agyTurn(t, w2, "/edit")
	if _, err := os.Stat(filepath.Join(w2.Worktree, "blocked.txt")); err != nil {
		t.Fatalf("expected blocked.txt to be written after approve lifted deny: %v", err)
	}
}

func TestAgyLookupSessionFallbackWhenNoHookReportedYet(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "lookup", Kind: "agy", Tier: "light",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	// No turn ran, so SessionId is still empty. Seed agy's conversation_summaries.db for its worktree.
	agyDir := filepath.Join(w.home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(agyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(agyDir, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		preview TEXT,
		workspace_uris TEXT,
		nesting_depth INTEGER,
		step_count INTEGER,
		last_modified_time INTEGER
	);
	INSERT INTO conversation_summaries VALUES ('conv-from-db', 'hi', '["file://` + a.Worktree + `"]', 0, 1, 100);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{
		Agent: a.Id, Tier: "mid", Why: "lookup fallback",
	}))
	if err != nil {
		t.Fatalf("SwitchAgent with LookupSession fallback failed: %v", err)
	}
	reopened := w.waitReopened(t, a.Id, a.TerminalId)
	if reopened.SessionId != "conv-from-db" || sw.Msg.Reopen.Model != "hydrogen" {
		t.Fatalf("expected conv-from-db on hydrogen, got %v", reopened)
	}
}

// An agent stopped by an API error or a usage limit is at its input box: a message to it still rings, or a
// transient error would leave it undeliverable for good.
func TestAgyAgentAtErrorOrLimitStillGetsItsDoorbell(t *testing.T) {
	for _, c := range []struct {
		line string
		want v1.AgentState
	}{{"/error", v1.AgentState_AGENT_STATE_ERROR}, {"/limit", v1.AgentState_AGENT_STATE_LIMITED}} {
		t.Run(c.want.String(), func(t *testing.T) {
			w := startReopen(t)
			ctx := context.Background()
			run := startRun(t, w.device.Mesh, "")
			sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
				RunId: run.Id, Repo: gitRepo(t), Slug: "stuck", Kind: "agy", Tier: "light",
			}))
			if err != nil {
				t.Fatal(err)
			}
			a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
			w.typeInto(a, c.line+"\r")
			w.waitState(a.Id, c.want)
			res, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: a.Id,
				Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "continue"}))
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the doorbell of an agent at "+c.want.String(), func() bool { return w.delivered(t, a.Id, res.Msg.Message.Id) })
			w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE) // the doorbell's turn ran and ended
		})
	}
}

// An exact agy id named without an effort runs as named: its name fixes the effort.
func TestAgyExactModelIDWithoutEffortLaunchesAsNamed(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "agy-argv.jsonl")
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Env["FAKEAGY_ARGV_LOG"] = argvLog })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "exact", Kind: "agy", Model: "gemini-4-neon-low",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Model != "gemini-4-neon-low" || a.Effort != "low" {
		t.Errorf("recorded %s at %s, want gemini-4-neon-low at low", a.Model, a.Effort)
	}
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"--model","gemini-4-neon-low"`) {
		t.Errorf("launched %s, want --model gemini-4-neon-low", b)
	}
	// A later effort switch moves it to its family at the new effort.
	a = w.agyTurn(t, a, "hi")
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "high", Why: "t"})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, a.Id, a.TerminalId)
	if b, _ = os.ReadFile(argvLog); !strings.Contains(string(b), `"--model","gemini-4-neon-high"`) {
		t.Errorf("the effort switch launched %s, want --model gemini-4-neon-high", b)
	}
}

// A TUI still drawing at start-up is not idle yet: a doorbell typed then could be dropped.
func TestAgyStartupWaitsForQuietOutput(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Env["FAKEAGY_DRAW_FOR"] = "2s" })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "drawing", Kind: "agy", Tier: "light",
	}))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // past the old cap (5 settle periods) while it still draws
	if a := w.agent(sp.Msg.Agent.Id); a.State != v1.AgentState_AGENT_STATE_STARTING {
		t.Fatalf("an agent still drawing is %s", a.State)
	}
	w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}
