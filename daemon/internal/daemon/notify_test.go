package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// userNotices are a run's UserNotice events, in order.
func userNotices(t *testing.T, h *harness, runID string) []*v1.UserNotice {
	t.Helper()
	evs, err := h.d.Store().Read(context.Background()).ListEvents(runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.UserNotice
	for _, e := range evs {
		if n, ok := e.Payload.(*v1.Event_UserNotice); ok {
			out = append(out, n.UserNotice)
		}
	}
	return out
}

// noticeOf waits for a notice of the kind about the agent and returns it.
func noticeOf(t *testing.T, h *harness, runID, kind, agent string) *v1.UserNotice {
	t.Helper()
	var got *v1.UserNotice
	waitFor(t, "a "+kind+" notice about "+agent, func() bool {
		for _, n := range userNotices(t, h, runID) {
			if n.Kind == kind && n.AgentId == agent {
				got = n
			}
		}
		return got != nil
	})
	return got
}

// setState moves an agent to a state with the event the reducer would write.
func setState(t *testing.T, h *harness, id string, to v1.AgentState) {
	t.Helper()
	err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(id)
		if err != nil {
			return err
		}
		from := a.State
		a.State = to
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: id, Payload: &v1.Event_AgentStateChanged{AgentStateChanged: &v1.AgentStateChanged{From: from, To: to}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waiting(t *testing.T, h *harness) []*v1.Waiting {
	t.Helper()
	res, err := h.device.Mesh.ListWaiting(context.Background(), connect.NewRequest(&v1.ListWaitingRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Waiting
}

// A worker's plan is the user's only where the user approves plans; elsewhere the lead decides it and the plan is
// in Activity alone.
func TestAWorkersPlanIsTheUsersOnlyWhereTheUserApprovesPlans(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		run     *v1.Run
		forUser bool
	}{
		{"lead decides", startRun(t, h.device.Mesh, ""), false},
		{"user approves", startUserRun(t, h.device.Mesh), true},
	} {
		h.seedAgent(tc.run.Id, tc.name+"-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
		w := h.seedAgent(tc.run.Id, tc.name+"-w", "fix", tc.name+"-root", v1.AgentRole_AGENT_ROLE_WORKER)
		readInbox(t, h.as(w))
		if _, err := h.as(w).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "a plan"})); err != nil {
			t.Fatal(err)
		}
		n := noticeOf(t, h, tc.run.Id, "plan", tc.name+"-w")
		if n.ForUser != tc.forUser {
			t.Errorf("%s: the plan notice for_user = %v, want %v", tc.name, n.ForUser, tc.forUser)
		}
	}
	// ListWaiting marks the same.
	byRun := map[string]bool{}
	for _, w := range waiting(t, h) {
		if w.Kind == v1.WaitingKind_WAITING_KIND_PLAN {
			byRun[w.AgentId] = w.ForUser
		}
	}
	if forUser, ok := byRun["lead decides-w"]; !ok || forUser {
		t.Errorf("the lead's plan waits, not for the user: %v (listed %v)", forUser, ok)
	}
	if forUser := byRun["user approves-w"]; !forUser {
		t.Error("the plan of a run the user approves is for the user")
	}
}

// A lead puts something to the user with `send user`: stored in the run, for the user, answered from the app (an
// answer to the lead closes it). A worker may not.
func TestALeadAsksTheUserAndTheAnswerClosesIt(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	lead := h.as(h.seedAgent(run.Id, "u-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT))
	w := h.as(h.seedAgent(run.Id, "u-w", "fix", "u-root", v1.AgentRole_AGENT_ROLE_WORKER))
	if _, err := w.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "user", Type: v1.MessageType_MESSAGE_TYPE_QUESTION, Body: "may I?"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a worker writing to the user: %v", err)
	}
	if _, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "user", Type: v1.MessageType_MESSAGE_TYPE_QUESTION, Body: "me to me"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a device writing to the user: %v", err)
	}
	if _, err := lead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "user", Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a task to the user: %v", err)
	}
	askUser(t, &lead, v1.MessageType_MESSAGE_TYPE_QUESTION, "ship it?")
	n := noticeOf(t, h, run.Id, "question", "u-root")
	if !n.ForUser || n.Body != "ship it?" || n.Title != "lead asks you" {
		t.Fatalf("notice %+v", n)
	}
	var item *v1.Waiting
	for _, w := range waiting(t, h) {
		if w.AgentId == "u-root" && w.Kind == v1.WaitingKind_WAITING_KIND_QUESTION {
			item = w
		}
	}
	if item == nil || !item.ForUser || item.Message.Body != "ship it?" {
		t.Fatalf("waiting %+v", item)
	}
	// The user answers the lead; the item closes, and the answer reaches the lead as any answer does.
	if _, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: "u-root", Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "yes"})); err != nil {
		t.Fatal(err)
	}
	for _, w := range waiting(t, h) {
		if w.AgentId == "u-root" {
			t.Fatalf("still waiting after the answer: %+v", w)
		}
	}
}

// A run is finished when its lead is idle and every worker is exited or reported: once, until it has work again.
func TestARunFinishedIsToldOnceAndAgainAfterNewWork(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	leadTok := h.seedAgent(run.Id, "f-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w := h.seedAgent(run.Id, "f-w", "fix", "f-root", v1.AgentRole_AGENT_ROLE_WORKER)
	finished := func() int {
		n := 0
		for _, x := range userNotices(t, h, run.Id) {
			if x.Kind == "finished" {
				n++
			}
		}
		return n
	}
	// The lead ended a turn while its worker works: not finished.
	setState(t, h, "f-w", v1.AgentState_AGENT_STATE_WORKING)
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_WORKING)
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(200 * time.Millisecond)
	if finished() != 0 {
		t.Fatal("told as finished while a worker works")
	}
	// The worker reports done and goes idle; the lead's next turn end finishes the run.
	readInbox(t, h.as(w))
	if _, err := h.as(w).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done"})); err != nil {
		t.Fatal(err)
	}
	setState(t, h, "f-w", v1.AgentState_AGENT_STATE_IDLE)
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_WORKING)
	readInbox(t, h.as(leadTok)) // the lead took the report
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "the run finished", func() bool { return finished() == 1 })
	n := noticeOf(t, h, run.Id, "finished", "f-root")
	if !n.ForUser || !strings.HasSuffix(n.Title, "finished") {
		t.Fatalf("notice %+v", n)
	}
	// Nothing new: another idle is no new finish.
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_UNKNOWN)
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(200 * time.Millisecond)
	if finished() != 1 {
		t.Fatalf("told again with no new work: %d", finished())
	}
	// The lead works again: its next finish is news.
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_WORKING)
	setState(t, h, "f-root", v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "the second finish", func() bool { return finished() == 2 })
}

// A reviewer a worker started reports to that worker, so it counts as done once it told it; and a question the lead
// put to the user keeps the run from being finished until the user answers.
func TestARunWithAReviewerUnderAWorkerAndAnOpenQuestionFinishesWhenBothAreSettled(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	leadTok := h.seedAgent(run.Id, "g-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	wTok := h.seedAgent(run.Id, "g-w", "fix", "g-root", v1.AgentRole_AGENT_ROLE_WORKER)
	rvTok := h.seedAgent(run.Id, "g-rv", "review", "g-w", v1.AgentRole_AGENT_ROLE_REVIEWER)
	finished := func() int {
		n := 0
		for _, x := range userNotices(t, h, run.Id) {
			if x.Kind == "finished" {
				n++
			}
		}
		return n
	}
	send := func(tok string, to string, typ v1.MessageType) {
		t.Helper()
		if _, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: to, Type: typ, Body: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	send(rvTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE)
	setState(t, h, "g-rv", v1.AgentState_AGENT_STATE_IDLE)
	send(wTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE)
	setState(t, h, "g-w", v1.AgentState_AGENT_STATE_IDLE)
	setState(t, h, "g-root", v1.AgentState_AGENT_STATE_WORKING)
	readInbox(t, h.as(leadTok))
	readInbox(t, h.as(wTok))
	// The lead asks the user, and ends its turn to wait: not finished.
	lead := h.as(leadTok)
	askUser(t, &lead, v1.MessageType_MESSAGE_TYPE_QUESTION, "merge it?")
	setState(t, h, "g-root", v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(200 * time.Millisecond)
	if finished() != 0 {
		t.Fatal("finished while the user owes an answer")
	}
	// The user answers; the lead acts on it and ends its turn: finished, with the reviewer idle under its worker.
	if _, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: "g-root", Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "yes"})); err != nil {
		t.Fatal(err)
	}
	setState(t, h, "g-root", v1.AgentState_AGENT_STATE_WORKING)
	readInbox(t, h.as(leadTok))
	setState(t, h, "g-root", v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "the run finished", func() bool { return finished() == 1 })
}

// A worker re-tasked after its `done` is not done until it reports again; a question left by a worker that exited
// is not open (ListWaiting drops it), so it does not hold the finish back.
func TestAReTaskedWorkerIsNotDoneAndAnExitedWorkersQuestionDoesNotHoldTheFinish(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	leadTok := h.seedAgent(run.Id, "h-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	wTok := h.seedAgent(run.Id, "h-w", "fix", "h-root", v1.AgentRole_AGENT_ROLE_WORKER)
	gone := h.seedAgent(run.Id, "h-gone", "gone", "h-root", v1.AgentRole_AGENT_ROLE_WORKER)
	finished := func() int {
		n := 0
		for _, x := range userNotices(t, h, run.Id) {
			if x.Kind == "finished" {
				n++
			}
		}
		return n
	}
	send := func(tok string, to string, typ v1.MessageType) {
		t.Helper()
		if _, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: to, Type: typ, Body: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	// The gone worker asked, never got an answer and exited; the other one reported done and is idle.
	send(gone, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION)
	setState(t, h, "h-gone", v1.AgentState_AGENT_STATE_EXITED)
	send(wTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE)
	setState(t, h, "h-w", v1.AgentState_AGENT_STATE_IDLE)
	setState(t, h, "h-root", v1.AgentState_AGENT_STATE_WORKING)
	readInbox(t, h.as(leadTok))
	// The lead gives the worker a new task and ends its turn: the old `done` does not count.
	send(leadTok, "h-w", v1.MessageType_MESSAGE_TYPE_TASK)
	setState(t, h, "h-root", v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(200 * time.Millisecond)
	if finished() != 0 {
		t.Fatal("finished with a worker re-tasked and not yet done")
	}
	// It reports again: the run is finished although the exited worker's question is still stored.
	readInbox(t, h.as(wTok))
	send(wTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE)
	setState(t, h, "h-root", v1.AgentState_AGENT_STATE_WORKING)
	readInbox(t, h.as(leadTok))
	setState(t, h, "h-root", v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "the run finished", func() bool { return finished() == 1 })
}

// A plain agent (no parent) that ends a turn is "finished"; a worker's turn end is its lead's business.
func TestAPlainAgentEndingATurnIsFinished(t *testing.T) {
	h := start(t, "")
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "p-plain", "scratch", "", v1.AgentRole_AGENT_ROLE_WORKER)
	h.seedAgent(run.Id, "p-lead", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	h.seedAgent(run.Id, "p-w", "fix", "p-lead", v1.AgentRole_AGENT_ROLE_WORKER)
	// Coming up to the first prompt is no finish, for a plain agent or a lead.
	setState(t, h, "p-plain", v1.AgentState_AGENT_STATE_STARTING)
	setState(t, h, "p-plain", v1.AgentState_AGENT_STATE_IDLE)
	setState(t, h, "p-lead", v1.AgentState_AGENT_STATE_STARTING)
	setState(t, h, "p-lead", v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(200 * time.Millisecond)
	if got := userNotices(t, h, run.Id); len(got) != 0 {
		t.Fatalf("agents coming up made notices: %+v", got)
	}
	setState(t, h, "p-w", v1.AgentState_AGENT_STATE_WORKING)
	setState(t, h, "p-w", v1.AgentState_AGENT_STATE_IDLE)
	setState(t, h, "p-plain", v1.AgentState_AGENT_STATE_WORKING)
	setState(t, h, "p-plain", v1.AgentState_AGENT_STATE_IDLE)
	n := noticeOf(t, h, run.Id, "finished", "p-plain")
	if !n.ForUser || n.Title != "scratch finished" {
		t.Fatalf("notice %+v", n)
	}
	for _, x := range userNotices(t, h, run.Id) {
		if x.AgentId == "p-w" {
			t.Fatalf("a worker's turn end made a notice: %+v", x)
		}
	}
}

// A PR is ready when its checks first pass, and the user's when they merge it.
func TestAPRReadyToMergeIsTheUsersWhereTheyMerge(t *testing.T) {
	h := start(t, "")
	mine := startUserRun(t, h.device.Mesh)
	h.seedAgent(mine.Id, "r-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	h.seedAgent(mine.Id, "r-w", "fix", "r-root", v1.AgentRole_AGENT_ROLE_WORKER)
	pr := func(runID, agent, state, checks, fromState, fromChecks string) {
		t.Helper()
		err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			_, err := tx.AppendEvent(&v1.Event{RunId: runID, AgentId: agent, Payload: &v1.Event_PullRequestChanged{PullRequestChanged: &v1.PullRequestChanged{
				PullRequest: &v1.PullRequest{AgentId: agent, Url: "https://github.com/o/r/pull/7", Number: 7, State: state, Checks: checks},
				FromState:   fromState, FromChecks: fromChecks}}})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pr(mine.Id, "r-w", "open", "pending", "open", "")
	pr(mine.Id, "r-w", "open", "pass", "open", "pending")
	n := noticeOf(t, h, mine.Id, "ready", "r-w")
	if !n.ForUser || !strings.Contains(n.Body, "pull/7") {
		t.Fatalf("notice %+v", n)
	}
	// Another change of the same PR, already passing, is not ready again.
	pr(mine.Id, "r-w", "open", "pass", "open", "pass")
	time.Sleep(200 * time.Millisecond)
	count := 0
	for _, x := range userNotices(t, h, mine.Id) {
		if x.Kind == "ready" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d ready notices", count)
	}
	// Where the lead merges, a passing PR is nobody's notice.
	theirs := startRun(t, h.device.Mesh, "")
	h.seedAgent(theirs.Id, "q-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	h.seedAgent(theirs.Id, "q-w", "fix", "q-root", v1.AgentRole_AGENT_ROLE_WORKER)
	pr(theirs.Id, "q-w", "open", "pass", "open", "pending")
	time.Sleep(200 * time.Millisecond)
	for _, x := range userNotices(t, h, theirs.Id) {
		if x.Kind == "ready" {
			t.Fatalf("a PR the lead merges notified: %+v", x)
		}
	}
}

// A permission prompt held for a lead that nobody answers in a while is the user's.
func TestAPermissionPromptNobodyAnswersIsToldToTheUser(t *testing.T) {
	defer daemon.SetPermNoticeAfter(300 * time.Millisecond)()
	w := startReviews(t)
	wk := w.worker(t, "wk")
	w.typeInto(wk, "/perm Bash rm -rf build\r")
	p := w.pendingFor(t, wk.Id)
	var got *v1.UserNotice
	waitFor(t, "the permission notice", func() bool {
		evs, err := w.d.Store().Read(context.Background()).ListEvents(w.run.Id)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if n, ok := e.Payload.(*v1.Event_UserNotice); ok && n.UserNotice.Tag == "perm:"+p.Id {
				got = n.UserNotice
			}
		}
		return got != nil
	})
	if !got.ForUser || got.Kind != "input" || got.AgentId != wk.Id || !strings.Contains(got.Body, "rm -rf build") {
		t.Fatalf("notice %+v", got)
	}
}
