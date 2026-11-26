package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
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
