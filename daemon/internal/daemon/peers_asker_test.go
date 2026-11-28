package daemon_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// patchPeer changes a registered peer's record, as an older daemon's registry could hold it (a peer in the clone's
// own checkout, a worktree outside the daemon's own).
func (w *peerWorld) patchPeer(t *testing.T, name string, fn func(*v1.Peer)) {
	t.Helper()
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		p, err := tx.GetPeer(name)
		if err != nil {
			return err
		}
		fn(p)
		return tx.PutPeer(p)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A peers ask that names no tier and no model runs its askers on the asker role (`roles.asker.*`); naming any of
// them keeps the tier path, and each ask of a peer is a fresh asker named after it, then `<peer>-2`.
func TestAnAskOfPeersTakesTheAskerRoleUnlessTheAskNamesATierAKindOrAModel(t *testing.T) {
	w := startPeers(t)
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g", About: "alpha is for queues"})
	ask := func(req *v1.AskRequest) *v1.Agent {
		t.Helper()
		req.Peers, req.Body, req.NoRefresh = p.Name, "q", true
		return w.ask(t, req).Agents[0]
	}
	// The role's first CLI is OpenCode: the first model of its list the (fake) listing has, at high.
	a := ask(&v1.AskRequest{})
	if a.Name != p.Name || a.Kind != "opencode" || !strings.HasPrefix(a.Model, "opencode/") || a.Effort != "high" || !a.Peer || a.About != "alpha is for queues" {
		t.Errorf("the role's asker %v", a)
	}
	// With OpenCode left out of the role, Claude's Sonnet at medium.
	w.set(t, "roles.asker.kinds", []any{"claude"})
	// A second ask of the same peer, in the same run, is another fresh asker: no waiting for the first.
	if b := ask(&v1.AskRequest{}); b.Name != p.Name+"-2" || b.Id == a.Id || b.Kind != "claude" || b.Model != "sonnet" || b.Effort != "medium" || b.About != "alpha is for queues" {
		t.Errorf("a second asker %v", b)
	}
	if b := ask(&v1.AskRequest{Effort: "high"}); b.Name != p.Name+"-3" || b.Model != "sonnet" || b.Effort != "high" {
		t.Errorf("an effort named %v", b)
	}
	// A tier is the tier path; a kind alone narrows the role to that CLI and its own list.
	if c := ask(&v1.AskRequest{Tier: "strong"}); c.Name != p.Name+"-4" || c.Model != "opus" || c.Effort != "high" {
		t.Errorf("a named tier %v", c)
	}
	w.set(t, "roles.asker.claude.model", []any{"haiku"})
	if d := ask(&v1.AskRequest{Kind: "claude"}); d.Model != "haiku" || d.Effort != "medium" {
		t.Errorf("a named kind %v", d)
	}
	if e := ask(&v1.AskRequest{Model: "opus"}); e.Model != "opus" {
		t.Errorf("a named model %v", e)
	}
	// The role walks its CLIs in order: agy first when the host says so.
	w.set(t, "roles.asker.kinds", []any{"agy", "claude"})
	if f := ask(&v1.AskRequest{}); f.Kind != "agy" || f.Model != "hydrogen" || f.Effort != "high" {
		t.Errorf("the role's second CLI %v", f)
	}
	// None can take it: the error says why of each.
	w.set(t, "roles.asker.kinds", []any{"copilot"})
	_, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Peers: p.Name, Body: "q", NoRefresh: true}))
	if err == nil || !strings.Contains(err.Error(), "roles.asker.kinds") || !strings.Contains(err.Error(), "copilot: not installed") {
		t.Errorf("no CLI of the role: %v", err)
	}
}

// A plain question to a repo runs on the asker role too.
func TestAPlainAskNamingNoTierRunsOnTheAskerRole(t *testing.T) {
	w := startReviews(t)
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Body: "Where is the retry logic?"})
	if a := res.Agents[0]; a.Kind != "opencode" || a.Effort != "high" {
		t.Errorf("the plain asker %v", a)
	}
	w.set(t, "roles.asker.kinds", []any{"claude"})
	if a := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Body: "Where is the retry logic, then?"}).Agents[0]; a.Kind != "claude" || a.Model != "sonnet" || a.Effort != "medium" {
		t.Errorf("the plain asker on Claude %v", a)
	}
	// A tier keeps today's path.
	if a := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Tier: "strong", Body: "Where is the retry logic, really?"}).Agents[0]; a.Model != "opus" {
		t.Errorf("a named tier %v", a)
	}
	if _, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Body: "q", Continue: true})); err == nil || !strings.Contains(err.Error(), "continue is for peers") {
		t.Errorf("continue without peers: %v", err)
	}
}

// --continue stays on its conversation's CLI, and a peer with no conversation is asked fresh and told so.
func TestContinuingAPeerStaysOnItsCLIAndAnUnaskedPeerIsAskedFresh(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	w.set(t, "roles.asker.kinds", []any{"claude"})
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Continue: true, Body: "first", NoRefresh: true})
	if m := res.Ask.Members[0]; m.How != "fresh (it has no earlier conversation to continue)" || m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
		t.Errorf("a peer never asked: %v", m)
	}
	first := res.Agents[0]
	w.quiet(t, first.Id)
	w.say(t, first.Id, v1.MessageType_MESSAGE_TYPE_DONE, "one")
	waitFor(t, "the first answer", func() bool { return len(w.answers(t)) == 1 })
	w.quiet(t, first.Id)
	// Its conversation is claude's: another CLI is refused, and nothing was started.
	_, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p.Name, Continue: true, Kind: "agy", Body: "second", NoRefresh: true}))
	if err == nil || !strings.Contains(err.Error(), "its last conversation is on claude") {
		t.Errorf("--continue on another CLI: %v", err)
	}
	// On its own CLI it goes back to the same asker of this run, given the question.
	again := w.ask(t, &v1.AskRequest{Peers: p.Name, Continue: true, Body: "second", NoRefresh: true})
	if again.Agents[0].Id != first.Id || again.Ask.Members[0].How != "continued" {
		t.Errorf("continued %v, how %q", again.Agents[0], again.Ask.Members[0].How)
	}
	// Without it the same peer gets a new asker.
	w.quiet(t, first.Id)
	w.say(t, first.Id, v1.MessageType_MESSAGE_TYPE_DONE, "two")
	waitFor(t, "the second round's message", func() bool { return len(w.answers(t)) == 2 })
	other := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "third", NoRefresh: true})
	if other.Agents[0].Id == first.Id || other.Agents[0].Name != p.Name+"-2" {
		t.Errorf("a fresh ask went to %v", other.Agents[0])
	}
}

// A checkout another run's asker is reading is not moved under it: the new asker is told, and reads the newer code
// with git show.
func TestACheckoutAnotherRunsAskerReadsIsNotMoved(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	head := up.git(t, "rev-parse", "HEAD")
	w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "first"}) // run 1's asker stays up
	next := up.commit(t, "a.go", "package a")
	run2 := startRun(t, w.device.Mesh, "")
	_, asRoot2 := w.root(t, run2.Id)
	res, err := asRoot2.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p.Name, Body: "second"}))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := w.d.Store().Read(ctx).Inbox(res.Msg.Agents[0].Id, true)
	if err != nil || len(inbox) == 0 {
		t.Fatalf("inbox %v %v", inbox, err)
	}
	task := inbox[0].Body
	for _, want := range []string{"Upstream moved since you last looked", "Another asker (" + p.Name + ", run " + w.run.Id + ") is reading this checkout, so it stays at " + head[:9],
		"git show " + next[:9] + ":<path>", "Your checkout is at " + head[:9] + "."} {
		if !strings.Contains(task, want) {
			t.Errorf("the second asker's task lacks %q:\n%s", want, task)
		}
	}
	if out := gitOut(t, p.Worktree, "rev-parse", "HEAD"); out != head {
		t.Errorf("the checkout moved to %s under the asker reading it", out)
	}
	// The checkout stayed behind, so the peer has not "seen" the new head: a later ask moves it and says so until it does.
	if got := w.peer(t, p.Name); got.Seen != head {
		t.Errorf("seen %s, want %s (the head the checkout is at)", got.Seen, head)
	}
}

// A peer an older daemon registered in the clone's own checkout keeps working: its checkout is never moved or
// removed, and a clone that is gone is said so.
func TestAPeerInTheClonesOwnCheckoutIsNeverMovedOrRemoved(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "inclone")
	clone := cloneOf(t, up)
	seen := up.git(t, "rev-parse", "HEAD")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: clone, Name: "inclone-research", Group: "g"})
	w.patchPeer(t, p.Name, func(p *v1.Peer) { p.Worktree, p.InClone = clone, true })
	p2 := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Name: "vanish-research", Group: "g"})
	gone := cloneOf(t, up)
	w.patchPeer(t, p2.Name, func(p *v1.Peer) { p.Worktree, p.InClone = gone, true })
	moved := up.commit(t, "new.go", "package x")
	before, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "What is new?"})
	task := w.task(t, res.Agents[0].Id)
	if !strings.Contains(task, "Upstream moved since you last looked") || !strings.Contains(task, "clone's own checkout, which stays at "+seen[:9]) || !strings.Contains(task, "git show "+moved[:9]) {
		t.Errorf("the note %s", task)
	}
	if after, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output(); !bytes.Equal(before, after) {
		t.Errorf("the clone's checkout moved: %s -> %s", before, after)
	}
	// An agy asker would write its hooks into the user's checkout: refused for such a peer.
	if _, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p.Name, Body: "q", Kind: "agy"})); err == nil || !strings.Contains(err.Error(), "agy asker's hooks") {
		t.Errorf("an agy asker in the clone's own checkout: %v", err)
	}
	// Removing the peer leaves the clone and its checkout, once its askers are stopped.
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: w.run.Id})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the peer's asker stopped", func() bool {
		a := w.agent(res.Agents[0].Id)
		return a.State == v1.AgentState_AGENT_STATE_RETIRED || a.State == v1.AgentState_AGENT_STATE_EXITED
	})
	if _, err := w.device.Mesh.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: p.Name})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Errorf("removing the peer took the clone's checkout: %v", err)
	}
	// A clone that is gone is said so, not a git error from somewhere later.
	os.RemoveAll(gone)
	run2 := startRun(t, w.device.Mesh, "") // the stopped run's root is no more
	_, asRoot2 := w.root(t, run2.Id)
	_, err := asRoot2.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: p2.Name, Body: "hello"}))
	if err == nil || !strings.Contains(err.Error(), "clone's own checkout") || !strings.Contains(err.Error(), "not there") {
		t.Errorf("ask of a peer whose checkout is gone: %v", err)
	}
}

// Removing a peer whose worktree is not the daemon's own (an older registry's, bin/mesh's) leaves it, and one that
// another peer still works in.
func TestRemovingAPeerLeavesAWorktreeThatIsNotTheDaemonsOrThatAnotherPeerSharesAlone(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "shared")
	clone := cloneOf(t, up)
	wt, dirty := filepath.Join(t.TempDir(), "shared-wt"), filepath.Join(t.TempDir(), "dirty-wt")
	worktreeOf(t, clone, wt)
	worktreeOf(t, clone, dirty)
	os.WriteFile(filepath.Join(dirty, "wip.txt"), []byte("uncommitted"), 0o644)
	exec.Command("git", "-C", dirty, "add", "wip.txt").Run()
	for _, n := range []string{"one-research", "two-research", "dirty-research"} {
		w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: clone, Name: n, Group: "g"})
	}
	for _, n := range []string{"one-research", "two-research"} {
		w.patchPeer(t, n, func(p *v1.Peer) { p.Worktree = wt })
	}
	w.patchPeer(t, "dirty-research", func(p *v1.Peer) { p.Worktree = dirty })
	remove := func(n string) {
		t.Helper()
		if _, err := w.device.Mesh.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: n})); err != nil {
			t.Fatal(err)
		}
	}
	remove("one-research")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("removing one peer took the folder the other still works in: %v", err)
	}
	remove("two-research")
	remove("dirty-research")
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("a folder outside the daemon's worktrees was removed with its last peer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirty, "wip.txt")); err != nil {
		t.Errorf("uncommitted work in a worktree outside the daemon's was deleted: %v", err)
	}
}

// An asker of this run that is still answering holds the checkout as well: a second ask of the peer does not move it.
func TestAnAskerOfThisRunStillAnsweringHoldsTheCheckout(t *testing.T) {
	w := startPeers(t)
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	head := up.git(t, "rev-parse", "HEAD")
	first := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "first"}).Agents[0] // starting: no answer yet
	next := up.commit(t, "a.go", "package a")
	second := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "second"}).Agents[0]
	if second.Id == first.Id || second.Name != p.Name+"-2" {
		t.Fatalf("second asker %v", second)
	}
	if task := w.task(t, second.Id); !strings.Contains(task, "Another asker ("+p.Name+", run "+w.run.Id+") is reading this checkout") || !strings.Contains(task, "git show "+next[:9]) {
		t.Errorf("the second asker's task:\n%s", task)
	}
	if out := gitOut(t, p.Worktree, "rev-parse", "HEAD"); out != head {
		t.Errorf("the checkout moved to %s under the first asker", out)
	}
}

// A retired asker is brought back by a task while another asker of the peer runs in the same checkout.
func TestAskingARetiredAskerWorksWhileASiblingRunsInTheSameCheckout(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	w.set(t, "roles.asker.kinds", []any{"claude"})
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	first := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "first", NoRefresh: true}).Agents[0]
	w.quiet(t, first.Id)
	w.say(t, first.Id, v1.MessageType_MESSAGE_TYPE_DONE, "one")
	waitFor(t, "the answer", func() bool { return len(w.answers(t)) == 1 })
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: first.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(first.Id, v1.AgentState_AGENT_STATE_RETIRED)
	sibling := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "second", NoRefresh: true}).Agents[0]
	if sibling.Name != p.Name+"-2" {
		t.Fatalf("sibling %v", sibling)
	}
	res := w.ask(t, &v1.AskRequest{Agent: first.Id, Body: "third"})
	if len(res.Agents) != 1 || res.Agents[0].Id != first.Id || !strings.Contains(strings.Join(res.Notes, "|"), "resumed in its own conversation") {
		t.Errorf("asking the retired asker beside its sibling: %v %v", res.Agents, res.Notes)
	}
}

// The role walks past a Claude model at its quota reserve to the next model of its list.
func TestTheAskerRoleWalksPastAClaudeModelAtItsReserve(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	w.set(t, "roles.asker.kinds", []any{"claude"})
	w.set(t, "roles.asker.claude.model", []any{"opus", "sonnet"})
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	w.typeInto(w.spawnClaude(t, w.run, "usage", v1.AgentRole_AGENT_ROLE_WORKER),
		`/ratelimits {"seven_day_opus":{"used_percentage":97,"resets_at":"`+reset+`"}}`+"\r")
	waitFor(t, "the Opus window to fill", func() bool {
		return w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), w.run.Id, "claude", "opus", "high", "test", false) != nil
	})
	if a := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "q", NoRefresh: true}).Agents[0]; a.Model != "sonnet" {
		t.Errorf("the asker took %s past a full Opus window", a.Model)
	}
}

// Two agy askers don't share a checkout (agy's hooks are per checkout): the role falls through to the next CLI.
func TestASecondAgyAskerOfAPeerFallsThroughToTheNextCLI(t *testing.T) {
	w := startPeers(t)
	w.set(t, "roles.asker.kinds", []any{"agy", "claude"})
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	if a := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "first", NoRefresh: true}).Agents[0]; a.Kind != "agy" {
		t.Fatalf("first asker %v", a)
	}
	if b := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "second", NoRefresh: true}).Agents[0]; b.Kind != "claude" {
		t.Errorf("a second asker beside a live agy one: %v", b)
	}
}

// An asker given the question again is not the holder of its own checkout (its first ask timed out with no answer).
func TestContinuingAnAskerThatDidNotAnswerDoesNotHoldItsOwnCheckout(t *testing.T) {
	w := startPeers(t)
	w.set(t, "roles.asker.kinds", []any{"claude"})
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	first := w.ask(t, &v1.AskRequest{Peers: p.Name, Body: "first", Timeout: 1}).Agents[0]
	w.quiet(t, first.Id) // idle, and no answer given
	waitFor(t, "the first ask's timeout", func() bool { return len(w.answers(t)) == 1 })
	next := up.commit(t, "a.go", "package a")
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Continue: true, Body: "again", Timeout: 60})
	if m := res.Ask.Members[0]; m.AgentId != first.Id {
		t.Fatalf("not given to the same asker: %v", m)
	}
	if task := w.task(t, first.Id); strings.Contains(task, "is reading this checkout") || !strings.Contains(task, "Your checkout is at "+next[:9]) {
		t.Errorf("the asker was told it holds its own checkout:\n%s", task)
	}
}

// A peer's conversation is the registry's the moment it answers, while the other peers of its round still work.
func TestAPeerThatAnsweredIsContinuableBeforeTheRestOfItsRoundFinishes(t *testing.T) {
	w := startPeers(t)
	w.set(t, "roles.asker.kinds", []any{"claude"})
	pa := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: newUpstream(t, "alpha").url(), Group: "g"})
	w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: newUpstream(t, "beta").url(), Group: "g"})
	res := w.ask(t, &v1.AskRequest{Peers: "g", Body: "q", NoRefresh: true})
	var alpha *v1.Agent
	for _, a := range res.Agents {
		if a.Name == pa.Name {
			alpha = a
		}
	}
	w.quiet(t, alpha.Id)
	w.say(t, alpha.Id, v1.MessageType_MESSAGE_TYPE_DONE, "alpha answers")
	waitFor(t, "alpha's conversation in the registry", func() bool { return w.peer(t, pa.Name).SessionId == alpha.SessionId })
	if len(w.answers(t)) != 0 {
		t.Errorf("the round finished with beta still pending")
	}
}
