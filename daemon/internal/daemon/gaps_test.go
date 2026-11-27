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
	// `gh auth status` failing: only its own "not logged in" words make it so; offline says nothing.
	failed := func(stderr string) error {
		_, err := exec.Command("sh", "-c", "echo '"+stderr+"' >&2; exit 1").Output()
		if err == nil {
			t.Fatal("the command succeeded")
		}
		return err
	}
	set(failed("You are not logged into any GitHub hosts. To log in, run: gh auth login"))
	if p := problem(); p != "not_logged_in" {
		t.Errorf("a gh that is not logged in: %q", p)
	}
	set(failed("error connecting to api.github.com"))
	if p := problem(); p != "" {
		t.Errorf("a gh that is offline: %q", p)
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
	add(reopen("sonnet", "high", "moved")) // effort only
	add(reopen("haiku", "", "quota"))      // a model with no effort
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
	want := "opus/high spawned|sonnet/medium plan approved|sonnet/high moved|haiku/ quota"
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
	w.advance(11 * time.Second) // an agent may ask once in 10 s
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
	w.advance(11 * time.Second)
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

func TestAnAgentCannotSetOpenViewsAndAskingForViewsIsRateLimited(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	root := w.as(w.rootTok)
	// Only the user decides whether an agent may switch their view by itself.
	if _, err := root.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "app.open_views", Value: mustValue(t, true)})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a root set app.open_views: %v", err)
	}
	if _, err := root.ResetSetting(ctx, connect.NewRequest(&v1.ResetSettingRequest{Meta: meta(""), Key: "app.open_views"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a root reset app.open_views: %v", err)
	}
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "app.open_views", Value: mustValue(t, true)})); err != nil {
		t.Errorf("the user set app.open_views: %v", err)
	}
	open := func() error {
		_, err := root.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(""), View: "settings"}))
		return err
	}
	if err := open(); err != nil {
		t.Fatal(err)
	}
	if err := open(); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a second view at once: %v", err)
	}
	w.advance(11 * time.Second)
	if err := open(); err != nil {
		t.Errorf("a view after the wait: %v", err)
	}
	// A device is the user: no limit.
	for i := 0; i < 3; i++ {
		if _, err := w.device.Mesh.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(""), View: "settings"})); err != nil {
			t.Errorf("a device's view %d: %v", i, err)
		}
	}
}

// A retry of an applied request (the same mutation id) gets its first answer and uses up none of the agent's turns.
func TestARetriedOpenViewIsNotLimited(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	root := w.as(w.rootTok)
	open := func(mid string) error {
		_, err := root.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(mid), View: "settings"}))
		return err
	}
	if err := open("same"); err != nil {
		t.Fatal(err)
	}
	if err := open("same"); err != nil {
		t.Errorf("a replay within the wait: %v", err)
	}
	if err := open("other"); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a new request within the wait: %v", err)
	}
}

func TestOpenViewLooksForTheTargetInsideTheRunNamed(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	other := startRun(t, w.device.Mesh, "")
	w.seedAgent(other.Id, "o-fix", "demo-fix", "", v1.AgentRole_AGENT_ROLE_WORKER) // the same name in another run
	open := func(view, target, run string) (*v1.OpenViewResponse, error) {
		res, err := w.device.Mesh.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(""), View: view, Target: target, RunId: run}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	app, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Activity: true}))
	if err != nil || !app.Receive() {
		t.Fatalf("subscribe: %v", err)
	}
	// Without a run the name is ambiguous; with one it names that run's agent.
	if _, err := open("agent", "demo-fix", ""); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("an ambiguous name: %v", err)
	}
	if _, err := open("agent", "demo-fix", other.Id); err != nil || !app.Receive() || app.Msg().GetView().Target != "o-fix" || app.Msg().GetView().RunId != other.Id {
		t.Errorf("demo-fix in the other run: %v %v", err, app.Msg())
	}
	if _, err := open("agent", "o-fix", w.run.Id); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an agent of another run: %v", err)
	}
	if _, err := open("run", other.Id, w.run.Id); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a run and a different run id: %v", err)
	}
}

// The messages, plan and pr views: the daemon resolves the target, so the frame names ids; a plan or a pull request there is not
// is refused, and a pull request is looked for in the run named only.
func TestOpenViewNamesMessagesAPlanAndAPullRequest(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	app, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Activity: true}))
	if err != nil || !app.Receive() {
		t.Fatalf("subscribe: %v", err)
	}
	open := func(c interface {
		OpenView(context.Context, *connect.Request[v1.OpenViewRequest]) (*connect.Response[v1.OpenViewResponse], error)
	}, view, target, run string) (*v1.ViewRequest, error) {
		t.Helper()
		if _, err := c.OpenView(ctx, connect.NewRequest(&v1.OpenViewRequest{Meta: meta(""), View: view, Target: target, RunId: run})); err != nil {
			return nil, err
		}
		for app.Receive() { // the events of the messages sent meanwhile come first
			if v := app.Msg().GetView(); v != nil {
				return v, nil
			}
		}
		t.Fatalf("the app got no view frame: %v", app.Err())
		return nil, nil
	}
	// Messages: an agent by name, or a run (its lead).
	if v, err := open(w.device.Mesh, "messages", "demo-fix", ""); err != nil || v.View != "messages" || v.Target != "a-w1" || v.RunId != w.run.Id {
		t.Errorf("messages of an agent = %v, %v", v, err)
	}
	if v, err := open(w.device.Mesh, "messages", w.run.Id, ""); err != nil || v.Target != "a-root" {
		t.Errorf("messages of a run = %v, %v", v, err)
	}
	if _, err := open(w.device.Mesh, "messages", "nobody", ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("messages of nobody: %v", err)
	}
	// A plan: refused until the agent has sent one, then the newest.
	if _, err := open(w.device.Mesh, "plan", "demo-fix", ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a plan nobody sent: %v", err)
	}
	send := func(body string) string {
		res, err := w.as(w.workTok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: "repo-lead", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: body}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Message.Id
	}
	send("first plan")
	second := send("second plan")
	if v, err := open(w.device.Mesh, "plan", "demo-fix", ""); err != nil || v.View != "plan" || v.Target != "a-w1" || v.MessageId != second {
		t.Errorf("the newest plan = %v, %v (want %s)", v, err, second)
	}
	// A pull request: by URL or number, with its owner named.
	url := "https://github.com/acme/repo/pull/7"
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutAgentPR(&v1.PullRequest{AgentId: "a-w1", Url: url, Number: 7})
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{url, "#7", "7"} {
		if v, err := open(w.device.Mesh, "pr", target, ""); err != nil || v.Target != url || v.AgentId != "a-w1" || v.RunId != w.run.Id {
			t.Errorf("pr %s = %v, %v", target, v, err)
		}
	}
	if _, err := open(w.device.Mesh, "pr", "#8", ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a pull request nobody has: %v", err)
	}
	// A number is looked for in the run named only.
	other := startRun(t, w.device.Mesh, "")
	if _, err := open(w.device.Mesh, "pr", "#7", other.Id); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a pull request of another run: %v", err)
	}
}
