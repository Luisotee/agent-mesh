package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

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

// autonomyBrief says, for each repo the user listed, what the root may not do, and what to ask before.
func (d *Daemon) autonomyBrief(r store.Reader) []string {
	approves := d.strings(r, "repos.user_approves_plans")
	merges := d.strings(r, "repos.user_merges")
	ask := d.strings(r, "autonomy.ask_before")
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
