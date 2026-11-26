package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func TestTheBriefGivesARootItsCardNotesWorkersAndMailWithoutReadingIt(t *testing.T) {
	share, err := filepath.Abs(filepath.Join("..", "..", "share"))
	if err != nil {
		t.Fatal(err)
	}
	w := startReset(t, daemon.ResetTimings{}, func(c *daemon.AgentConfig) { c.Share = share })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	notes := filepath.Join(w.state, "runs", run.Id, "notes.md")
	os.MkdirAll(filepath.Dir(notes), 0o755)
	os.WriteFile(notes, []byte("the user wants the retry fix\n"+strings.Repeat("x", 5000)), 0o644)
	w.size(t, w.agent(lead.Id), 200000, five, 10*time.Minute)

	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "strong",
		Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan: change X\n\nmore"})); err != nil {
		t.Fatal(err)
	}

	brief, err := asLead.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	text := brief.Msg.Text
	for _, want := range []string{
		"run " + run.Id, "you are the run's root", "## Root card (the daemon)", "`mesh protocol <name>`",
		"NOW: " + worker.Name + "'s plan waits: before you approve or answer it, `mesh protocol root.6`",
		"root context 200k, cache cold since",
		"## notes (" + notes + ")", "the user wants the retry fix", "more characters: cat " + notes,
		"## agents (mesh agents)", "- " + worker.Name + " (claude opus)", "its plan waits on you ← needs you", "waiting on you: " + worker.Name,
		"## your unread mail (headers; `mesh inbox` reads it)", "- plan from " + worker.Name + " at ", ": the plan: change X",
		"## board",
	} {
		if want == "## board" {
			if strings.Contains(text, want) {
				t.Errorf("a board that doesn't exist is shown:\n%s", text)
			}
			continue
		}
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "x") > 5000+200 {
		t.Errorf("notes not capped: %d chars", len(text))
	}
	// Reading the brief marks nothing read.
	inbox, err := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{}))
	if err != nil || len(inbox.Msg.Messages) != 1 || inbox.Msg.Messages[0].Type != v1.MessageType_MESSAGE_TYPE_PLAN {
		t.Fatalf("the plan is still unread: %v %v", inbox, err)
	}
	// A worker's brief is its view of the run, without the card or anything to act on.
	wb, err := asWorker.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wb.Msg.Text, "not this run's root ("+lead.Name+")") || strings.Contains(wb.Msg.Text, "Root card") ||
		strings.Contains(wb.Msg.Text, "the user wants the retry fix") {
		t.Errorf("worker brief:\n%s", wb.Msg.Text)
	}
	// A device names the run; another run is not an agent's to read.
	if _, err := w.device.Mesh.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a device with no run: %v", err)
	}
	if db, err := w.device.Mesh.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{RunId: run.Id})); err != nil || !strings.Contains(db.Msg.Text, "run "+run.Id) {
		t.Errorf("a device's brief: %v %v", db, err)
	}
	if _, err := asLead.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{RunId: "someone-elses"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("another run: %v", err)
	}
}

func TestADoneTheRootHasReadIsNotWaitingOnItInTheBrief(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "strong", Task: "fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done: fixed"})); err != nil {
		t.Fatal(err)
	}
	brief := func() string {
		b, err := asLead.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return b.Msg.Text
	}
	if text := brief(); !strings.Contains(text, "its done waits on you ← needs you") {
		t.Fatalf("an unread done is not listed:\n%s", text)
	}
	readInbox(t, asLead)
	if text := brief(); strings.Contains(text, "waits on you") || strings.Contains(text, "waiting on you:") {
		t.Errorf("a done the root has read still waits:\n%s", text)
	}
}

func TestTheBriefCarriesTheModelNoticeToTheRootOnly(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	if err := w.d.SeedModelNotice(ctx, "agy", "xenon"); err != nil {
		t.Fatal(err)
	}
	b, err := asLead.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.Msg.Text, "mesh: new agy models no tier names: xenon") {
		t.Errorf("the root's brief lacks the model notice:\n%s", b.Msg.Text)
	}
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "strong", Task: "fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	wb, err := w.as(w.token(t, worker.Id)).GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wb.Msg.Text, "new agy models") {
		t.Errorf("a worker is told what the root decides:\n%s", wb.Msg.Text)
	}
}

func TestTheBriefAfterAResetCarriesWhatTheRootSaidNext(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	w.size(t, w.agent(lead.Id), 200000, hour, 0)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{Next: "check the PR"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "done")
	fresh := w.as(w.token(t, lead.Id))
	brief, err := fresh.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## last reset (", ", done)", "next, as you said then: check the PR",
		"your old session's transcript: " + after.LastReset.Transcript, "An untrusted reference"} {
		if !strings.Contains(brief.Msg.Text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief.Msg.Text)
		}
	}
}

func TestASessionThatStartsAgainIsToldWhereItIs(t *testing.T) {
	share, _ := filepath.Abs(filepath.Join("..", "..", "share"))
	w := startReset(t, daemon.ResetTimings{}, func(c *daemon.AgentConfig) { c.Share = share })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid",
		Task: "Direct task (no plan): fix the flaky test\n\nit fails on CI only"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "its task delivered", func() bool {
		return w.delivered(t, worker.Id, res.Msg.Agent.Id) || strings.Contains(w.typed(), "task from")
	})
	w.typeInto(w.agent(worker.Id), "/compact keep it\r")
	var screen string
	waitFor(t, "the reminder on its screen", func() bool {
		screen = w.screen(t, w.agent(worker.Id))
		return strings.Contains(screen, "agent-mesh reminder (compact): you are "+worker.Name)
	})
	for _, want := range []string{"a worker in run " + run.Id, "Current task: fix the flaky test", "Phase: a direct task", "Run `mesh inbox` now."} {
		if !strings.Contains(screen, want) {
			t.Errorf("the reminder lacks %q:\n%s", want, screen)
		}
	}
}

func TestARootsReminderIsSilentWhileItsResetTypes(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	w.setSetting(t, "root_reset.relaunch", false)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	w.waitReset(t, lead.Id, "done")
	// The /clear's SessionStart came while the reset was typing: no reminder (the bootstrap says `mesh brief`).
	screen := w.screen(t, w.agent(lead.Id))
	if strings.Contains(screen, "agent-mesh reminder (clear)") {
		t.Errorf("a reminder during a reset:\n%s", screen)
	}
	// Once it is over, a compaction's reminder tells the root to run `mesh brief`.
	w.typeInto(w.agent(lead.Id), "/compact\r")
	waitFor(t, "the root's reminder", func() bool {
		return strings.Contains(w.screen(t, w.agent(lead.Id)), "agent-mesh reminder (compact): you are "+lead.Name+", the root of run "+run.Id)
	})
}

func TestAnAgentCannotBriefAnotherAgent(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	if _, err := asWorker.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{Agent: lead.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker briefing the root: %v", err)
	}
	if _, err := asWorker.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{Agent: worker.Name})); err != nil {
		t.Errorf("a worker briefing itself: %v", err)
	}
}
