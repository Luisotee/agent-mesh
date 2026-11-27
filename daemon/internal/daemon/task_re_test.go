package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// sendRe sends from tok, naming the task it answers (re), and returns the stored message.
func (a *acct) sendRe(tok, to string, typ v1.MessageType, body, re string) (*v1.Message, error) {
	a.t.Helper()
	res, err := a.as(tok).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: to, Type: typ, Body: body, Re: re}))
	if err != nil {
		return nil, err
	}
	return res.Msg.Message, nil
}

// taskID sends the worker a task, has it read it and returns its id.
func (a *acct) taskID(body string) string {
	a.t.Helper()
	m, err := a.sendRe(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_TASK, body, "")
	if err != nil {
		a.t.Fatal(err)
	}
	readInbox(a.t, a.as(a.workerTok))
	return m.Id
}

// A report is stamped with the task it answers, named or not; a task, an info or a report to a child is not.
func TestAReportIsStampedWithTheTaskItAnswers(t *testing.T) {
	a := newAcct(t)
	task := a.taskID("do it")
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION} {
		m, err := a.sendRe(a.workerTok, "parent", typ, "x", "")
		if err != nil || m.Re != task {
			t.Errorf("%s without --re: re %q, err %v; want %s", typ, m.GetRe(), err, task)
		}
	}
	m, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "x", task)
	if err != nil || m.Re != task {
		t.Errorf("done --re: re %q, err %v; want %s", m.GetRe(), err, task)
	}
	// The root's own messages (a task, an answer) and a worker's info are no reports.
	if m, err := a.sendRe(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_ANSWER, "x", ""); err != nil || m.Re != "" {
		t.Errorf("answer: re %q, err %v", m.GetRe(), err)
	}
	if m, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_INFO, "x", ""); err != nil || m.Re != "" {
		t.Errorf("info: re %q, err %v", m.GetRe(), err)
	}
}

// A report for an older task is refused, and says which task is current; the current one goes through.
func TestAReportForAnOlderTaskIsRefused(t *testing.T) {
	a := newAcct(t)
	old := a.taskID("first job")
	cur := a.taskID("second job")
	_, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "late", old)
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), cur) || !strings.Contains(err.Error(), "second job") {
		t.Fatalf("a stale done: %v", err)
	}
	if n := len(msgsOfType(t, a, v1.MessageType_MESSAGE_TYPE_DONE)); n != 0 {
		t.Errorf("%d done messages stored by a refused report", n)
	}
	if _, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "right", cur); err != nil {
		t.Errorf("done for the current task: %v", err)
	}
	// An id that is no task of this agent's is refused the same way.
	if _, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION, "x", "m-nope"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("an unknown task: %v", err)
	}
}

// A second done or failed for a task already reported is refused when it names the task; a new task or an answer
// opens it again, and a report that names no task is as before.
func TestARepeatedDoneIsRefusedUntilAnAnswerOrTask(t *testing.T) {
	a := newAcct(t)
	task := a.taskID("do it")
	if _, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "x", task); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED} {
		_, err := a.sendRe(a.workerTok, "parent", typ, "again", task)
		if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already reported") {
			t.Errorf("%s again: %v", typ, err)
		}
	}
	// Without --re the report goes through, as it always did.
	if _, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "again", ""); err != nil {
		t.Errorf("done without --re: %v", err)
	}
	// An answer opens the work again.
	if err := a.send(a.rootTok, "repo-one", v1.MessageType_MESSAGE_TYPE_ANSWER, "one more fix"); err != nil {
		t.Fatal(err)
	}
	readInbox(t, a.as(a.workerTok))
	if _, err := a.sendRe(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "fixed", task); err != nil {
		t.Errorf("done after an answer: %v", err)
	}
}

// A replay of a report by its mutation id returns the stored message, with its re.
func TestAReplayedReportKeepsItsTask(t *testing.T) {
	a := newAcct(t)
	task := a.taskID("do it")
	send := func() *v1.Message {
		res, err := a.as(a.workerTok).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
			Meta: meta("mut-done-1"), To: "parent", Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "x", Re: task}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Message
	}
	first, second := send(), send()
	if first.Id != second.Id || second.Re != task {
		t.Errorf("replay: %s/%q then %s/%q", first.Id, first.Re, second.Id, second.Re)
	}
}

func msgsOfType(t *testing.T, a *acct, typ v1.MessageType) []*v1.Message {
	t.Helper()
	all, err := a.d.Store().Read(context.Background()).RunMessages(a.run.Id, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.Message
	for _, m := range all {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}
