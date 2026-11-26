package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeHome is an isolated HOME with a ~/.claude.json holding content.
func claudeHome(t *testing.T, content string) (env map[string]string, file string) {
	t.Helper()
	home := t.TempDir()
	file = filepath.Join(home, ".claude.json")
	if content != "" {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"HOME": home}, file
}

func projects(t *testing.T, file string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	return top.Projects
}

func realDir(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestPretrustAddsTheWorktreeAndKeepsTheRestOfTheFile(t *testing.T) {
	env, file := claudeHome(t, `{"numStartups": 12345678901234567890, "oauthAccount": {"email": "a<b>@x"}, `+
		`"projects": {"/other": {"hasTrustDialogAccepted": true, "allowedTools": ["x"]}, "/unseen": {"allowedTools": ["Bash"]}}}`)
	wt := realDir(t, t.TempDir())
	if err := Pretrust(env, wt); err != nil {
		t.Fatal(err)
	}
	ps := projects(t, file)
	if ps[wt]["hasTrustDialogAccepted"] != true {
		t.Errorf("not trusted: %v", ps)
	}
	if ps["/other"]["hasTrustDialogAccepted"] != true || ps["/unseen"]["allowedTools"] == nil {
		t.Errorf("other projects changed: %v", ps)
	}
	b, _ := os.ReadFile(file)
	for _, keep := range []string{"12345678901234567890", `"a<b>@x"`} {
		if !strings.Contains(string(b), keep) {
			t.Errorf("%s lost from %s", keep, b)
		}
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if _, err := os.Stat(file + ".lock"); err == nil {
		t.Error("the lock was left")
	}
	// An entry that exists without the flag keeps its other keys.
	if err := Pretrust(env, "/unseen"); err != nil {
		t.Fatal(err)
	}
	if ps := projects(t, file); ps["/unseen"]["hasTrustDialogAccepted"] != true || ps["/unseen"]["allowedTools"] == nil {
		t.Errorf("/unseen: %v", ps["/unseen"])
	}
}

func TestPretrustSkipsATrustedFolder(t *testing.T) {
	root := realDir(t, t.TempDir())
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	env, file := claudeHome(t, `{"projects": {`+jsonKey(root)+`: {"hasTrustDialogAccepted": true}}}`)
	before, _ := os.ReadFile(file)
	// A parent is trusted: Claude reads it for every folder under it.
	if err := Pretrust(env, sub); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("the file changed for a trusted parent: %s", after)
	}
}

func jsonKey(s string) string { b, _ := json.Marshal(s); return string(b) }

// A linked worktree is trusted when its main checkout is (Claude keys a project by the canonical git root).
func TestPretrustSkipsAWorktreeOfATrustedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := realDir(t, t.TempDir())
	wt := filepath.Join(realDir(t, t.TempDir()), "wt")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"worktree", "add", "-q", wt}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if got := canonicalRoot(wt); got != repo {
		t.Fatalf("canonical root %q, want %q", got, repo)
	}
	env, file := claudeHome(t, `{"projects": {`+jsonKey(repo)+`: {"hasTrustDialogAccepted": true}}}`)
	before, _ := os.ReadFile(file)
	if err := Pretrust(env, wt); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Errorf("the file changed for a worktree of a trusted repo: %s", after)
	}
	// Not trusted: the worktree's own path is added, not the repo's.
	env, file = claudeHome(t, `{"projects": {}}`)
	if err := Pretrust(env, wt); err != nil {
		t.Fatal(err)
	}
	ps := projects(t, file)
	if len(ps) != 1 || ps[wt]["hasTrustDialogAccepted"] != true {
		t.Errorf("projects %v", ps)
	}
}

func TestPretrustWithoutAConfigFileOrWithAnUnreadableOne(t *testing.T) {
	env, file := claudeHome(t, "")
	if err := Pretrust(env, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a first-run config file was made")
	}
	env, _ = claudeHome(t, `{not json`)
	if err := Pretrust(env, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not readable JSON") {
		t.Errorf("an unreadable file: %v", err)
	}
	if err := Pretrust(map[string]string{}, t.TempDir()); err != nil {
		t.Errorf("no HOME in the environment: %v", err)
	}
	if err := Pretrust(env, ""); err != nil {
		t.Errorf("no folder: %v", err)
	}
}

func TestPretrustUsesTheConfigDirAndTheLegacyFile(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": t.TempDir(), "CLAUDE_CONFIG_DIR": dir}
	if got, _ := configFile(env); got != filepath.Join(dir, ".claude.json") {
		t.Errorf("config dir: %s", got)
	}
	os.WriteFile(filepath.Join(dir, ".config.json"), []byte(`{}`), 0o600)
	if got, _ := configFile(env); got != filepath.Join(dir, ".config.json") {
		t.Errorf("legacy: %s", got)
	}
	home := t.TempDir()
	if got, _ := configFile(map[string]string{"HOME": home}); got != filepath.Join(home, ".claude.json") {
		t.Errorf("home: %s", got)
	}
}

func TestPretrustWaitsForALiveLockAndBreaksAStaleOne(t *testing.T) {
	env, file := claudeHome(t, `{"projects": {}}`)
	wt := realDir(t, t.TempDir())
	// A live lock held a moment is waited for.
	os.Mkdir(file+".lock", 0o755)
	go func() { time.Sleep(200 * time.Millisecond); os.Remove(file + ".lock") }()
	start := time.Now()
	if err := Pretrust(env, wt); err != nil || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("live lock: %v after %v", err, time.Since(start))
	}
	// A lock older than Claude's stale limit is its dead holder's.
	env, file = claudeHome(t, `{"projects": {}}`)
	os.Mkdir(file+".lock", 0o755)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(file+".lock", old, old)
	if err := Pretrust(env, wt); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if projects(t, file)[wt]["hasTrustDialogAccepted"] != true {
		t.Error("not trusted after a stale lock")
	}
}
