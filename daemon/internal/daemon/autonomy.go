package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The user's autonomy settings (docs/apps.md, "Autonomy per project"), as lines of the root's brief. A repo in no list is
// handled as before, so with all three settings empty the brief has no autonomy line at all.

func (d *Daemon) strings(r store.Reader, key string) []string {
	v, err := d.Setting(r, key)
	if err != nil {
		return nil
	}
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// runAutonomyBrief says what the user set for this run alone (`StartRun`'s autonomy): it replaces the settings' repo
// lists for every repo of the run, and its ask-before list is added to the settings'.
func runAutonomyBrief(a *v1.RunAutonomy, ask []string) []string {
	var out []string
	switch {
	case a.UserApprovesPlans:
		out = append(out, "this run: the user approves its plans and merges its PRs: don't `mesh approve` a plan (put it to the user: a `question` in your notes, then wait), and never merge a PR")
	case a.UserMerges:
		out = append(out, "this run: the user merges its PRs: never merge one; tell the user when it is ready")
	default:
		out = append(out, "this run: the user set no brake on plans or merges here; the repos' settings do not apply to it")
	}
	if len(ask) > 0 {
		out = append(out, "ask the user before: "+strings.Join(ask, "; "))
	}
	return append([]string{"the user's autonomy for this run (set when it started; it replaces `repos.user_approves_plans` and `repos.user_merges` here, and `autonomy.ask_before` still applies):"}, out...)
}

// autonomyBrief says, for each repo the user listed, what the root may not do, and what to ask before. A run started
// with its own autonomy (run.Autonomy) gets that for all its repos instead of the repo lists.
func (d *Daemon) autonomyBrief(r store.Reader, run *v1.Run) []string {
	approves := d.strings(r, "repos.user_approves_plans")
	merges := d.strings(r, "repos.user_merges")
	ask := d.strings(r, "autonomy.ask_before")
	if run != nil && run.Autonomy != nil {
		seen := map[string]bool{}
		var all []string
		for _, e := range append(ask, run.Autonomy.AskBefore...) {
			if e = strings.TrimSpace(e); e != "" && !seen[e] {
				seen[e] = true
				all = append(all, e)
			}
		}
		if !run.Autonomy.KeepRepoLists {
			return runAutonomyBrief(run.Autonomy, all)
		}
		ask = all // the repo lists below, with this run's lines added
	}
	var out []string
	listed := map[string]bool{}
	for _, repo := range approves {
		listed[filepath.Clean(repo)] = true
		out = append(out, fmt.Sprintf("repo %s: the user approves its plans and merges its PRs: don't `mesh approve` a plan there (put it to the user: a `question` in your notes, then wait), and never merge its PRs", repo))
	}
	for _, repo := range merges {
		if !listed[filepath.Clean(repo)] {
			out = append(out, fmt.Sprintf("repo %s: the user merges its PRs: never merge one; tell the user when it is ready", repo))
		}
	}
	if len(ask) > 0 {
		out = append(out, "ask the user before: "+strings.Join(ask, "; "))
	}
	if len(out) > 0 {
		out = append([]string{"the user's autonomy settings (`mesh settings`, keys `repos.user_approves_plans`, `repos.user_merges`, `autonomy.ask_before`); a repo not listed is yours as before:"}, out...)
	}
	return out
}
