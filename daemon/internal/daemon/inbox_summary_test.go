package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A root's inbox shows a long plan and a long done as summaries (with the file each is kept in), a short one
// whole; --full and --id print the stored text whole; another caller always gets whole bodies.
func TestARootsInboxSummarisesLongPlansAndReports(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix", Tier: "strong", Plan: true, Task: "plan it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	readInbox(t, asLead)

	plan := "# Plan: fix the thing\n\nRepo: /r · Branch: b · Agent: " + worker.Name + "\n\n## Goal\nFix it.\n\n## Findings\n" +
		strings.Repeat("a long finding. ", 200) + "\n\n```\n## not a heading\n```\n\n## Open questions / risks\n- Which retry count? I recommend 3.\n\n## Out of scope\nNothing.\n"
	planFile := filepath.Join(w.state, "runs", run.Id, "plans", worker.Name+".md")
	os.MkdirAll(filepath.Dir(planFile), 0o755)
	os.WriteFile(planFile, []byte(plan), 0o644)
	done := "Opened https://github.com/o/r/pull/12 and https://github.com/o/r/pull/12 again.\n" + strings.Repeat("detail line of the report.\n", 200)
	send := func(typ v1.MessageType, body string) *v1.Message {
		t.Helper()
		r, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: typ, Body: body}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Message
	}
	pm := send(v1.MessageType_MESSAGE_TYPE_PLAN, plan)
	dm := send(v1.MessageType_MESSAGE_TYPE_DONE, done)
	short := send(v1.MessageType_MESSAGE_TYPE_DONE, "short report, all good")
	// One paragraph on one line is cut too, a done's and a plan's header alike.
	paragraph := send(v1.MessageType_MESSAGE_TYPE_DONE, strings.Repeat("one long paragraph. ", 300))
	longHead := send(v1.MessageType_MESSAGE_TYPE_PLAN, "# Plan: "+strings.Repeat("a very long title ", 300)+"\n\n## Goal\nFix it.\n")

	inbox, err := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true}))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*v1.Message{}
	for _, m := range inbox.Msg.Messages {
		byID[m.Id] = m
	}
	p := byID[pm.Id].Body
	for _, want := range []string{"# Plan: fix the thing", "Repo: /r", "sections (characters): Goal ", "Findings ", "Open questions / risks ",
		"Which retry count? I recommend 3.", "file: " + planFile, "[summary: a plan of ", "`mesh inbox --id " + pm.Id + "` prints it whole]"} {
		if !strings.Contains(p, want) {
			t.Errorf("the plan summary lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "a long finding") {
		t.Errorf("the plan summary holds the body:\n%s", p)
	}
	if strings.Contains(p, "not a heading") { // a fenced `## ` line is no section
		t.Errorf("a fenced heading became a section:\n%s", p)
	}
	d := byID[dm.Id].Body
	if len(d) > 1800 || !strings.Contains(d, "PRs: https://github.com/o/r/pull/12\n") || !strings.Contains(d, "prints it whole]") ||
		strings.Count(d, "detail line") > 60 {
		t.Errorf("the done summary (%d chars):\n%s", len(d), d)
	}
	if strings.Contains(d, "file: ") {
		t.Errorf("a report file that doesn't exist is named: %s", d)
	}
	for name, m := range map[string]*v1.Message{"a one-line done": paragraph, "a plan with a long title": longHead} {
		if n := len([]rune(byID[m.Id].Body)); n > 1800 || !strings.Contains(byID[m.Id].Body, "…") {
			t.Errorf("%s was not cut (%d characters)", name, n)
		}
	}
	if got := byID[short.Id].Body; got != "short report, all good" {
		t.Errorf("a short report was summarised: %q", got)
	}
	// The stored message is whole: --full and --id print it.
	full, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true, Full: true}))
	for _, m := range full.Msg.Messages {
		if m.Id == pm.Id && m.Body != plan {
			t.Errorf("--full changed the plan")
		}
	}
	one, err := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Id: dm.Id}))
	if err != nil || len(one.Msg.Messages) != 1 || one.Msg.Messages[0].Body != done {
		t.Fatalf("--id: %v %v", one, err)
	}
	if left, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true})); len(left.Msg.Messages) != 4 {
		t.Errorf("--id read more than its own message: %d unread", len(left.Msg.Messages))
	}
	// Not someone else's message, and not by a name that doesn't exist.
	if _, err := asWorker.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Id: pm.Id})); code(err) != connect.CodeNotFound {
		t.Errorf("a worker read the root's message: %v", err)
	}
	// A worker gets whole bodies of what it is sent: a long done from its own reviewer.
	long := strings.Repeat("finding. ", 500)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name, Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: long})); err != nil {
		t.Fatal(err)
	}
	wi, _ := asWorker.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true}))
	found := false
	for _, m := range wi.Msg.Messages {
		if m.Body == long {
			found = true
		}
	}
	if !found {
		t.Errorf("a worker's inbox was summarised")
	}
	// The threshold is a setting; 0 prints everything whole.
	w.set(t, "inbox.summary_over", 0)
	all, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true}))
	for _, m := range all.Msg.Messages {
		if m.Id == pm.Id && m.Body != plan {
			t.Errorf("inbox.summary_over 0 still summarised")
		}
	}
	_ = lead
}
