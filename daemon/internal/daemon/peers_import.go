package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// What `ImportPeers` carries over from bin/mesh besides the registry entry: the peer's conversation (found in the
// run.json of the run that last asked it, live or archived), the worktree path it ran in (Claude keys a conversation
// by its folder, so the path must not change), when it was last asked and where its answers went.

// sessionRE is a conversation id as any CLI makes one; anything else (or an option) is not taken from a file.
var sessionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// binMeshRun is the part of bin/mesh's `<state>/<run>/run.json` an import reads.
type binMeshRun struct {
	Agents map[string]struct {
		Kind    string `json:"kind"`
		Repo    string `json:"repo"`
		Session string `json:"session"`
		Status  string `json:"status"`
		Peer    string `json:"peer"`
	} `json:"agents"`
}

// peerConversation is a peer's conversation in bin/mesh's state.
type peerConversation struct {
	session string
	run     string
	agent   string
	// running: its agent is in a run that isn't archived and not retired (starting, running, or any state this
	// doesn't know), so bin/mesh's process may still be on the conversation (one process per conversation).
	running bool
}

// sameDir reports whether two paths are one folder.
func sameDir(a, b string) bool {
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea != nil || eb != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// readBinMeshRun reads a run's run.json from the live or the archived folder.
func readBinMeshRun(stateDir, run string) (*binMeshRun, bool, time.Time, bool) {
	for _, c := range []struct {
		path     string
		archived bool
	}{{filepath.Join(stateDir, run, "run.json"), false}, {filepath.Join(stateDir, "archive", run, "run.json"), true}} {
		b, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		var r binMeshRun
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		var mod time.Time
		if st, err := os.Stat(c.path); err == nil {
			mod = st.ModTime()
		}
		return &r, c.archived, mod, true
	}
	return nil, false, time.Time{}, false
}

// findPeerConversation finds the conversation of the peer `name` (cloned at `repo`, of `kind`): first the run and
// agent peers.json names, else the newest run whose agent of that name (or marked as that peer) ran on that clone.
func findPeerConversation(stateDir, name, run, as, repo, kind string) (peerConversation, bool) {
	pick := func(r *binMeshRun, archived bool, runID string, agents ...string) (peerConversation, bool) {
		for _, n := range agents {
			e, ok := r.Agents[n]
			if !ok || !sessionRE.MatchString(e.Session) || (e.Repo != "" && !sameDir(e.Repo, repo)) ||
				(e.Kind != "" && e.Kind != kind) {
				continue
			}
			return peerConversation{session: e.Session, run: runID, agent: n,
				running: !archived && e.Status != "retired"}, true
		}
		return peerConversation{}, false
	}
	if run != "" && !strings.ContainsAny(run, `/\`) {
		if r, archived, _, ok := readBinMeshRun(stateDir, run); ok {
			names := []string{name}
			if as != "" {
				names = []string{as, name}
			}
			if c, ok := pick(r, archived, run, names...); ok {
				return c, true
			}
		}
	}
	// peers.json names no run, or its run is gone: the newest run that has the peer.
	type cand struct {
		run string
		mod time.Time
	}
	var all []cand
	for _, dir := range []string{stateDir, filepath.Join(stateDir, "archive")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() || e.Name() == "archive" {
				continue
			}
			if st, err := os.Stat(filepath.Join(dir, e.Name(), "run.json")); err == nil {
				all = append(all, cand{e.Name(), st.ModTime()})
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, c := range all {
		r, archived, _, ok := readBinMeshRun(stateDir, c.run)
		if !ok {
			continue
		}
		names := []string{name}
		for n, e := range r.Agents {
			if e.Peer == name && n != name {
				names = append(names, n)
			}
		}
		sort.Strings(names[1:])
		if conv, ok := pick(r, archived, c.run, names...); ok {
			return conv, true
		}
	}
	return peerConversation{}, false
}

// adoptConversation sets a peer's conversation from bin/mesh's state, when it has one that can come across: the
// session id, the folder it ran in (and whether that is the clone's own checkout), when it was last asked, by what,
// and its answers file. It changes nothing and returns a note when it can't (the note says why the peer is
// imported without it), and returns "" with nothing set for a peer bin/mesh never asked.
func (d *Daemon) adoptConversation(ctx context.Context, stateDir, name string, e peersFileEntry, p *v1.Peer) string {
	conv, ok := findPeerConversation(stateDir, name, e.Run, e.As, e.Repo, p.Kind)
	if !ok {
		return ""
	}
	if conv.running {
		return "its conversation is still running in bin/mesh (run " + conv.run + ", " + conv.agent + "): imported without it; retire it there and import again"
	}
	cwd, inClone := e.Cwd, e.InClone
	if inClone {
		cwd = e.Repo
	}
	switch {
	case cwd == "" || !filepath.IsAbs(cwd) || strings.Contains(cwd, "/.git/") || strings.HasSuffix(cwd, "/.git"):
		return "no usable folder is recorded for its conversation: imported without it"
	case sameDir(cwd, e.Repo):
		inClone = true // it ran in the clone's own checkout, whatever peers.json says
	}
	// A folder outside the daemon's own worktrees is adopted only when it is a worktree of this clone: peers.json
	// is the user's file, and a path in it may be any folder (a dotfiles repo, say) that git also calls a repository.
	if !inClone && !within(cwd, d.worktrees()) {
		if _, err := os.Stat(cwd); err == nil {
			mine, ok1 := gitCommonDir(ctx, cwd)
			clone, ok2 := gitCommonDir(ctx, e.Repo)
			if !ok1 || !ok2 || mine != clone {
				return "its folder " + cwd + " is not a worktree of " + e.Repo + ": imported without its conversation"
			}
		}
	}
	p.Worktree, p.InClone = cwd, inClone
	p.SessionId = conv.session
	p.LastRun, p.LastAgent = conv.run, conv.agent
	if t, err := time.Parse(time.RFC3339, e.Asked); err == nil {
		p.Asked = timestamppb.New(t)
	}
	p.LastAnswers = answersPath(stateDir, e.Answers.Run, e.Answers.File)
	return ""
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), filepath.Clean(dir)+string(filepath.Separator))
}

// gitCommonDir is the git directory a checkout shares with its worktrees, resolved, so two worktrees of one clone
// give the same path.
func gitCommonDir(ctx context.Context, dir string) (string, bool) {
	out, err := git(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return "", false
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	if r, err := filepath.EvalSymlinks(out); err == nil {
		out = r
	}
	return filepath.Clean(out), true
}

// answersPath is the file of the last `ask --peers` a peer answered, in bin/mesh's live or archived run folder;
// "" when peers.json names none or the file is gone.
func answersPath(stateDir, run, file string) string {
	if run == "" || file == "" || strings.Contains(run+file, "..") || strings.ContainsAny(run, `/\`) {
		return ""
	}
	for _, dir := range []string{filepath.Join(stateDir, run), filepath.Join(stateDir, "archive", run)} {
		if p := filepath.Join(dir, filepath.FromSlash(file)); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// peerUpgrade is a registered peer that was never asked, given the conversation bin/mesh has for it.
type peerUpgrade struct {
	peer        *v1.Peer
	oldWorktree string // the daemon's own worktree it leaves, to remove when clean
	made        bool   // the worktree at the conversation's folder was made by this call
	applied     bool   // the write took it
}

// upgradePeer prepares the upgrade of a registered peer, or says why not: a peer already asked (or running, or
// with a conversation) keeps its own.
func (d *Daemon) upgradePeer(ctx context.Context, stateDir, name string, e peersFileEntry, have *v1.Peer, unbriefed bool) (*peerUpgrade, string) {
	if unbriefed || have.SessionId != "" || have.Asked != nil || have.InClone {
		return nil, ""
	}
	if peerRunning(d.store.Read(ctx), name, "") != nil {
		return nil, ""
	}
	// bin/mesh's conversation is of the clone peers.json names: a peer registered on another clone is not its peer.
	if !sameDir(have.Repo, e.Repo) {
		return nil, "registered on another clone (" + have.Repo + ", peers.json has " + e.Repo + "): not upgraded"
	}
	up := proto.Clone(have).(*v1.Peer)
	if note := d.adoptConversation(ctx, stateDir, name, e, up); note != "" {
		return nil, note
	}
	if up.SessionId == "" {
		return nil, ""
	}
	// What bin/mesh last showed the conversation: the resumed peer is told what moved since that, not since an older
	// commit, and a worktree made for it starts there.
	if e.Seen != "" {
		if _, err := git(ctx, e.Repo, "cat-file", "-e", e.Seen+"^{commit}"); err == nil {
			up.Seen = e.Seen
		}
	}
	u := &peerUpgrade{peer: up}
	if up.Worktree != have.Worktree && !up.InClone {
		_, statErr := os.Stat(up.Worktree)
		if err := d.ensurePeerWorktree(ctx, up); err != nil {
			return nil, "its worktree " + up.Worktree + " can't be used (" + err.Error() + "): not upgraded"
		}
		u.made = statErr != nil
	}
	if up.Worktree != have.Worktree {
		u.oldWorktree = have.Worktree
	}
	return u, ""
}
