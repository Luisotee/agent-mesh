package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/creack/pty"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestOpenParsesExtraArgsAfterDoubleDashAndShowsInUsage(t *testing.T) {
	a := action(t, "open")
	var helpOut bytes.Buffer
	newCommand(a, Env{Stdin: strings.NewReader(""), Stdout: &helpOut, Stderr: &helpOut, Getenv: func(string) string { return "" }}, "").usage(&helpOut)
	if !strings.Contains(helpOut.String(), "usage: mesh open [<agent>] [-- <extra_args>...] [flags]") {
		t.Errorf("unexpected usage:\n%s", helpOut.String())
	}

	m, err := Request(a, []string{"--model", "opus", "repo-fix", "--", "--verbose", "--debug"})
	if err != nil {
		t.Fatal(err)
	}
	req := as(t, m, &v1.OpenAgentRequest{})
	if req.Agent != "repo-fix" || req.Model != "opus" {
		t.Fatalf("req = %+v", req)
	}
	if !reflect.DeepEqual(req.ExtraArgs, []string{"--verbose", "--debug"}) {
		t.Errorf("ExtraArgs = %v, want [--verbose --debug]", req.ExtraArgs)
	}

	// Empty trailing `--` leaves extra_args empty without error.
	m2, err := Request(a, []string{"repo-fix", "--"})
	if err != nil {
		t.Fatal(err)
	}
	if got := as(t, m2, &v1.OpenAgentRequest{}).ExtraArgs; len(got) != 0 {
		t.Errorf("empty `--` gave %v", got)
	}
}

func TestBuildStandaloneEnvStripsMeshAndSessionVars(t *testing.T) {
	hostEnv := []string{
		"HOME=/home/user",
		"PATH=/usr/bin",
		"AGENT_MESH_STATE=/tmp/st",
		"AGENT_MESH_TOKEN=secret",
		"MESH_RUN=r1",
		"CLAUDECODE=1",
		"CLAUDE_PID=123",
		"ANTIGRAVITY_AGENT=1",
		"ANTIGRAVITY_CSRF_TOKEN=tok",
		"KEEP_ME=yes",
	}
	env := buildStandaloneEnv(hostEnv, map[string]string{
		"OPENCODE_CONFIG_CONTENT":     `{"agent":{}}`,
		"OPENCODE_DISABLE_AUTOUPDATE": "1",
	})
	joined := strings.Join(env, "\n")
	for _, banned := range []string{"AGENT_MESH_STATE=", "AGENT_MESH_TOKEN=", "MESH_RUN=", "CLAUDECODE=", "CLAUDE_PID=", "ANTIGRAVITY_AGENT=", "ANTIGRAVITY_CSRF_TOKEN="} {
		if strings.Contains(joined, banned) {
			t.Errorf("banned var %q remained in env:\n%s", banned, joined)
		}
	}
	for _, want := range []string{"HOME=/home/user", "KEEP_ME=yes", "OPENCODE_CONFIG_CONTENT={\"agent\":{}}", "OPENCODE_DISABLE_AUTOUPDATE=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q in env:\n%s", want, joined)
		}
	}
}

func TestExecOpenPrintsBannerStripsEnvAndRunsExec(t *testing.T) {
	var errOut bytes.Buffer
	var gotArgv, gotEnv []string
	var gotDir string
	res := &v1.OpenAgentResponse{
		Agent:  &v1.Agent{Id: "a1", Name: "repo-fix", Kind: "claude"},
		Mode:   v1.OpenMode_OPEN_MODE_EXEC,
		Argv:   []string{"/opt/claude", "--resume", "sess-1", "--verbose"},
		Env:    map[string]string{"EXTRA": "1"},
		Cwd:    "/tmp/wt",
		Branch: "mesh/r1/fix",
		Notes:  []string{"the worktree was recreated"},
	}
	env := Env{
		Stderr:  &errOut,
		Environ: func() []string { return []string{"HOME=/h", "AGENT_MESH_TOKEN=tok", "CLAUDECODE=1"} },
		Exec: func(argv, envv []string, dir string) error {
			gotArgv, gotEnv, gotDir = argv, envv, dir
			return nil
		},
	}
	if err := runOpen(t.Context(), nil, res, env); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotArgv, res.Argv) || gotDir != "/tmp/wt" {
		t.Errorf("exec got argv=%v dir=%q", gotArgv, gotDir)
	}
	if !slices.Contains(gotEnv, "EXTRA=1") || !slices.Contains(gotEnv, "HOME=/h") {
		t.Errorf("missing expected env in %v", gotEnv)
	}
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "AGENT_MESH_") || strings.HasPrefix(kv, "CLAUDECODE=") {
			t.Errorf("leaked %q", kv)
		}
	}
	if !strings.Contains(errOut.String(), "Reopening repo-fix (claude) on branch mesh/r1/fix") || !strings.Contains(errOut.String(), "worktree was recreated") {
		t.Errorf("unexpected banner:\n%s", errOut.String())
	}
}

func TestAttachOpenRequiresTTY(t *testing.T) {
	err := runOpen(t.Context(), nil, &v1.OpenAgentResponse{
		Agent:        &v1.Agent{Name: "w1"},
		Mode:         v1.OpenMode_OPEN_MODE_ATTACH,
		TerminalPath: "/v1/term/a1?ticket=t",
	}, Env{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("expected terminal error, got %v", err)
	}

	// A real PTY file descriptor IS recognized as a terminal by x/term.
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open: %v", err)
	}
	defer ptm.Close()
	defer tty.Close()
	if !filepath.IsAbs(tty.Name()) || os.Getenv("NEVER_SET") != "" {
		t.Fatalf("unexpected tty name %q", tty.Name())
	}
}
