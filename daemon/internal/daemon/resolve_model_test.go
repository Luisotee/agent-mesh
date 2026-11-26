package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func resolve(h *harness, req *v1.ResolveModelRequest) (*v1.ResolveModelResponse, error) {
	res, err := h.device.Mesh.ResolveModel(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func chainText(r *v1.ResolveModelResponse) string {
	var parts []string
	for _, c := range r.Chain {
		p := c.Model
		if c.Standing != "" {
			p += " [" + c.Standing + "]"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}

func TestResolveModelShowsWhatASpawnWouldRun(t *testing.T) {
	h := start(t, "")
	r, err := resolve(h, &v1.ResolveModelRequest{Tier: "mid"})
	if err != nil || r.Kind != "claude" || r.Model != "sonnet" || r.Effort != "high" || chainText(r) != "sonnet" {
		t.Fatalf("%v %v", r, err)
	}
	r, _ = resolve(h, &v1.ResolveModelRequest{Kind: "agy", Tier: "strong"})
	if r.Model != "neon" || r.Effort != "high" || chainText(r) != "neon,opus,carbon,hydrogen" {
		t.Errorf("agy strong: %v", r)
	}
	r, _ = resolve(h, &v1.ResolveModelRequest{Kind: "claude", Tier: "light", Effort: "low"})
	if r.Model != "haiku" || r.Effort != "low" {
		t.Errorf("an effort named: %v", r)
	}
	// A named model is used as it is, at the effort given, else high.
	r, _ = resolve(h, &v1.ResolveModelRequest{Kind: "claude", Model: "opus-4-9"})
	if r.Model != "opus-4-9" || r.Effort != "high" || len(r.Chain) != 0 {
		t.Errorf("named: %v", r)
	}
}

func TestResolveModelSkipsAHeldModelAndSaysSo(t *testing.T) {
	h := start(t, "")
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta("c"), Key: "tiers.claude.mid.model", Value: val(t, []any{"sonnet", "haiku"})})); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Hour)
	if err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutModelLimit("claude", "sonnet", until)
	}); err != nil {
		t.Fatal(err)
	}
	r, err := resolve(h, &v1.ResolveModelRequest{Tier: "mid"})
	if err != nil || r.Model != "haiku" {
		t.Fatalf("%v %v", r, err)
	}
	if got := chainText(r); got != "sonnet [held until "+until.Local().Format("15:04")+"],haiku" {
		t.Errorf("chain %q", got)
	}
}

func TestResolveModelRefusesWhatASpawnWould(t *testing.T) {
	h := start(t, "")
	for name, req := range map[string]*v1.ResolveModelRequest{
		"no tier or model": {Kind: "claude"},
		"unknown tier":     {Kind: "agy", Tier: "top"},
		"unknown kind":     {Kind: "codex", Tier: "mid"},
		"bad effort":       {Tier: "mid", Effort: "turbo"},
	} {
		if _, err := resolve(h, req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolveModelFollowsTheSettings(t *testing.T) {
	h := start(t, "")
	file := writePolicy(t, `{"copilot_model": "gpt-5", "tiers": {"claude": {"mid": {"model": ["opus"], "effort": "low"}}}}`)
	if _, err := h.device.Mesh.ImportSettings(context.Background(), connect.NewRequest(&v1.ImportSettingsRequest{Meta: meta("i"), From: file})); err != nil {
		t.Fatal(err)
	}
	if r, _ := resolve(h, &v1.ResolveModelRequest{Tier: "mid"}); r.Model != "opus" || r.Effort != "low" {
		t.Errorf("claude mid: %v", r)
	}
	if r, _ := resolve(h, &v1.ResolveModelRequest{Kind: "copilot", Tier: "strong"}); r.Model != "gpt-5" {
		t.Errorf("copilot strong: %v", r)
	}
}

// A bare --repo name is a repo of repos.dir; the CLI sends one only when no folder of that name is in its cwd.
func TestRepoTopResolvesABareNameInReposDir(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	root := t.TempDir()
	mk := func(name string, repo bool) string {
		dir := filepath.Join(root, "projects", name)
		os.MkdirAll(dir, 0o755)
		if repo {
			if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
		}
		return dir
	}
	shop, plain := mk("shop", true), mk("plain", false)
	os.MkdirAll(filepath.Join(plain, "inner"), 0o755)
	if _, err := h.d.RepoTop(ctx, "shop"); err == nil || !strings.Contains(err.Error(), "repos.dir") || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no repos.dir: %v", err)
	}
	if _, err := h.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta("r"), Key: "repos.dir",
		Value: val(t, []any{filepath.Join(root, "nowhere"), filepath.Join(root, "projects")})})); err != nil {
		t.Fatal(err)
	}
	if top, err := h.d.RepoTop(ctx, "shop"); err != nil || top != shop {
		t.Errorf("shop: %q %v", top, err)
	}
	if _, err := h.d.RepoTop(ctx, "plain"); err == nil || !strings.Contains(err.Error(), "a folder, not a repo's top") {
		t.Errorf("a plain folder: %v", err)
	}
	if _, err := h.d.RepoTop(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "no repo of that name") {
		t.Errorf("missing: %v", err)
	}
	// A path is taken as before, whatever repos.dir says.
	if top, err := h.d.RepoTop(ctx, shop); err != nil || top != shop {
		t.Errorf("by path: %q %v", top, err)
	}
}

func TestSpawnTakesABareRepoNameFromReposDir(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	repo := gitRepo(t) // <tmp>/shop-api
	if _, err := h.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta("r"),
		Key: "repos.dir", Value: val(t, []any{filepath.Dir(repo)})})); err != nil {
		t.Fatal(err)
	}
	a := h.spawn(run.Id, "shop-api", "named", "do it")
	if a.Name != "shop-api-named" || a.Worktree == "" {
		t.Fatalf("spawned %v", a)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE) // its hooks have landed before the state dir goes
}

// ResolveModel warms what SpawnAgent warms before it picks, so a cold daemon answers as a spawn would.
func TestResolveModelSkipsAnAgyModelAtTheReserveOnAColdDaemon(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Env["FAKEAGY_USAGE"] = strings.Join([]string{
			"Gemini 4 Neon\tNeon\t5%\t1h",
			"Gemini and Claude models\tShared\t80%\t2h",
		}, "\n")
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	w.set(t, "tiers.agy.strong.model", []any{"neon", "hydrogen"})
	for _, effort := range []string{"", "low"} { // the tier's own effort, and another one
		res, err := w.device.Mesh.ResolveModel(context.Background(), connect.NewRequest(&v1.ResolveModelRequest{
			Kind: "agy", Tier: "strong", Effort: effort}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Msg.Model != "hydrogen" || chainText(res.Msg) != "neon [at the reserve (5% left)],hydrogen" {
			t.Errorf("effort %q: model %q, chain %q", effort, res.Msg.Model, chainText(res.Msg))
		}
	}
}

func TestResolveModelExpandsOpenCodeFreeOnAColdDaemon(t *testing.T) {
	w := startReopen(t)
	w.set(t, "tiers.opencode.mid.model", []any{"free"})
	res, err := w.device.Mesh.ResolveModel(context.Background(), connect.NewRequest(&v1.ResolveModelRequest{Kind: "opencode", Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Model != "opencode/big-pickle" || len(res.Msg.Chain) != 5 {
		t.Errorf("model %q chain %q", res.Msg.Model, chainText(res.Msg))
	}
	named, err := w.device.Mesh.ResolveModel(context.Background(), connect.NewRequest(&v1.ResolveModelRequest{Kind: "opencode", Model: "free"}))
	if err != nil || named.Msg.Model != "opencode/big-pickle" {
		t.Errorf("--model free: %v %v", named, err)
	}
}

// A mesh tools servers --repo <name> takes a bare name as a spawn does.
func TestListMCPServersTakesABareRepoName(t *testing.T) {
	w := startReopen(t)
	repo := gitRepo(t) // <tmp>/shop-api
	os.WriteFile(filepath.Join(repo, ".mcp.json"), []byte(`{"mcpServers":{"shopsrv":{"command":"x"}}}`), 0o644)
	os.MkdirAll(filepath.Join(repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"enableAllProjectMcpServers":true}`), 0o644)
	os.WriteFile(filepath.Join(w.home, ".claude.json"), []byte(`{}`), 0o644)
	w.set(t, "repos.dir", []any{filepath.Dir(repo)})
	res, err := w.device.Mesh.ListMCPServers(context.Background(), connect.NewRequest(&v1.ListMCPServersRequest{Repo: "shop-api"}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range res.Msg.Servers {
		names = append(names, s.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "shopsrv") {
		t.Errorf("servers %v: the repo's own was not found by its bare name", names)
	}
}
