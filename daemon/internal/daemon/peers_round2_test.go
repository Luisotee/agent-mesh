package daemon_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A peer resumed into another run by ResumeAgent is still a peer: read-only whatever the task says, and its
// worktree is kept when that run stops.
func TestAPeerResumedIntoAnotherRunStaysAPeerAndKeepsItsWorktree(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Tier: "mid", Body: "first?", Timeout: 60})
	a1 := res.Agents[0]
	w.quiet(t, a1.Id)
	w.say(t, a1.Id, v1.MessageType_MESSAGE_TYPE_DONE, "first answer")
	waitFor(t, "run 1's message", func() bool { return len(w.answers(t)) == 1 })
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: w.run.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Worktree); err != nil {
		t.Fatalf("StopRun removed the peer's worktree: %v", err)
	}
	run2 := startRun(t, w.device.Mesh, "")
	rres, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a1.Id,
		RunId: run2.Id, Task: "Direct task: change the code."}))
	if err != nil {
		t.Fatal(err)
	}
	created := rres.Msg.Agent
	if !created.Peer || created.Role != v1.AgentRole_AGENT_ROLE_ASKER || !created.DenyEdits {
		t.Fatalf("the follow-up is %v", created)
	}
	w.waitState(created.Id, v1.AgentState_AGENT_STATE_IDLE)
	// Its process counts as the peer's: another run's ask is refused while it runs.
	run3 := startRun(t, w.device.Mesh, "")
	_, as3 := w.root(t, run3.Id)
	if _, err := as3.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p.Name, Tier: "mid", Body: "third?", Timeout: 60})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("run 3's ask of a peer that run 2 holds: %v", err)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run2.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Worktree); err != nil {
		t.Errorf("run 2's stop removed the peer's worktree: %v", err)
	}
}

// A peer's model is picked at its launch, so the reserve of that model is checked after the pick: five-hour and
// seven-day windows under the stop do not let an Opus peer past a full Opus window.
func TestAPeerLaunchChecksTheReserveOfTheModelItPicked(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	w.typeInto(w.spawnClaude(t, w.run, "usage", v1.AgentRole_AGENT_ROLE_WORKER),
		`/ratelimits {"seven_day_opus":{"used_percentage":97,"resets_at":"`+reset+`"}}`+"\r")
	waitFor(t, "the Opus window to fill", func() bool {
		return w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), w.run.Id, "claude", "opus", "high", "test", false) != nil
	})
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), w.run.Id, "claude", "", "", "test", false); err != nil {
		t.Fatalf("the guard with no model is the point of this test: %v", err)
	}
	_, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p.Name, Tier: "strong", Body: "first?", Timeout: 60}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "Opus") {
		t.Errorf("an Opus peer past a full Opus window: %v", err)
	}
}

// Something at the peer's worktree path that is not an empty folder is the user's: a file is not removed.
func TestAddingAPeerOverAFileAtItsWorktreePathLeavesTheFile(t *testing.T) {
	w := startPeers(t)
	up := newUpstream(t, "alpha")
	first := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	if _, err := w.asRoot.RemovePeer(context.Background(), connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: first.Name})); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first.Worktree); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Worktree, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.asRoot.AddPeer(context.Background(), connect.NewRequest(&v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g", Name: first.Name})); err == nil {
		t.Fatal("added over a file")
	}
	if b, err := os.ReadFile(first.Worktree); err != nil || string(b) != "mine\n" {
		t.Errorf("the file: %q %v", b, err)
	}
}
