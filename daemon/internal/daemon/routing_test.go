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

// Approving a planner onto a tier with no effort runs it at the tier's execution effort: Opus executes a plan at
// medium though it planned at high; --effort still wins, and a tier whose execution effort is its own keeps it.
func TestApprovingOntoATierExecutesAtTheTiersExecutionEffort(t *testing.T) {
	for _, tc := range []struct {
		name, tier, effort, wantModel, wantEffort string
	}{
		{"opus executes a plan at medium", "strong", "", "opus", "medium"},
		{"an effort named at approve wins", "strong", "high", "opus", "high"},
		{"sonnet keeps high", "mid", "", "sonnet", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startReopen(t)
			ctx := context.Background()
			run := startRun(t, w.device.Mesh, "")
			_, asLead := w.root(t, run.Id)
			res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix",
				Tier: "strong", Plan: true, Task: "plan the fix", About: "plan a fix"}))
			if err != nil {
				t.Fatal(err)
			}
			worker := res.Msg.Agent
			if worker.Effort != "high" {
				t.Fatalf("a planner on strong plans at high: %v", worker)
			}
			w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
			asWorker := w.as(w.token(t, worker.Id))
			readInbox(t, asWorker)
			if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
				Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
				t.Fatal(err)
			}
			ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: tc.tier, Effort: tc.effort}))
			if err != nil {
				t.Fatal(err)
			}
			if r := ap.Msg.Reopen; r == nil || r.Model != tc.wantModel || r.Effort != tc.wantEffort {
				t.Fatalf("approved onto %s with effort %q: reopen %v, want %s at %s", tc.tier, tc.effort, r, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// A fresh reviewer runs at its CLI's and tier's review effort, not one effort for all: a Claude reviewer on strong at
// medium, one on mid at high, agy's at medium; a named effort wins over every one of them.
func TestAReviewerRunsAtItsCLIsAndTiersReviewEffort(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	w.set(t, "review.reviewers.small", []any{[]any{"claude"}, []any{"agy"}})
	efforts := func(res *v1.AskResponse) map[string]string {
		out := map[string]string{}
		for _, a := range res.Agents {
			out[a.Kind] = a.Effort
		}
		return out
	}
	got := efforts(w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "strong", Reviewers: "auto"}))
	if got["claude"] != "medium" || got["agy"] != "medium" {
		t.Errorf("strong reviewers' efforts = %v, want claude and agy at medium", got)
	}
	w.pr.push(t, "a second head: a fresh round")
	got = efforts(w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"}))
	if got["claude"] != "high" || got["agy"] != "medium" {
		t.Errorf("mid reviewers' efforts = %v, want claude at high and agy at medium", got)
	}
	w.pr.push(t, "a third head")
	got = efforts(w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto", Effort: "high"}))
	if got["claude"] != "high" || got["agy"] != "high" {
		t.Errorf("a named effort = %v, want high for both (agy's own is medium)", got)
	}
	if _, err := w.d.Store().Read(context.Background()).Setting("review.effort"); err == nil {
		t.Error("review.effort is not a setting any more")
	}
}

// A slot that lists agy falls through to the next kind when every model of the tier is held: agy's plan is small, so
// the slot does not name a reviewer the reserve guard would refuse at its start.
func TestAReviewerSlotFallsThroughWhenAgyIsHeld(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	w.set(t, "review.reviewers.large", []any{[]any{"agy", "claude"}})
	until := time.Now().Add(6 * 24 * time.Hour)
	if err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		for _, m := range []string{"neon", "opus", "carbon", "hydrogen"} {
			if err := tx.PutModelLimit("agy", m, until); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "strong", Reviewers: "auto"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != "claude" {
		t.Fatalf("agents %v: the slot should take claude while agy is held", res.Agents)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "all agy strong models are held until") {
		t.Errorf("no note on agy being held: %v", res.Notes)
	}
	// Named by hand it is still asked: the user's word is not the picker's.
	w.pr.push(t, "another head")
	res = w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "strong", Reviewers: "agy"})
	for _, a := range res.Agents {
		if a.Kind == "claude" {
			t.Errorf("a named agy was swapped for claude: %v", res.Agents)
		}
	}
	// A held agy named by hand is asked, or refused at its start by the guard (a note or a failed member): the
	// picker doesn't leave it out silently.
	asked := false
	for _, a := range res.Agents {
		asked = asked || a.Kind == "agy"
	}
	if !asked && len(res.Notes) == 0 && (res.Ask == nil || len(res.Ask.Members) == 0) {
		t.Errorf("a named agy vanished: %v", res)
	}
}

// A reviewer runs at agy's review effort (medium), not the tier's effort the usage cache used to be warmed at: a
// plan at its reserve must still make the slot fall through, though no model name was resolved at medium before.
func TestAReviewerSlotFallsThroughWhenAgyIsAtItsReserveAtTheReviewEffort(t *testing.T) {
	w := startReviews(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage: func(context.Context) (string, error) {
				return "Gemini 4 Neon\tNeon\t5%\t1h\n", nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-neon-high\tGemini 4 Neon (High)\ngemini-4-neon-medium\tGemini 4 Neon (Medium)\n", nil
			},
		}
	})
	w.pr.push(t, "the feature")
	w.set(t, "tiers.agy.strong.model", []any{"neon"})
	w.set(t, "review.reviewers.large", []any{[]any{"agy", "claude"}})
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "strong", Reviewers: "auto"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != "claude" {
		t.Fatalf("agents %v: the slot should take claude while agy is at its reserve", res.Agents)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "agy is at its quota reserve") {
		t.Errorf("no note on agy being at its reserve: %v", res.Notes)
	}
}

// The shipped large-PR slots for a Claude author's own PR: a free model first, then another family (agy) for the
// second slot, and the author's CLI in a fresh session when agy is held (its plan is small).
func TestALargePRsDefaultSlotsTakeAFreeModelThenAnotherFamilyAndFallBackWhenAgyIsHeld(t *testing.T) {
	kindsOf := func(res *v1.AskResponse) []string {
		var out []string
		for _, a := range res.Agents {
			out = append(out, a.Kind)
		}
		return out
	}
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "large", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kindsOf(res), ","); got != "opencode,agy" {
		t.Errorf("a Claude worker's large PR: reviewers %q, want opencode,agy (a free model, then another family)", got)
	}
	if res.Ask.Tier != "mid" {
		t.Errorf("a round that names no tier takes review.tier.large = mid, got %q", res.Ask.Tier)
	}

	b := startAuthor(t, "")
	b.pr.push(t, "the feature")
	until := time.Now().Add(6 * 24 * time.Hour)
	if err := b.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		for _, m := range []string{"hydrogen", "neon", "opus", "carbon"} {
			if err := tx.PutModelLimit("agy", m, until); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err = b.ask(t, &v1.AskRequest{Review: "7", Size: "large", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kindsOf(res), ","); got != "opencode,claude" {
		t.Errorf("with agy held: reviewers %q, want opencode,claude (the author's CLI, fresh)", got)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "all agy mid models are held until") {
		t.Errorf("no note on agy being held: %v", res.Notes)
	}
}
