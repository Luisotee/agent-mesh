package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termhost"
)

// quickRestart shortens the timings of the resume after a restart: the gap between two resumes, the retry of a refused
// one and the window it is retried in.
func quickRestart(c *daemon.AgentConfig) {
	c.RestartGap, c.RestartRetry, c.RestartWindow = 50*time.Millisecond, 100*time.Millisecond, 700*time.Millisecond
}

// machineRestart is a reboot: the daemon stops, the terminal host dies with every terminal in it, and a new daemon
// starts over the same state.
func (w *reopenWorld) machineRestart(t *testing.T) {
	t.Helper()
	w.stop()
	stopTermHost(t, w.state)
	waitFor(t, "the host to stop", func() bool {
		return !fsutil.Held(termhost.Paths{Dir: statedir.Paths{Dir: w.state}.Term()}.Lock())
	})
	w.agentsHarness = w.restart(t)
}

// stopped is an agent that has no process.
func stopped(a *v1.Agent) bool {
	return a.State == v1.AgentState_AGENT_STATE_EXITED || a.State == v1.AgentState_AGENT_STATE_RETIRED
}

// waitResumed is waitReopened (it is running again, not necessarily at its prompt: the info it is told starts a turn) that says, when the agent does not come back, what became of it and what its parent was told.
func (w *reopenWorld) waitResumed(t *testing.T, a *v1.Agent, parent *v1.Agent, old string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := w.agent(a.Id)
		if got.TerminalId != "" && got.TerminalId != old && got.PendingReopen == nil && !stopped(got) {
			return
		}
		if time.Now().After(deadline) {
			var told []string
			for _, m := range w.mailStarting(t, parent.Id, "[mesh alert") {
				told = append(told, m.Body)
			}
			t.Fatalf("%s not resumed: state %v, terminal %q (was %q), reopen %v; the parent was told %q", a.Name, got.State, got.TerminalId, old, got.PendingReopen, told)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mailStarting is the messages of an agent's inbox (read or not) whose body starts with prefix, oldest first.
func (w *reopenWorld) mailStarting(t *testing.T, agentID, prefix string) []*v1.Message {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).Inbox(agentID, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.Message
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, prefix) {
			out = append(out, m)
		}
	}
	return out
}

// A machine restart brings the run back by itself: the root first, then a worker that owed its report, each told what
// died with the machine; a worker that had reported stays stopped; the parent is told once that a worker is back, and not
// that it was lost.
func TestAMachineRestartResumesTheRunRootFirstAndTellsEachWhatDied(t *testing.T) {
	w := startReopen(t, quickRestart)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "doing", &v1.SpawnAgentRequest{Task: "do the thing"})
	reporter := w.spawnUnder(t, asLead, "reported", &v1.SpawnAgentRequest{Task: "do the other thing"})
	w.sendToParent(t, reporter, v1.MessageType_MESSAGE_TYPE_DONE)
	oldTerminals := map[string]string{lead.Id: w.agent(lead.Id).TerminalId, worker.Id: w.agent(worker.Id).TerminalId}

	w.machineRestart(t)

	w.waitResumed(t, lead, lead, oldTerminals[lead.Id])
	w.waitResumed(t, worker, lead, oldTerminals[worker.Id])
	infoOf := func(id string) []*v1.Message { return w.mailStarting(t, id, "[mesh restart]") }
	for _, a := range []*v1.Agent{lead, worker} {
		if got := infoOf(a.Id); len(got) != 1 || !strings.Contains(got[0].Body, "Re-run it") {
			t.Errorf("%s was told %v", a.Name, got)
		}
	}
	waitFor(t, "the worker's info rung", func() bool { return w.delivered(t, worker.Id, infoOf(worker.Id)[0].Id) })
	if l, wk := infoOf(lead.Id)[0], infoOf(worker.Id)[0]; l.CreatedAt.AsTime().After(wk.CreatedAt.AsTime()) {
		t.Errorf("the worker was resumed before the root: %v then %v", wk.CreatedAt.AsTime(), l.CreatedAt.AsTime())
	}
	if got := w.agent(reporter.Id); got.State != v1.AgentState_AGENT_STATE_EXITED || len(infoOf(reporter.Id)) != 0 {
		t.Errorf("a worker that had reported was resumed: %v, told %v", got.State, infoOf(reporter.Id))
	}
	if n := w.alerts(t, lead.Id, "[mesh alert exited "+worker.Id+"]"); n != 0 {
		t.Errorf("the parent was told it lost a worker that was resumed: %d", n)
	}
	if n := w.alerts(t, lead.Id, "[mesh alert resumed "+worker.Id+"]"); n != 1 {
		t.Errorf("the parent was told of the resume %d times", n)
	}
	if n := w.alerts(t, lead.Id, "[mesh alert exited "+reporter.Id+"]"); n != 0 {
		t.Errorf("a worker that had reported was alerted about: %d", n)
	}
}

// With the setting off nothing is resumed, and the parent hears of the loss as it did.
func TestAMachineRestartResumesNothingWhenTheSettingIsOff(t *testing.T) {
	w := startReopen(t, quickRestart)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "doing", &v1.SpawnAgentRequest{Task: "do the thing"})
	w.set(t, "restart.resume", false)

	w.machineRestart(t)

	waitFor(t, "the loss told", func() bool { return w.alerts(t, lead.Id, "[mesh alert exited "+worker.Id+"]") == 1 })
	time.Sleep(500 * time.Millisecond)
	for _, id := range []string{lead.Id, worker.Id} {
		if got := w.agent(id); got.State != v1.AgentState_AGENT_STATE_EXITED {
			t.Errorf("%s is %v", got.Name, got.State)
		}
	}
}

// A reviewer lost in a restart is resumed with its round still open, and the round finishes when it answers.
func TestAReviewerLostInARestartIsResumedAndItsRoundStillFinishes(t *testing.T) {
	w := startReviews(t, quickRestart)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	old := map[string]string{}
	for _, a := range res.Agents {
		w.quiet(t, a.Id)
		old[a.Id] = w.agent(a.Id).TerminalId
	}

	w.machineRestart(t)

	for _, a := range res.Agents {
		w.waitReopened(t, a.Id, old[a.Id])
		if got := w.mailStarting(t, a.Id, "[mesh restart]"); len(got) != 1 || !strings.Contains(got[0].Body, "(ask "+res.Ask.Id+") is still open") {
			t.Errorf("%s was told %v", a.Name, got)
		}
	}
	if len(w.answers(t)) != 0 {
		t.Fatalf("the round finished with its members lost: %v", w.answers(t))
	}
	for _, a := range res.Agents {
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, a.Kind+": fine")
	}
	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	if body := w.answers(t)[0].Body; !strings.Contains(body, "2 of 2 reviewers answered") {
		t.Errorf("message %s", body)
	}
}

// A member the restart lost and that cannot be resumed (here the host's cap is 1, and the root took its place) reads
// `lost`, not `settled`, and its asker is told in the round's message, which says how to bring it back.
func TestAReviewerThatCannotBeResumedAfterARestartSettlesLostAndItsAskerIsTold(t *testing.T) {
	w := startReviews(t, quickRestart)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	for _, a := range res.Agents {
		w.quiet(t, a.Id)
	}
	w.set(t, "caps.agents", 1)

	w.machineRestart(t)

	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	body := w.answers(t)[0].Body
	for _, want := range []string{"0 of 2 reviewers answered", "LOST, its answer is not coming", "`mesh resume ", "this round didn't count"} {
		if !strings.Contains(body, want) {
			t.Errorf("message lacks %q: %s", want, body)
		}
	}
	ask, err := w.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ask.Members {
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_LOST || m.Type != "lost" {
			t.Errorf("member %s: %v %q", m.Name, m.Status, m.Type)
		}
	}
	for _, a := range res.Agents {
		if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_EXITED {
			t.Errorf("%s is %v", got.Name, got.State)
		}
	}
}

// What is still waiting to be resumed when the daemon stops is told to its parent as lost, since the hold on its alert is in
// memory: a daemon started again finds the agent stopped and would never tell.
func TestWhatIsNotResumedWhenTheDaemonStopsIsToldToItsParent(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.RestartGap, c.RestartRetry, c.RestartWindow = 50*time.Millisecond, 100*time.Millisecond, time.Hour
	})
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "doing", &v1.SpawnAgentRequest{Task: "do the thing"})
	w.set(t, "caps.agents", 1)
	oldLead := w.agent(lead.Id).TerminalId

	w.machineRestart(t)
	w.waitResumed(t, lead, lead, oldLead)
	time.Sleep(500 * time.Millisecond) // the worker is refused by the cap, and tried again every 100 ms
	if n := w.alerts(t, lead.Id, "[mesh alert exited "+worker.Id+"]"); n != 0 {
		t.Fatalf("told before the window ended: %d", n)
	}
	w.stop()
	w.agentsHarness = w.restart(t)
	got := w.alertText(t, lead.Id, "[mesh alert exited "+worker.Id+"]")
	if !strings.Contains(got, "could not be resumed (meshd stopped before it could be resumed") {
		t.Errorf("the parent was told %q", got)
	}
}
