package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// prInfo is a pull request as a review needs it: its head commit, local in the repo.
type prInfo struct {
	Number  int32
	URL     string
	Head    string // the full commit id
	HeadRef string
	BaseRef string
	// Show is the command that shows the change: a git diff from the merge base when that is known, else gh's.
	Show string
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// ghBinary is the gh CLI: tests point Binaries["gh"] at a fake. The daemon only ever runs `gh pr view`: it
// reads, and never posts.
func (d *Daemon) ghBinary() string {
	if b := d.rt.cfg.Binaries["gh"]; b != "" {
		return b
	}
	if pathList := d.cliBaseEnv()["PATH"]; pathList != "" {
		if bin, ok := lookPath("gh", pathList); ok {
			return bin
		}
	}
	return "gh"
}

// resolvePR reads a pull request (its number in the repo, or its URL) and makes its head commit local, so a
// reviewer's worktree can start on it and a re-review can diff against an earlier head. A fetch that fails
// is an error: a reviewer on another commit would review the wrong code.
func (d *Daemon) resolvePR(ctx context.Context, repo, ref string) (*prInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.ghBinary(), "pr", "view", ref, "--json", "number,url,headRefOid,headRefName,baseRefName")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("gh pr view %s in %s: %s", ref, repo, msg))
	}
	var v struct {
		Number      int32  `json:"number"`
		URL         string `json:"url"`
		HeadRefOid  string `json:"headRefOid"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Number == 0 || v.HeadRefOid == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("gh pr view %s: unexpected answer %q", ref, strings.TrimSpace(string(out))))
	}
	pr := &prInfo{Number: v.Number, URL: v.URL, Head: v.HeadRefOid, HeadRef: v.HeadRefName, BaseRef: v.BaseRefName}
	if pr.URL == "" {
		pr.URL = "#" + strconv.Itoa(int(pr.Number))
	}
	if err := fetchCommit(ctx, repo, pr.Head, pr.Number); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	pr.Show = "gh pr diff " + strconv.Itoa(int(pr.Number))
	if pr.BaseRef != "" {
		git(ctx, repo, "fetch", "--quiet", "origin", pr.BaseRef) // best effort: the merge base needs it
		if mb, err := git(ctx, repo, "merge-base", pr.Head, "origin/"+pr.BaseRef); err == nil && mb != "" {
			pr.Show = "git diff " + short(mb) + ".." + short(pr.Head)
		}
	}
	return pr, nil
}

// fetchCommit makes a commit available in the repo: by its id, else by the PR's head ref.
func fetchCommit(ctx context.Context, repo, sha string, number int32) error {
	have := func() bool { _, err := git(ctx, repo, "cat-file", "-e", sha+"^{commit}"); return err == nil }
	if have() {
		return nil
	}
	git(ctx, repo, "fetch", "--quiet", "origin", sha)
	if have() {
		return nil
	}
	if _, err := git(ctx, repo, "fetch", "--quiet", "origin", fmt.Sprintf("pull/%d/head", number)); err != nil {
		return fmt.Errorf("the PR's head %s is not in %s and could not be fetched: %v", short(sha), repo, err)
	}
	if !have() {
		return fmt.Errorf("the PR's head %s is not in %s after fetching pull/%d/head", short(sha), repo, number)
	}
	return nil
}

// narrowSince reports whether `last` is an ancestor of `head` with no merge commits between them: then
// `git diff last head` holds exactly the fixes since, and a reviewer may read only that.
func narrowSince(ctx context.Context, repo, last, head string) bool {
	if last == "" {
		return false
	}
	if _, err := git(ctx, repo, "merge-base", "--is-ancestor", last, head); err != nil {
		return false
	}
	merges, err := git(ctx, repo, "rev-list", "--merges", last+".."+head)
	return err == nil && merges == ""
}
