package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// wildcard is OpenCode 1.19.6's pattern match (read from its binary): `*` is any run, `?` one character, the
// regex specials are literal, a trailing ` *` also matches the bare command, the match is anchored and dot-all.
func wildcard(pattern, s string) bool {
	var b strings.Builder
	for _, c := range pattern {
		switch {
		case c == '*':
			b.WriteString(".*")
		case c == '?':
			b.WriteString(".")
		case strings.ContainsRune(`.+^${}()|[]\`, c):
			b.WriteString(`\` + string(c))
		default:
			b.WriteRune(c)
		}
	}
	r := b.String()
	if strings.HasSuffix(r, " .*") {
		r = strings.TrimSuffix(r, " .*") + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + r + "$").MatchString(s)
}

// orderedMap decodes a JSON object keeping its key order, which is the rule order.
func orderedMap(t *testing.T, raw json.RawMessage) []orderedKV {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var out []orderedKV
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, orderedKV{k.(string), v})
	}
	return out
}

// verdict is the action of the last rule that matches, OpenCode's `findLast`; no rule is "ask".
func verdict(rules []orderedKV, s string) string {
	for i := len(rules) - 1; i >= 0; i-- {
		if wildcard(rules[i].key, s) {
			return rules[i].val
		}
	}
	return "ask"
}

// bashVerdict is what OpenCode answers for a command line: it asks for each simple command alone, and one deny
// refuses the line, any ask asks (nobody answers in a mesh).
func bashVerdict(rules []orderedKV, simple ...string) string {
	out := "allow"
	for _, c := range simple {
		switch verdict(rules, c) {
		case "deny":
			return "deny"
		case "ask":
			out = "ask"
		}
	}
	return out
}

type permEnv struct {
	cwd, repo, runDir string
	perm              map[string]json.RawMessage
}

func launchPermission(t *testing.T, role agentmeshv1.AgentRole, denyEdits bool) permEnv {
	t.Helper()
	base := shortdir.New(t)
	e := permEnv{cwd: filepath.Join(base, "worktrees", "r1", "agent"), repo: filepath.Join(base, "proj"), runDir: filepath.Join(base, "runs", "r1")}
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: role, Cwd: e.cwd, Repo: e.repo, DenyEdits: denyEdits, Binary: "/opt/opencode",
		Env: map[string]string{"HOME": base, "AGENT_MESH_RUN_DIR": e.runDir}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(res.Msg.Env["OPENCODE_PERMISSION"]), &e.perm); err != nil {
		t.Fatalf("OPENCODE_PERMISSION = %q: %v", res.Msg.Env["OPENCODE_PERMISSION"], err)
	}
	return e
}

// What a read-only or planning OpenCode agent can run: the read-only commands, and nothing that deletes, moves,
// writes, commits or publishes, however the command line is built (OpenCode asks per simple command).
func TestAReadOnlyOrPlanningAgentCannotRunAMutatingCommand(t *testing.T) {
	allowed := []string{
		"git status", "git status --short", "git diff main...HEAD", "git --no-pager log -5", "git show HEAD:a.go",
		"git rev-parse HEAD", "git ls-files", "git merge-base HEAD main", "git blame a.go", "git grep -n foo",
		"git branch --list", "git worktree list", "gh pr view 12 --json title", "gh pr diff 12", "gh pr checks 12",
		"gh pr list", "ls -la", "cat README.md", "head -n 5 x", "tail -n 5 x", "wc -l x", "grep -rn foo .", "rg foo",
		"find . -name '*.go'", "jq .a x.json", "awk '{print $1}' x", "go test ./...", "go vet ./...",
		"go build -o /tmp/mesh ./cmd/mesh", "go env GOPATH", "gofmt -l .", "buf lint", "pnpm typecheck", "pnpm test",
		"uvx pytest -q tests", "mesh send parent hi", "mesh inbox", "pwd",
	}
	denied := []string{
		"rm -rf /tmp/opencode/x", "rm -rf /tmp/go-build", "mv a b", "cp a b", "tee f", "touch f", "mkdir d", "ln -s a b",
		"chmod 777 x", "truncate -s0 f", "dd of=f", "git commit -m x", "git -C . push", "git push origin x",
		"git push", "git reset --hard", "git checkout main", "git switch x", "git stash", "git clean -fd", "git add .",
		"git branch -D x", "git branch --list -D x", "git branch -m x", "git branch x", "git fetch", "git -C /x status",
		"git diff --output=f", "git log --output=f", "git grep -O sh x", "gh pr merge 1", "gh pr create", "gh api x",
		"sed -i s/a/b/ f", "sed -n 1p f", "cat a > b", "echo hi >> f", "echo x > f", "ls > f", "go test ./... 2>&1",
		"find . -delete", "find . -exec rm {} ;", "find . -fprint f", "awk 'BEGIN{system(\"rm x\")}'",
		"awk '{print > \"f\"}' x", "awk 'BEGIN{\"id\" | getline x}'", "rg --pre=sh x", "rg --pre sh x",
		"go test -exec rm ./...", "go test -coverprofile=c ./...", "go test -o /tmp/t ./...", "go test -c",
		"go build -o /tmp/../etc/x", "go build ./cmd/mesh", "go build -toolexec x", "go env -w GOFLAGS=x", "gofmt -w x.go",
		"gofmt -l -w x.go", "sort -o f x", "xargs rm", "sh -c 'rm x'", "bash -c x", "env rm x", "eval x", "CC=x rm y",
		"CC=x go test ./...", "FOO=1 git status", "$x y", "curl x", "python3 -c x", "export X=1", "exec rm x",
	}
	lines := [][]string{ // each simple command of a compound line, as OpenCode splits it
		{"ls", "rm -rf x"}, {"git status", "git commit -m x"}, {"cat a", "tee b"}, {"ls $(rm -rf x)", "rm -rf x"}, {"cat `rm x`", "rm x"},
	}
	for _, c := range []struct {
		name string
		role agentmeshv1.AgentRole
		deny bool
	}{
		{"reviewer", agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, false},
		{"asker", agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false},
		{"planner", agentmeshv1.AgentRole_AGENT_ROLE_WORKER, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rules := orderedMap(t, launchPermission(t, c.role, c.deny).perm["bash"])
			if len(rules) == 0 || rules[0].key != "*" || rules[0].val != "deny" {
				t.Fatalf("bash rules do not begin by denying everything: %v", rules)
			}
			for _, cmd := range allowed {
				if got := bashVerdict(rules, cmd); got != "allow" {
					t.Errorf("%q = %s, want allow", cmd, got)
				}
			}
			for _, cmd := range denied {
				if got := bashVerdict(rules, cmd); got != "deny" {
					t.Errorf("%q = %s, want deny", cmd, got)
				}
			}
			for _, l := range lines {
				if got := bashVerdict(rules, l...); got != "deny" {
					t.Errorf("%q = %s, want deny", l, got)
				}
			}
		})
	}
}

// An agent that may edit keeps today's bash: the launch sets no bash rule for it.
func TestAnExecutingWorkerHasNoBashRules(t *testing.T) {
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "w", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: t.TempDir(), Binary: "/opt/opencode"}))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := res.Msg.Env["OPENCODE_PERMISSION"]; ok {
		t.Errorf("OPENCODE_PERMISSION = %s", p)
	}
	if strings.Contains(res.Msg.Env["OPENCODE_CONFIG_CONTENT"], `"bash"`) {
		t.Errorf("a bash rule in %s", res.Msg.Env["OPENCODE_CONFIG_CONTENT"])
	}
}

// OpenCode matches an edit's pattern as the path relative to the worktree: a reviewer may write only its report,
// a planner its plan and its report, and nothing else, not a sibling worktree nor /tmp.
func TestEditsAreDeniedButTheRunFoldersOutputs(t *testing.T) {
	for _, c := range []struct {
		name    string
		role    agentmeshv1.AgentRole
		deny    bool
		allowed []string
		denied  []string
	}{
		{"reviewer", agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, false,
			[]string{"{run}/reports/review.md"}, []string{"{run}/plans/p.md", "{run}/other.md", "{run}/../x"}},
		{"planner", agentmeshv1.AgentRole_AGENT_ROLE_WORKER, true,
			[]string{"{run}/reports/r.md", "{run}/plans/p.md"}, []string{"{run}/other.md", "{run}/../x"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := launchPermission(t, c.role, c.deny)
			rules := orderedMap(t, e.perm["edit"])
			for _, r := range rules {
				if r.key == "../*" {
					t.Errorf("the sibling-worktree allow is back: %v", rules)
				}
			}
			path := func(p string) string {
				p = strings.ReplaceAll(p, "{run}", e.runDir)
				rel, err := filepath.Rel(e.cwd, p)
				if err != nil {
					t.Fatal(err)
				}
				return filepath.ToSlash(rel)
			}
			for _, p := range c.allowed {
				if got := verdict(rules, path(p)); got != "allow" {
					t.Errorf("edit %s = %s, want allow (rules %v)", path(p), got, rules)
				}
			}
			others := []string{filepath.Join(e.cwd, "a.go"), filepath.Join(e.cwd, "sub", "a.go"),
				filepath.Join(filepath.Dir(e.cwd), "other", "x"), filepath.Join(e.repo, "a.go"), "/tmp/x", "/var/tmp/x",
				filepath.Join(e.runDir, "reports", "..", "x")}
			for _, p := range append(others, c.denied...) {
				rel := path(p)
				if got := verdict(rules, rel); got != "deny" {
					t.Errorf("edit %s = %s, want deny (rules %v)", rel, got, rules)
				}
			}
		})
	}
}

// Edit denies in the share and the toolchain are written relative to the worktree too, or they match nothing.
func TestToolchainEditDeniesAreRelativeToTheWorktree(t *testing.T) {
	base := shortdir.New(t)
	cwd, share := filepath.Join(base, "worktrees", "r", "a"), filepath.Join(base, "share")
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "w", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: cwd, Binary: "/opt/opencode",
		Env: map[string]string{"HOME": base, "AGENT_MESH_SHARE": share}}))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permission struct {
			Edit map[string]string `json:"edit"`
		} `json:"permission"`
	}
	if err := json.Unmarshal([]byte(res.Msg.Env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{share, filepath.Join(base, ".local", "go")} {
		rel, _ := filepath.Rel(cwd, d)
		rel = filepath.ToSlash(rel)
		if cfg.Permission.Edit[rel+"/*"] != "deny" {
			t.Errorf("edits in %s are not denied as %s/*: %v", d, rel, cfg.Permission.Edit)
		}
		file := filepath.ToSlash(filepath.Join(rel, "bin", "go"))
		hit := false
		for k, v := range cfg.Permission.Edit {
			if v == "deny" && wildcard(k, file) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("no deny matches the edit of %s", file)
		}
	}
}

// A non-default TMPDIR is allowed like /tmp, and a bare directory is, not only what is under it.
func TestExternalAllowsTheTmpdirAndTheBareDirectories(t *testing.T) {
	a := New()
	perm, err := a.externalPermission(map[string]string{"HOME": "/home/u", "TMPDIR": "/scratch/tmp", "AGENT_MESH_RUN_DIR": "/state/runs/r"}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(perm)
	keys, actions := externalRules(t, `{"permission":`+string(b)+`}`)
	for _, d := range []string{"/scratch/tmp", "/tmp", "/state/runs/r"} {
		for _, k := range []string{d, d + "/*", d + "/**"} {
			if actions[k] != "allow" {
				t.Errorf("%s is not allowed: %v", k, keys)
			}
		}
	}
	// HOME or / as TMPDIR is not a scratch dir.
	for _, tmp := range []string{"/", "/home/u", "relative"} {
		perm, _ := a.externalPermission(map[string]string{"HOME": "/home/u", "TMPDIR": tmp}, "")
		b, _ := json.Marshal(perm)
		_, actions := externalRules(t, `{"permission":`+string(b)+`}`)
		for _, k := range []string{"/", "/*", "/home/u", "/home/u/*", "relative", "relative/*"} {
			if actions[k] != "" {
				t.Errorf("TMPDIR %q allowed %q", tmp, k)
			}
		}
	}
}

// An asker or a reviewer launches without the project's OpenCode config, which a PR head can use to run code (a
// plugin, an MCP server, a formatter); a worker and a planner work on their own repo and keep it.
func TestAReadOnlyOpenCodeAgentIgnoresTheProjectsConfig(t *testing.T) {
	for _, c := range []struct {
		role agentmeshv1.AgentRole
		deny bool
		want string
	}{
		{agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false, "1"},
		{agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, false, "1"},
		{agentmeshv1.AgentRole_AGENT_ROLE_WORKER, true, ""},
		{agentmeshv1.AgentRole_AGENT_ROLE_WORKER, false, ""},
		{agentmeshv1.AgentRole_AGENT_ROLE_ROOT, false, ""},
	} {
		// A fresh launch and a resumed one (a re-review reopens the reviewer's own conversation).
		for _, resume := range []string{"", "ses_1"} {
			res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
				AgentId: "a", Role: c.role, DenyEdits: c.deny, Cwd: t.TempDir(), Binary: "/opt/opencode", ResumeSessionId: resume}))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Msg.Env["OPENCODE_DISABLE_PROJECT_CONFIG"]; got != c.want {
				t.Errorf("%v deny=%v resume=%q: OPENCODE_DISABLE_PROJECT_CONFIG = %q, want %q", c.role, c.deny, resume, got, c.want)
			}
			if !strings.Contains(res.Msg.Env["OPENCODE_CONFIG_CONTENT"], `"external_directory"`) {
				t.Errorf("%v: the mesh's own config is gone: %s", c.role, res.Msg.Env["OPENCODE_CONFIG_CONTENT"])
			}
		}
	}
}

// planLaunchEnv launches an agent with the share set and returns its environment.
func planLaunchEnv(t *testing.T, role agentmeshv1.AgentRole, denyEdits, standalone bool, share string) map[string]string {
	t.Helper()
	base := shortdir.New(t)
	req := &adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: role, Cwd: filepath.Join(base, "w"), Repo: filepath.Join(base, "proj"), DenyEdits: denyEdits, Binary: "/opt/opencode",
		Env: map[string]string{"HOME": base, "AGENT_MESH_RUN_DIR": filepath.Join(base, "runs", "r1"), "AGENT_MESH_SHARE": share}}
	if standalone {
		req.Standalone, req.ResumeSessionId = true, "ses_1"
	}
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Env
}

func guardPlugins(t *testing.T, env map[string]string) []string {
	t.Helper()
	var cfg struct {
		Plugin []string `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Plugin
}

// A bare redirect is no command to OpenCode 1.19.6, so no bash pattern sees it: the launch of every agent whose bash is fenced
// loads the guard that refuses a `>`, and no other agent gets it. Every agent but a standalone session loads the guard
// that refuses a kill by pattern.
func TestAReadOnlyOrPlanningLaunchLoadsTheRedirectGuard(t *testing.T) {
	share := filepath.Join(shortdir.New(t), "share")
	kill := "file://" + share + "/opencode/kill-guard.js"
	want := []string{kill, "file://" + share + "/opencode/readonly-guard.js"}
	for _, c := range []struct {
		name       string
		role       agentmeshv1.AgentRole
		denyEdits  bool
		standalone bool
		guarded    bool
	}{
		{"an asker", agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false, false, true},
		{"a reviewer", agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, false, false, true},
		{"a planner", agentmeshv1.AgentRole_AGENT_ROLE_WORKER, true, false, true},
		{"a worker", agentmeshv1.AgentRole_AGENT_ROLE_WORKER, false, false, false},
		{"a root", agentmeshv1.AgentRole_AGENT_ROLE_ROOT, false, false, false},
		{"a standalone session", agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false, true, false},
	} {
		got := guardPlugins(t, planLaunchEnv(t, c.role, c.denyEdits, c.standalone, share))
		wantHere := []string{kill}
		if c.guarded {
			wantHere = want
		}
		if c.standalone {
			wantHere = nil
		}
		if !reflect.DeepEqual(got, wantHere) && len(got)+len(wantHere) > 0 {
			t.Errorf("%s: plugin = %v, want %v", c.name, got, wantHere)
		}
	}
	if got := guardPlugins(t, planLaunchEnv(t, agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false, false, "")); len(got) != 0 {
		t.Errorf("no share: plugin = %v", got)
	}
}

// The guard file itself, run by Node when it is there: a bash command with a `>` is refused, any other call passes.
func TestTheRedirectGuardRefusesABashCommandWithARedirect(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run the guard with")
	}
	file, err := filepath.Abs("../../../share/opencode/readonly-guard.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `import(process.argv[1]).then(async (m) => {
  const hook = (await m.ReadOnlyGuard())["tool.execute.before"]
  const refused = async (tool, args) => { try { await hook({ tool }, { args }); return false } catch { return true } }
  const out = {
    bare: await refused("bash", { command: "> f" }), append: await refused("bash", { command: ">> f" }),
    stderr: await refused("bash", { command: "2> f" }), piped: await refused("bash", { command: "ls; > f" }),
    plain: await refused("bash", { command: "ls -la" }), other: await refused("edit", { command: "> f" }),
    none: await refused("bash", {}),
  }
  console.log(JSON.stringify(out))
})`
	res, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+file).Output()
	if err != nil {
		t.Fatalf("node: %v: %s", err, res)
	}
	var got map[string]bool
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("%s: %v", res, err)
	}
	want := map[string]bool{"bare": true, "append": true, "stderr": true, "piped": true, "plain": false, "other": false, "none": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refused %v, want %v", got, want)
	}
}

// The kill guard itself, run by Node when it is there: a bash command that kills by pattern is refused, a kill of a
// PID one holds and any other call pass.
func TestTheKillGuardRefusesAKillByPattern(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run the guard with")
	}
	file, err := filepath.Abs("../../../share/opencode/kill-guard.js")
	if err != nil {
		t.Fatal(err)
	}
	refused := []string{`pkill -f "sleep 30"`, `killall node`, `echo x; pkill sleep`, `sudo pkill x`, `timeout 5 pkill x`,
		`bash -c "pkill x"`, `/usr/bin/killall5 -9`, `kill $(pgrep -f sleep)`, "kill `pidof node`", `pgrep -f x | xargs kill`,
		`ps aux | grep sleep | awk '{print $2}' | xargs kill`, `(pkill x)`, `kill $(ps aux | grep sleep | awk '{print $2}')`,
		`if true; then pkill x; fi`, `for p in a; do pkill $p; done`, `FOO=1 pkill x`, `! pkill x`, `echo x | xargs pkill`,
		`kill $(pgrep -G 1000)`, `pid=$(pgrep -f sleep); kill $pid`, `for p in $(pgrep node); do kill $p; done`,
		`pgrep node | while read p; do kill $p; done`, `kill -9 $(/usr/bin/pgrep -f sleep)`, `kill $(command pgrep -f sleep)`}
	passed := []string{`kill 1234`, `kill -9 $!`, `kill $(cat pidfile)`, `kill $(pgrep -P $$)`, `pgrep -P 5 | xargs kill`,
		`pgrep -f sleep`, `kill $(cat pidfile) && pgrep -f sleep`, `grep pkill notes.md`, `echo "no pkill"`, `man killall`,
		`ls -la`, `go test ./...`, `grep -rn time pkill notes.md`, `echo see exec pkill docs`, `kill $(pgrep -P1234)`,
		`kill -0 $(pgrep foo)`, `pgrep -P $$ | xargs kill; pgrep -f server`, `pid=$(cat pidfile); kill $pid`,
		`for p in $(pgrep -P $$); do kill $p; done`}
	script := `import(process.argv[1]).then(async (m) => {
  const hook = (await m.KillGuard())["tool.execute.before"]
  const refused = async (tool, args) => { try { await hook({ tool }, { args }); return false } catch { return true } }
  const out = { other: await refused("edit", { command: "pkill x" }), none: await refused("bash", {}) }
  for (const c of JSON.parse(process.argv[2])) out[c] = await refused("bash", { command: c })
  console.log(JSON.stringify(out))
})`
	all, _ := json.Marshal(append(append([]string{}, refused...), passed...))
	res, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+file, string(all)).Output()
	if err != nil {
		t.Fatalf("node: %v: %s", err, res)
	}
	var got map[string]bool
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("%s: %v", res, err)
	}
	for _, c := range refused {
		if !got[c] {
			t.Errorf("%q passed", c)
		}
	}
	for _, c := range append(passed, "other", "none") {
		if got[c] {
			t.Errorf("%q was refused", c)
		}
	}
}
