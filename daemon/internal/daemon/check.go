package daemon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
)

const (
	checkCommits = 15
	checkFiles   = 15
	checkDirty   = 10
)

// CheckAgent is the root's look at a worker's branch in one call: what `git log`, `git diff --stat`, `git status`
// and `gh pr view` would each print, capped. The PRs are read from GitHub now (a read-only device gets the stored ones).
func (d *Daemon) CheckAgent(ctx context.Context, req *connect.Request[v1.CheckAgentRequest]) (*connect.Response[v1.CheckAgentResponse], error) {
	r := d.store.Read(ctx)
	a, err := d.FindAgent(r, auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CheckAgentResponse{Text: d.checkText(ctx, a)}), nil
}

func (d *Daemon) checkText(ctx context.Context, a *v1.Agent) string {
	out := []string{fmt.Sprintf("%s (%s, %s)", a.Name, strings.ToLower(strings.TrimPrefix(a.Role.String(), "AGENT_ROLE_")),
		strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_")))}
	if !ownsPRs(a) {
		return strings.Join(append(out, "no branch of its own to check (only a change worker has one)"), "\n")
	}
	if a.Repo == "" || a.Branch == "" {
		return strings.Join(append(out, "no repo or branch recorded"), "\n")
	}
	tip, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", "refs/heads/"+a.Branch)
	if err != nil {
		return strings.Join(append(out, "branch "+a.Branch+" does not exist (any more)"), "\n")
	}
	short := func(sha string) string {
		if len(sha) > 8 {
			return sha[:8]
		}
		return sha
	}
	// The base is where the worker's own work starts: its start commit, or, once it has merged the default branch
	// in, the newest commit of that branch it has merged (what main brought is not the worker's).
	base := a.BaseCommit
	if def := defaultBaseRO(ctx, a.Repo); def != "HEAD" {
		if mb, err := gitRO(ctx, a.Repo, "merge-base", def, tip); err == nil && mb != "" {
			base = mb
		}
	}
	out = append(out, fmt.Sprintf("branch %s at %s, base %s", a.Branch, short(tip), short(base)))
	if base != "" {
		span := base + ".." + tip
		if log, err := gitRO(ctx, a.Repo, "log", "--no-merges", "--format=%h %s", span); err == nil {
			lines := nonEmpty(strings.Split(log, "\n"))
			out = append(out, fmt.Sprintf("commits past the base: %d", len(lines)))
			for i, l := range lines {
				if i == checkCommits {
					out = append(out, fmt.Sprintf("  … %d more (`git log %s`)", len(lines)-checkCommits, span))
					break
				}
				out = append(out, "  "+l)
			}
		}
		if num, err := gitRO(ctx, a.Repo, "diff", "--numstat", base, tip); err == nil {
			out = append(out, diffStat(num)...)
		}
	}
	ev := workEvidence(ctx, a)
	state := []string{}
	if ev.HasPushed && ev.Commits > 0 {
		state = append(state, map[bool]string{true: "tip pushed", false: "tip NOT pushed"}[ev.Pushed])
	}
	if ev.HasDirty {
		state = append(state, map[bool]string{true: "worktree dirty", false: "worktree clean"}[ev.Dirty])
	}
	if len(state) > 0 {
		out = append(out, strings.Join(state, ", "))
	}
	if ev.Dirty && a.Worktree != "" {
		if st, err := os.Stat(a.Worktree); err == nil && st.IsDir() {
			if porcelain, err := gitRO(ctx, a.Worktree, "status", "--porcelain"); err == nil {
				files := nonEmpty(strings.Split(porcelain, "\n"))
				for i, f := range files {
					if i == checkDirty {
						out = append(out, fmt.Sprintf("  … %d more", len(files)-checkDirty))
						break
					}
					out = append(out, "  "+f)
				}
			}
		}
	}
	// A read-only device sees the PRs the daemon has stored: a phone polling this must not spend the host's GitHub
	// rate limit or write the store.
	readOnly := false
	if actor := auth.ActorFrom(ctx); actor != nil && actor.Class == v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE {
		readOnly = true
	} else {
		d.refreshPRs(ctx, ctx, a, true)
	}
	prs, _ := d.store.Read(ctx).AgentPRs(a.Id)
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	if txt := prText(&v1.AgentPullRequests{PullRequests: prs, ReadFailed: !readOnly && !d.prs.read(a.Id)}, false); txt != "" {
		out = append(out, "PRs: "+txt)
	} else {
		out = append(out, "PRs: none found")
	}
	return strings.Join(out, "\n")
}

func nonEmpty(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// diffStat prints `git diff --numstat` as a total and the files that changed most.
func diffStat(numstat string) []string {
	type file struct {
		path     string
		add, del int
	}
	var files []file
	var add, del int
	for _, l := range nonEmpty(strings.Split(numstat, "\n")) {
		f := strings.SplitN(l, "\t", 3)
		if len(f) != 3 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" for a binary file: 0
		dl, _ := strconv.Atoi(f[1])
		files = append(files, file{f[2], a, dl})
		add += a
		del += dl
	}
	if len(files) == 0 {
		return []string{"diff against the base: none"}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].add+files[i].del > files[j].add+files[j].del })
	out := []string{fmt.Sprintf("diff against the base: %d files, +%d -%d", len(files), add, del)}
	for i, f := range files {
		if i == checkFiles {
			out = append(out, fmt.Sprintf("  … %d more files", len(files)-checkFiles))
			break
		}
		out = append(out, fmt.Sprintf("  +%d -%d %s", f.add, f.del, f.path))
	}
	return out
}
