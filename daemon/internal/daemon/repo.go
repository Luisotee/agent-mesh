package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luisotee/agent-mesh/daemon/internal/repoarg"
)

// A repo named bare (`mesh ask --repo agent-mesh`) is a folder of the `repos.dir` setting that is a repo's own
// top, as in bin/mesh's `resolve_repo`. The CLI sends a bare name as it is only when no folder of that name is in
// its working directory (the `repo_name` option in options.proto), so a repo in the caller's directory never needs
// the setting. Anything with a slash, `.`, `..` or a leading `~` is a path, resolved as before.

// repoTop is the top folder of the git repository `arg` names: a path as topOf takes it, or a bare name looked up
// in `repos.dir`.
func (d *Daemon) repoTop(ctx context.Context, arg string) (string, error) {
	if !repoarg.IsBareName(arg) {
		return topOf(ctx, arg)
	}
	dirs, err := d.Setting(d.store.Read(ctx), "repos.dir")
	if err != nil {
		return "", err
	}
	var plain []string
	var where []string
	for _, e := range dirs.([]any) {
		dir := expandHome(e.(string))
		where = append(where, dir)
		cand := filepath.Join(dir, arg)
		if st, err := os.Stat(cand); err != nil || !st.IsDir() {
			continue
		}
		top, err := git(ctx, cand, "rev-parse", "--show-toplevel")
		if err == nil && sameDir(top, cand) {
			return top, nil
		}
		plain = append(plain, cand)
	}
	switch {
	case len(where) == 0:
		return "", invalid("--repo %s: a bare name is looked up in the `repos.dir` setting, which is empty: give a path, or `mesh set --key repos.dir --value '[\"~/projects\"]'`", arg)
	case len(plain) > 0:
		return "", invalid("--repo %s: no repo of that name in %s (%s: a folder, not a repo's top)", arg, strings.Join(where, ", "), strings.Join(plain, ", "))
	}
	return "", invalid("--repo %s: no repo of that name in %s (the `repos.dir` setting); give a path", arg, strings.Join(where, ", "))
}
