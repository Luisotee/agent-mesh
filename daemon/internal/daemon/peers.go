package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Peers: registered projects (a clone, a group, what it is for), not standing agents. The registry is the daemon's
// own (tables peers and peer_groups); bin/mesh's peers.json is only ever read, by an explicit ImportPeers. Each
// `ask --peers` starts a fresh read-only asker per peer (an agent record of the run, `Peer` set, named as the peer
// and then `<peer>-2`, …) in the peer's own detached worktree, on the asker role (asker.go) or what the ask names; it
// is closed by the idle retire once it has answered. The registry keeps the last asker's conversation (`session_id`,
// `kind`): continuing it (`--continue`) is the asking agent's choice, never the default, and a conversation runs in
// one process at a time.

const maxPeerAbout = 400

// refPartRE is a remote or a branch name git may be given as an argument: plain characters, never a leading dash
// (empty is allowed: no upstream).
var refPartRE = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._/-]*)?$`)

var peerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// peerName is the default name of a peer: the clone's name cut to 20 characters, and "-research".
func peerName(stem string) string {
	stem = strings.TrimSuffix(stem, ".git")
	if r := []rune(stem); len(r) > 20 {
		stem = strings.TrimRight(string(r[:20]), "-._")
	}
	return stem + "-research"
}

// expandHome reads a leading ~ as the daemon's home.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// cloneURLOK is the URLs a clone may come from: the usual transports, never git's `ext::` (which runs a command)
// or an option.
var cloneURLOK = regexp.MustCompile(`^(https?://|ssh://|git://|file://|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:)`)

func isURL(s string) bool { return strings.Contains(s, "://") || cloneURLOK.MatchString(s) }

func (d *Daemon) peerWorktree(name string) string { return filepath.Join(d.worktrees(), "peers", name) }

// ensurePeerWorktree makes the peer's own detached worktree of its clone, on the commit it last saw.
func (d *Daemon) ensurePeerWorktree(ctx context.Context, p *v1.Peer) error {
	if p.InClone {
		// It works in the clone's own checkout: the daemon makes nothing, and says so when that is gone.
		if _, err := os.Stat(p.Worktree); err != nil {
			return fmt.Errorf("%s works in the clone's own checkout %s, which is not there: restore it, or remove the peer and add it again", p.Name, p.Worktree)
		}
		return nil
	}
	if _, err := git(ctx, p.Worktree, "rev-parse", "--git-dir"); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.Worktree), 0o700); err != nil {
		return err
	}
	// Something else is where the worktree goes: an empty folder is replaced (and put back if the worktree can't
	// be made), anything else is the user's.
	var emptied os.FileMode
	if fi, err := os.Stat(p.Worktree); err == nil {
		if !fi.IsDir() || os.Remove(p.Worktree) != nil {
			return fmt.Errorf("%s exists and is not a worktree of %s: move it away", p.Worktree, p.Repo)
		}
		emptied = fi.Mode().Perm() | 0o700
	}
	git(ctx, p.Repo, "worktree", "prune")
	at := p.Seen
	if at == "" {
		at = "HEAD"
	}
	if _, err := git(ctx, p.Repo, "worktree", "add", "--detach", "--force", p.Worktree, at); err != nil {
		if emptied != 0 {
			os.Mkdir(p.Worktree, emptied)
		}
		return fmt.Errorf("making %s's worktree: %w", p.Name, err)
	}
	return nil
}

// ── the registry's actions ──

// ListPeers lists the peers (with where each is running now) and the groups.
func (d *Daemon) ListPeers(ctx context.Context, req *connect.Request[v1.ListPeersRequest]) (*connect.Response[v1.ListPeersResponse], error) {
	r := d.store.Read(ctx)
	peers, err := r.ListPeers(req.Msg.Group)
	if err != nil {
		return nil, err
	}
	groups, err := r.ListPeerGroups()
	if err != nil {
		return nil, err
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return nil, err
	}
	for _, p := range peers {
		for _, a := range agents {
			if peerOwns(a, p) && hasProcess(a) {
				p.RunningRun, p.RunningAgent = a.RunId, a.Id
			}
		}
	}
	out := &v1.ListPeersResponse{Peers: peers}
	for _, g := range groups {
		if req.Msg.Group == "" || g.Name == req.Msg.Group {
			out.Groups = append(out.Groups, g)
		}
	}
	return connect.NewResponse(out), nil
}

// peerUpstream reads a clone's upstream: its remote, branch and URL (empty for a clone with no remote).
func peerUpstream(ctx context.Context, repo string) *v1.PeerUpstream {
	up := &v1.PeerUpstream{}
	remotes, _ := git(ctx, repo, "remote")
	remote := "origin"
	if !strings.Contains("\n"+remotes+"\n", "\norigin\n") {
		remote, _, _ = strings.Cut(remotes, "\n")
	}
	if remote == "" {
		return up
	}
	up.Remote = remote
	up.Url, _ = git(ctx, repo, "remote", "get-url", remote)
	if ref, err := git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		up.Branch = strings.TrimPrefix(ref, remote+"/")
	} else if b, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "HEAD" {
		up.Branch = b
	}
	return up
}

// AddPeer registers a project as a peer: a URL is cloned under `peers.dir`/<group>/<repo>, a path is used as it
// is (the daemon never removes a clone it didn't make), and the peer gets a worktree of its own.
func (d *Daemon) AddPeer(ctx context.Context, req *connect.Request[v1.AddPeerRequest]) (*connect.Response[v1.AddPeerResponse], error) {
	m := req.Msg
	actor := auth.ActorFrom(ctx)
	if mid := m.Meta.GetMutationId(); mid != "" {
		if proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid); err == nil && proc == agentmeshv1connect.MeshServiceAddPeerProcedure {
			out := &v1.AddPeerResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	src := strings.TrimSpace(m.Source)
	group := m.Group
	if group == "" {
		group = "peers"
	}
	switch {
	case src == "" || strings.HasPrefix(src, "-"):
		return nil, invalid("give the URL to clone, or the path of a clone")
	case !peerNameRE.MatchString(group):
		return nil, invalid("group %q: letters, digits, dots, dashes and underscores", group)
	case utf8.RuneCountInString(m.About) > maxPeerAbout:
		return nil, invalid("about is %d characters: at most %d", utf8.RuneCountInString(m.About), maxPeerAbout)
	case m.Name != "" && !peerNameRE.MatchString(m.Name):
		return nil, invalid("name %q: letters, digits, dots, dashes and underscores", m.Name)
	}
	var clone string
	cloned := false
	if isURL(src) {
		if !cloneURLOK.MatchString(src) {
			return nil, invalid("%q is not a URL git may clone from here (https, ssh, git, file or user@host:path)", src)
		}
		stem := strings.TrimSuffix(filepath.Base(strings.TrimRight(src, "/")), ".git")
		if i := strings.LastIndex(stem, ":"); i >= 0 {
			stem = stem[i+1:]
		}
		if !peerNameRE.MatchString(stem) {
			return nil, invalid("can't name a clone from %q", src)
		}
		v, err := d.Setting(d.store.Read(ctx), "peers.dir")
		if err != nil {
			return nil, err
		}
		clone = filepath.Join(expandHome(v.(string)), group, stem)
	} else {
		abs, err := filepath.Abs(expandHome(src))
		if err != nil {
			return nil, invalid("source: %v", err)
		}
		if clone, err = git(ctx, abs, "rev-parse", "--show-toplevel"); err != nil {
			return nil, invalid("%s is not a git repository", src)
		}
	}
	name := m.Name
	if name == "" {
		name = peerName(filepath.Base(clone))
		if !peerNameRE.MatchString(name) {
			return nil, invalid("can't name a peer from %q: give --name (letters, digits, dots, dashes and underscores)", filepath.Base(clone))
		}
	}
	if _, err := d.store.Read(ctx).GetPeer(name); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("a peer named %s exists: give another name (--name)", name))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if isURL(src) {
		if _, err := os.Stat(clone); err == nil {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s exists already: add it by its path", clone))
		}
		if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", "clone", "--quiet", "--", src, clone)
		cmd.Env = append(os.Environ(), "GIT_ALLOW_PROTOCOL=https:http:ssh:git:file", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(clone)
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("cloning %s: %v: %s", src, err, strings.TrimSpace(string(out))))
		}
		cloned = true
	}
	up := peerUpstream(ctx, clone)
	seen := ""
	if up.Remote != "" && up.Branch != "" {
		seen, _ = git(ctx, clone, "rev-parse", up.Remote+"/"+up.Branch)
	}
	if seen == "" {
		seen, _ = git(ctx, clone, "rev-parse", "HEAD")
	}
	p := &v1.Peer{Name: name, Repo: clone, Group: group, About: m.About, Upstream: up, Worktree: d.peerWorktree(name),
		Seen: seen, Added: timestamppb.New(d.now()), Cloned: cloned}
	_, statErr := os.Stat(p.Worktree)
	madeWorktree := statErr != nil // one that was there is another call's: it stays if this one fails
	if err := d.ensurePeerWorktree(ctx, p); err != nil {
		if cloned {
			os.RemoveAll(clone)
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	out := &v1.AddPeerResponse{Peer: p}
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceAddPeerProcedure, out, func(tx *store.Tx) error {
		if _, err := tx.GetPeerGroup(group); errors.Is(err, store.ErrNotFound) {
			if err := tx.PutPeerGroup(&v1.PeerGroup{Name: group}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.GetPeer(name); err == nil {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("a peer named %s exists", name))
		}
		return tx.PutPeer(p)
	})
	if err != nil {
		if madeWorktree {
			git(ctx, clone, "worktree", "remove", "--force", p.Worktree)
		}
		if cloned {
			os.RemoveAll(clone)
		}
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// EditPeer replaces the `about` of a peer or of a group.
func (d *Daemon) EditPeer(ctx context.Context, req *connect.Request[v1.EditPeerRequest]) (*connect.Response[v1.EditPeerResponse], error) {
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("name a peer or a group")
	}
	if utf8.RuneCountInString(m.About) > maxPeerAbout {
		return nil, invalid("about is %d characters: at most %d", utf8.RuneCountInString(m.About), maxPeerAbout)
	}
	out := &v1.EditPeerResponse{}
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceEditPeerProcedure, out, func(tx *store.Tx) error {
		if p, err := tx.GetPeer(m.Target); err == nil {
			p.About = m.About
			out.Peer = p
			return tx.PutPeer(p)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		g, err := tx.GetPeerGroup(m.Target)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no peer or group %q (a group exists once a peer is added to it: `mesh peers` lists them)", m.Target)
		} else if err != nil {
			return err
		}
		g.About = m.About
		out.Group = g
		return tx.PutPeerGroup(g)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// RemovePeer removes a peer from the registry and its worktree, refusing while its agent runs.
func (d *Daemon) RemovePeer(ctx context.Context, req *connect.Request[v1.RemovePeerRequest]) (*connect.Response[v1.RemovePeerResponse], error) {
	m := req.Msg
	// Under the lock an ask of this peer takes, so the check that nothing runs holds until it is removed.
	lock := d.agentLock("peer:" + m.Name)
	lock.Lock()
	defer lock.Unlock()
	r := d.store.Read(ctx)
	p, err := r.GetPeer(m.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no peer %q", m.Name)
	} else if err != nil {
		return nil, err
	}
	if other := peerRunning(r, p, ""); other != nil {
		return nil, failedPrecondition("%s is running in run %s (%s): retire it first", p.Name, other.RunId, other.Name)
	}
	// The clone goes only if the daemon made it and no other peer works in it.
	if m.DeleteClone {
		if p.InClone {
			return nil, invalid("%s works in the clone's own checkout (%s): remove that yourself, or remove the peer without --delete-clone", p.Name, p.Repo)
		}
		if !p.Cloned {
			return nil, invalid("%s's clone is %s, which the daemon did not make: remove it yourself, or remove the peer without --delete-clone", p.Name, p.Repo)
		}
		peers, err := r.ListPeers("")
		if err != nil {
			return nil, err
		}
		for _, o := range peers {
			if o.Name != p.Name && o.Repo == p.Repo {
				return nil, failedPrecondition("%s also uses %s: remove it first", o.Name, p.Repo)
			}
		}
	}
	out := &v1.RemovePeerResponse{Worktree: p.Worktree}
	if p.InClone {
		out.Worktree = ""
	}
	if _, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceRemovePeerProcedure, out, func(tx *store.Tx) error {
		return tx.DeletePeer(p.Name)
	}); err != nil {
		return nil, err
	}
	// The peer is gone from the registry either way. A worktree of the daemon's own that git won't remove is
	// deleted, and one that still stands is said. A peer in the clone's own checkout has no worktree of its own
	// (that folder is the user's), and a folder outside the daemon's worktrees (one imported from bin/mesh, which may
	// still list it) is left whatever its state: it is not the daemon's to delete.
	own := strings.HasPrefix(filepath.Clean(p.Worktree)+string(filepath.Separator), filepath.Clean(d.peerWorktree(""))+string(filepath.Separator))
	// Another peer imported from the same bin/mesh folder still works in it: it stays.
	shared := false
	if all, err := d.store.Read(ctx).ListPeers(""); err == nil {
		for _, o := range all {
			if o.Name != p.Name && !o.InClone && sameDir(o.Worktree, p.Worktree) {
				shared = true
			}
		}
	}
	switch {
	case p.InClone:
	case shared:
		out.Worktree = "" // left where it is
	case !own:
		out.Worktree = "" // bin/mesh's, or the user's: left where it is
	default:
		if _, err := git(ctx, p.Repo, "worktree", "remove", "--force", p.Worktree); err != nil {
			os.RemoveAll(p.Worktree)
			git(ctx, p.Repo, "worktree", "prune")
			if _, serr := os.Stat(p.Worktree); serr == nil {
				return nil, fmt.Errorf("%s is removed from the registry, but its worktree %s could not be: %w", p.Name, p.Worktree, err)
			}
		}
	}
	if m.DeleteClone {
		if err := os.RemoveAll(p.Repo); err != nil {
			return nil, err
		}
		out.DeletedClone = p.Repo
	}
	return connect.NewResponse(out), nil
}

// peersFileEntry is one peer of bin/mesh's peers.json, as far as an import reads it: the clone, group, about and
// upstream. What bin/mesh recorded of a conversation is not read.
type peersFileEntry struct {
	Repo     string `json:"repo"`
	Group    string `json:"group"`
	About    string `json:"about"`
	Seen     string `json:"seen"`
	Upstream struct {
		Remote string `json:"remote"`
		Branch string `json:"branch"`
		URL    string `json:"url"`
	} `json:"upstream"`
}

// peersFile is bin/mesh's peers.json, as far as an import reads it.
type peersFile struct {
	Peers  map[string]peersFileEntry `json:"peers"`
	Groups map[string]struct {
		About string `json:"about"`
	} `json:"groups"`
}

// ImportPeers registers what bin/mesh's peers.json lists and the registry doesn't have: the clone, its group, what
// it is for and its upstream, each with a worktree of the daemon's own. No conversation comes across: a peer's
// asker is a fresh one each ask. A peer already registered is left alone. The file is only read.
func (d *Daemon) ImportPeers(ctx context.Context, req *connect.Request[v1.ImportPeersRequest]) (*connect.Response[v1.ImportPeersResponse], error) {
	m := req.Msg
	path := m.From
	if path == "" {
		path = filepath.Join(os.Getenv("HOME"), ".local", "state", "agent-mesh", "peers.json")
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".local", "state", "agent-mesh", "peers.json")
		}
	}
	b, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("reading %s: %v", path, err))
	}
	var f peersFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, invalid("%s is not a peers.json: %v", path, err)
	}
	out := &v1.ImportPeersResponse{}
	var names []string
	for n := range f.Peers {
		names = append(names, n)
	}
	sort.Strings(names)
	var peers, created []*v1.Peer
	for _, n := range names {
		e := f.Peers[n]
		skip := func(why string) { out.Skipped = append(out.Skipped, n+": "+why) }
		switch {
		case !peerNameRE.MatchString(n):
			skip("not a name the daemon accepts")
			continue
		case e.Repo == "":
			skip("no clone recorded")
			continue
		}
		if _, err := d.store.Read(ctx).GetPeer(n); err == nil {
			skip("already registered")
			continue
		}
		if _, err := git(ctx, e.Repo, "rev-parse", "--git-dir"); err != nil {
			skip("its clone " + e.Repo + " is not a git repository here")
			continue
		}
		about := e.About
		if utf8.RuneCountInString(about) > maxPeerAbout {
			about = string([]rune(about)[:maxPeerAbout])
		}
		seen := e.Seen
		if _, err := git(ctx, e.Repo, "cat-file", "-e", seen+"^{commit}"); seen == "" || err != nil {
			seen, _ = git(ctx, e.Repo, "rev-parse", "HEAD")
		}
		group := e.Group
		if group == "" {
			group = "peers"
		}
		if !peerNameRE.MatchString(group) {
			skip("its group " + strconv.Quote(group) + " is not a name the daemon accepts")
			continue
		}
		// The remote and branch go to git as arguments: one that could read as an option is left out.
		up := &v1.PeerUpstream{Remote: e.Upstream.Remote, Branch: e.Upstream.Branch, Url: e.Upstream.URL}
		if !refPartRE.MatchString(up.Remote) || !refPartRE.MatchString(up.Branch) {
			if up.Remote != "" || up.Branch != "" {
				skip("imported without its upstream " + strconv.Quote(up.Remote+"/"+up.Branch) + ": not a plain remote and branch name")
			}
			up = &v1.PeerUpstream{}
		}
		p := &v1.Peer{Name: n, Repo: e.Repo, Group: group, About: about, Seen: seen, Worktree: d.peerWorktree(n),
			Upstream: up, Added: timestamppb.New(d.now())}
		_, statErr := os.Stat(p.Worktree)
		if err := d.ensurePeerWorktree(ctx, p); err != nil {
			skip(err.Error())
			continue
		}
		if statErr != nil {
			created = append(created, p)
		}
		peers = append(peers, p)
	}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceImportPeersProcedure, out, func(tx *store.Tx) error {
		out.Imported = nil
		var gnames []string
		for g := range f.Groups {
			gnames = append(gnames, g)
		}
		sort.Strings(gnames)
		for _, g := range gnames {
			if _, err := tx.GetPeerGroup(g); errors.Is(err, store.ErrNotFound) && peerNameRE.MatchString(g) {
				about := f.Groups[g].About
				if r := []rune(about); len(r) > maxPeerAbout {
					about = string(r[:maxPeerAbout])
				}
				if err := tx.PutPeerGroup(&v1.PeerGroup{Name: g, About: about}); err != nil {
					return err
				}
				out.Imported = append(out.Imported, "group "+g)
			}
		}
		for _, p := range peers {
			if _, err := tx.GetPeer(p.Name); err == nil {
				out.Skipped = append(out.Skipped, p.Name+": already registered")
				continue
			}
			if _, err := tx.GetPeerGroup(p.Group); errors.Is(err, store.ErrNotFound) {
				if err := tx.PutPeerGroup(&v1.PeerGroup{Name: p.Group}); err != nil {
					return err
				}
			}
			if err := tx.PutPeer(p); err != nil {
				return err
			}
			out.Imported = append(out.Imported, p.Name)
		}
		return nil
	})
	if err != nil {
		for _, p := range created {
			git(ctx, p.Repo, "worktree", "remove", "--force", p.Worktree)
		}
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// ── asking peers ──

// resolvePeers is the peers a `peers` argument names: "all", a group, or a comma list of peers and groups.
func resolvePeers(r store.Reader, spec string) ([]*v1.Peer, error) {
	all, err := r.ListPeers("")
	if err != nil {
		return nil, err
	}
	var out []*v1.Peer
	seen := map[string]bool{}
	add := func(p *v1.Peer) {
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, p)
		}
	}
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		found := false
		for _, p := range all {
			if tok == "all" || p.Name == tok || p.Group == tok {
				add(p)
				found = true
			}
		}
		if !found {
			return nil, notFound("no peer or group %q (`mesh peers` lists them)", tok)
		}
	}
	if len(out) == 0 {
		return nil, invalid("no peers named")
	}
	return out, nil
}

// preparePeer brings a peer's checkout up to its upstream before it is asked, and says what it found: the
// note goes at the top of its task. A fetch that fails leaves the checkout as it was; a checkout with local
// changes is not moved, and neither is one an asker of another run is reading (held): that asker keeps its own
// head, and the new one is told so and reads the newer code with git show.
func (d *Daemon) preparePeer(ctx context.Context, p *v1.Peer, refresh bool, held *v1.Agent) (note string, newCommits int32, at string) {
	at, _ = git(ctx, p.Worktree, "rev-parse", "HEAD")
	stay := p.InClone || held != nil
	stayNote := func(head string) string {
		if p.InClone {
			return fmt.Sprintf("You run in the clone's own checkout, which stays at %s: read the newer code with `git show %s:<path>`.", short(at), short(head))
		}
		return fmt.Sprintf("Another asker (%s, run %s) is reading this checkout, so it stays at %s: read the newer code with `git show %s:<path>`.",
			held.Name, held.RunId, short(at), short(head))
	}
	var b strings.Builder
	up := p.Upstream
	where := ""
	if up != nil && up.Remote != "" && up.Branch != "" {
		where = up.Remote + "/" + up.Branch
	}
	switch {
	case !refresh:
		b.WriteString("Your checkout was not refreshed (no_refresh): it is as you left it.")
	case where == "":
		b.WriteString("This project has no upstream recorded: your checkout is as you left it.")
	default:
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cmd := exec.CommandContext(fctx, "git", "-C", p.Repo, "fetch", "--quiet", "--", up.Remote)
		// Never a prompt for a password or a passphrase: a peer that needs one is reported, not waited on.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
		fout, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(fout)))
		}
		cancel()
		head, herr := git(ctx, p.Repo, "rev-parse", where)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "Could not fetch %s (%s): your checkout is as you left it.", up.Remote, lineOf(err.Error()))
		case herr != nil:
			fmt.Fprintf(&b, "Upstream %s was not found: your checkout is as you left it.", where)
		case p.Seen == "":
			// Nothing recorded to compare with: the checkout goes to the head when it is clean, and only then
			// has it seen it.
			fmt.Fprintf(&b, "Upstream (%s) is at %s; you have not looked at this project before.", where, short(head))
			if at == head {
				p.Seen = head
			} else if stay {
				// The clone's own checkout is never moved, nor one another asker reads: the peer reads the newer code with git
				// show. A checkout that is only held keeps its old `seen`, so the next ask moves it and says so until it does.
				if p.InClone {
					p.Seen = head
				}
				b.WriteString(" " + stayNote(head))
			} else if st, err := git(ctx, p.Worktree, "status", "--porcelain"); err == nil && st == "" {
				if _, err := git(ctx, p.Worktree, "checkout", "--quiet", "--detach", head); err == nil {
					at, p.Seen = head, head
				}
			}
		case head == p.Seen:
			fmt.Fprintf(&b, "Upstream (%s) hasn't changed since you last looked.", where)
		default:
			cnt, _ := git(ctx, p.Repo, "rev-list", "--count", p.Seen+".."+head)
			n, _ := strconv.Atoi(cnt)
			newCommits = int32(n)
			stat, _ := git(ctx, p.Repo, "diff", "--shortstat", p.Seen, head)
			fmt.Fprintf(&b, "Upstream moved since you last looked: %d commit(s) on %s, %s..%s (%s). Re-check what your answer relies on against them (`git log --stat %s..%s`):\n```\n",
				n, where, short(p.Seen), short(head), strings.TrimSpace(stat), short(p.Seen), short(head))
			log, _ := git(ctx, p.Repo, "log", "--oneline", "-30", p.Seen+".."+head)
			b.WriteString(log + "\n```")
			if n > 30 {
				fmt.Fprintf(&b, "\n… and %d more", n-30)
			}
			// The checkout moves to the head when it is clean. Only then has it "seen" the head: a checkout left
			// behind is told so again on the next ask, with the same commits.
			if stay {
				if p.InClone {
					p.Seen = head
				}
				b.WriteString("\n" + stayNote(head))
			} else if st, err := git(ctx, p.Worktree, "status", "--porcelain"); err == nil && st == "" {
				if _, err := git(ctx, p.Worktree, "checkout", "--quiet", "--detach", head); err == nil {
					at = head
					p.Seen = head
				} else {
					b.WriteString("\nYour checkout could not be moved to it.")
				}
			} else {
				b.WriteString("\nYour checkout has local changes, so it was not moved.")
			}
		}
	}
	fmt.Fprintf(&b, "\nYour checkout is at %s.", short(at))
	return b.String(), newCommits, at
}

// peerOwns tells whether an agent is an asker of this peer: a peer agent in its checkout.
func peerOwns(a *v1.Agent, p *v1.Peer) bool {
	return a.Peer && a.Worktree == p.Worktree && a.Repo == p.Repo
}

// peerOfAgent is the registered peer an asker of a peer asks: its askers are named as the peer and then `<peer>-2`, …,
// so it is found by the checkout it works in. nil when the peer was removed.
func peerOfAgent(r store.Reader, a *v1.Agent) *v1.Peer {
	if !a.Peer {
		return nil
	}
	if p, err := r.GetPeer(a.Name); err == nil && peerOwns(a, p) {
		return p
	}
	peers, err := r.ListPeers("")
	if err != nil {
		return nil
	}
	for _, p := range peers {
		if peerOwns(a, p) {
			return p
		}
	}
	return nil
}

// peerRunning is an asker of this peer that has a process in a run other than runID ("" for any run): starting,
// running, or with a reopen asked for.
func peerRunning(r store.Reader, p *v1.Peer, runID string) *v1.Agent {
	agents, err := r.ListAgents("")
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if peerOwns(a, p) && (runID == "" || a.RunId != runID) && hasProcess(a) {
			return a
		}
	}
	return nil
}

// checkoutHolder is an asker of this peer that is reading its checkout: one with a process in another run, or one of
// this run that has not answered yet or is not at its prompt (at a dialog, at a limit, with a reopen asked for). The
// checkout is not moved under it.
func checkoutHolder(r store.Reader, p *v1.Peer, runID string) *v1.Agent {
	agents, err := r.ListAgents("")
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if !peerOwns(a, p) || !hasProcess(a) {
			continue
		}
		if a.RunId != runID || a.State != v1.AgentState_AGENT_STATE_IDLE || a.PendingReopen != nil {
			return a
		}
		if rep, err := lastReport(r, a); err == nil && rep == nil {
			return a // idle, but its answer is not given yet
		}
	}
	return nil
}

// agyAskerOn is another agy asker with a process on the peer's checkout.
func agyAskerOn(r store.Reader, p *v1.Peer) *v1.Agent {
	agents, err := r.ListAgents("")
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if peerOwns(a, p) && a.Kind == "agy" && hasProcess(a) {
			return a
		}
	}
	return nil
}

// skipAgy says why an agy asker can't work in the peer's checkout now (mine, if any, is the agent being given the question).
func skipAgy(r store.Reader, p *v1.Peer, kind string, mine *v1.Agent) string {
	if kind != "agy" {
		return ""
	}
	if p.InClone {
		return "it works in the clone's own checkout, where an agy asker's hooks would be written"
	}
	if other := agyAskerOn(r, p); other != nil && (mine == nil || other.Id != mine.Id) {
		return "another agy asker (" + other.Name + ", run " + other.RunId + ") is on its checkout, and agy's hooks are per checkout"
	}
	return ""
}

// conversationRunning is the agent of a run other than runID that has a process on this conversation: one process
// per conversation.
func conversationRunning(r store.Reader, session, runID string) *v1.Agent {
	if session == "" {
		return nil
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if a.SessionId == session && a.RunId != runID && hasProcess(a) {
			return a
		}
	}
	return nil
}

// lastAskerOfRun is the asker this run last asked the peer with (the registry's `last_agent`), whether or not it has
// answered yet: `--continue` goes back to it first.
func lastAskerOfRun(r store.Reader, p *v1.Peer, runID string) *v1.Agent {
	if p.LastRun != runID || p.LastAgent == "" {
		return nil
	}
	a, err := r.GetAgent(p.LastAgent)
	if err != nil || !peerOwns(a, p) || a.RunId != runID {
		return nil
	}
	return a
}

// lastAskerOf is the newest agent of this run that holds the conversation `session` of the peer: the asker `--continue`
// goes back to (retired or not).
func lastAskerOf(r store.Reader, p *v1.Peer, runID, session string) *v1.Agent {
	agents, err := r.ListAgents(runID)
	if err != nil {
		return nil
	}
	var mine *v1.Agent
	for _, a := range agents {
		if peerOwns(a, p) && a.SessionId == session && (mine == nil || a.CreatedAt.AsTime().After(mine.CreatedAt.AsTime())) {
			mine = a
		}
	}
	return mine
}

// askPeers asks peers one question, all at once, collected like a round into reports/peers-<stamp>.md.
func (d *Daemon) askPeers(ctx context.Context, actor *v1.Actor, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	switch {
	case body == "":
		return nil, invalid("no question given (--body or --file)")
	case m.Reviewers != "" || m.Review != "" || m.Repo != "" || m.Slug != "" || m.Agent != "" || m.Since != "" || m.Size != "":
		return nil, invalid("peers is a question for peers: body, continue, tier, kind, model, effort, timeout and no_refresh only")
	}
	r := d.store.Read(ctx)
	peers, err := resolvePeers(r, m.Peers)
	if err != nil {
		return nil, err
	}
	timeout, err := d.askTimeout(r, m.Timeout)
	if err != nil {
		return nil, err
	}
	now := d.now()
	ask := &v1.Ask{Id: askIDFor(actor, m.Meta), RunId: actor.RunId, CallerId: actor.Id, CallerName: actor.Name, Kind: v1.AskKind_ASK_KIND_PEERS,
		Status: v1.AskStatus_ASK_STATUS_OPEN, Tier: m.Tier, Body: body, Collected: true, CreatedAt: timestamppb.New(now),
		AnswersPath: filepath.Join(d.paths.RunDir(actor.RunId), "reports", "peers-"+stamp(now)+".md"), Deadline: timestamppb.New(now.Add(timeout))}
	groups := map[string]bool{}
	var ctxText []string
	for _, p := range peers {
		if g, err := r.GetPeerGroup(p.Group); err == nil && g.About != "" && !groups[p.Group] {
			ctxText = append(ctxText, g.About)
		}
		groups[p.Group] = true
	}
	ask.Context = strings.Join(ctxText, "\n\n")
	out := &v1.AskResponse{Ask: ask}
	names := make([]string, len(peers))
	for i, p := range peers {
		names[i] = p.Name
	}
	return d.collectRound(ctx, actor, m.Meta, ask, out, names, false, nil, func(_ context.Context, i int) (*v1.AskMember, *v1.Agent, string) {
		return d.startPeer(ctx, actor, peers[i], m, body)
	}, func(store.Reader, *v1.Ask) string { return "" }, nil)
}

// startPeer asks one peer: a fresh asker in this run, on the asker role or what the ask names. With `continue` it goes
// back to the peer's last conversation instead: this run's agent for it (given the question, resumed if retired), or
// an earlier run's conversation resumed in a new agent record. A peer that can't be asked is a member with a line
// saying why.
func (d *Daemon) startPeer(ctx context.Context, actor *v1.Actor, p *v1.Peer, m *v1.AskRequest, body string) (*v1.AskMember, *v1.Agent, string) {
	member := &v1.AskMember{Name: p.Name, Tier: m.Tier, PeerRepo: filepath.Base(p.Repo), About: p.About,
		Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING}
	fail := func(line string) (*v1.AskMember, *v1.Agent, string) {
		member.Status, member.Line = v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED, line
		return member, nil, fmt.Sprintf("%s: not asked: %s", p.Name, line)
	}
	// One ask of a peer at a time, from any run: the fetch and the checkout move, and the check that no other
	// process holds its conversation must hold until this one has launched.
	lock := d.agentLock("peer:" + p.Name)
	lock.Lock()
	defer lock.Unlock()
	r := d.store.Read(ctx)
	// Re-read: another ask of this run may have moved it.
	if cur, err := r.GetPeer(p.Name); err == nil {
		p = cur
	} else if errors.Is(err, store.ErrNotFound) {
		return fail("it was removed from the registry meanwhile")
	} else {
		return fail(err.Error())
	}
	if err := d.ensurePeerWorktree(ctx, p); err != nil {
		return fail(err.Error())
	}
	// The conversation to continue, if the caller asked to: this run's last asker of the peer (answered or not), else
	// the conversation of the asker that last answered, which may be an earlier run's.
	session, kind, freshNote := "", m.Kind, ""
	var mine *v1.Agent
	if m.Continue {
		if la := lastAskerOfRun(r, p, actor.RunId); la != nil {
			mine, session, kind = la, la.SessionId, la.Kind
		} else if p.SessionId != "" {
			session, kind = p.SessionId, orClaude(p.Kind) // a peer registered before its CLI was kept ran on Claude
			mine = lastAskerOf(r, p, actor.RunId, session)
		} else {
			freshNote = "fresh (it has no earlier conversation to continue)"
		}
		if (mine != nil || session != "") && m.Kind != "" && m.Kind != kind {
			return fail(fmt.Sprintf("its last conversation is on %s: --continue stays on that CLI (drop --kind %s, or ask without --continue)", kind, m.Kind))
		}
		if mine == nil && session != "" {
			if other := conversationRunning(r, session, actor.RunId); other != nil {
				return fail(fmt.Sprintf("its last conversation is running in run %s (%s): one process per conversation; retire it there, wait for its answer, or ask fresh (without --continue)", other.RunId, other.Id))
			}
		}
	}
	if mine != nil {
		// A stopped asker is brought back by the task, and the conversation may have gone on in another run meanwhile.
		if !hasProcess(mine) {
			if other := conversationRunning(r, session, ""); other != nil {
				return fail(fmt.Sprintf("its last conversation is running in run %s (%s): one process per conversation; retire it there, wait for its answer, or ask fresh (without --continue)", other.RunId, other.Id))
			}
		}
		if open, _, err := collecting(r, mine.Id, actor.Id); err != nil {
			return fail(err.Error())
		} else if mine.State == v1.AgentState_AGENT_STATE_WORKING || mine.State == v1.AgentState_AGENT_STATE_STARTING ||
			mine.PendingReopen != nil || open != nil {
			return fail("it is still answering an earlier question of this run")
		}
		// Another caller of this run that it still owes an answer, or whose answer it waits for, holds it (its
		// report goes to its parent only, and the parent can answer only its own child).
		if mine.ParentId != actor.Id {
			if why, err := peerHeldBy(r, mine); err != nil {
				return fail(err.Error())
			} else if why != "" {
				return fail(why)
			}
		}
		kind = mine.Kind
	}
	var model, effort string
	if mine == nil {
		var err error
		// agy writes its hooks into the checkout it works in: not into the user's own clone, and not where another agy
		// asker already is (the file names one agent).
		skip := map[string]string{}
		if p.InClone {
			skip["agy"] = "it works in the clone's own checkout, where an agy asker's hooks would be written"
		} else if other := agyAskerOn(r, p); other != nil {
			skip["agy"] = "another agy asker (" + other.Name + ", run " + other.RunId + ") is on its checkout, and agy's hooks are per checkout"
		}
		if kind, model, effort, err = d.askerFor(ctx, r, actor.RunId, p.Repo, kind, m.Tier, m.Model, m.Effort, "asking "+p.Name, skip); err != nil {
			var ce *connect.Error
			msg := err.Error()
			if errors.As(err, &ce) {
				msg = ce.Message()
			}
			return fail(msg)
		}
	}
	if why := skipAgy(r, p, kind, mine); why != "" {
		return fail(why + ": ask on another CLI")
	}
	if err := d.ReserveGuard(ctx, d.store.Read(ctx), actor.RunId, kind, model, effort, "asking "+p.Name, false); err != nil {
		return fail(err.Error())
	}
	before, _ := git(ctx, p.Worktree, "rev-parse", "HEAD")
	note, newCommits, at := d.preparePeer(ctx, p, !m.NoRefresh, checkoutHolder(r, p, actor.RunId))
	member.Head, member.UpstreamNew = at, newCommits
	// An ask refused after the checkout moved puts it back: another asker may be working on it.
	restore := func() {
		if before == "" || at == before || p.InClone {
			return
		}
		if now, err := git(ctx, p.Worktree, "rev-parse", "HEAD"); err == nil && now == at { // not moved again meanwhile
			git(ctx, p.Worktree, "checkout", "--quiet", "--detach", before)
		}
	}
	task := askPrefix + "\n\n" + note + "\n\n" + body
	var agent *v1.Agent
	if mine != nil {
		// It answers whoever asks it this time: its parent is the caller of the ask that brought it into the run,
		// and a report to anyone else is refused ("an agent sends only to its parent or its children"). SendMessage
		// moves the parent in the write that stores the task, so a refused or failed ask leaves it where it was.
		res, err := d.SendMessage(withReparent(ctx, actor.Id), connect.NewRequest(&v1.SendMessageRequest{Meta: subMeta(m.Meta, "send-"+p.Name), To: mine.Id,
			Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: task}))
		if err != nil {
			restore()
			return fail(err.Error())
		}
		if res.Msg.ResumeError != "" {
			restore()
			// The task was stored with the refusal: it is not for the next resume, which brings the asker back for
			// a later question, so it is marked read.
			d.dropStoredTask(ctx, actor, mine.Id, res.Msg.Message.GetId())
			return fail("could not be resumed: " + res.Msg.ResumeError)
		}
		member.How = "continued"
		if res.Msg.Resumed != nil {
			member.How = "continued (resumed)"
		}
		cur, _ := d.getAgent(d.store.Read(ctx), mine.Id)
		if cur == nil {
			cur = mine
		}
		agent = d.view(cur)
		member.AgentId, member.Kind, member.Model, member.Effort = mine.Id, mine.Kind, mine.Model, mine.Effort
	} else {
		a, missing, err := d.launchPeerAgent(ctx, actor, p, kind, model, effort, m.Tier, session, task)
		if err != nil {
			restore()
			var ce *connect.Error
			msg := err.Error()
			if errors.As(err, &ce) {
				msg = ce.Message()
			}
			return fail(msg)
		}
		member.How = "fresh"
		switch {
		case freshNote != "":
			member.How = freshNote
		case session != "" && missing:
			member.How = "fresh (its earlier conversation was not found)"
		case session != "":
			member.How = "continued (its earlier conversation resumed)"
		}
		member.AgentId, member.Kind, member.Model, member.Effort = a.Id, a.Kind, a.Model, a.Effort
		agent = a
	}
	// What it was asked at, by whom and by which asker is the registry's from now on.
	d.store.Write(ctx, actor, "", func(tx *store.Tx) error {
		cur, err := tx.GetPeer(p.Name)
		if err != nil {
			return err
		}
		cur.Seen, cur.LastRun, cur.LastAgent, cur.Asked = p.Seen, actor.RunId, member.AgentId, timestamppb.New(tx.Now())
		// The conversation `--continue` goes back to is the last one that answered (recordPeerAnswers), not one this ask
		// has only started: it may time out or fail.
		return tx.PutPeer(cur)
	})
	return member, agent, ""
}

// dropStoredTask marks a task message read and delivered: it was stored for an agent that could not be brought
// back, and must not be what it does when it is.
func (d *Daemon) dropStoredTask(ctx context.Context, actor *v1.Actor, agentID, msgID string) {
	if msgID == "" {
		return
	}
	d.store.Write(ctx, actor, "", func(tx *store.Tx) error {
		in, err := tx.Reader.Inbox(agentID, true)
		if err != nil {
			return err
		}
		for _, m := range in {
			if m.Id == msgID {
				m.DeliveredAt, m.ReadAt = tsOf(tx.Now()), tsOf(tx.Now())
				return tx.PutMessage(m)
			}
		}
		return nil
	})
}

// launchPeerAgent starts an asker of a peer in this run: a new agent record of role asker named as the peer (then
// `<peer>-2`, …), in the peer's own worktree, on the kind, model and effort chosen. With a session it resumes that
// conversation (an earlier run's); without, it starts a fresh one. missing says the conversation was asked for and
// is gone.
func (d *Daemon) launchPeerAgent(ctx context.Context, actor *v1.Actor, p *v1.Peer, kind, model, effort, tier, session, task string) (*v1.Agent, bool, error) {
	if d.rt.term == nil {
		return nil, false, connect.NewError(connect.CodeUnavailable, errors.New("this daemon runs without a terminal host"))
	}
	ad, err := d.rt.adapters.Get(kind)
	if err != nil {
		return nil, false, err
	}
	r := d.store.Read(ctx)
	if err := d.reserveCheck(r, actor.RunId, kind, model, effort, "asking "+p.Name, false); err != nil {
		return nil, false, err
	}
	if err := d.capGuard(r, "peer "+p.Name, ""); err != nil {
		return nil, false, err
	}
	if err := d.checkMemoryGuard(r, "peer "+p.Name); err != nil {
		return nil, false, err
	}
	if err := d.ensureRunDir(actor.RunId); err != nil {
		return nil, false, err
	}
	name, err := uniqueAgentName(r, actor.RunId, p.Name)
	if err != nil {
		return nil, false, err
	}
	token := auth.NewToken()
	now := d.now()
	agent := &v1.Agent{Id: ids.New("a", 10), RunId: actor.RunId, Name: name, Role: v1.AgentRole_AGENT_ROLE_ASKER, ParentId: actor.Id,
		Kind: kind, Repo: p.Repo, Worktree: p.Worktree, BaseCommit: p.Seen, Model: model, Effort: effort, Tier: tier,
		State: v1.AgentState_AGENT_STATE_STARTING, LastActivity: timestamppb.New(now), CreatedAt: timestamppb.New(now),
		DenyEdits: true, Peer: true, SessionId: session}
	if p.About != "" {
		agent.About, agent.AboutSource = capAbout(p.About), aboutPeer
	}
	mu := d.agentLock(agent.Id)
	mu.Lock()
	defer mu.Unlock()
	plan, err := d.planLaunch(ctx, ad, agent, token, session)
	if err != nil {
		os.RemoveAll(d.paths.Agent(agent.Id))
		return nil, false, err
	}
	missing := session != "" && plan.ResumeMissing
	if session == "" {
		agent.SessionId = plan.SessionId
	} else if plan.ResumeMissing {
		// Its conversation is gone (Claude Code deletes old ones): it answers from its worktree only, and says so.
		task = "Your earlier conversation was not found where Claude Code keeps it, so you start with no memory of it: answer from " +
			"your worktree and what you can read there, and say in your answer that your earlier research is not in your context.\n\n" + task
	}
	if err := d.store.Write(ctx, actor, "", func(tx *store.Tx) error {
		if run, err := tx.GetRun(actor.RunId); err != nil {
			return err
		} else if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			return failedPrecondition("run %s is closed", actor.RunId)
		}
		// One process per conversation: a resumed one is not on another process, of this or any run. Askers of one
		// peer share its checkout, so that is no reason to refuse.
		if other := conversationRunning(tx.Reader, session, ""); other != nil {
			return failedPrecondition("its conversation is running in %s (run %s): one process per conversation", other.Id, other.RunId)
		}
		if same, err := tx.Reader.AgentsNamed(actor.RunId, name); err != nil {
			return err
		} else if len(same) > 0 {
			return failedPrecondition("this run already has an agent named %s", name)
		}
		if err := d.mayLaunch(tx.Reader, "peer "+p.Name, agent.Id); err != nil {
			return err
		}
		if err := tx.PutAgent(agent); err != nil {
			return err
		}
		if err := tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(agent.Role), SubjectID: agent.Id, Name: agent.Name, RunID: agent.RunId}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: agent.RunId, AgentId: agent.Id,
			Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: agent}}}); err != nil {
			return err
		}
		msg := &v1.Message{Id: ids.New("m", 10), RunId: agent.RunId, From: tx.Actor, ToId: agent.Id, ToName: agent.Name,
			Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: withOpenCodeBodyNote(kind, task), CreatedAt: timestamppb.New(tx.Now())}
		if err := tx.PutMessage(msg); err != nil {
			return err
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: agent.RunId, AgentId: agent.Id,
			Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: msg}}})
		return err
	}); err != nil {
		d.rt.guard.release(agent.Id)
		os.RemoveAll(d.paths.Agent(agent.Id)) // planLaunch wrote its MCP file, which may hold servers' secrets
		return nil, false, err
	}
	launched, err := d.launch(ctx, agent, token, plan)
	if err != nil {
		d.log.Error("launch failed", "agent", agent.Name, "err", err)
		d.failLaunch(ctx, actor, "", agent, err)
		d.rt.guard.release(agent.Id)
		return nil, false, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("launching %s: %w", agent.Name, err))
	}
	d.deliverSoon(agent.Id)
	return d.view(launched), missing, nil
}

// writePeerAnswers writes a finished peers ask's answers into its file (bin/mesh's peers-<stamp>.md).
func writePeerAnswers(a *v1.Ask) error {
	first := strings.TrimSpace(a.Body)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if r := []rune(first); len(r) > 120 {
		first = string(r[:120])
	}
	var names []string
	for _, m := range a.Members {
		names = append(names, m.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Peer answers: %s\n\nAsked %s in run %s, of %d peer(s): %s.\n", first, a.CreatedAt.AsTime().UTC().Format(time.RFC3339),
		a.RunId, len(a.Members), strings.Join(names, ", "))
	if c := strings.TrimSpace(a.Context); c != "" {
		b.WriteString("\n" + c + "\n")
	}
	fmt.Fprintf(&b, "\n## Question\n\n%s\n", strings.TrimSpace(a.Body))
	for _, m := range a.Members {
		fmt.Fprintf(&b, "\n## %s\n\n", m.Name)
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
			fmt.Fprintf(&b, "*%s at %s, upstream +%d since last asked*\n\n", m.PeerRepo, short(m.Head), m.UpstreamNew)
		}
		if m.About != "" {
			fmt.Fprintf(&b, "*%s*\n\n", m.About)
		}
		switch {
		case m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED:
			b.WriteString(strings.TrimSpace(m.Text) + "\n")
		case strings.TrimSpace(m.Text) != "":
			fmt.Fprintf(&b, "**%s:** %s\n", m.Type, strings.TrimSpace(m.Text))
		default:
			fmt.Fprintf(&b, "No answer: %s\n", m.Line)
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.AnswersPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.AnswersPath, []byte(b.String()), 0o600)
}

// peerSummary is the one message the caller gets when a peers ask finishes.
func peerSummary(a *v1.Ask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mesh answers %s] %d of %d peers answered\n", a.Id, answered(a), len(a.Members))
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			fmt.Fprintf(&b, "- %s: answered\n", m.Name)
		} else if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_LOST {
			fmt.Fprintf(&b, "- %s: %s\n", m.Name, lostLine(m))
		} else {
			fmt.Fprintf(&b, "- %s: no answer: %s\n", m.Name, m.Line)
		}
	}
	if a.AnswersPath != "" {
		fmt.Fprintf(&b, "answers: %s", a.AnswersPath)
	}
	if answered(a) < len(a.Members) {
		b.WriteString("\n(A peer that didn't answer can be asked again with `mesh ask --peers <name> --body …`: a fresh asker, or `--continue` to go on in its last conversation.)")
	}
	return b.String()
}

// recordPeerAnswers notes in the registry what a finished peers ask got from each peer that answered: the
// conversation of the asker that answered (an agy agent learns its id after it starts), for `--continue`, and the
// answers file.
func recordPeerAnswers(tx *store.Tx, a *v1.Ask) error {
	for _, m := range a.Members {
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			continue
		}
		p, err := tx.GetPeer(m.Name)
		if err != nil {
			continue // removed meanwhile
		}
		// Asks of one peer overlap: an older one that answers last must not roll the record back (the answers files are
		// named by their ask's stamp).
		if p.LastAnswers != "" && filepath.Base(a.AnswersPath) < filepath.Base(p.LastAnswers) {
			continue
		}
		if ag, err := tx.GetAgent(m.AgentId); err == nil && ag.SessionId != "" {
			p.Kind, p.SessionId = ag.Kind, ag.SessionId
		}
		p.LastAnswers = a.AnswersPath
		if err := tx.PutPeer(p); err != nil {
			return err
		}
	}
	return nil
}

type reparentKey struct{}

// withReparent marks a send to a peer's agent as the ask of a new caller: SendMessage makes the caller its parent
// in the write that stores the task.
func withReparent(ctx context.Context, callerID string) context.Context {
	return context.WithValue(ctx, reparentKey{}, callerID)
}

// reparentTo is the caller a send under ctx moves a peer's agent to ("" for any other send).
func reparentTo(ctx context.Context) string {
	id, _ := ctx.Value(reparentKey{}).(string)
	return id
}

// peerHeldBy says why a peer's agent can't be moved to another caller: it is answering its parent's ask, has a
// task or answer of its parent it hasn't read, or sent its parent a question or plan and waits for the answer.
// Empty when it is free.
func peerHeldBy(r store.Reader, mine *v1.Agent) (string, error) {
	parent, err := r.GetAgent(mine.ParentId)
	name := mine.ParentId
	if err != nil {
		parent = nil
	} else {
		name = parent.Name
	}
	if mine.State == v1.AgentState_AGENT_STATE_WORKING || mine.State == v1.AgentState_AGENT_STATE_STARTING || mine.PendingReopen != nil {
		return fmt.Sprintf("it is still answering an earlier question of %s", name), nil
	}
	if held, _, err := collecting(r, mine.Id, mine.ParentId); err != nil {
		return "", err
	} else if held != nil {
		return fmt.Sprintf("it is still answering an earlier question of %s", held.CallerName), nil
	}
	// Mail it has not read only holds a peer that can still read it: a stopped one (retired, or gone without
	// reading) would otherwise stay with its caller for good.
	var unread []*v1.Message
	if !stopped(mine) {
		var err error
		if unread, err = r.Inbox(mine.Id, false); err != nil {
			return "", err
		}
	}
	for _, m := range unread {
		if m.From.GetId() == mine.ParentId && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER) {
			return fmt.Sprintf("it has not yet read a message of %s", name), nil
		}
	}
	// A task or answer of its parent that it has read and not yet reported on (as reportOwed reads it): the
	// parent waits for that report, from a peer that can still give it.
	if parent != nil && hasProcess(parent) && !stopped(mine) {
		msgs, err := r.RunMessages(mine.RunId, "", -1) // -1: no limit
		if err != nil {
			return "", err
		}
		owes := false
		for _, m := range msgs {
			switch {
			case m.ToId == mine.Id && m.From.GetId() == mine.ParentId && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
				owes = true
			case m.From.GetId() == mine.Id && m.ToId == mine.ParentId && isReport(m.Type):
				owes = false
			}
		}
		if owes {
			return fmt.Sprintf("it still owes %s a report on its task", name), nil
		}
	}
	last, err := lastReport(r, mine)
	if err != nil {
		return "", err
	}
	// Its caller can answer only while both are alive: a stopped peer, or one whose caller is stopped, is let go
	// (retiring a peer is the way out of a question nobody answers).
	if parent != nil && hasProcess(parent) && !stopped(mine) && last != nil && (last.Type == v1.MessageType_MESSAGE_TYPE_QUESTION || last.Type == v1.MessageType_MESSAGE_TYPE_PLAN) {
		return fmt.Sprintf("it is waiting for the answer of %s to its %s", name, strings.ToLower(strings.TrimPrefix(last.Type.String(), "MESSAGE_TYPE_"))), nil
	}
	return "", nil
}
