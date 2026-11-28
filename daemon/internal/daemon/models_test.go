package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

const (
	agyListingBase = "gemini-4-neon-high\tGemini 4 Neon (High)\n" +
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\n" +
		"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n" +
		"gemini-4-hydrogen-medium\tGemini 4 Hydrogen (Medium)\n" +
		"gemini-4-carbon-high\tGemini 4 Carbon (High)\n" +
		"gemini-4-carbon-medium\tGemini 4 Carbon (Medium)\n" +
		"gemini-3.10-flash-low\tGemini 3.10 Flash (Low)\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n" +
		"claude-opus-5-5-medium\tClaude Opus 5.5 (Medium)\n" +
		"claude-sonnet-5-5-high\tClaude Sonnet 5.5 (High)\n" +
		"gemini-4-argon-high\tGemini 4 Argon (High)\n"
	agyListingNew = agyListingBase + "gemini-5-xenon-high\tGemini 5 Xenon (High)\n"
)

const (
	opencodeListingBase = "opencode/deepseek-v4.1-flash-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-20\"}\n" +
		"opencode/flint-40b-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-18\"}\n" +
		"opencode/hy4-flash-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-19\"}\n" +
		"opencode/seaglass-alpha-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-17\"}\n" +
		"opencode/mimo-v2.6-flash-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-16\"}\n" +
		"opencode/beam-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-15\"}\n"
	opencodeListingNew = opencodeListingBase +
		"opencode/nova-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-25\"}\n"
)

func startModelsHarness(t *testing.T, agyListing, ocListing *string, now *time.Time) *harness {
	t.Helper()
	home := t.TempDir()
	ag := agy.New()
	if agyListing != nil {
		ag.ListModels = func(context.Context, string, map[string]string) (string, error) {
			return *agyListing, nil
		}
	}
	oc := opencode.New()
	if ocListing != nil {
		oc.ListModels = func(context.Context, string, map[string]string) (string, error) {
			return *ocListing, nil
		}
	}
	state := shortdir.State(t)
	d, err := daemon.New(daemon.Config{
		StateDir: state,
		TCP:      "127.0.0.1:0",
		Now: func() time.Time {
			if now != nil {
				return *now
			}
			return time.Now()
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: daemon.AgentConfig{
			Adapters: []adapter.Adapter{claude.New(), ag, oc},
			LookPath: func(string) (string, bool) { return "", false },
			Env:      map[string]string{"HOME": home},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, d, state)
}

func kindEntry(res *v1.CheckModelsResponse, kind string) *v1.KindModels {
	for _, k := range res.Kinds {
		if k.Kind == kind {
			return k
		}
	}
	return nil
}

func TestModelsFlagsNewAndRetiredFamiliesAndPersistsAck(t *testing.T) {
	listing := agyListingNew
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	h := startModelsHarness(t, &listing, nil, &now)
	ctx := context.Background()

	// Fresh check picks up argon, flash, sonnet, xenon (not in agy tiers).
	res, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{
		Meta: meta("m1"), Check: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ak := kindEntry(res.Msg, "agy")
	if ak == nil || !slices.Equal(ak.NewFamilies, []string{"argon", "flash", "sonnet", "xenon"}) || len(ak.RetiredFamilies) != 0 {
		t.Fatalf("agy = %+v", ak)
	}
	if !strings.Contains(res.Msg.Notice, "new agy models no tier names: argon, flash, sonnet, xenon") {
		t.Fatalf("notice = %q", res.Msg.Notice)
	}

	// StartRun picks up the cached model notice without re-listing.
	listing = agyListingBase
	sr, err := h.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: meta("sr1"), Title: "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sr.Msg.ModelNotice, "xenon") {
		t.Fatalf("StartRun.ModelNotice = %q", sr.Msg.ModelNotice)
	}

	// Without --check within 24h, cached result is reused.
	cached, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("m2")}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kindEntry(cached.Msg, "agy").NewFamilies, []string{"argon", "flash", "sonnet", "xenon"}) {
		t.Fatalf("expected cached result, got %+v", kindEntry(cached.Msg, "agy"))
	}

	// --ack records decisions and forces a re-check.
	listing = agyListingNew
	acked, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{
		Meta: meta("m3"), Ack: []string{"agy:sonnet", "agy:argon", "agy:flash"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kindEntry(acked.Msg, "agy").NewFamilies, []string{"xenon"}) {
		t.Fatalf("after ack: %+v", kindEntry(acked.Msg, "agy"))
	}
	if !strings.Contains(acked.Msg.Notice, "new agy models no tier names: xenon.") {
		t.Fatalf("notice after ack = %q", acked.Msg.Notice)
	}

	// Place xenon in a tier -> argon/carbon/neon stay acked or in tiers; then remove xenon from listing -> retired!
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta("s1"), Key: "tiers.agy.strong.model", Value: val(t, []any{"xenon", "opus"}),
	})); err != nil {
		t.Fatal(err)
	}
	listing = agyListingBase
	retired, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{
		Meta: meta("m4"), Check: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	rak := kindEntry(retired.Msg, "agy")
	if !slices.Equal(rak.RetiredFamilies, []string{"xenon"}) {
		t.Fatalf("retired = %+v", rak)
	}
	if !strings.Contains(retired.Msg.Notice, "agy no longer lists: xenon (a tier names it)") {
		t.Fatalf("retired notice = %q", retired.Msg.Notice)
	}
	if !strings.Contains(h.d.ModelNotice(ctx), "agy no longer lists: xenon") {
		t.Fatalf("ModelNotice = %q", h.d.ModelNotice(ctx))
	}

	// A listing that returns nothing (a failed CLI) keeps the earlier findings instead of wiping them.
	listing = ""
	kept, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{
		Meta: meta("m5"), Check: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kept.Msg.Notice, "agy no longer lists: xenon") {
		t.Fatalf("notice after an empty listing = %q", kept.Msg.Notice)
	}
}

func TestModelsTracksOpenCodeNewAndRetiredFreeModelsAndChecksAuth(t *testing.T) {
	oc := opencodeListingBase
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	h := startModelsHarness(t, nil, &oc, &now)
	ctx := context.Background()

	// First check establishes baseline (nothing new yet).
	r1, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("o1"), Check: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(kindEntry(r1.Msg, "opencode").NewFamilies) != 0 || r1.Msg.Notice != "" {
		t.Fatalf("first opencode check = %+v, notice=%q", r1.Msg.Kinds, r1.Msg.Notice)
	}

	// New free model appears -> flagged as new.
	oc = opencodeListingNew
	r2, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("o2"), Check: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kindEntry(r2.Msg, "opencode").NewFamilies, []string{"opencode/nova-free"}) {
		t.Fatalf("second opencode check = %+v", kindEntry(r2.Msg, "opencode"))
	}

	// Bad --ack format is InvalidArgument, and a worker cannot call CheckModels.
	if _, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{
		Meta: meta("bad"), Ack: []string{"nope"},
	})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad ack err = %v", err)
	}
	run := startRun(t, h.device.Mesh, "r")
	wtok := h.seedAgent(run.Id, "w1", "repo-w1", "", v1.AgentRole_AGENT_ROLE_WORKER)
	if _, err := h.as(wtok).CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("w")})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("worker CheckModels err = %v", err)
	}
}

func TestModelsAPartialListingKeepsTheFailedKindsBaseline(t *testing.T) {
	agyList := agyListingBase
	oc := opencodeListingBase
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	h := startModelsHarness(t, &agyList, &oc, &now)
	ctx := context.Background()
	check := func(id string) *v1.CheckModelsResponse {
		t.Helper()
		res, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta(id), Check: true}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	check("p1") // baseline: both CLIs listed

	// OpenCode's listing fails while agy's works; a model OpenCode gains meanwhile must still be flagged later.
	oc = ""
	partial := check("p2")
	if ok := kindEntry(partial, "opencode"); ok == nil || len(ok.Listed) == 0 {
		t.Fatalf("opencode baseline dropped by a partial listing: %+v", ok)
	}
	oc = opencodeListingNew
	again := check("p3")
	if got := kindEntry(again, "opencode"); got == nil || !slices.Equal(got.NewFamilies, []string{"opencode/nova-free"}) {
		t.Fatalf("opencode after the failed listing = %+v", got)
	}
}

func TestModelsAckDuringAFailedListingClearsTheCarriedNotice(t *testing.T) {
	agyList := agyListingBase
	oc := opencodeListingBase
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	h := startModelsHarness(t, &agyList, &oc, &now)
	ctx := context.Background()
	call := func(id string, check bool, ack ...string) *v1.CheckModelsResponse {
		t.Helper()
		res, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta(id), Check: check, Ack: ack}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	call("a0", true)
	oc = opencodeListingNew
	if got := kindEntry(call("a1", true), "opencode"); !slices.Equal(got.NewFamilies, []string{"opencode/nova-free"}) {
		t.Fatalf("new model not flagged: %+v", got)
	}
	// The listing fails, and the user acks the model meanwhile: the carried notice must go.
	oc = ""
	res := call("a2", false, "opencode:opencode/nova-free")
	if got := kindEntry(res, "opencode"); got == nil || len(got.NewFamilies) != 0 || !slices.Equal(got.AckedFamilies, []string{"opencode/nova-free"}) {
		t.Fatalf("after ack during a failed listing: %+v", got)
	}
	if strings.Contains(res.Notice, "opencode") {
		t.Fatalf("notice = %q", res.Notice)
	}
}

func TestModelsAKindWhoseCLIIsGoneIsDroppedAndTheCacheApplies(t *testing.T) {
	agyList := agyListingBase
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	script := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat <<'EOF'\n"+opencodeListingBase+"EOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := agy.New()
	ag.ListModels = func(context.Context, string, map[string]string) (string, error) { return agyList, nil }
	state := shortdir.State(t)
	d, err := daemon.New(daemon.Config{
		StateDir: state, TCP: "127.0.0.1:0",
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: daemon.AgentConfig{
			Adapters: []adapter.Adapter{claude.New(), ag, opencode.New()},
			Binaries: map[string]string{"opencode": script},
			Env:      map[string]string{"HOME": t.TempDir()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := serve(t, d, state)
	ctx := context.Background()
	first, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("g1"), Check: true}))
	if err != nil {
		t.Fatal(err)
	}
	if kindEntry(first.Msg, "opencode") == nil {
		t.Fatalf("opencode not listed while installed: %+v", first.Msg.Kinds)
	}
	// Uninstall opencode: its kind is dropped, and the result is cached again (a checked_at is kept).
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	second, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("g2"), Check: true}))
	if err != nil {
		t.Fatal(err)
	}
	if kindEntry(second.Msg, "opencode") != nil {
		t.Fatalf("a kind whose CLI is gone is still carried: %+v", kindEntry(second.Msg, "opencode"))
	}
	if second.Msg.CheckedAt == nil {
		t.Fatal("checked_at unset: the 24h cache would never apply")
	}
}

// `mesh models --check` flags a tier effort a model of the chain doesn't offer: agy with no id of that family at
// that level, OpenCode with no variant of that name; a model that offers it, or none at all, is left alone.
func TestModelsFlagsATierEffortAModelDoesNotOffer(t *testing.T) {
	// hydrogen has only high; the shipped agy light and mid tiers name medium (and high).
	listing := strings.Replace(agyListingBase, "gemini-4-hydrogen-medium\tGemini 4 Hydrogen (Medium)\n", "", 1)
	oc := opencodeListingBase +
		"opencode/cinder-free\n{\"cost\":{\"input\":0,\"output\":0},\"capabilities\":{\"toolcall\":true},\"release_date\":\"2026-11-10\",\"variants\":{\"high\":{},\"max\":{}}}\n"
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	h := startModelsHarness(t, &listing, &oc, &now)
	ctx := context.Background()
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta("s1"), Key: "tiers.opencode.light.model", Value: val(t, []any{"opencode/cinder-free", "opencode/beam-free"})})); err != nil {
		t.Fatal(err)
	}
	res, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("e1"), Check: true}))
	if err != nil {
		t.Fatal(err)
	}
	agyProblems := strings.Join(kindEntry(res.Msg, "agy").EffortProblems, "\n")
	if !strings.Contains(agyProblems, "tiers.agy.light.effort = medium: hydrogen (has high)") {
		t.Errorf("agy problems:\n%s", agyProblems)
	}
	if strings.Contains(agyProblems, "neon") || strings.Contains(agyProblems, "carbon") {
		t.Errorf("a model that offers the effort is flagged:\n%s", agyProblems)
	}
	ocProblems := strings.Join(kindEntry(res.Msg, "opencode").EffortProblems, "\n")
	if !strings.Contains(ocProblems, "tiers.opencode.light.effort = low: opencode/cinder-free (has high, max)") || strings.Contains(ocProblems, "beam") {
		t.Errorf("opencode problems:\n%s", ocProblems)
	}
	if !strings.Contains(res.Msg.Notice, "settings name an effort a model doesn't offer") {
		t.Errorf("notice = %q", res.Msg.Notice)
	}
	// Fixing the setting stops the report at once, though the listing itself is cached for a day.
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta("s2"), Key: "tiers.agy.light.effort", Value: val(t, "high")})); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tiers.agy.light.exec_effort", "tiers.agy.light.review_effort", "tiers.agy.mid.review_effort", "tiers.agy.strong.review_effort"} {
		if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta("s-" + k), Key: k, Value: val(t, "high")})); err != nil {
			t.Fatal(err)
		}
	}
	again, err := h.device.Mesh.CheckModels(ctx, connect.NewRequest(&v1.CheckModelsRequest{Meta: meta("e2")}))
	if err != nil {
		t.Fatal(err)
	}
	if got := kindEntry(again.Msg, "agy").EffortProblems; len(got) != 0 {
		t.Errorf("agy problems after the fix: %v", got)
	}
}
