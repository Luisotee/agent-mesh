package daemon_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// testDirectURL is an address a pairing link may name: the tests only read the link, no device connects to it.
const testDirectURL = "https://127.0.0.1:9"

func (w *reopenWorld) changes(t *testing.T, agent string) *v1.GetAgentChangesResponse {
	t.Helper()
	res, err := w.device.Mesh.GetAgentChanges(context.Background(), connect.NewRequest(&v1.GetAgentChangesRequest{Agent: agent}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

// What an agent's worktree holds that no remote has: the files changed since its branch started (uncommitted ones too), how
// many are uncommitted, and the commits no remote branch has.
func TestAgentChangesListsFilesUncommittedWorkAndUnpushedCommits(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	if ch := w.changes(t, a.Id); !ch.Available || len(ch.Files) != 0 || ch.Uncommitted != 0 || ch.Commits != 0 || ch.Unpushed != 0 {
		t.Fatalf("a fresh worktree: %v", ch)
	}
	writeFile(t, filepath.Join(a.Worktree, "a.txt"), "one\ntwo\n", 0o644)
	gitIn(t, a.Worktree, "add", "a.txt")
	gitIn(t, a.Worktree, "commit", "-q", "-m", "add a")
	writeFile(t, filepath.Join(a.Worktree, "a.txt"), "one\ntwo\nthree\n", 0o644)
	writeFile(t, filepath.Join(a.Worktree, "new.txt"), "wip\n", 0o644)
	ch := w.changes(t, a.Id)
	if ch.Commits != 1 || ch.Unpushed != 1 || ch.Uncommitted != 2 || len(ch.Files) != 2 {
		t.Fatalf("changes = %v", ch)
	}
	byPath := map[string]*v1.ChangedFile{}
	for _, f := range ch.Files {
		byPath[f.Path] = f
	}
	if f := byPath["a.txt"]; f == nil || f.Status != "added" || f.Additions != 3 || !f.Uncommitted {
		t.Errorf("a.txt = %v", f)
	}
	if f := byPath["new.txt"]; f == nil || f.Status != "untracked" || !f.Uncommitted {
		t.Errorf("new.txt = %v", f)
	}
	// Pushed: no remote lacks the commit any more.
	gitIn(t, a.Worktree, "add", "-A")
	gitIn(t, a.Worktree, "commit", "-q", "-m", "more")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, a.Repo, "init", "-q", "--bare", remote)
	gitIn(t, a.Repo, "remote", "add", "origin", remote)
	gitIn(t, a.Worktree, "push", "-q", "origin", a.Branch)
	if ch := w.changes(t, a.Id); ch.Unpushed != 0 || ch.Uncommitted != 0 || ch.Commits != 2 {
		t.Errorf("after the push: %v", ch)
	}
}

// A tracked file a commit added and the working tree lost is uncommitted and deleted, not untracked; and when git can't count the
// commits the answer is unavailable, not "nothing".
func TestAgentChangesNamesADeletedTrackedFileAndAnUnreadableBranch(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	writeFile(t, filepath.Join(a.Worktree, "x.txt"), "x\n", 0o644)
	gitIn(t, a.Worktree, "add", "x.txt")
	gitIn(t, a.Worktree, "commit", "-q", "-m", "add x")
	if err := os.Remove(filepath.Join(a.Worktree, "x.txt")); err != nil { // the working tree is as the base had it again, uncommitted
		t.Fatal(err)
	}
	ch := w.changes(t, a.Id)
	var f *v1.ChangedFile
	for _, x := range ch.Files {
		if x.Path == "x.txt" {
			f = x
		}
	}
	if f == nil || f.Status != "deleted" || !f.Uncommitted || ch.Uncommitted != 1 {
		t.Errorf("x.txt = %v (changes %v)", f, ch)
	}
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		cur.BaseCommit = "0123456789012345678901234567890123456789"
		return tx.PutAgent(cur)
	}); err != nil {
		t.Fatal(err)
	}
	if ch := w.changes(t, a.Id); ch.Available || ch.Reason == "" {
		t.Errorf("a base git can't find: %v", ch)
	}
	if _, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: a.Id, Force: true})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "guess") {
		t.Errorf("archiving what can't be read: %v", err)
	}
}

// Archive removes the worktree and keeps the branch, never discards uncommitted work, asks to be forced past unpushed
// commits, and a message brings the agent back with its worktree made again from the branch.
func TestArchiveRemovesTheWorktreeKeepsTheBranchAndResumeBringsItBack(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	archive := func(force bool) (*v1.ArchiveAgentResponse, error) {
		res, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: a.Id, Force: force}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if _, err := asLead.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: a.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root archived: %v", err)
	}
	writeFile(t, filepath.Join(a.Worktree, "wip.txt"), "half", 0o644)
	if _, err := archive(true); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("uncommitted work, forced: %v", err)
	}
	if got := w.agent(a.Id); got.Archived || got.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("a refused archive changed the agent: %v", got)
	}
	gitIn(t, a.Worktree, "add", "wip.txt")
	gitIn(t, a.Worktree, "commit", "-q", "-m", "work")
	if _, err := archive(false); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no remote branch has") {
		t.Fatalf("unpushed commit: %v", err)
	}
	res, err := archive(true)
	if err != nil || !res.Agent.Archived || res.Unpushed != 1 {
		t.Fatalf("forced archive = %v, %v", res, err)
	}
	got := w.agent(a.Id)
	if !got.Archived || got.State != v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != "" {
		t.Errorf("agent after archive: %v", got)
	}
	if _, err := os.Stat(a.Worktree); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if !strings.Contains(gitIn(t, a.Repo, "branch", "--list", a.Branch), a.Branch) {
		t.Errorf("the branch is gone")
	}
	// Its changes are still readable, from the branch.
	if ch := w.changes(t, a.Id); !ch.Available || ch.Commits != 1 || len(ch.Files) != 1 || ch.Reason == "" {
		t.Errorf("changes of an archived agent: %v", ch)
	}
	if _, err := archive(true); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("archiving twice: %v", err)
	}
	evs, _ := w.d.Store().Read(ctx).ListEvents(run.Id)
	var archived *v1.AgentArchived
	for _, ev := range evs {
		if p := ev.GetAgentArchived(); p != nil {
			archived = p
		}
	}
	if archived == nil || archived.Unpushed != 1 || !archived.Forced || archived.Branch != a.Branch {
		t.Errorf("archive event = %v", archived)
	}

	// Restore: the same call that resumes any retired agent, and the worktree is back with its commit.
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent back", func() bool {
		cur := w.agent(a.Id)
		return !cur.Archived && cur.State == v1.AgentState_AGENT_STATE_IDLE
	})
	if b, err := os.ReadFile(filepath.Join(a.Worktree, "wip.txt")); err != nil || string(b) != "half" {
		t.Errorf("the worktree after the restore: %q, %v", b, err)
	}
}

// Only a worker's worktree is archived (a root's, asker's or reviewer's is not made again when it is resumed).
func TestArchiveIsForWorkersOnly(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	for name, role := range map[string]v1.AgentRole{"root": v1.AgentRole_AGENT_ROLE_ROOT, "asker": v1.AgentRole_AGENT_ROLE_ASKER, "reviewer": v1.AgentRole_AGENT_ROLE_REVIEWER} {
		if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil {
				return err
			}
			cur.Role = role
			return tx.PutAgent(cur)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: a.Id, Force: true})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "not a worker") {
			t.Errorf("archiving a %s: %v", name, err)
		}
	}
	if _, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: lead.Id, Force: true})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("archiving the lead: %v", err)
	}
	if got := w.agent(a.Id); got.Archived || got.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("a refused archive changed the agent: %v", got)
	}
}

// A worktree another agent shares (a follow-up run resumed the agent in it) stays while that agent is alive or its run is open.
func TestArchiveKeepsAWorktreeAnotherAgentShares(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	b := w.spawnUnder(t, asLead, "other", &v1.SpawnAgentRequest{})
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(b.Id)
		if err != nil {
			return err
		}
		cur.Worktree = a.Worktree
		return tx.PutAgent(cur)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: a.Id, Force: true})); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "shares its worktree") {
		t.Errorf("archiving a shared worktree: %v", err)
	}
	if _, err := os.Stat(a.Worktree); err != nil {
		t.Errorf("the shared worktree was touched: %v", err)
	}
	if got := w.agent(a.Id); got.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("a refused archive retired the agent")
	}
}

// A retry of a call that finished gets its first answer; one that failed half way is not recorded, and its retry carries on;
// two calls at once archive once.
func TestArchiveRetriesAndRacesRecordOneArchive(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	call := func(mid string) (*v1.ArchiveAgentResponse, error) {
		res, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(mid), Agent: a.Id}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	writeFile(t, filepath.Join(a.Worktree, "wip.txt"), "half", 0o644)
	if _, err := call("again"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("uncommitted work: %v", err)
	}
	if err := os.Remove(filepath.Join(a.Worktree, "wip.txt")); err != nil {
		t.Fatal(err)
	}
	// The same id after the failure runs again (the failure was not recorded as an answer).
	first, err := call("again")
	if err != nil || !first.Agent.Archived {
		t.Fatalf("the retry of a failed call: %v, %v", first, err)
	}
	replay, err := call("again")
	if err != nil || !replay.Agent.Archived {
		t.Errorf("the retry of a finished call: %v, %v", replay, err)
	}
	var n int
	evs, _ := w.d.Store().Read(ctx).ListEvents(run.Id)
	for _, ev := range evs {
		if ev.GetAgentArchived() != nil {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d archive events", n)
	}

	b := w.spawnUnder(t, asLead, "racer", &v1.SpawnAgentRequest{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			_, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(fmt.Sprintf("race-%d", i)), Agent: b.Id}))
			results <- err
		}(i)
	}
	ok := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			ok++
		} else if code(err) != connect.CodeFailedPrecondition {
			t.Errorf("the other call: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of two concurrent archives succeeded", ok)
	}
	n = 0
	evs, _ = w.d.Store().Read(ctx).ListEvents(run.Id)
	for _, ev := range evs {
		if p := ev.GetAgentArchived(); p != nil && p.Branch == b.Branch {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d archive events for the raced agent", n)
	}
}

func TestArchiveRefusesWhatIsNotTheDaemonsWorktree(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, _ := w.root(t, run.Id)
	// The lead works in a repo of its own, outside the daemon's worktrees folder only when it was adopted; here it is inside, so
	// move its record to the user's checkout to see the refusal.
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(lead.Id)
		if err != nil {
			return err
		}
		a.Worktree = a.Repo
		return tx.PutAgent(a)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.ArchiveAgent(ctx, connect.NewRequest(&v1.ArchiveAgentRequest{Meta: meta(""), Agent: lead.Id, Force: true})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("archiving the user's own checkout: %v", err)
	}
}

// OpenInEditor runs the host's editor with the path, with no shell, for the host's own device only, and only on a path inside the
// agent's worktree (symlinks resolved).
func TestOpenInEditorRunsTheEditorOnAPathInsideTheWorktree(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "work", &v1.SpawnAgentRequest{})
	record := filepath.Join(t.TempDir(), "args")
	editor := filepath.Join(t.TempDir(), "myeditor")
	writeFile(t, editor, "#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\n", 0o755)
	w.set(t, "editor.command", editor+" --reuse")
	writeFile(t, filepath.Join(a.Worktree, "src", "main.go"), "package main", 0o644)
	outside := filepath.Join(t.TempDir(), "secret")
	writeFile(t, outside, "x", 0o644)
	if err := os.Symlink(outside, filepath.Join(a.Worktree, "escape")); err != nil {
		t.Fatal(err)
	}
	open := func(c interface {
		OpenInEditor(context.Context, *connect.Request[v1.OpenInEditorRequest]) (*connect.Response[v1.OpenInEditorResponse], error)
	}, path string) (*v1.OpenInEditorResponse, error) {
		res, err := c.OpenInEditor(ctx, connect.NewRequest(&v1.OpenInEditorRequest{Meta: meta(""), Agent: a.Id, Path: path}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	real, _ := filepath.EvalSymlinks(a.Worktree)
	res, err := open(w.device.Mesh, "src/main.go")
	if err != nil || res.Opened != filepath.Join(real, "src", "main.go") {
		t.Fatalf("open = %v, %v", res, err)
	}
	waitFor(t, "the editor to run", func() bool { b, _ := os.ReadFile(record); return strings.Contains(string(b), "main.go") })
	if b, _ := os.ReadFile(record); string(b) != "--reuse\n"+filepath.Join(real, "src", "main.go")+"\n" {
		t.Errorf("the editor got %q", b)
	}
	if res, err := open(w.device.Mesh, ""); err != nil || res.Opened != real {
		t.Errorf("the worktree itself: %v, %v", res, err)
	}
	for _, bad := range []string{"escape", "../outside", outside, "nope.txt"} {
		if _, err := open(w.device.Mesh, bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
	if _, err := asLead.OpenInEditor(ctx, connect.NewRequest(&v1.OpenInEditorRequest{Meta: meta(""), Agent: a.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root opened an editor: %v", err)
	}
	// What an agent may not set: the program that runs.
	if _, err := asLead.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "editor.command", Value: mustValue(t, "sh")})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root set editor.command: %v", err)
	}
	w.set(t, "editor.command", "no-such-editor-xyz")
	if _, err := open(w.device.Mesh, ""); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "PATH") {
		t.Errorf("an editor that isn't there: %v", err)
	}
}

func TestSetTailnetWritesTheMarkerAndRestartsOnlyUnderAServiceManager(t *testing.T) {
	ctx := context.Background()
	restarts := make(chan struct{}, 4)
	h := startWithConfig(t, daemon.Config{Restart: func() { restarts <- struct{}{} }})
	marker := daemon.TailnetMarker(h.state)
	set := func(enabled bool) *v1.SetTailnetResponse {
		t.Helper()
		res, err := h.device.Mesh.SetTailnet(ctx, connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: enabled}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	if res := set(false); res.Enabled || res.Restarting || res.Note != "already off" {
		t.Errorf("off when off: %v", res)
	}
	res := set(true)
	if !res.Enabled || !res.Restarting || res.RestartNeeded {
		t.Errorf("turning on: %v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("no marker: %v", err)
	}
	select {
	case <-restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("meshd was not asked to restart")
	}
	// A restart that never came leaves the marker: asking again restarts again.
	if res := set(true); !res.Restarting {
		t.Errorf("turning on twice: %v", res)
	}
	<-restarts
	if res := set(false); res.Enabled || !res.RestartNeeded && res.Restarting {
		t.Errorf("turning off: %v", res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker after off: %v", err)
	}
	select {
	case <-restarts:
		t.Error("a restart for a node that never ran")
	case <-time.After(1200 * time.Millisecond):
	}
}

// A retry of an applied request (the same mutation id) gets the first answer and restarts nothing again.
func TestARetriedSetTailnetRestartsOnce(t *testing.T) {
	restarts := make(chan struct{}, 4)
	h := startWithConfig(t, daemon.Config{Restart: func() { restarts <- struct{}{} }})
	for i := 0; i < 2; i++ {
		res, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta("once"), Enabled: true}))
		if err != nil || !res.Msg.Restarting {
			t.Fatalf("call %d: %v, %v", i, res, err)
		}
	}
	<-restarts
	select {
	case <-restarts:
		t.Error("a replay restarted meshd again")
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestSetTailnetWithoutAServiceManagerSaysARestartIsNeeded(t *testing.T) {
	h := startWithConfig(t, daemon.Config{})
	res, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: true}))
	if err != nil || !res.Msg.Enabled || res.Msg.Restarting || !res.Msg.RestartNeeded {
		t.Errorf("SetTailnet = %v, %v", res, err)
	}
}

func TestSetTailnetCannotTurnOffANodeStartedWithTheFlag(t *testing.T) {
	tn := &fakeTailnet{closed: make(chan struct{}), status: &v1.TailnetStatusResponse{State: "Running"}}
	restarts := make(chan struct{}, 1)
	h := startWithConfig(t, daemon.Config{Tailnet: tn, TailnetFromFlag: true, Restart: func() { restarts <- struct{}{} }})
	res, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: false}))
	if err != nil || !res.Msg.Enabled || res.Msg.Restarting || !strings.Contains(res.Msg.Note, "--tailscale") {
		t.Errorf("off with the flag: %v, %v", res, err)
	}
	select {
	case <-restarts:
		t.Error("restarted for nothing")
	case <-time.After(1200 * time.Millisecond):
	}
}

// A node the marker started goes off when asked: the answer says it is off, and the marker is gone.
func TestSetTailnetOffReportsOffForANodeTheMarkerStarted(t *testing.T) {
	tn := &fakeTailnet{closed: make(chan struct{}), status: &v1.TailnetStatusResponse{State: "Running"}}
	restarts := make(chan struct{}, 1)
	h := startWithConfig(t, daemon.Config{Tailnet: tn, Restart: func() { restarts <- struct{}{} }})
	marker := daemon.TailnetMarker(h.state)
	writeFile(t, marker, "", 0o600)
	res, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: false}))
	if err != nil || res.Msg.Enabled || !res.Msg.Restarting {
		t.Errorf("off: %v, %v", res, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker: %v", err)
	}
	<-restarts
}

// A binary that can't run the node is told so before the marker is written: it would exit at every start.
func TestSetTailnetOnRefusesABinaryThatCannotRunTheNode(t *testing.T) {
	restarts := make(chan struct{}, 1)
	h := startWithConfig(t, daemon.Config{Restart: func() { restarts <- struct{}{} }, TailnetCheck: func() error { return fmt.Errorf("built without the tags") }})
	_, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: true}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "built without the tags") {
		t.Errorf("on: %v", err)
	}
	if _, err := os.Stat(daemon.TailnetMarker(h.state)); !os.IsNotExist(err) {
		t.Errorf("a marker was written: %v", err)
	}
	select {
	case <-restarts:
		t.Error("restarted for nothing")
	case <-time.After(1200 * time.Millisecond):
	}
}

// A marker left from before the flag would keep the node on once the flag is removed: turning off clears it.
func TestSetTailnetOffClearsAMarkerLeftBesideTheFlag(t *testing.T) {
	tn := &fakeTailnet{closed: make(chan struct{}), status: &v1.TailnetStatusResponse{State: "Running"}}
	h := startWithConfig(t, daemon.Config{Tailnet: tn, TailnetFromFlag: true})
	marker := daemon.TailnetMarker(h.state)
	writeFile(t, marker, "", 0o600)
	if _, err := h.device.Mesh.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: false})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the stale marker stays: %v", err)
	}
}

func TestSetTailnetIsForTheHostsOwnDeviceOnly(t *testing.T) {
	w := startPR(t)
	ro := readOnlyDevice(t, w)
	if _, err := ro.SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: true})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only device: %v", err)
	}
	if _, err := w.as(w.rootTok).SetTailnet(context.Background(), connect.NewRequest(&v1.SetTailnetRequest{Meta: meta(""), Enabled: true})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root: %v", err)
	}
}

func TestCreatePairingRefusesWithTheCauseWhenTheHostHasNoRoute(t *testing.T) {
	ctx := context.Background()
	h := startWithConfig(t, daemon.Config{})
	_, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p1"), Name: "phone"}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "--listen") || !strings.Contains(err.Error(), "Tailscale is off") {
		t.Fatalf("no route: %v", err)
	}
	// Each way of giving it one makes the link.
	if _, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p2"), DirectUrl: testDirectURL})); err != nil {
		t.Errorf("a direct url: %v", err)
	}
	tn := &fakeTailnet{closed: make(chan struct{}), status: &v1.TailnetStatusResponse{State: "NeedsLogin"}}
	h2 := startWithConfig(t, daemon.Config{Tailnet: tn})
	_, err = h2.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p3")}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "node isn't connected yet") {
		t.Errorf("a node that isn't up: %v", err)
	}
	tn.setStatus(&v1.TailnetStatusResponse{State: "Running", Addresses: []string{"100.64.0.7"}})
	if _, err := h2.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p4")})); err != nil {
		t.Errorf("a running node: %v", err)
	}
}
