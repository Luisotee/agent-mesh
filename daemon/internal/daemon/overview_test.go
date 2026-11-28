package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func TestARootSaysWhatAChangeWorkerIsForAndAFollowUpKeepsIt(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	repo := gitRepo(t)

	if _, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repo, Slug: "fix", Tier: "strong", Task: "fix it"})); code(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "--about") {
		t.Fatalf("a root's worker with no about: %v", err)
	}
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: repo, Slug: "fix", Tier: "strong",
		Task: "# Fix the retry loop\n\nlong text", About: "  Retry fix\n for the relay "}))
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Msg.Agent; a.About != "Retry fix for the relay" || a.AboutSource != "set" {
		t.Fatalf("the about the root gave: %q (%s)", a.About, a.AboutSource)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)

	// A plan's title never replaces a line the root set.
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "# Plan: Backoff in the relay\n\nbody"})); err != nil {
		t.Fatal(err)
	}
	readInbox(t, asLead)
	if a := w.agent(worker.Id); a.About != "Retry fix for the relay" {
		t.Errorf("the plan replaced a set about: %q", a.About)
	}
	// A task without --about keeps it and says so; with --about it changes.
	kept, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): fix the review findings"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kept.Msg.AboutNote, `about kept: "Retry fix for the relay"`) {
		t.Errorf("no note for a kept about: %q", kept.Msg.AboutNote)
	}
	if a := w.agent(worker.Id); a.About != "Retry fix for the relay" {
		t.Errorf("a follow-up changed the about: %q", a.About)
	}
	if res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): now the docs", About: "Relay docs"})); err != nil || res.Msg.AboutNote != "" {
		t.Fatalf("a task with an about: %v %q", err, res.Msg.GetAboutNote())
	}
	if a := w.agent(worker.Id); a.About != "Relay docs" || a.AboutSource != "set" {
		t.Errorf("the task's about: %q (%s)", a.About, a.AboutSource)
	}
	// A device edits it; empty brings the derived line back (the plan's title).
	if _, err := w.device.Mesh.SetAgentAbout(ctx, connect.NewRequest(&v1.SetAgentAboutRequest{Agent: worker.Id, Text: "By the user"})); err != nil {
		t.Fatal(err)
	}
	if a := w.agent(worker.Id); a.About != "By the user" {
		t.Errorf("the device's about: %q", a.About)
	}
	cleared, err := w.device.Mesh.SetAgentAbout(ctx, connect.NewRequest(&v1.SetAgentAboutRequest{Agent: worker.Id}))
	if err != nil || cleared.Msg.Agent.About != "Backoff in the relay" || cleared.Msg.Agent.AboutSource != "plan" {
		t.Errorf("a cleared about: %v %v", cleared, err)
	}
	// A worker may not set anyone's.
	if _, err := asWorker.SetAgentAbout(ctx, connect.NewRequest(&v1.SetAgentAboutRequest{Agent: worker.Id, Text: "me"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker setting an about: %v", err)
	}
}

func TestADeviceStartedWorkerGetsItsTaskHeadingAsItsAbout(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t), Slug: "fix", Tier: "strong",
		Plan: true, Task: "# Tidy the settings screen\n\nbody"}))
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Msg.Agent; a.About != "Tidy the settings screen" || a.AboutSource != "task" {
		t.Fatalf("derived from the task: %q (%s)", a.About, a.AboutSource)
	}
}

func TestTheRunOverviewHoldsWhatTheUserComesBackTo(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "strong",
		Task: "fix it", About: "Retry fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	overview := func(c interface {
		GetRunOverview(context.Context, *connect.Request[v1.GetRunOverviewRequest]) (*connect.Response[v1.GetRunOverviewResponse], error)
	}, since int64) *v1.RunOverview {
		t.Helper()
		r, err := c.GetRunOverview(ctx, connect.NewRequest(&v1.GetRunOverviewRequest{RunId: run.Id, SinceSeq: since}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.Overview
	}
	names := func(rows []*v1.OverviewAgent) (out []string) {
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return
	}
	o := overview(asLead, 0)
	if o.Headline != "" || o.EventsSinceHeadline != 0 || o.Since != nil {
		t.Errorf("a fresh overview: %+v", o)
	}
	found := false
	for _, a := range append(append([]*v1.OverviewAgent{}, o.Working...), o.Idle...) {
		if a.Name == worker.Name {
			found = a.About == "Retry fix" && a.Now != ""
		}
	}
	if !found {
		t.Errorf("the worker with its about and what it does: working %v idle %v", names(o.Working), names(o.Idle))
	}
	seen := o.Seq

	// The lead writes its headline and a decision; a worker may not.
	if _, err := asWorker.SetRunHeadline(ctx, connect.NewRequest(&v1.SetRunHeadlineRequest{RunId: run.Id, Text: "x"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker's headline: %v", err)
	}
	if _, err := asLead.SetRunHeadline(ctx, connect.NewRequest(&v1.SetRunHeadlineRequest{RunId: run.Id, Text: " "})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an empty headline: %v", err)
	}
	if _, err := asLead.SetRunHeadline(ctx, connect.NewRequest(&v1.SetRunHeadlineRequest{RunId: run.Id, Text: "Fixing the retry.\nNext: review.\nline3\nline4"})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.RecordDecision(ctx, connect.NewRequest(&v1.RecordDecisionRequest{RunId: run.Id, Text: "declined finding 2: it cannot happen"})); err != nil {
		t.Fatal(err)
	}
	if _, err := asWorker.RecordDecision(ctx, connect.NewRequest(&v1.RecordDecisionRequest{RunId: run.Id, Text: "x"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker's decision: %v", err)
	}
	// What the headline does not cover yet: a done.
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done: fixed the retry\nmore"})); err != nil {
		t.Fatal(err)
	}
	o = overview(w.device.Mesh, seen)
	if got := strings.Count(o.Headline, "\n") + 1; got != 3 {
		t.Errorf("the headline is cut to three lines: %q", o.Headline)
	}
	if o.EventsSinceHeadline != 1 {
		t.Errorf("one thing happened since the headline: %d", o.EventsSinceHeadline)
	}
	if len(o.Decisions) != 1 || !strings.Contains(o.Decisions[0].Text, "declined finding 2") {
		t.Errorf("decisions: %v", o.Decisions)
	}
	if o.Since == nil || o.Since.Done != 1 || o.Since.Started != 0 {
		t.Errorf("since the last look: %+v", o.Since)
	}
	var reported *v1.OverviewAgent
	for _, a := range append(append([]*v1.OverviewAgent{}, o.Working...), o.Idle...) {
		if a.Name == worker.Name {
			reported = a
		}
	}
	if reported == nil || reported.Report != "done: fixed the retry" || reported.ReportType != v1.MessageType_MESSAGE_TYPE_DONE {
		t.Errorf("its report: %+v", reported)
	}
	if len(o.DoneReports) != 1 || o.DoneReports[0].Name != worker.Name {
		t.Errorf("a done with no pull request is listed as one: %v", names(o.DoneReports))
	}
	// A worker reads its own run's overview, and no other.
	if _, err := asWorker.GetRunOverview(ctx, connect.NewRequest(&v1.GetRunOverviewRequest{RunId: "someone-elses"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("another run's overview: %v", err)
	}
	// The lead's brief carries the headline with its staleness, and the decision.
	b, err := asLead.GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"headline (what the user reads first; written", "1 things happened since: update it with `mesh headline`", "Fixing the retry. / Next: review.",
		"decisions you recorded for the user", "declined finding 2"} {
		if !strings.Contains(b.Msg.Text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, b.Msg.Text)
		}
	}
}
