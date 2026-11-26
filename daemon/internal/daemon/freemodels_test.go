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
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// A repo listed in `repos.no_free_models` never gets a zero-cost OpenCode model (freemodels.go): a spawn on a tier
// whose chain is all free, or on a free model named, is refused, and every other repo, kind and paid model is as before.
func TestFreeModelsOffRefusesZeroCostOpenCodeModelsForAListedRepo(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	repo, other := gitRepo(t), gitRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	spawn := func(repo, slug, kind, tier, model string) (*v1.Agent, error) {
		res, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
			RunId: w.run.Id, Repo: repo, Slug: slug, Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: kind, Tier: tier, Model: model,
		}))
		if err != nil {
			return nil, err
		}
		return res.Msg.Agent, nil
	}
	refused := func(err error, what string) {
		t.Helper()
		if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "repos.no_free_models") {
			t.Errorf("%s: want InvalidArgument naming repos.no_free_models, got %v", what, err)
		}
	}

	// Nothing listed: every repo may, as before.
	if _, err := spawn(repo, "before", "opencode", "light", ""); err != nil {
		t.Fatalf("an unlisted repo: %v", err)
	}

	// The repo is listed through a symlink to it: the same folder.
	w.set(t, "repos.no_free_models", []any{link})
	_, err := spawn(repo, "tier", "opencode", "light", "")
	refused(err, "a tier of free models")
	_, err = spawn(repo, "named", "opencode", "", "opencode/beam-free")
	refused(err, "a named free model")
	_, err = spawn(repo, "family", "opencode", "", opencode.FreeFamily)
	refused(err, "the free family")

	// What is not held back: another repo, another kind, and a model the daemon lists as paid (not in the zero-cost list).
	if _, err := spawn(other, "elsewhere", "opencode", "light", ""); err != nil {
		t.Errorf("another repo: %v", err)
	}
	if a, err := spawn(repo, "agy", "agy", "light", ""); err != nil || a.Kind != "agy" {
		t.Errorf("another kind in the listed repo: %v", err)
	}
	if a, err := spawn(repo, "paid", "opencode", "", "opencode/gpt-paid"); err != nil || a.Model != "opencode/gpt-paid" {
		t.Errorf("a paid model in the listed repo: %v (%v)", err, a)
	}

	// A tier with a paid model in its chain gets that one, not the free model before it.
	w.set(t, "tiers.opencode.light.model", []any{"opencode/deepseek-v4.1-flash-free", "opencode/gpt-paid"})
	a, err := spawn(repo, "chain", "opencode", "light", "")
	if err != nil || a.Model != "opencode/gpt-paid" {
		t.Errorf("a tier's chain skips its free models: %v (%v)", err, a)
	}

	// Taken off the list, the repo may again.
	w.set(t, "repos.no_free_models", []any{})
	w.set(t, "tiers.opencode.light.model", []any{"opencode/deepseek-v4.1-flash-free"})
	if _, err := spawn(repo, "after", "opencode", "light", ""); err != nil {
		t.Errorf("a repo taken off the list: %v", err)
	}
}

// A reviewer round leaves OpenCode out for a listed repo (it falls to the next kind of its slot, with a note), and
// takes it back through a paid model of its tier.
func TestFreeModelsOffKeepsOpenCodeOutOfAReviewRound(t *testing.T) {
	w := startReviews(t)
	w.set(t, "review.reviewers.small", []any{[]any{"opencode", "agy"}})
	w.pr.push(t, "the feature")
	w.set(t, "repos.no_free_models", []any{w.pr.repo})
	ctx := context.Background()

	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != "agy" {
		t.Fatalf("a listed repo's slot should fall to agy, got %v (notes %v)", res.Agents, res.Notes)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "keep its code from free models") {
		t.Errorf("the round should say why OpenCode is out: %v", res.Notes)
	}

	// With a paid model on the tier, OpenCode takes the slot again, on that model.
	w.set(t, "tiers.opencode.mid.model", []any{"opencode/deepseek-v4.1-flash-free", "opencode/gpt-paid"})
	run2 := startRun(t, w.device.Mesh, "")
	_, asRoot2 := w.root(t, run2.Id)
	res2, err := asRoot2.Ask(ctx, connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Msg.Agents) != 1 || res2.Msg.Agents[0].Kind != opencode.Kind || res2.Msg.Agents[0].Model != "opencode/gpt-paid" {
		t.Fatalf("a paid model should give the slot back to OpenCode, got %v (notes %v)", res2.Msg.Agents, res2.Msg.Notes)
	}
}

// A running OpenCode agent in a repo that gets listed afterwards can't be switched onto a free model, by name, by
// the `free` family or by walking its chain.
func TestFreeModelsOffRefusesASwitchOntoAFreeModel(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	w.set(t, "tiers.opencode.mid.model", []any{"openai/gpt-5.2", "free"})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "oc-sw", Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	a = w.opencodeTurn(t, a, "hello") // a turn, so the conversation has an id to reopen
	if a.Model != "openai/gpt-5.2" {
		t.Fatalf("started on %q", a.Model)
	}
	w.set(t, "repos.no_free_models", []any{repo})

	switchTo := func(req *v1.SwitchAgentRequest) error {
		req.Agent = a.Id
		_, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(req))
		return err
	}
	for name, req := range map[string]*v1.SwitchAgentRequest{
		"a free model by name": {Model: "opencode/beam-free"},
		"the free family":      {Model: opencode.FreeFamily},
	} {
		if err := switchTo(req); err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "repos.no_free_models") {
			t.Errorf("%s: want InvalidArgument naming repos.no_free_models, got %v", name, err)
		}
	}
	// The chain is [paid, free...]: for this repo it ends at the paid model, so there is nothing to walk to.
	if err := switchTo(&v1.SwitchAgentRequest{Next: true}); err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "last model") {
		t.Errorf("--next: want the end of the chain, got %v", err)
	}
	if got := w.agent(a.Id); got.Model != "openai/gpt-5.2" {
		t.Errorf("the agent moved to %q", got.Model)
	}
	// Another paid model is fine.
	if err := switchTo(&v1.SwitchAgentRequest{Model: "openai/gpt-5.3"}); err != nil {
		t.Errorf("a paid model: %v", err)
	}
}

// "Zero-cost" is what OpenCode itself lists as a model's price (both costs 0), whatever the model's provider, status
// or tool support, not only what the `free` family expands to; a model with no listed price is refused as unknown.
func TestFreeModelsOffGoesByTheCostOpenCodeLists(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	repo := gitRepo(t)
	w.set(t, "repos.no_free_models", []any{repo})
	spawn := func(slug, model string) error {
		_, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
			RunId: w.run.Id, Repo: repo, Slug: slug, Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: opencode.Kind, Model: model,
		}))
		return err
	}
	for model, why := range map[string]string{
		"openrouter/some-model:free": "another provider's zero-cost model",
		"opencode/old-free":          "a zero-cost model that is deprecated",
		"opencode/mute-free":         "a zero-cost model that can't call tools",
	} {
		if err := spawn("z-"+strings.NewReplacer("/", "-", ":", "-").Replace(model), model); err == nil || !strings.Contains(err.Error(), "repos.no_free_models") {
			t.Errorf("%s (%s): want a refusal naming repos.no_free_models, got %v", model, why, err)
		}
	}
	// Unknown cost: listed without a price, and not listed at all.
	for _, model := range []string{"local/mystery", "nowhere/never-listed"} {
		if err := spawn("u-"+strings.NewReplacer("/", "-", ":", "-").Replace(model), model); err == nil || !strings.Contains(err.Error(), "no cost") {
			t.Errorf("%s: want a refusal saying no cost is listed, got %v", model, err)
		}
	}
	if err := spawn("paid", "openai/gpt-5.2"); err != nil {
		t.Errorf("a model listed with a price: %v", err)
	}
}

// A setup whose OpenCode lists no zero-cost model (only paid ones) works for a listed repo: the family's expansion
// being empty does not make the paid models unknowable.
func TestFreeModelsOffAllowsPaidModelsWhenNothingIsFree(t *testing.T) {
	w := startReopen(t, fastPass, func(c *daemon.AgentConfig) {
		c.Env["FAKEOPENCODE_MODELS"] = "openai/gpt-5.2\n{\"id\":\"gpt-5.2\",\"providerID\":\"openai\",\"status\":\"active\",\"cost\":{\"input\":5,\"output\":15},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-20\"}\n"
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	w.set(t, "repos.no_free_models", []any{repo})
	w.set(t, "tiers.opencode.mid.model", []any{"openai/gpt-5.2"})
	for slug, req := range map[string]*v1.SpawnAgentRequest{
		"by-tier":  {Tier: "mid"},
		"by-model": {Model: "openai/gpt-5.2"},
	} {
		req.RunId, req.Repo, req.Slug, req.Kind = run.Id, repo, slug, opencode.Kind
		a, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(req))
		if err != nil || a.Msg.Agent.Model != "openai/gpt-5.2" {
			t.Errorf("%s: %v (%v)", slug, err, a)
		}
	}
}

// An OpenCode root in a listed repo that names no model would run on the CLI's default, which may be a free one: refused;
// naming a paid model is fine.
func TestFreeModelsOffRefusesARootOnTheDefaultModel(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	repo := gitRepo(t)
	w.set(t, "repos.no_free_models", []any{repo})
	spawn := func(req *v1.SpawnAgentRequest) error {
		run := startRun(t, w.device.Mesh, "")
		req.RunId, req.Repo, req.Slug, req.Role, req.Kind = run.Id, repo, "oc-root", v1.AgentRole_AGENT_ROLE_ROOT, opencode.Kind
		_, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(req))
		return err
	}
	for name, req := range map[string]*v1.SpawnAgentRequest{"nothing named": {}, "an effort alone": {Effort: "high"}} {
		if err := spawn(req); err == nil || !strings.Contains(err.Error(), "repos.no_free_models") {
			t.Errorf("a root with %s: want a refusal naming repos.no_free_models, got %v", name, err)
		}
	}
	if err := spawn(&v1.SpawnAgentRequest{Model: "openai/gpt-5.2"}); err != nil {
		t.Errorf("a root on a paid model: %v", err)
	}
}

// An OpenCode agent that stopped on a free model is not brought back onto it once its repo is listed: a resume, or a
// task sent to it, is refused (the send reports why it stayed stopped), and a resume onto a paid model works.
func TestFreeModelsOffRefusesResumingAStoppedAgentOnAFreeModel(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	repo := gitRepo(t)
	a := w.spawnUnder(t, w.asRoot, "oc-stopped", &v1.SpawnAgentRequest{Repo: repo, Kind: opencode.Kind, Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER})
	a = w.opencodeTurn(t, a, "hello") // a turn, so the conversation has an id to resume
	if !strings.HasSuffix(a.Model, "-free") && a.Model != "opencode/big-pickle" {
		t.Fatalf("started on %q, want a zero-cost model", a.Model)
	}
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_RETIRED)
	w.set(t, "repos.no_free_models", []any{repo})

	if _, err := w.asRoot.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id, Task: "Direct task: go on"})); err == nil ||
		!strings.Contains(err.Error(), "repos.no_free_models") {
		t.Errorf("resume on the free model it had: want a refusal naming repos.no_free_models, got %v", err)
	}
	sent, err := w.asRoot.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "one more thing"}))
	if err != nil {
		t.Fatal(err)
	}
	if sent.Msg.Resumed != nil || !strings.Contains(sent.Msg.ResumeError, "repos.no_free_models") {
		t.Errorf("a task to the stopped agent: resumed %v, reason %q", sent.Msg.Resumed, sent.Msg.ResumeError)
	}
	if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("the agent left %v", got.State)
	}
	if _, err := w.asRoot.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id, Task: "Direct task: go on", Model: "openai/gpt-5.2"})); err != nil {
		t.Errorf("a resume onto a paid model: %v", err)
	}
}

// A round-2 review does not send the new head to an OpenCode reviewer of round 1 that ran on a zero-cost model: the
// repo was listed in between, and a fresh reviewer starts on the tier's paid model.
func TestFreeModelsOffDoesNotReuseAFreeOpenCodeReviewer(t *testing.T) {
	w := startReviews(t)
	w.set(t, "review.reviewers.small", []any{[]any{"opencode", "agy"}})
	w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(round1.Agents) != 1 || round1.Agents[0].Kind != opencode.Kind {
		t.Fatalf("round 1: %v", round1.Agents)
	}
	first := round1.Agents[0]
	w.quiet(t, first.Id)
	w.say(t, first.Id, v1.MessageType_MESSAGE_TYPE_DONE, "looks fine")
	waitFor(t, "round 1's answer", func() bool { return len(w.answers(t)) == 1 })

	w.set(t, "repos.no_free_models", []any{w.pr.repo})
	w.set(t, "tiers.opencode.mid.model", []any{"opencode/deepseek-v4.1-flash-free", "opencode/gpt-paid"})
	w.pr.push(t, "the fix")
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(round2.Ask.Members) != 1 {
		t.Fatalf("round 2: %v", round2.Ask.Members)
	}
	m := round2.Ask.Members[0]
	if m.Kind != opencode.Kind || m.How != "fresh" || m.AgentId == first.Id || m.Model != "opencode/gpt-paid" {
		t.Errorf("round 2 member = %v, want a fresh opencode reviewer on opencode/gpt-paid, not agent %s", m, first.Id)
	}
	if !strings.Contains(strings.Join(round2.Notes, "\n"), "repos.no_free_models") {
		t.Errorf("round 2 should say why: %v", round2.Notes)
	}
	w.waitState(first.Id, v1.AgentState_AGENT_STATE_RETIRED) // not left running beside its replacement
}

// A worker's own worktree is the repo of its main checkout: a repo is listed by the main checkout's folder, and a
// spawn or an ask `--repo .` from a linked worktree is held to the same setting.
func TestFreeModelsOffCoversALinkedWorktree(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-b", "side", wt)
	w.set(t, "repos.no_free_models", []any{repo})
	_, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: w.run.Id, Repo: wt, Slug: "from-wt", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: opencode.Kind, Tier: "light",
	}))
	if err == nil || !strings.Contains(err.Error(), "repos.no_free_models") {
		t.Errorf("a spawn from a linked worktree of the listed repo: want a refusal naming repos.no_free_models, got %v", err)
	}
}

// A message that resumes a stopped OpenCode agent reads the model costs first, whatever its type: after a restart
// (a cold cache) an answer to a stopped agent on a paid model in a listed repo still resumes it.
func TestFreeModelsOffResumesAPaidAgentAfterARestartOnAnAnswer(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "paid-oc", Kind: opencode.Kind, Model: "openai/gpt-5.2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.opencodeTurn(t, w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE), "hello")
	w.set(t, "repos.no_free_models", []any{repo})
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_RETIRED)

	w.stop()
	w.agentsHarness = w.restart(t) // nothing has read OpenCode's model list yet
	sent, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "yes, go on"}))
	if err != nil {
		t.Fatal(err)
	}
	if sent.Msg.ResumeError != "" || sent.Msg.Resumed == nil {
		t.Errorf("an answer to a stopped agent on a paid model: resumed %v, reason %q", sent.Msg.Resumed, sent.Msg.ResumeError)
	}
}

// A refused resume writes nothing: a stopped planner on a free model, its repo listed since, gets a direct task and
// stays in its plan phase with no phase event; a running planner's deny list is not lifted onto the free model either.
func TestFreeModelsOffRefusalsLeaveAPlannerAsItWas(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	repo := gitRepo(t)
	phaseEvents := func() int {
		return w.count(t, w.run.Id, func(ev *v1.Event) bool { return ev.GetAgentPhaseChanged() != nil })
	}
	spawn := func(slug string) *v1.Agent {
		res, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
			RunId: w.run.Id, Repo: repo, Slug: slug, Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: opencode.Kind, Tier: "mid", Plan: true, Task: "plan it",
		}))
		if err != nil {
			t.Fatal(err)
		}
		a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
		waitFor(t, slug+"'s conversation id", func() bool { a = w.agent(a.Id); return a.SessionId != "" })
		return a
	}
	running, stopped := spawn("run-plan"), spawn("stop-plan")
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: stopped.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(stopped.Id, v1.AgentState_AGENT_STATE_RETIRED)
	w.set(t, "repos.no_free_models", []any{repo})
	before := phaseEvents()

	sent, err := w.asRoot.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: stopped.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task: build it"}))
	if err != nil {
		t.Fatal(err)
	}
	if sent.Msg.Resumed != nil || !strings.Contains(sent.Msg.ResumeError, "repos.no_free_models") {
		t.Errorf("a direct task to the stopped planner: resumed %v, reason %q", sent.Msg.Resumed, sent.Msg.ResumeError)
	}
	_, err = w.asRoot.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: running.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task: build it"}))
	if err == nil || !strings.Contains(err.Error(), "repos.no_free_models") {
		t.Errorf("a direct task to the running planner: want a refusal naming repos.no_free_models, got %v", err)
	}
	if got := phaseEvents(); got != before {
		t.Errorf("a refusal wrote %d phase event(s)", got-before)
	}
	for _, a := range []*v1.Agent{running, stopped} {
		if cur := w.agent(a.Id); cur.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !cur.DenyEdits {
			t.Errorf("%s left its plan phase: %v, deny edits %v", cur.Name, cur.Phase, cur.DenyEdits)
		}
	}
}

// A switch of effort alone keeps the model, so it is held to the setting as a resume on that model is.
func TestFreeModelsOffRefusesAnEffortOnlySwitchOnAFreeModel(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: repo, Slug: "oc-eff", Kind: opencode.Kind, Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.opencodeTurn(t, w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE), "hello")
	w.set(t, "repos.no_free_models", []any{repo})
	_, err = w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "high"}))
	if err == nil || !strings.Contains(err.Error(), "repos.no_free_models") {
		t.Errorf("an effort-only switch on %s: want a refusal naming repos.no_free_models, got %v", a.Model, err)
	}
}

// An OpenCode agent at its limit whose repo was listed while it waited is not reopened on its free model when the hold
// ends; its parent is told once what to do.
func TestFreeModelsOffTellsTheParentWhenALimitedAgentIsNotReopened(t *testing.T) {
	w := startReviews(t, fastPass)
	ctx := context.Background()
	repo := gitRepo(t)
	w.set(t, "limits.auto", "continue")
	w.set(t, "limits.hold", 2) // seconds
	res, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: w.run.Id, Repo: repo, Slug: "oc-lim", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.opencodeTurn(t, w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE), "hello")
	w.typeInto(a, "/limit-free\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_LIMITED)
	w.set(t, "repos.no_free_models", []any{repo})
	// The recorded hold goes (as when its reset has passed): the pass holds it limits.hold (2 s) and then it is over.
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error { return tx.DeleteModelLimit(opencode.Kind, a.Model) }); err != nil {
		t.Fatal(err)
	}
	notes := func() int {
		msgs, err := w.d.Store().Read(ctx).RunMessages(w.run.Id, "", -1)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range msgs {
			if m.ToId == w.lead.Id && strings.Contains(m.Body, "repos.no_free_models") {
				n++
			}
		}
		return n
	}
	waitFor(t, "the parent's note", func() bool { return notes() > 0 })
	time.Sleep(600 * time.Millisecond) // more passes: still one note
	if n := notes(); n != 1 {
		t.Errorf("the parent was told %d times, want once", n)
	}
	if cur := w.agent(a.Id); cur.State != v1.AgentState_AGENT_STATE_LIMITED || cur.PendingReopen != nil {
		t.Errorf("the agent is %v (reopen %v), want still limited", cur.State, cur.PendingReopen)
	}
}
