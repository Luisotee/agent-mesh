package daemon_test

// The notes `bin/mesh watch` gave a root that the daemon's accountability pass now gives too (docs/apps.md,
// "Switchover"): evidence on a silent or lost worker, a wait that ended, a prompt nobody answers, a failed turn,
// and what meshd did about a usage limit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// stateDetail puts the worker in a state with a detail and a last activity.
func (a *acct) stateDetail(s v1.AgentState, detail string, at time.Time) {
	a.t.Helper()
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		w, err := tx.GetAgent("a-w1")
		if err != nil {
			return err
		}
		w.State, w.StateDetail, w.LastActivity = s, detail, timestamppb.New(at)
		return tx.PutAgent(w)
	})
	if err != nil {
		a.t.Fatal(err)
	}
}

// onRepo gives the worker the branch of a repo with one commit past its base, not pushed.
func (a *acct) onRepo() {
	a.t.Helper()
	repo, _ := githubBackedRepo(a.t)
	base := runGit(a.t, repo, "rev-parse", "HEAD")
	runGit(a.t, repo, "checkout", "-b", "mesh/"+a.run.Id+"/fix")
	runGit(a.t, repo, "commit", "--allow-empty", "-m", "work")
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		w, err := tx.GetAgent("a-w1")
		if err != nil {
			return err
		}
		w.Repo, w.Worktree, w.Branch, w.BaseCommit = repo, repo, "mesh/"+a.run.Id+"/fix", base
		return tx.PutAgent(w)
	})
	if err != nil {
		a.t.Fatal(err)
	}
}

func (a *acct) alertBody(head string) string {
	a.t.Helper()
	msgs, err := a.d.Store().Read(context.Background()).Inbox("a-root", true)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, head) {
			return m.Body
		}
	}
	return ""
}

func TestASilentWorkersAlertCarriesItsWorkAndTheWaitThatEnded(t *testing.T) {
	a := newAcct(t)
	a.onRepo()
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_WAIT, "CI is running"); err != nil {
		t.Fatal(err)
	}
	// It declared its wait and ended its turn: held, however long it is idle.
	a.pass()
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged during its wait: %d", n)
	}
	// Something woke it (activity after the turn end that declared the wait) and it ended a turn with no report:
	// judged as any silent stop, nudged once, then its parent told.
	a.state(v1.AgentState_AGENT_STATE_WORKING, time.Now())
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("a worker that woke from its wait and stopped silent was nudged %d times, want 1", n)
	}
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	body := a.alertBody("[mesh alert silent a-w1]")
	for _, want := range []string{"ended a turn without reporting", "It had declared a wait", "which is over",
		"Its work: 1 commit(s) past its base, not pushed, no PR, worktree clean."} {
		if !strings.Contains(body, want) {
			t.Errorf("the alert lacks %q:\n%s", want, body)
		}
	}
}

func TestAWaitHoldsWhileTheWorkerIsNotWokenAndAgainstQuietReadings(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_WAIT, "CI is running"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a.pass()
		time.Sleep(600 * time.Millisecond)
	}
	if n := a.mailOf("a-w1", "[mesh nudge]") + a.mailOf("a-root", "[mesh alert"); n != 0 {
		t.Fatalf("a waiting worker nobody woke was chased: %d", n)
	}
}

func TestAPromptNobodyAnswersIsToldToTheParentOncePerPrompt(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.input_after", 1.0)
	a.stateDetail(v1.AgentState_AGENT_STATE_NEEDS_INPUT, "Bash: rm -rf build", time.Now().Add(-5*time.Second))
	a.pass()
	a.pass()
	body := a.alertBody("[mesh alert input a-w1]")
	if a.mailOf("a-root", "[mesh alert input a-w1]") != 1 || !strings.Contains(body, "Bash: rm -rf build") || !strings.Contains(body, "tell the user") {
		t.Fatalf("input alert %q", body)
	}
	// Answered, and a new prompt later: told again. A prompt that is only just up is not.
	a.state(v1.AgentState_AGENT_STATE_WORKING, time.Now())
	a.stateDetail(v1.AgentState_AGENT_STATE_NEEDS_INPUT, "Edit: main.go", time.Now())
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert input a-w1]"); n != 1 {
		t.Fatalf("told about a prompt that just appeared: %d alerts", n)
	}
	a.setting("accountability.input_after", 0.1)
	time.Sleep(200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert input a-w1]"); n != 2 {
		t.Fatalf("a second prompt: %d alerts, want 2", n)
	}
	a.setting("accountability.input_after", 0.0)
	a.stateDetail(v1.AgentState_AGENT_STATE_NEEDS_INPUT, "Write: x", time.Now().Add(-time.Hour))
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert input a-w1]"); n != 2 {
		t.Fatalf("input_after 0 still told: %d", n)
	}
}

func TestAFailedTurnIsToldAtOnceAndADeadLoginIsNotNudged(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	a.stateDetail(v1.AgentState_AGENT_STATE_ERROR, "authentication_failed: please run /login", time.Now())
	a.pass()
	a.pass()
	body := a.alertBody("[mesh alert error a-w1]")
	if a.mailOf("a-root", "[mesh alert error a-w1]") != 1 || !strings.Contains(body, "login expired") || !strings.Contains(body, "/login") {
		t.Fatalf("login alert %q", body)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("a worker with a dead login was nudged: %d", n)
	}
	// Any other failed turn is told too, and is still nudged: a retry may work.
	a.stateDetail(v1.AgentState_AGENT_STATE_ERROR, "overloaded_error", time.Now())
	a.pass()
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if body := a.alertBody("[mesh alert error a-w1]"); !strings.Contains(body, "login expired") {
		t.Errorf("first alert changed: %q", body)
	}
	if n := a.mailOf("a-root", "[mesh alert error a-w1]"); n != 2 {
		t.Errorf("error alerts %d, want 2", n)
	}
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Errorf("an overloaded worker's nudges: %d, want 1", n)
	}
	// One that reported owes nothing.
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	if err := a.send(a.workerTok, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done"); err != nil {
		t.Fatal(err)
	}
	a.stateDetail(v1.AgentState_AGENT_STATE_ERROR, "overloaded_error", time.Now())
	a.pass()
	if n := a.mailOf("a-root", "[mesh alert error a-w1]"); n != 2 {
		t.Errorf("a failed turn after a report was told: %d", n)
	}
}

func TestTheLimitAlertSaysWhatMeshdDoesNext(t *testing.T) {
	a := newAcct(t)
	for auto, want := range map[string]string{
		"off":      "`limits.auto` is off",
		"continue": "continues it on the same model after the reset",
		"switch":   "moves it to the next model of its chain",
	} {
		a.setting("limits.auto", auto)
		a.state(v1.AgentState_AGENT_STATE_WORKING, time.Now())
		time.Sleep(5 * time.Millisecond)
		a.state(v1.AgentState_AGENT_STATE_LIMITED, time.Now())
		a.pass()
		var got string
		msgs, _ := a.d.Store().Read(context.Background()).Inbox("a-root", true)
		for _, m := range msgs {
			if strings.HasPrefix(m.Body, "[mesh alert limited a-w1]") && strings.Contains(m.Body, want) {
				got = m.Body
			}
		}
		if got == "" {
			t.Errorf("limits.auto %s: no alert says %q", auto, want)
		}
	}
}

// The exit alert says what the dead worker left: its commits, whether they are pushed, its PRs, a dirty worktree.
func TestTheExitAlertCarriesWhatTheWorkerLeft(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	crasher := w.spawnUnder(t, asLead, "crasher", &v1.SpawnAgentRequest{})
	if err := os.WriteFile(filepath.Join(crasher.Worktree, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.typeInto(crasher, "/crash\r")
	w.waitState(crasher.Id, v1.AgentState_AGENT_STATE_EXITED)
	head := "[mesh alert exited " + crasher.Id + "]"
	waitFor(t, "the exit alert", func() bool { return w.alerts(t, lead.Id, head) == 1 })
	got := w.alertText(t, lead.Id, head)
	if !strings.Contains(got, "Its work: no commits past its base, worktree dirty.") {
		t.Errorf("exit alert %q", got)
	}
}

// What meshd did about a usage limit is told to the agent's parent, as an info that settles nothing.
func TestTheParentIsToldWhenMeshdMovesAWorkerOffItsLimit(t *testing.T) {
	w := startReopen(t, fastPass)
	w.set(t, "tiers.claude.mid.model", []any{"sonnet", "haiku"})
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "lim", &v1.SpawnAgentRequest{})
	w.typeInto(a, "/limit You've hit your Sonnet limit · resets 11pm (UTC)\r")
	re := w.waitReopened(t, a.Id, a.TerminalId)
	head := "[mesh alert moved " + a.Id + "]"
	waitFor(t, "the note on the move", func() bool { return w.alerts(t, lead.Id, head) == 1 })
	if got := w.alertText(t, lead.Id, head); !strings.Contains(got, "sonnet") || !strings.Contains(got, re.Model) ||
		!strings.Contains(got, "same conversation") {
		t.Errorf("note %q", got)
	}
}
