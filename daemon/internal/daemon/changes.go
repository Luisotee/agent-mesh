package daemon

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
)

// maxChangedFiles bounds the file list of one answer.
const maxChangedFiles = 200

// GetAgentChanges says what an agent's work holds that no remote has yet: the files that differ from the commit its
// branch started at (uncommitted work included), how many files are uncommitted, and the commits no remote branch has. It
// reads only (git with no optional locks), so it never disturbs the agent.
func (d *Daemon) GetAgentChanges(ctx context.Context, req *connect.Request[v1.GetAgentChangesRequest]) (*connect.Response[v1.GetAgentChangesResponse], error) {
	a, err := d.findAgentCrossRun(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	out := agentChanges(ctx, a)
	// What archiving would delete, when the caller asks (it walks the worktree): only a worktree on disk has any.
	if req.Msg.WithIgnored && out.Available {
		if st, err := os.Stat(a.Worktree); err == nil && st.IsDir() {
			out.Ignored, out.IgnoredMore = ignoredEntries(ctx, a.Repo, a.Worktree)
		}
	}
	return connect.NewResponse(out), nil
}

// agentChanges reads an agent's worktree, or, when that is gone (archived), its branch in the repo.
func agentChanges(ctx context.Context, a *v1.Agent) *v1.GetAgentChangesResponse {
	a = onItsBranch(ctx, a)
	out := &v1.GetAgentChangesResponse{}
	if a.Peer || a.Repo == "" || a.Worktree == "" || a.Branch == "" {
		out.Reason = "it has no worktree of its own"
		return out
	}
	base := a.BaseCommit
	dir, tip := a.Worktree, "HEAD"
	onDisk := true
	if st, err := os.Stat(a.Worktree); err != nil || !st.IsDir() {
		onDisk = false
		// Archived, or removed by hand: the branch is still in the repo.
		if _, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", "-q", "refs/heads/"+a.Branch+"^{commit}"); err != nil {
			out.Reason = "its worktree and its branch are gone"
			return out
		}
		dir, tip = a.Repo, "refs/heads/"+a.Branch
		out.Reason = "its worktree is not on disk: showing its branch"
	}
	if base == "" {
		base = defaultBaseRO(ctx, a.Repo)
	}
	// What git can't read is not "nothing": a count that fails makes the answer unavailable, so nothing is archived on a guess.
	var err1, err2 error
	out.Commits, err1 = countCommits(ctx, dir, base+".."+tip)
	out.Unpushed, err2 = countCommits(ctx, dir, tip, "--not", base, "--remotes")
	if err1 != nil || err2 != nil {
		return &v1.GetAgentChangesResponse{Reason: "git could not count its commits (is its base commit gone?)"}
	}
	out.Available = true
	files := map[string]*v1.ChangedFile{}
	// Against the commit the branch started at: what the branch changed, and what is uncommitted on top of it.
	diffTarget := []string{base}
	if !onDisk {
		diffTarget = []string{base, tip}
	}
	diffTarget = append(diffTarget, "--")
	if raw, _, _, err := gitRORaw(ctx, dir, append([]string{"diff", "--numstat", "-z", "-M"}, diffTarget...)...); err == nil {
		for _, f := range parseNumstat(string(raw)) {
			files[f.Path] = f
		}
	}
	if raw, _, _, err := gitRORaw(ctx, dir, append([]string{"diff", "--name-status", "-z", "-M"}, diffTarget...)...); err == nil {
		for path, status := range parseNameStatus(string(raw)) {
			if f := files[path]; f != nil {
				f.Status = status
			}
		}
	}
	if onDisk {
		raw, _, _, err := gitRORaw(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
		if err != nil {
			return &v1.GetAgentChangesResponse{Reason: "git could not read its worktree's status"}
		}
		for path, xy := range parseStatus(string(raw)) {
			out.Uncommitted++
			f := files[path]
			if f == nil {
				// Not in the diff against the base: a new file git doesn't know, or one a commit changed and the working tree put
				// back (still uncommitted, and tracked).
				f = &v1.ChangedFile{Path: path, Status: statusWord(xy)}
				files[path] = f
			}
			f.Uncommitted = true
		}
	}
	for _, f := range files {
		if f.Status == "" {
			f.Status = "modified"
		}
		out.Files = append(out.Files, f)
	}
	sort.Slice(out.Files, func(i, j int) bool {
		wi, wj := out.Files[i].Additions+out.Files[i].Deletions, out.Files[j].Additions+out.Files[j].Deletions
		if wi != wj {
			return wi > wj
		}
		return out.Files[i].Path < out.Files[j].Path
	})
	if len(out.Files) > maxChangedFiles {
		out.Files, out.Truncated = out.Files[:maxChangedFiles], true
	}
	return out
}

func countCommits(ctx context.Context, dir string, args ...string) (int32, error) {
	s, err := gitRO(ctx, dir, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	return int32(n), err
}

// parseNumstat reads `git diff --numstat -z`: `<added>\t<deleted>\t<path>\0`, and for a rename `<a>\t<d>\t\0<old>\0<new>\0`.
// A binary file has `-` for both counts.
func parseNumstat(raw string) []*v1.ChangedFile {
	var out []*v1.ChangedFile
	parts := strings.Split(raw, "\x00")
	for i := 0; i < len(parts); i++ {
		rec := parts[i]
		if rec == "" {
			continue
		}
		cols := strings.SplitN(rec, "\t", 3)
		if len(cols) < 3 {
			continue
		}
		path := cols[2]
		if path == "" && i+2 < len(parts) { // a rename: the old path, then the new one
			path = parts[i+2]
			i += 2
		}
		add, _ := strconv.Atoi(cols[0])
		del, _ := strconv.Atoi(cols[1])
		out = append(out, &v1.ChangedFile{Path: path, Additions: int32(add), Deletions: int32(del)})
	}
	return out
}

// parseNameStatus reads `git diff --name-status -z`: `<status>\0<path>\0`, a rename or copy `R<score>\0<old>\0<new>\0`.
func parseNameStatus(raw string) map[string]string {
	out := map[string]string{}
	parts := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(parts); i++ {
		st := parts[i]
		if st == "" {
			continue
		}
		word := map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'R': "renamed", 'C': "added", 'T': "modified"}[st[0]]
		if word == "" {
			word = "modified"
		}
		if st[0] == 'R' || st[0] == 'C' {
			if i+2 >= len(parts) {
				break
			}
			out[parts[i+2]] = word
			i += 2
			continue
		}
		out[parts[i+1]] = word
		i++
	}
	return out
}

// statusWord is the word for a porcelain status pair: "??" untracked, else by its first letter that is set.
func statusWord(xy string) string {
	if xy == "??" {
		return "untracked"
	}
	for _, c := range []byte{xy[1], xy[0]} {
		switch c {
		case 'D':
			return "deleted"
		case 'A':
			return "added"
		case 'R':
			return "renamed"
		}
	}
	return "modified"
}

// parseStatus reads `git status --porcelain=v1 -z`: the paths with uncommitted changes, and each one's status pair.
func parseStatus(raw string) map[string]string {
	out := map[string]string{}
	parts := strings.Split(raw, "\x00")
	for i := 0; i < len(parts); i++ {
		rec := parts[i]
		if len(rec) < 4 {
			continue
		}
		xy, path := rec[:2], rec[3:]
		out[path] = xy
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the rename's source follows
		}
	}
	return out
}
