package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// A deploying command's subcommand is found wherever it stands in its shell segment: after the global flags of
// kubectl, terraform and helm, behind a wrapper, in a later segment. Reads and builds are not kept.
func TestNeverForwardFindsADeployingCommandsSubcommandAfterItsFlags(t *testing.T) {
	wt := t.TempDir()
	for _, tc := range []struct {
		cmd  string
		kept bool
	}{
		{"kubectl apply -f k8s.yaml", true},
		{"kubectl -n prod apply -f k8s.yaml", true},
		{"kubectl --context prod delete pod web", true},
		{"terraform -chdir=infra apply -auto-approve", true},
		{"terraform apply", true},
		{"helm -n prod upgrade app ./chart", true},
		{"helm --namespace=prod --kube-context x install app ./chart", true},
		{"cd infra && sudo env X=1 terraform -chdir=. destroy", true},
		{"echo ok; /usr/local/bin/kubectl -n a rollout restart deploy/web", true},
		{"kubectl get pods -n prod", false},
		{"kubectl -n prod describe pod web", false},
		{"terraform plan -chdir=infra", false},
		{"helm -n prod list", false},
		{"kubectl get pods; terraform plan", false},
		{"go test ./...", false},
	} {
		got := neverForward(wt, "", "Bash", tc.cmd)
		if (got != "") != tc.kept {
			t.Errorf("neverForward(%q) = %q, kept = %v", tc.cmd, got, tc.kept)
		}
	}
}

// A file write that can't be placed inside the worktree (or the run's dir) stays with the person: ~ paths (expanded
// against the home), no path, no worktree, a path that goes up out of it.
func TestNeverForwardFailsClosedOnAWriteThatCantBePlaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wt := filepath.Join(home, "wt")
	runDir := filepath.Join(home, "state", "runs", "r1")
	for _, tc := range []struct {
		name, worktree, tool, path string
		kept                       bool
	}{
		{"tilde into the ssh dir", wt, "Write", "~/.ssh/authorized_keys", true},
		{"tilde alone", wt, "Edit", "~", true},
		{"tilde into the claude settings", wt, "MultiEdit", "~/.claude/settings.json", true},
		{"another user's home", wt, "Write", "~root/.bashrc", true},
		{"tilde that leads into the worktree", wt, "Write", "~/wt/a.go", false},
		{"tilde that leads into the run dir", wt, "Write", "~/state/runs/r1/reports/w.md", false},
		{"no path", wt, "Write", "", true},
		{"a blank path", wt, "NotebookEdit", "   ", true},
		{"no worktree", "", "Edit", filepath.Join(wt, "a.go"), true},
		{"no worktree, relative", "", "Edit", "a.go", true},
		{"up out of the worktree", wt, "Edit", "../other/a.go", true},
		{"a relative path inside", wt, "Edit", "internal/a.go", false},
		{"an absolute path inside", wt, "Write", filepath.Join(wt, "docs", "a.md"), false},
	} {
		if got := neverForward(tc.worktree, runDir, tc.tool, tc.path); (got != "") != tc.kept {
			t.Errorf("%s: neverForward(%q, %q) = %q, kept = %v", tc.name, tc.worktree, tc.path, got, tc.kept)
		}
	}
	// With no home to expand against, a ~ path stays.
	t.Setenv("HOME", "")
	if os.Getenv("HOME") == "" {
		if got := neverForward(wt, runDir, "Write", "~/x"); got == "" {
			// UserHomeDir may fall back to the passwd entry: then the path is outside the worktree anyway.
			t.Error("a ~ path with no HOME was forwarded")
		}
	}
}
