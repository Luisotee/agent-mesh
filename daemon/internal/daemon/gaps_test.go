package daemon_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestRenameRunChangesTheTitleAndLogsIt(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	rename := func(c interface {
		RenameRun(context.Context, *connect.Request[v1.RenameRunRequest]) (*connect.Response[v1.RenameRunResponse], error)
	}, run, title, mid string) (*v1.Run, error) {
		res, err := c.RenameRun(ctx, connect.NewRequest(&v1.RenameRunRequest{Meta: meta(mid), RunId: run, Title: title}))
		if err != nil {
			return nil, err
		}
		return res.Msg.Run, nil
	}
	run, err := rename(w.device.Mesh, w.run.Id, "  the  shop   checkout ", "m1")
	if err != nil || run.Title != "the shop checkout" {
		t.Fatalf("rename = %v, %v", run, err)
	}
	// A replay of the same mutation answers the same and logs once.
	if _, err := rename(w.device.Mesh, w.run.Id, "the shop checkout", "m1"); err != nil {
		t.Fatal(err)
	}
	// A root renames its own run (its run id is implied), never another.
	if run, err := rename(w.as(w.rootTok), "", "root's title", "m2"); err != nil || run.Title != "root's title" {
		t.Fatalf("a root's own run: %v, %v", run, err)
	}
	other := startRun(t, w.device.Mesh, "")
	if _, err := rename(w.as(w.rootTok), other.Id, "x", "m3"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a root renamed another run: %v", err)
	}
	if _, err := rename(w.device.Mesh, w.run.Id, "  ", "m4"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a blank title: %v", err)
	}
	if _, err := rename(w.device.Mesh, "nope", "x", "m5"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown run: %v", err)
	}
	if _, err := rename(w.as(w.workTok), "", "x", "m6"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a worker renamed a run: %v", err)
	}
	evs, err := w.d.Store().Read(ctx).ListEvents(w.run.Id)
	if err != nil {
		t.Fatal(err)
	}
	var renames []*v1.RunRenamed
	for _, ev := range evs {
		if r := ev.GetRunRenamed(); r != nil {
			renames = append(renames, r)
		}
	}
	if len(renames) != 2 || renames[0].Previous != "demo" || renames[0].Title != "the shop checkout" || renames[1].Title != "root's title" {
		t.Errorf("rename events = %v", renames)
	}
}

func TestARunStartedWithItsOwnAutonomyReplacesTheRepoListsInItsBrief(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	brief := func(run *v1.Run, root string) string {
		t.Helper()
		res, err := w.as(root).GetBrief(ctx, connect.NewRequest(&v1.GetBriefRequest{RunId: run.Id}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Text
	}
	if text := brief(w.run, w.rootTok); strings.Contains(text, "autonomy") {
		t.Errorf("an autonomy line for a run with none:\n%s", text)
	}
	started, err := w.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: meta(""), Title: "careful",
		Autonomy: &v1.RunAutonomy{UserApprovesPlans: true, AskBefore: []string{" deploys ", "", "deploys", "migrations"}}}))
	if err != nil {
		t.Fatal(err)
	}
	run := started.Msg.Run
	if a := run.Autonomy; a == nil || !a.UserApprovesPlans || !a.UserMerges || len(a.AskBefore) != 2 {
		t.Fatalf("stored autonomy = %v", run.Autonomy)
	}
	rootTok := w.seedAgent(run.Id, "b-root", "repo-lead2", "", v1.AgentRole_AGENT_ROLE_ROOT)
	text := brief(run, rootTok)
	for _, want := range []string{"the user's autonomy for this run", "the user approves its plans and merges its PRs", "ask the user before: deploys; migrations"} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
	// A listed repo does not change a run that has its own.
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "repos.user_merges",
		Value: mustValue(t, []any{"/p/shop"})})); err != nil {
		t.Fatal(err)
	}
	if text := brief(run, rootTok); strings.Contains(text, "/p/shop") {
		t.Errorf("the repo list reached a run with its own autonomy:\n%s", text)
	}
	if text := brief(w.run, w.rootTok); !strings.Contains(text, "repo /p/shop: the user merges its PRs") {
		t.Errorf("the repo list is gone for the other run:\n%s", text)
	}
	// A run that only adds to what is asked keeps the repo lists.
	keep, err := w.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: meta(""), Title: "extra",
		Autonomy: &v1.RunAutonomy{KeepRepoLists: true, UserApprovesPlans: true, AskBefore: []string{"deploys"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if a := keep.Msg.Run.Autonomy; a.UserApprovesPlans || a.UserMerges || !a.KeepRepoLists {
		t.Errorf("keep_repo_lists stored with the flags: %v", a)
	}
	keepTok := w.seedAgent(keep.Msg.Run.Id, "c-root", "repo-lead3", "", v1.AgentRole_AGENT_ROLE_ROOT)
	text = brief(keep.Msg.Run, keepTok)
	for _, want := range []string{"repo /p/shop: the user merges its PRs", "ask the user before: deploys"} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief of a run that keeps the repo lists lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "autonomy for this run") {
		t.Errorf("the run-level line shows for a run that keeps the repo lists:\n%s", text)
	}
}

func TestListPullRequestsSaysWhyGitHubCannotBeRead(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	problem := func() string {
		t.Helper()
		res, err := w.device.Mesh.ListPullRequests(ctx, connect.NewRequest(&v1.ListPullRequestsRequest{RunId: w.run.Id}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GithubProblem
	}
	if p := problem(); p != "" {
		t.Errorf("a working gh is a problem: %q", p)
	}
	set := func(err error) {
		w.mu.Lock()
		w.fail = err
		w.mu.Unlock()
		w.advance(2 * time.Minute) // past the kept answer
	}
	set(&exec.Error{Name: "gh", Err: exec.ErrNotFound})
	if p := problem(); p != "no_gh" {
		t.Errorf("a missing gh: %q", p)
	}
	exit := exec.Command("sh", "-c", "exit 1").Run()
	if exit == nil {
		t.Fatal("sh -c 'exit 1' succeeded")
	}
	set(exit)
	if p := problem(); p != "not_logged_in" {
		t.Errorf("a gh that is not logged in: %q", p)
	}
	set(errors.New("timeout"))
	if p := problem(); p != "" {
		t.Errorf("an unknown failure is no claim: %q", p)
	}
}

func TestGetAgentListsTheModelsItRanOn(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	add := func(ev *v1.Event) {
		t.Helper()
		if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error { _, err := tx.AppendEvent(ev); return err }); err != nil {
			t.Fatal(err)
		}
	}
	reopen := func(model, effort, why string) *v1.Event {
		return &v1.Event{RunId: w.run.Id, AgentId: "a-w1", Payload: &v1.Event_AgentReopened{AgentReopened: &v1.AgentReopened{
			Reopen: &v1.Reopen{Model: model, Effort: effort, Why: why}}}}
	}
	add(&v1.Event{RunId: w.run.Id, AgentId: "a-w1", Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{
		Agent: &v1.Agent{Id: "a-w1", Model: "opus", Effort: "high"}}}})
	add(reopen("opus", "high", "resumed")) // no change: no row
	add(reopen("sonnet", "medium", "plan approved"))
	add(reopen("", "high", "moved")) // effort only
	add(&v1.Event{RunId: w.run.Id, AgentId: "a-other", Payload: &v1.Event_AgentReopened{AgentReopened: &v1.AgentReopened{
		Reopen: &v1.Reopen{Model: "haiku", Why: "quota"}}}})
	res, err := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: "a-w1"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Msg.ModelHistory {
		got = append(got, m.Model+"/"+m.Effort+" "+m.Why)
	}
	want := "opus/high spawned|sonnet/medium plan approved|sonnet/high moved"
	if strings.Join(got, "|") != want {
		t.Errorf("history = %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestOpenViewReachesOnlyTheAppsOfADevice(t *testing.T) {
	w := startPR(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Activity: true}))
	if err != nil {
		t.Fatal(err)
	}
	// A subscriber that wants no activity frames is no app; neither is an agent's.
	plain, _ := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	agentSub, _ := w.as(w.rootTok).SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Activity: true}))
	for _, s := range []*connect.ServerStreamForClient[v1.SubscribeEventsResponse]{app, plain, agentSub} {
		if !s.Receive() || s.Msg().GetSnapshot() == nil {
			t.Fatalf("no snapshot: %v", s.Err())
		}
	}
	root := w.as(w.rootTok)
	open := func(c interface {
		OpenView(context.Context, *connect.Request[v1.OpenViewRequest]) (*connect.Response[v1.OpenViewResponse], error)
	}, view, target, run string) (*v1.OpenViewResponse, error) {
		res, err := c.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(""), View: view, Target: target, RunId: run}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	res, err := open(root, "agent", "demo-fix", "")
	if err != nil || res.Delivered != 1 {
		t.Fatalf("open agent = %v, %v", res, err)
	}
	if !app.Receive() {
		t.Fatalf("the app got no frame: %v", app.Err())
	}
	v := app.Msg().GetView()
	if v == nil || v.View != "agent" || v.Target != "a-w1" || v.RunId != w.run.Id || v.From != "repo-lead" {
		t.Errorf("view frame = %v", app.Msg())
	}
	if _, err := open(root, "run", "", ""); err != nil {
		t.Errorf("a root's own run: %v", err)
	}
	other := startRun(t, w.device.Mesh, "")
	for _, c := range []struct{ view, target, run string }{{"run", other.Id, ""}, {"agent", "nobody", ""}, {"settings", "", other.Id}} {
		if _, err := open(root, c.view, c.target, c.run); err == nil {
			t.Errorf("a root opened %v", c)
		}
	}
	for _, c := range []struct{ view, target string }{{"bogus", ""}, {"agent", ""}, {"terminal", ""}} {
		if _, err := open(w.device.Mesh, c.view, c.target, ""); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("open %v: %v", c, err)
		}
	}
	if res, err := open(w.device.Mesh, "settings", "devices", ""); err != nil || res.Delivered != 1 {
		t.Errorf("settings = %v, %v", res, err)
	}
	if _, err := open(w.as(w.workTok), "settings", "", ""); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a worker opened a view: %v", err)
	}
}

func mustValue(t *testing.T, v any) *structpb.Value {
	t.Helper()
	out, err := structpb.NewValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
