package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// upstream is a project on the network: a repo the daemon clones with a file:// URL.
type upstream struct{ dir string }

func newUpstream(t *testing.T, name string) *upstream {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o755)
	u := &upstream{dir: dir}
	u.git(t, "init", "-q", "-b", "main")
	u.commit(t, "README", "hello")
	return u
}

func (u *upstream) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", u.dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (u *upstream) commit(t *testing.T, file, content string) string {
	t.Helper()
	os.WriteFile(filepath.Join(u.dir, file), []byte(content), 0o644)
	u.git(t, "add", file)
	u.git(t, "commit", "-q", "-m", "change "+file+" "+content)
	return u.git(t, "rev-parse", "HEAD")
}

func (u *upstream) url() string { return "file://" + u.dir }

// peerWorld is a daemon with a peers dir of its own and a root that adds and asks peers.
type peerWorld struct {
	*reviewWorld
	dir string
}

func startPeers(t *testing.T) *peerWorld {
	t.Helper()
	w := startReviews(t)
	dir := t.TempDir()
	w.set(t, "peers.dir", dir)
	return &peerWorld{reviewWorld: w, dir: dir}
}

func (w *peerWorld) add(t *testing.T, req *v1.AddPeerRequest) *v1.Peer {
	t.Helper()
	res, err := w.asRoot.AddPeer(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Peer
}

func (w *peerWorld) peer(t *testing.T, name string) *v1.Peer {
	t.Helper()
	res, err := w.asRoot.ListPeers(context.Background(), connect.NewRequest(&v1.ListPeersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Msg.Peers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no peer %s in %v", name, res.Msg.Peers)
	return nil
}

func TestAddingAPeerClonesItDetachesAWorktreeAndRefusesWhatGitMustNotRun(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "orchestrator-with-a-very-long-name")
	head := up.git(t, "rev-parse", "HEAD")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "orchestrators", About: "an orchestrator; ask about its queue"})
	if p.Name != "orchestrator-with-a-research" || p.Group != "orchestrators" || p.Seen != head || p.Kind != "claude" {
		t.Fatalf("peer %v", p)
	}
	if p.Repo != filepath.Join(w.dir, "orchestrators", "orchestrator-with-a-very-long-name") || p.Upstream.Branch != "main" || p.Upstream.Remote != "origin" {
		t.Errorf("clone %s upstream %v", p.Repo, p.Upstream)
	}
	if _, err := os.Stat(filepath.Join(p.Repo, "README")); err != nil {
		t.Errorf("the clone: %v", err)
	}
	// Its own worktree, detached on the commit it saw: not the clone's checkout.
	if p.Worktree == p.Repo || !strings.Contains(p.Worktree, "/peers/"+p.Name) {
		t.Errorf("worktree %s", p.Worktree)
	}
	if out, _ := exec.Command("git", "-C", p.Worktree, "symbolic-ref", "-q", "HEAD").Output(); len(out) != 0 {
		t.Errorf("the worktree is on a branch: %s", out)
	}
	if got := w.peer(t, p.Name); got.Group != "orchestrators" {
		t.Errorf("listed %v", got)
	}
	groups, _ := w.asRoot.ListPeers(ctx, connect.NewRequest(&v1.ListPeersRequest{}))
	if len(groups.Msg.Groups) != 1 || groups.Msg.Groups[0].Name != "orchestrators" {
		t.Errorf("groups %v", groups.Msg.Groups)
	}
	// The same project again: the name is taken; --name makes another.
	if _, err := w.asRoot.AddPeer(ctx, connect.NewRequest(&v1.AddPeerRequest{Meta: meta(""), Source: p.Repo, Group: "orchestrators"})); code(err) != connect.CodeAlreadyExists {
		t.Errorf("a second add: %v", err)
	}
	// A path is used as it is, the clone untouched.
	again := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: p.Repo, Group: "other", Name: "second-research"})
	if again.Repo != p.Repo || again.Group != "other" {
		t.Errorf("by path %v", again)
	}
	// Never an option, an ext:: URL (it runs a command), a long about, or a non-repository.
	for name, req := range map[string]*v1.AddPeerRequest{
		"an option":     {Source: "--upload-pack=touch /tmp/x"},
		"ext":           {Source: "ext::sh -c 'touch /tmp/pwned'"},
		"a long about":  {Source: up.url(), About: strings.Repeat("x", 401), Name: "x-research"},
		"no repository": {Source: t.TempDir()},
		"no source":     {},
	} {
		req.Meta = meta("")
		if _, err := w.asRoot.AddPeer(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Errorf("an ext:: URL ran a command")
	}
	// Edit a peer's about and a group's.
	if _, err := w.asRoot.EditPeer(ctx, connect.NewRequest(&v1.EditPeerRequest{Meta: meta(""), Target: p.Name, About: "now about the queue"})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.asRoot.EditPeer(ctx, connect.NewRequest(&v1.EditPeerRequest{Meta: meta(""), Target: "orchestrators", About: "peers of agent-mesh"})); err != nil {
		t.Fatal(err)
	}
	list, _ := w.asRoot.ListPeers(ctx, connect.NewRequest(&v1.ListPeersRequest{Group: "orchestrators"}))
	if len(list.Msg.Peers) != 1 || list.Msg.Peers[0].About != "now about the queue" || list.Msg.Groups[0].About != "peers of agent-mesh" {
		t.Errorf("after the edits %v", list.Msg)
	}
	// A worker may read the list but not add.
	asWorker := w.as(w.token(t, w.spawnUnder(t, w.asRoot, "wk", &v1.SpawnAgentRequest{}).Id))
	if _, err := asWorker.ListPeers(ctx, connect.NewRequest(&v1.ListPeersRequest{})); err != nil {
		t.Errorf("a worker's list: %v", err)
	}
	if _, err := asWorker.AddPeer(ctx, connect.NewRequest(&v1.AddPeerRequest{Meta: meta(""), Source: p.Repo})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker's add: %v", err)
	}
	// Removing it takes its worktree, not the clone.
	if _, err := w.asRoot.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: again.Name})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(again.Worktree); err == nil {
		t.Errorf("the removed peer's worktree stays")
	}
	if _, err := os.Stat(p.Repo); err != nil {
		t.Errorf("the clone went: %v", err)
	}
}

func TestImportingPeersReadsBinMeshsFileWithoutWritingItAndSkipsWhatItCant(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "paseo")
	clone := filepath.Join(t.TempDir(), "paseo")
	if out, err := exec.Command("git", "clone", "-q", up.dir, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	seen := up.git(t, "rev-parse", "HEAD")
	file := filepath.Join(t.TempDir(), "peers.json")
	data, _ := json.MarshalIndent(map[string]any{
		"peers": map[string]any{
			"paseo-research": map[string]any{"repo": clone, "group": "orchestrators", "kind": "claude", "about": "a daemon per host", "seen": seen, "cwd": "/old/worktree",
				"upstream": map[string]any{"remote": "origin", "branch": "main", "url": up.dir}, "run": "r1", "as": "paseo-research"},
			"gone-research":  map[string]any{"repo": "/nowhere/gone", "group": "orchestrators", "about": "x"},
			"codex-research": map[string]any{"repo": clone, "group": "g", "kind": "codex", "about": "y"},
		},
		"groups": map[string]any{"orchestrators": map[string]any{"about": "agent orchestration tools"}},
	}, "", "  ")
	os.WriteFile(file, data, 0o600)
	before, _ := os.ReadFile(file)

	res, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Msg.Imported, ","); got != "group orchestrators,paseo-research" {
		t.Errorf("imported %q", got)
	}
	if len(res.Msg.Skipped) != 2 || !strings.Contains(strings.Join(res.Msg.Skipped, "|"), "gone-research: its clone") || !strings.Contains(strings.Join(res.Msg.Skipped, "|"), "codex-research: its kind codex has no adapter") {
		t.Errorf("skipped %v", res.Msg.Skipped)
	}
	p := w.peer(t, "paseo-research")
	if p.Repo != clone || p.Seen != seen || p.SessionId != "" || p.About != "a daemon per host" || p.Upstream.Url != up.dir {
		t.Errorf("imported %v", p)
	}
	if p.Worktree == "/old/worktree" || !strings.Contains(p.Worktree, "/peers/paseo-research") {
		t.Errorf("an imported peer gets a worktree of the daemon's own: %s", p.Worktree)
	}
	groups, _ := w.asRoot.ListPeers(ctx, connect.NewRequest(&v1.ListPeersRequest{}))
	if len(groups.Msg.Groups) != 1 || groups.Msg.Groups[0].About != "agent orchestration tools" {
		t.Errorf("groups %v", groups.Msg.Groups)
	}
	if after, _ := os.ReadFile(file); !bytes.Equal(before, after) {
		t.Errorf("the import wrote peers.json")
	}
	// Again: nothing new.
	res, err = w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil || len(res.Msg.Imported) != 0 || !strings.Contains(strings.Join(res.Msg.Skipped, "|"), "paseo-research: already registered") {
		t.Errorf("second import %v, %v", res, err)
	}
	if _, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: filepath.Join(t.TempDir(), "missing.json")})); code(err) != connect.CodeNotFound {
		t.Errorf("a missing file: %v", err)
	}
}

func TestAskingPeersCollectsOneFileResumesTheirConversationAndKeepsTheirWorktrees(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	upA, upB := newUpstream(t, "alpha"), newUpstream(t, "beta")
	pa := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: upA.url(), Group: "g", About: "alpha is for queues"})
	pb := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: upB.url(), Group: "g", About: "beta is for stores"})
	w.asRoot.EditPeer(ctx, connect.NewRequest(&v1.EditPeerRequest{Meta: meta(""), Target: "g", About: "peers of the demo"}))
	headA := upA.git(t, "rev-parse", "HEAD")

	// Upstream moves for alpha before the first ask: the note says so, and its checkout follows.
	newA := upA.commit(t, "queue.go", "package queue")
	res := w.ask(t, &v1.AskRequest{Peers: "g", Tier: "mid", Body: "How do you order a queue?\nSecond line.", Timeout: 60})
	if res.Ask.Kind != v1.AskKind_ASK_KIND_PEERS || !res.Ask.Collected || len(res.Agents) != 2 || len(res.Ask.Members) != 2 {
		t.Fatalf("response %v", res)
	}
	agents := map[string]*v1.Agent{}
	for _, a := range res.Agents {
		agents[a.Name] = a
		if !a.Peer || a.Role != v1.AgentRole_AGENT_ROLE_ASKER || !a.DenyEdits || a.ParentId != w.lead.Id {
			t.Errorf("peer agent %v", a)
		}
	}
	a1 := agents[pa.Name]
	if a1 == nil || a1.Worktree != pa.Worktree || a1.Repo != pa.Repo {
		t.Fatalf("alpha's agent %v, want worktree %s", a1, pa.Worktree)
	}
	task := w.task(t, a1.Id)
	for _, want := range []string{"Question (read-only)", "Upstream moved since you last looked: 1 commit(s) on origin/main, " + headA[:9] + ".." + newA[:9],
		"git log --stat " + headA[:9] + ".." + newA[:9], "change queue.go package queue", "Your checkout is at " + newA[:9] + ".", "How do you order a queue?"} {
		if !strings.Contains(task, want) {
			t.Errorf("alpha's task lacks %q:\n%s", want, task)
		}
	}
	if !strings.Contains(w.task(t, agents[pb.Name].Id), "Upstream (origin/main) hasn't changed since you last looked.") {
		t.Errorf("beta's task: %s", w.task(t, agents[pb.Name].Id))
	}
	if out, _ := exec.Command("git", "-C", pa.Worktree, "rev-parse", "HEAD").Output(); strings.TrimSpace(string(out)) != newA {
		t.Errorf("alpha's checkout stayed at %s", out)
	}
	// The registry noted who asked, and the conversation.
	got := w.peer(t, pa.Name)
	if got.SessionId == "" || got.SessionId != a1.SessionId || got.LastRun != w.run.Id || got.LastAgent != a1.Id || got.Seen != newA || got.Asked == nil {
		t.Errorf("registry after the ask %v", got)
	}
	if got.RunningRun != w.run.Id || got.RunningAgent != a1.Id {
		t.Errorf("running %s/%s", got.RunningRun, got.RunningAgent)
	}

	// Both answer: one message, one file.
	w.quiet(t, a1.Id)
	w.say(t, a1.Id, v1.MessageType_MESSAGE_TYPE_DONE, "Alpha orders by priority, then age.")
	if len(w.answers(t)) != 0 {
		t.Fatalf("rang with a peer pending")
	}
	w.quiet(t, agents[pb.Name].Id)
	w.say(t, agents[pb.Name].Id, v1.MessageType_MESSAGE_TYPE_DONE, "Beta has no queue.")
	waitFor(t, "the peers' message", func() bool { return len(w.answers(t)) == 1 })
	body := w.answers(t)[0].Body
	for _, want := range []string{"2 of 2 peers answered", "alpha-research: answered", "answers: " + res.Ask.AnswersPath} {
		if !strings.Contains(body, want) {
			t.Errorf("message lacks %q: %s", want, body)
		}
	}
	if !strings.Contains(filepath.Base(res.Ask.AnswersPath), "peers-") {
		t.Errorf("path %s", res.Ask.AnswersPath)
	}
	file, _ := os.ReadFile(res.Ask.AnswersPath)
	for _, want := range []string{"# Peer answers: How do you order a queue?\n", "in run " + w.run.Id + ", of 2 peer(s): alpha-research, beta-research.", "peers of the demo",
		"## Question\n\nHow do you order a queue?\nSecond line.", "## alpha-research\n\n*alpha at " + newA[:9] + ", upstream +1 since last asked*\n\n*alpha is for queues*\n\nAlpha orders by priority, then age.",
		"## beta-research\n\n*beta at " + upB.git(t, "rev-parse", "HEAD")[:9] + ", upstream +0 since last asked*\n\n*beta is for stores*\n\nBeta has no queue."} {
		if !strings.Contains(string(file), want) {
			t.Errorf("file lacks %q:\n%s", want, file)
		}
	}
	if got := w.peer(t, pa.Name); got.LastAnswers != res.Ask.AnswersPath {
		t.Errorf("last answers %q", got.LastAnswers)
	}

	// A plain task never makes a peer a worker.
	sendTask(t, w.asRoot, a1.Id, "Direct task: change the queue.")
	if got := w.agent(a1.Id); got.Role != v1.AgentRole_AGENT_ROLE_ASKER || !got.DenyEdits {
		t.Errorf("a peer given a change %v", got)
	}

	// Stopping the run retires its agents and keeps the peers' worktrees.
	stop, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: w.run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range stop.Msg.Worktrees {
		if (o.AgentId == a1.Id || o.AgentId == agents[pb.Name].Id) && (o.WorktreeRemoved || !strings.Contains(o.Reason, "peer")) {
			t.Errorf("outcome %v", o)
		}
	}
	for _, p := range []*v1.Peer{pa, pb} {
		if _, err := os.Stat(p.Worktree); err != nil {
			t.Errorf("%s's worktree: %v", p.Name, err)
		}
	}

	// A new run asks alpha again: a new agent record resumes the same conversation, in the same worktree.
	sid := got.SessionId
	run2 := startRun(t, w.device.Mesh, "")
	_, asRoot2 := w.root(t, run2.Id)
	res2, err := asRoot2.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: pa.Name, Tier: "mid", Body: "What changed?"}))
	if err != nil {
		t.Fatal(err)
	}
	b := res2.Msg.Agents[0]
	if b.Id == a1.Id || b.Name != pa.Name || b.SessionId != sid || b.Worktree != pa.Worktree || b.RunId != run2.Id || !b.Peer {
		t.Fatalf("run 2's agent %v", b)
	}
	if res2.Msg.Ask.Members[0].How != "resumed its conversation" {
		t.Errorf("how %q", res2.Msg.Ask.Members[0].How)
	}
	all := w.launches(t, sid)
	if f, deny := flags(all[len(all)-1]); f["--resume"] != sid || f["--session-id"] != "" || !deny {
		t.Errorf("run 2's launch %v", all)
	}
	// And while it runs there, a third run can't have a second process on its conversation.
	run3 := startRun(t, w.device.Mesh, "")
	_, asRoot3 := w.root(t, run3.Id)
	_, err = asRoot3.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: pa.Name, Tier: "mid", Body: "And now?"}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "one process per conversation") {
		t.Errorf("a peer running in another run: %v", err)
	}
	// Beta, not running anywhere, can be asked while alpha can't.
	res3, err := asRoot3.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: "g", Tier: "mid", Body: "Both?"}))
	if err != nil {
		t.Fatal(err)
	}
	var notAsked, asked int
	for _, m := range res3.Msg.Ask.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
			notAsked++
			if m.Name != pa.Name || !strings.Contains(m.Line, "running in run "+run2.Id) {
				t.Errorf("not asked %v", m)
			}
		} else {
			asked++
		}
	}
	if notAsked != 1 || asked != 1 {
		t.Errorf("members %v", res3.Msg.Ask.Members)
	}
}

func TestAskingPeersNeedsATierABodyAndAKnownPeer(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	for name, req := range map[string]*v1.AskRequest{
		"no tier":    {Peers: "g", Body: "q"},
		"no body":    {Peers: "g", Tier: "mid"},
		"a repo too": {Peers: "g", Tier: "mid", Body: "q", Repo: "."},
	} {
		if _, err := w.asRoot.Ask(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: "nobody", Tier: "mid", Body: "q"})); code(err) != connect.CodeNotFound {
		t.Errorf("an unknown peer: %v", err)
	}
	// Not refreshed: the note says so, whatever upstream did.
	up.commit(t, "x", "y")
	res := w.ask(t, &v1.AskRequest{Peers: "all", Tier: "mid", Body: "q", NoRefresh: true})
	if task := w.task(t, res.Agents[0].Id); !strings.Contains(task, "was not refreshed (no_refresh)") || strings.Contains(task, "Upstream moved") {
		t.Errorf("task %s", task)
	}
}

func TestAPeerCheckoutThatCantMoveIsToldSoOnEveryAskUntilItDoes(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	// The peer left a change in its checkout: it is not moved, and upstream's commits stay news.
	if err := os.WriteFile(filepath.Join(p.Worktree, "README"), []byte("scribbles"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := up.git(t, "rev-parse", "HEAD")
	next := up.commit(t, "a.go", "package a")
	for i := 0; i < 2; i++ {
		res := w.ask(t, &v1.AskRequest{Peers: p.Name, Tier: "mid", Body: fmt.Sprintf("question %d", i), NoRefresh: false})
		task := w.task(t, res.Agents[0].Id)
		for _, want := range []string{"Upstream moved since you last looked: 1 commit(s) on origin/main, " + head[:9] + ".." + next[:9], "has local changes, so it was not moved", "Your checkout is at " + head[:9] + "."} {
			if !strings.Contains(task, want) {
				t.Errorf("ask %d lacks %q:\n%s", i, want, task)
			}
		}
		// Done with this one so the next ask goes to the same agent.
		w.quiet(t, res.Agents[0].Id)
		w.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "answer")
		waitFor(t, "the message", func() bool { return len(w.answers(t)) == i+1 })
		w.quiet(t, res.Agents[0].Id)
	}
	if got := w.peer(t, p.Name); got.Seen != head {
		t.Errorf("seen %s moved to a head the checkout never reached", got.Seen)
	}
	// Cleaned, it follows.
	exec.Command("git", "-C", p.Worktree, "checkout", "--", "README").Run()
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Tier: "mid", Body: "again"})
	if task := w.task(t, res.Agents[0].Id); !strings.Contains(task, "Your checkout is at "+next[:9]+".") || strings.Contains(task, "not moved") {
		t.Errorf("task %s", task)
	}
	// A group exists once it has a peer; a typo makes none.
	if _, err := w.asRoot.EditPeer(ctx, connect.NewRequest(&v1.EditPeerRequest{Meta: meta(""), Target: "alpha-reserch", About: "x"})); code(err) != connect.CodeNotFound {
		t.Errorf("an edit of a typo: %v", err)
	}
	// A peers ask takes no other target.
	if _, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: "all", Agent: "x", Tier: "mid", Body: "q"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("peers and an agent: %v", err)
	}
	// A clone whose folder can't make a name needs --name.
	odd := filepath.Join(t.TempDir(), "my repo")
	if out, err := exec.Command("git", "clone", "-q", up.dir, odd).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := w.asRoot.AddPeer(ctx, connect.NewRequest(&v1.AddPeerRequest{Meta: meta(""), Source: odd})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a name with a space: %v", err)
	}
	if p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: odd, Name: "my-repo-research"}); p.Name != "my-repo-research" {
		t.Errorf("named %v", p)
	}
}

// binMeshState lays out what bin/mesh keeps next to peers.json: a run's run.json (live, or under archive/) with the
// agent that held a peer's conversation, and the answers file of its last `ask --peers`.
type binMeshState struct {
	dir string
}

func newBinMeshState(t *testing.T) *binMeshState { return &binMeshState{dir: t.TempDir()} }

func (s *binMeshState) run(t *testing.T, id string, archived bool, agents map[string]map[string]any) {
	t.Helper()
	dir := filepath.Join(s.dir, id)
	if archived {
		dir = filepath.Join(s.dir, "archive", id)
	}
	os.MkdirAll(filepath.Join(dir, "reports"), 0o755)
	b, _ := json.Marshal(map[string]any{"id": id, "agents": agents})
	os.WriteFile(filepath.Join(dir, "run.json"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "reports", "peers-1.md"), []byte("answers"), 0o644)
}

func (s *binMeshState) peersJSON(t *testing.T, peers map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"peers": peers, "groups": map[string]any{}})
	file := filepath.Join(s.dir, "peers.json")
	os.WriteFile(file, b, 0o600)
	return file
}

// worktree makes a detached worktree of a clone, at a path bin/mesh chose.
func worktreeOf(t *testing.T, clone, path string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", clone, "worktree", "add", "-q", "--detach", path).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func cloneOf(t *testing.T, up *upstream) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), filepath.Base(up.dir))
	if out, err := exec.Command("git", "clone", "-q", up.dir, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return clone
}

func TestImportingPeersCarriesTheirConversationsAndWorktreePathsAcrossAndAskResumesThem(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	st := newBinMeshState(t)
	upA, upB, upC := newUpstream(t, "alpha"), newUpstream(t, "beta"), newUpstream(t, "gamma")
	cloneA, cloneB, cloneC := cloneOf(t, upA), cloneOf(t, upB), cloneOf(t, upC)
	wtA := filepath.Join(t.TempDir(), "worktrees", "alpha-research") // bin/mesh's: absent, the import makes it again
	wtB := filepath.Join(t.TempDir(), "beta-research")
	worktreeOf(t, cloneB, wtB)
	// alpha's conversation is in an archived run, beta's in a live one that has retired it; gamma's run.json has
	// no agent of its name under the peers.json run, so it is found by scanning the other runs.
	st.run(t, "old1", true, map[string]map[string]any{"alpha-research": {"kind": "claude", "repo": cloneA, "session": "sess-alpha", "status": "retired"}})
	st.run(t, "live2", false, map[string]map[string]any{"beta-research": {"kind": "claude", "repo": cloneB, "session": "sess-beta", "status": "retired"}})
	st.run(t, "other3", true, map[string]map[string]any{"renamed": {"kind": "claude", "repo": cloneC, "session": "sess-gamma", "status": "retired", "peer": "gamma-research"}})
	st.run(t, "empty4", true, map[string]map[string]any{})
	file := st.peersJSON(t, map[string]any{
		"alpha-research": map[string]any{"repo": cloneA, "group": "g", "kind": "claude", "about": "a", "cwd": wtA, "run": "old1", "as": "alpha-research",
			"asked": "2026-11-20T10:00:00+00:00", "answers": map[string]any{"run": "old1", "file": "reports/peers-1.md"}, "upstream": map[string]any{"remote": "origin", "branch": "main"}},
		"beta-research":  map[string]any{"repo": cloneB, "group": "g", "kind": "claude", "about": "b", "cwd": wtB, "run": "live2", "as": "beta-research", "upstream": map[string]any{"remote": "origin", "branch": "main"}},
		"gamma-research": map[string]any{"repo": cloneC, "group": "g", "kind": "claude", "about": "c", "cwd": filepath.Join(t.TempDir(), "gamma-research"), "run": "empty4", "as": "gamma-research", "upstream": map[string]any{"remote": "origin", "branch": "main"}},
	})
	before, _ := os.ReadFile(file)

	res, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.Skipped) != 0 {
		t.Fatalf("skipped %v", res.Msg.Skipped)
	}
	a := w.peer(t, "alpha-research")
	if a.SessionId != "sess-alpha" || a.Worktree != wtA || a.InClone || a.LastRun != "old1" || a.LastAgent != "alpha-research" ||
		a.Asked.AsTime().Format("2006-01-02") != "2026-11-20" || a.LastAnswers != filepath.Join(st.dir, "archive", "old1", "reports", "peers-1.md") {
		t.Errorf("alpha %v", a)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Errorf("alpha's worktree is not back at its old path: %v", err)
	}
	if b := w.peer(t, "beta-research"); b.SessionId != "sess-beta" || b.Worktree != wtB {
		t.Errorf("beta %v", b)
	}
	if g := w.peer(t, "gamma-research"); g.SessionId != "sess-gamma" || g.LastAgent != "renamed" {
		t.Errorf("gamma (found by scanning) %v", g)
	}
	if after, _ := os.ReadFile(file); !bytes.Equal(before, after) {
		t.Errorf("the import wrote peers.json")
	}

	// Asked, it resumes the conversation in that worktree instead of starting one.
	run := w.run
	ar, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: "alpha-research", Tier: "mid", Body: "What changed?", NoRefresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	ag := ar.Msg.Agents[0]
	if ag.RunId != run.Id || ag.SessionId != "sess-alpha" || ag.Worktree != wtA || ar.Msg.Ask.Members[0].How != "resumed its conversation" {
		t.Fatalf("alpha's agent %v, how %q", ag, ar.Msg.Ask.Members[0].How)
	}
	all := w.launches(t, "sess-alpha")
	if f, deny := flags(all[len(all)-1]); f["--resume"] != "sess-alpha" || f["--session-id"] != "" || !deny {
		t.Errorf("alpha's launch %v", all)
	}
}

func TestImportingPeersKeepsAConversationRunningInBinMeshOutAndTheUnbriefedOptOut(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	st := newBinMeshState(t)
	up, up2 := newUpstream(t, "delta"), newUpstream(t, "eps")
	clone, clone2 := cloneOf(t, up), cloneOf(t, up2)
	wt := filepath.Join(t.TempDir(), "delta-research")
	worktreeOf(t, clone, wt)
	st.run(t, "live1", false, map[string]map[string]any{"delta-research": {"kind": "claude", "repo": clone, "session": "sess-delta", "status": "running"}})
	st.run(t, "arch2", true, map[string]map[string]any{"eps-research": {"kind": "claude", "repo": clone2, "session": "sess-eps", "status": "retired"}})
	file := st.peersJSON(t, map[string]any{
		"delta-research": map[string]any{"repo": clone, "group": "g", "about": "d", "cwd": wt, "run": "live1", "as": "delta-research"},
		"eps-research":   map[string]any{"repo": clone2, "group": "g", "about": "e", "cwd": filepath.Join(t.TempDir(), "eps-research"), "run": "arch2", "as": "eps-research"},
	})
	res, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Msg.Skipped, "|"), "delta-research: its conversation is still running in bin/mesh") {
		t.Errorf("skipped %v", res.Msg.Skipped)
	}
	d := w.peer(t, "delta-research")
	if d.SessionId != "" || d.Worktree == wt || !strings.Contains(d.Worktree, "/peers/delta-research") {
		t.Errorf("a conversation bin/mesh may still hold came across: %v", d)
	}
	if e := w.peer(t, "eps-research"); e.SessionId != "sess-eps" {
		t.Errorf("eps %v", e)
	}

	// --unbriefed: nothing carried, whatever was there.
	w2 := startPeers(t)
	res, err = w2.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file, Unbriefed: true}))
	if err != nil || len(res.Msg.Imported) < 2 {
		t.Fatalf("%v %v", res, err)
	}
	if e := w2.peer(t, "eps-research"); e.SessionId != "" || e.Asked != nil || e.LastRun != "" || !strings.Contains(e.Worktree, "/peers/eps-research") {
		t.Errorf("--unbriefed carried a conversation: %v", e)
	}
}

func TestImportingPeersUpgradesAnUnaskedPeerAndLeavesAskedOnesAlone(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	st := newBinMeshState(t)
	up := newUpstream(t, "zeta")
	clone := cloneOf(t, up)
	wt := filepath.Join(t.TempDir(), "zeta-research")
	// Imported before its conversation could be found (no run.json yet): an unbriefed peer.
	file := st.peersJSON(t, map[string]any{"zeta-research": map[string]any{"repo": clone, "group": "g", "about": "z", "cwd": wt, "run": "r9", "as": "zeta-research"}})
	if _, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file})); err != nil {
		t.Fatal(err)
	}
	own := w.peer(t, "zeta-research")
	if own.SessionId != "" {
		t.Fatalf("%v", own)
	}
	// Now the run is there: the import again upgrades it, moving it to the old path and dropping its own worktree.
	st.run(t, "r9", true, map[string]map[string]any{"zeta-research": {"kind": "claude", "repo": clone, "session": "sess-zeta", "status": "retired"}})
	res, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil || len(res.Msg.Imported) != 1 || !strings.HasPrefix(res.Msg.Imported[0], "zeta-research (conversation ") {
		t.Fatalf("upgrade: %v, %v", res, err)
	}
	up2 := w.peer(t, "zeta-research")
	if up2.SessionId != "sess-zeta" || up2.Worktree != wt {
		t.Fatalf("upgraded %v", up2)
	}
	if _, err := os.Stat(own.Worktree); err == nil {
		t.Errorf("the daemon's own worktree stays after the upgrade")
	}
	// An asked peer is not touched by another import.
	res, err = w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil || len(res.Msg.Imported) != 0 || !strings.Contains(strings.Join(res.Msg.Skipped, "|"), "already registered") {
		t.Errorf("third import %v, %v", res, err)
	}
}

func TestAPeerInTheClonesOwnCheckoutIsNeverMovedOrRemoved(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	st := newBinMeshState(t)
	up := newUpstream(t, "inclone")
	clone := cloneOf(t, up)
	seen := up.git(t, "rev-parse", "HEAD")
	st.run(t, "r1", true, map[string]map[string]any{"inclone-research": {"kind": "claude", "repo": clone, "session": "sess-in", "status": "retired"}})
	file := st.peersJSON(t, map[string]any{"inclone-research": map[string]any{"repo": clone, "group": "g", "about": "i", "cwd": clone, "in_clone": true,
		"seen": seen, "run": "r1", "as": "inclone-research", "upstream": map[string]any{"remote": "origin", "branch": "main", "url": up.dir}}})
	if _, err := w.asRoot.ImportPeers(ctx, connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file})); err != nil {
		t.Fatal(err)
	}
	p := w.peer(t, "inclone-research")
	if !p.InClone || p.Worktree != clone || p.SessionId != "sess-in" {
		t.Fatalf("imported %v", p)
	}
	// Upstream moves: the ask tells the peer, but the clone's checkout stays where it is.
	moved := up.commit(t, "new.go", "package x")
	before, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	res, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Peers: "inclone-research", Tier: "mid", Body: "What is new?"}))
	if err != nil {
		t.Fatal(err)
	}
	task := w.task(t, res.Msg.Agents[0].Id)
	if !strings.Contains(task, "Upstream moved since you last looked") || !strings.Contains(task, "clone's own checkout, which stays at "+seen[:9]) ||
		!strings.Contains(task, "git show "+moved[:9]) {
		t.Errorf("the note %s", task)
	}
	after, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
	if !bytes.Equal(before, after) {
		t.Errorf("the clone's checkout moved: %s -> %s", before, after)
	}
	if got := w.peer(t, "inclone-research"); got.Seen != moved {
		t.Errorf("seen %s, want %s", got.Seen, moved)
	}
	// Removing the peer leaves the clone and its checkout.
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: w.run.Id})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the peer's agent stopped", func() bool {
		a := w.agent(res.Msg.Agents[0].Id)
		return a.State == v1.AgentState_AGENT_STATE_RETIRED || a.State == v1.AgentState_AGENT_STATE_EXITED
	})
	if _, err := w.device.Mesh.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: "inclone-research"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Errorf("removing the peer took the clone's checkout: %v", err)
	}
}
