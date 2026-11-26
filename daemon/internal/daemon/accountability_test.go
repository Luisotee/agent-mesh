package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// acct is a seeded run: a root and a worker with no processes, so a test drives the accountability pass itself.
type acct struct {
	*harness
	run                *v1.Run
	rootTok, workerTok string
}

func newAcct(t *testing.T) *acct {
	t.Helper()
	h := start(t, "")
	run := startRun(t, h.device.Mesh, "")
	a := &acct{harness: h, run: run}
	a.rootTok = h.seedAgent(run.Id, "a-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	a.workerTok = h.seedAgent(run.Id, "a-w1", "repo-one", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	return a
}

func (a *acct) setting(key string, value any) {
	a.t.Helper()
	if _, err := a.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta(""), Key: key, Value: val(a.t, value)})); err != nil {
		a.t.Fatal(err)
	}
}

// state puts the worker in a state, with its last activity at `at`.
func (a *acct) state(s v1.AgentState, at time.Time) {
	a.t.Helper()
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		w, err := tx.GetAgent("a-w1")
		if err != nil {
			return err
		}
		w.State, w.LastActivity = s, timestamppb.New(at)
		return tx.PutAgent(w)
	})
	if err != nil {
		a.t.Fatal(err)
	}
}

func (a *acct) send(tok, to string, typ v1.MessageType, body string) error {
	a.t.Helper()
	_, err := a.as(tok).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: to, Type: typ, Body: body}))
	return err
}

// task gives the worker a task and has it read it, then lets it idle past `nudge_after`.
func (a *acct) taskRead() {
	a.t.Helper()
	if err := a.send(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_TASK, "do it"); err != nil {
		a.t.Fatal(err)
	}
	readInbox(a.t, a.as(a.workerTok))
}

// mailOf counts the messages in an agent's inbox (read or not) whose body starts with prefix.
func (a *acct) mailOf(agent, prefix string) int {
	a.t.Helper()
	msgs, err := a.d.Store().Read(context.Background()).Inbox(agent, true)
	if err != nil {
		a.t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, prefix) && m.From.GetName() == "mesh" {
			n++
		}
	}
	return n
}

func (a *acct) pass() { a.d.Accountability(context.Background()) }

// A worker that ends a turn with no report is nudged once; ending another turn the same way tells its parent,
// once. Reporting, or a declared wait, stops both.
func TestAnIdleWorkerWithNoReportIsNudgedOnceThenItsParentIsTold(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()

	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged before nudge_after: %d", n)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("nudges %d, want exactly 1", n)
	}
	if n := a.mailOf("a-root", "[mesh alert"); n != 0 {
		t.Fatalf("the parent was told before the nudge had a turn: %d", n)
	}
	// No turn since the nudge (its doorbell is held back): the agent hasn't had its chance, so the parent isn't told.
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert"); n != 0 {
		t.Fatalf("the parent was told before the nudge had a turn: %d", n)
	}
	// The nudge rang a turn that ended the same way: idle again, no report.
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert silent a-w1]"); n != 1 {
		t.Fatalf("alerts to the parent %d, want exactly 1", n)
	}
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("a second nudge: %d", n)
	}
}

func TestAReportAnUnreadMessageOrADeclaredWaitHoldsTheNudge(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())

	// No task yet: nothing is owed.
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged with no task: %d", n)
	}
	// A task still unread: work is coming (or a doorbell was lost), not a turn ended without a report.
	if err := a.send(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_TASK, "do it"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged with an unread task: %d", n)
	}
	readInbox(t, a.as(a.workerTok))
	// A declared wait holds it, up to wait_max.
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_WAIT, "CI is running"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged during a declared wait: %d", n)
	}
	a.setting("accountability.wait_max", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now().Add(-5*time.Second))
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("a wait past wait_max still holds the nudge: %d", n)
	}
}

func TestAReportEndsWhatIsOwed(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]") + a.mailOf("a-root", "[mesh alert"); n != 0 {
		t.Fatalf("a reported worker was chased: %d", n)
	}
	// A new task starts the count again.
	a.taskRead()
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("the next task's silence wasn't nudged: %d", n)
	}
}

// A usage limit and a silent worker are told to the parent once per stretch; the pass doesn't touch the worker.
func TestALimitedOrQuietWorkerTellsItsParentOnceAndIsLeftAlone(t *testing.T) {
	a := newAcct(t)
	a.state(v1.AgentState_AGENT_STATE_WORKING, time.Now().Add(-30*time.Minute))
	a.pass()
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert unknown a-w1]"); n != 1 {
		t.Fatalf("unknown alerts %d, want 1", n)
	}
	a.state(v1.AgentState_AGENT_STATE_LIMITED, time.Now())
	a.pass()
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert limited a-w1]"); n != 1 {
		t.Fatalf("limited alerts %d, want 1", n)
	}
	w, err := a.d.Store().Read(context.Background()).GetAgent("a-w1")
	if err != nil || w.State != v1.AgentState_AGENT_STATE_LIMITED || w.PendingReopen != nil {
		t.Fatalf("the worker was acted on: %v %v", w, err)
	}
	// The same alert isn't repeated, and a new limit after more activity is told again.
	a.state(v1.AgentState_AGENT_STATE_WORKING, time.Now())
	time.Sleep(10 * time.Millisecond)
	a.state(v1.AgentState_AGENT_STATE_LIMITED, time.Now())
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert limited a-w1]"); n != 2 {
		t.Fatalf("limited alerts after a second limit %d, want 2", n)
	}
	// A root has no parent to tell.
	if n := a.mailOf("a-w1", "[mesh alert"); n != 0 {
		t.Fatalf("alerts to the worker: %d", n)
	}
}

func TestAReportIsRefusedWhileATaskOrAnswerIsUnread(t *testing.T) {
	a := newAcct(t)
	if err := a.send(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_TASK, "do it"); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED} {
		err := a.send(a.workerTok, "parent", typ, "x")
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "mesh inbox") {
			t.Errorf("%s with an unread task: %v", typ, err)
		}
	}
	// An info or a wait isn't a report: they go through, as does a message to a child.
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_INFO, v1.MessageType_MESSAGE_TYPE_WAIT} {
		if err := a.send(a.workerTok, "parent", typ, "x"); err != nil {
			t.Errorf("%s with an unread task: %v", typ, err)
		}
	}
	// Reading it frees the report; a later answer holds it again.
	readInbox(t, a.as(a.workerTok))
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION, "which?"); err != nil {
		t.Fatal(err)
	}
	if err := a.send(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_ANSWER, "this one"); err != nil {
		t.Fatal(err)
	}
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "x"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("done with an unread answer: %v", err)
	}
	// The daemon's own nudge, unread, holds nothing.
	readInbox(t, a.as(a.workerTok))
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "x"); err != nil {
		t.Errorf("done after reading: %v", err)
	}
}

// A worker whose process ends without a report tells its parent how, once; one that reported first doesn't.
func TestAWorkerThatExitsWithoutReportingTellsItsParent(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	crasher := w.spawnUnder(t, asLead, "crasher", &v1.SpawnAgentRequest{})
	reporter := w.spawnUnder(t, asLead, "reporter", &v1.SpawnAgentRequest{})
	w.typeInto(crasher, "/crash\r")
	w.waitState(crasher.Id, v1.AgentState_AGENT_STATE_EXITED)
	waitFor(t, "the exit alert", func() bool { return w.alerts(t, lead.Id, "[mesh alert exited "+crasher.Id+"]") == 1 })
	if got := w.alertText(t, lead.Id, "[mesh alert exited "+crasher.Id+"]"); !strings.Contains(got, "exited with code 7") ||
		!strings.Contains(got, crasher.Name) {
		t.Errorf("alert %q", got)
	}
	w.sendToParent(t, reporter, v1.MessageType_MESSAGE_TYPE_DONE)
	w.typeInto(reporter, "/crash\r")
	w.waitState(reporter.Id, v1.AgentState_AGENT_STATE_EXITED)
	time.Sleep(500 * time.Millisecond)
	if n := w.alerts(t, lead.Id, "[mesh alert exited "+reporter.Id+"]"); n != 0 {
		t.Errorf("an agent that had reported was alerted about: %d", n)
	}
}

// The exit alert quotes what the agent last said, read before the exit event rewrites it: after a plain exit
// (which clears it) and after a kill (which sets "killed by signal").
func TestTheExitAlertQuotesWhatTheAgentLastSaid(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	say := func(id, detail string) {
		t.Helper()
		err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			a, err := tx.GetAgent(id)
			if err != nil {
				return err
			}
			a.StateDetail = detail
			return tx.PutAgent(a)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	crasher := w.spawnUnder(t, asLead, "crasher", &v1.SpawnAgentRequest{})
	say(crasher.Id, "reading the migration")
	w.typeInto(crasher, "/crash\r")
	w.waitState(crasher.Id, v1.AgentState_AGENT_STATE_EXITED)
	head := "[mesh alert exited " + crasher.Id + "]"
	waitFor(t, "the exit alert", func() bool { return w.alerts(t, lead.Id, head) == 1 })
	if got := w.alertText(t, lead.Id, head); !strings.Contains(got, "It last said: reading the migration") {
		t.Errorf("exit alert %q", got)
	}

	killed := w.spawnUnder(t, asLead, "killed", &v1.SpawnAgentRequest{})
	say(killed.Id, "running the tests")
	if _, err := asLead.StopAgent(context.Background(), connect.NewRequest(&v1.StopAgentRequest{Meta: meta(""), Agent: killed.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(killed.Id, v1.AgentState_AGENT_STATE_EXITED)
	head = "[mesh alert exited " + killed.Id + "]"
	waitFor(t, "the kill alert", func() bool { return w.alerts(t, lead.Id, head) == 1 })
	if got := w.alertText(t, lead.Id, head); !strings.Contains(got, "It last said: running the tests") || strings.Contains(got, "last said: killed") {
		t.Errorf("kill alert %q", got)
	}
}

// A message to its own child is no report to its parent: it neither stops the nudge nor is refused for unread
// instructions.
func TestAQuestionToAChildIsNotAReportToTheParent(t *testing.T) {
	a := newAcct(t)
	midTok := a.seedAgent(a.run.Id, "a-mid", "repo-mid", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	a.seedAgent(a.run.Id, "a-kid", "repo-kid", "a-mid", v1.AgentRole_AGENT_ROLE_WORKER)
	a.setting("accountability.nudge_after", 1.0)
	if err := a.send(a.rootTok, "repo-mid", v1.MessageType_MESSAGE_TYPE_TASK, "do it"); err != nil {
		t.Fatal(err)
	}
	// Its task unread, it may still ask its child; it may not report to its parent.
	if err := a.send(midTok, "repo-kid", v1.MessageType_MESSAGE_TYPE_QUESTION, "which file?"); err != nil {
		t.Errorf("a question to a child with an unread task: %v", err)
	}
	if err := a.send(midTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "x"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("done to the parent with an unread task: %v", err)
	}
	readInbox(t, a.as(midTok))
	if err := a.send(midTok, "repo-kid", v1.MessageType_MESSAGE_TYPE_QUESTION, "and which line?"); err != nil {
		t.Fatal(err)
	}
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		m, err := tx.GetAgent("a-mid")
		if err != nil {
			return err
		}
		m.State, m.LastActivity = v1.AgentState_AGENT_STATE_IDLE, timestamppb.New(time.Now())
		return tx.PutAgent(m)
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-mid", "[mesh nudge]"); n != 1 {
		t.Fatalf("a question to its child counted as a report: nudges %d, want 1", n)
	}
}

// A reopen whose relaunch fails leaves the agent exited, and its parent is told like for any exit.
func TestAFailedReopenTellsTheParent(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "doomed", &v1.SpawnAgentRequest{})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	// An agent of a kind no adapter serves can't be relaunched.
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(worker.Id)
		if err != nil {
			return err
		}
		a.Kind, a.StateDetail = "nosuch", "half way through the rename"
		return tx.PutAgent(a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.SwitchAgent(context.Background(), connect.NewRequest(&v1.SwitchAgentRequest{
		Meta: meta(""), Agent: worker.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_EXITED)
	head := "[mesh alert exited " + worker.Id + "]"
	waitFor(t, "the alert", func() bool { return w.alerts(t, lead.Id, head) == 1 })
	got := w.alertText(t, lead.Id, head)
	if !strings.Contains(got, "could not be reopened") || !strings.Contains(got, "It last said: half way through the rename") {
		t.Errorf("alert %q", got)
	}
}

func (w *reopenWorld) alerts(t *testing.T, agentID, prefix string) int {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).Inbox(agentID, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, prefix) {
			n++
		}
	}
	return n
}

func (w *reopenWorld) alertText(t *testing.T, agentID, prefix string) string {
	t.Helper()
	msgs, _ := w.d.Store().Read(context.Background()).Inbox(agentID, true)
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, prefix) {
			return m.Body
		}
	}
	return ""
}

// Live: the nudge is a message like any other, so it rings the worker's doorbell and starts a turn.
func TestTheNudgeRingsTheWorkersDoorbell(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.AccountEvery = 100 * time.Millisecond })
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	w.set(t, "accountability.nudge_after", 1.0)
	worker := w.spawnUnder(t, asLead, "slow", &v1.SpawnAgentRequest{Task: "do the thing"})
	waitFor(t, "the task rung", func() bool { return w.delivered(t, worker.Id, taskIDTo(t, w, worker.Id)) })
	readInbox(t, w.as(w.token(t, worker.Id)))
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	var nudge *v1.Message
	waitFor(t, "the nudge stored and rung", func() bool {
		msgs, _ := w.d.Store().Read(context.Background()).Inbox(worker.Id, true)
		for _, m := range msgs {
			if strings.HasPrefix(m.Body, "[mesh nudge]") && m.DeliveredAt != nil {
				nudge = m
			}
		}
		return nudge != nil
	})
	if !strings.Contains(nudge.Body, "mesh send parent") {
		t.Errorf("the nudge doesn't say how to report: %q", nudge.Body)
	}
}

func taskIDTo(t *testing.T, w *reopenWorld, agent string) string {
	t.Helper()
	msgs, _ := w.d.Store().Read(context.Background()).Inbox(agent, true)
	for _, m := range msgs {
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK {
			return m.Id
		}
	}
	t.Fatal("no task")
	return ""
}

// A worker that asked for its own tools is idle on purpose until it is reopened, for a wait's length at most; any
// other pending reopen (a switch, an approval) leaves the agent watched, so a hang mid-turn is still nudged.
func TestOnlyAToolsAddedReopenHoldsTheNudge(t *testing.T) {
	pending := func(a *acct, why string, at time.Time) {
		a.t.Helper()
		err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			w, err := tx.GetAgent("a-w1")
			if err != nil {
				return err
			}
			w.PendingReopen = &v1.Reopen{Id: "o-1", Why: why, RequestedAt: timestamppb.New(at)}
			return tx.PutAgent(w)
		})
		if err != nil {
			a.t.Fatal(err)
		}
	}
	for _, c := range []struct {
		why   string
		age   time.Duration
		nudge int
	}{
		{"tools added", 0, 0},
		{"tools added", 5 * time.Hour, 1}, // past accountability.wait_max: the reopen is stuck, not pending
		{"switch", 0, 1},
		{"approve", 0, 1},
	} {
		a := newAcct(t)
		a.setting("accountability.nudge_after", 1.0)
		a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
		a.taskRead()
		pending(a, c.why, time.Now().Add(-c.age))
		time.Sleep(1200 * time.Millisecond)
		a.pass()
		if n := a.mailOf("a-w1", "[mesh nudge]"); n != c.nudge {
			t.Errorf("pending %q requested %s ago: %d nudges, want %d", c.why, c.age, n, c.nudge)
		}
	}
}
