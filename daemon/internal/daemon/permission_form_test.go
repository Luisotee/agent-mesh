package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// pendingKind waits for a prompt of an agent and returns it.
func (w *reviewWorld) pendingKind(t *testing.T, agentID string, kind v1.PermissionKind) *v1.PendingPermission {
	t.Helper()
	p := w.pendingFor(t, agentID)
	if p.Kind != kind {
		t.Fatalf("pending kind = %v, want %v: %v", p.Kind, kind, p)
	}
	return p
}

func (w *reviewWorld) decideForm(c interface {
	DecidePermission(context.Context, *connect.Request[v1.DecidePermissionRequest]) (*connect.Response[v1.DecidePermissionResponse], error)
}, req *v1.DecidePermissionRequest) error {
	req.Meta = meta("")
	_, err := c.DecidePermission(context.Background(), connect.NewRequest(req))
	return err
}

// A question for the user is read into the prompt (typed questions, options, previews), tells no agent, and is
// answered from a device: the CLI gets the answers as the hook's updated input.
func TestAQuestionForTheUserIsAnsweredFromADevice(t *testing.T) {
	w := startReviews(t)
	w.typeInto(w.lead, "/question\r")
	p := w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_QUESTION)
	if !p.ForUser || p.ParentId != "" || p.ToolUseId != "toolu_fake_ask" || p.Tool != "AskUserQuestion" {
		t.Fatalf("prompt = %v", p)
	}
	if len(p.Questions) != 2 || p.Questions[0].Header != "Color" || p.Questions[0].MultiSelect || !p.Questions[1].MultiSelect ||
		p.Questions[0].Options[0].Label != "Red" || p.Questions[0].Options[0].Preview != "RED swatch" || p.Questions[1].Options[1].Label != "Ham" {
		t.Fatalf("questions = %v", p.Questions)
	}
	if m := w.infoTo(t, w.lead, permissionTag); m != nil {
		t.Errorf("an agent was told of the user's question: %s", m.Body)
	}
	color, toppings := p.Questions[0].Question, p.Questions[1].Question
	ans := func(q string, other string, labels ...string) *v1.PromptAnswer {
		return &v1.PromptAnswer{Question: q, Labels: labels, Other: other}
	}
	for name, bad := range map[string][]*v1.PromptAnswer{
		"no answers":           nil,
		"one question missing": {ans(color, "", "Red")},
		"an unknown option":    {ans(color, "", "Green"), ans(toppings, "", "Cheese")},
		"two for a single":     {ans(color, "", "Red", "Blue"), ans(toppings, "", "Cheese")},
		"a label and other":    {ans(color, "mine", "Red"), ans(toppings, "", "Cheese")},
		"an empty answer":      {ans(color, "  "), ans(toppings, "", "Cheese")},
		"an unknown question":  {ans(color, "", "Red"), ans(toppings, "", "Cheese"), ans("Why?", "", "x")},
		"a label twice":        {ans(color, "", "Red"), ans(toppings, "", "Cheese", "Cheese")},
		"a question twice":     {ans(color, "", "Red"), ans(color, "", "Blue"), ans(toppings, "", "Cheese")},
	} {
		if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow", Answers: bad}); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow", Mode: "acceptEdits",
		Answers: []*v1.PromptAnswer{ans(color, "", "Red"), ans(toppings, "", "Cheese")}}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a mode on a question: %v, want InvalidArgument", err)
	}
	// An agent never answers the user's prompt, not even its own root.
	if err := w.decideForm(w.asRoot, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow",
		Answers: []*v1.PromptAnswer{ans(color, "", "Red"), ans(toppings, "", "Cheese")}}); code(err) != connect.CodePermissionDenied {
		t.Errorf("an agent answering: %v, want PermissionDenied", err)
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow",
		Answers: []*v1.PromptAnswer{ans(color, "", "Blue"), ans(toppings, "extra jam", "Cheese", "Ham")}}); err != nil {
		t.Fatal(err)
	}
	want := `answered: {"Which color do you want?":"Blue","Which toppings do you want?":"Cheese, Ham, extra jam"}`
	waitFor(t, want, func() bool { return strings.Contains(w.screen(t, w.lead), want) })
	w.waitState(w.lead.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// Dismissing the question denies it with a reason the agent reads.
func TestAQuestionDismissedIsDeniedWithAReason(t *testing.T) {
	w := startReviews(t)
	w.typeInto(w.lead, "/question\r")
	p := w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_QUESTION)
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "deny"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a dismissal without a reason: %v", err)
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "deny", Why: "ask me in chat"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "denied", func() bool { return strings.Contains(w.screen(t, w.lead), "permission: deny ask me in chat") })
}

// A plan to approve: the plan is read into the prompt; approving it echoes the input back and may set the mode.
func TestAPlanIsApprovedWithAModeOrSentBack(t *testing.T) {
	w := startReviews(t)
	w.typeInto(w.lead, "/exitplan\r")
	p := w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_PLAN)
	if !p.ForUser || p.Plan != "# Plan\n\n1. Create hello.txt\n" || p.PlanFile != "/tmp/plan.md" || len(p.Questions) != 0 {
		t.Fatalf("plan prompt = %v", p)
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow", Mode: "bypassPermissions"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an unknown mode: %v, want InvalidArgument", err)
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow", Answers: []*v1.PromptAnswer{{Question: "x", Labels: []string{"y"}}}}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("answers on a plan: %v, want InvalidArgument", err)
	}
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "deny", Why: "add a test step"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent back", func() bool { return strings.Contains(w.screen(t, w.lead), "permission: deny add a test step") })
	w.waitState(w.lead.Id, v1.AgentState_AGENT_STATE_IDLE)

	w.typeInto(w.lead, "/exitplan\r")
	p = w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_PLAN)
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow", Mode: "acceptEdits"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "approved with the mode", func() bool { return strings.Contains(w.screen(t, w.lead), "plan approved: mode=acceptEdits") })
	w.waitState(w.lead.Id, v1.AgentState_AGENT_STATE_IDLE)

	w.typeInto(w.lead, "/exitplan\r")
	p = w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_PLAN)
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "approved, the mode left to the CLI", func() bool { return strings.Contains(w.screen(t, w.lead), "plan approved: mode= input=") })
}

// A root's own tool prompt, and the outward ones a parent is never asked, are the user's: held, listed for a device,
// told to no agent.
func TestAnOutwardPromptIsHeldForTheUser(t *testing.T) {
	w := startReviews(t)
	wk := w.worker(t, "wk")
	w.typeInto(wk, "/perm Bash git push origin main\r")
	p := w.pendingFor(t, wk.Id)
	if !p.ForUser || p.ParentId != "" || p.Kind != v1.PermissionKind_PERMISSION_KIND_TOOL || p.Summary != "git push origin main" {
		t.Fatalf("prompt = %v", p)
	}
	if m := w.infoTo(t, w.lead, permissionTag); m != nil {
		t.Errorf("the root was told of the user's prompt: %s", m.Body)
	}
	if err := w.decide(t, w.asRoot, p.Id, "allow", ""); code(err) != connect.CodePermissionDenied {
		t.Errorf("the root answering a push: %v, want PermissionDenied", err)
	}
	// It is not in the root's own list, but it is in a device's.
	if res, _ := w.asRoot.ListPermissions(context.Background(), connect.NewRequest(&v1.ListPermissionsRequest{})); len(res.Msg.Permissions) != 0 {
		t.Errorf("the root lists the user's prompt: %v", res.Msg.Permissions)
	}
	if err := w.decide(t, w.device.Mesh, p.Id, "deny", "not from a worker"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "denied", func() bool { return strings.Contains(w.screen(t, wk), "permission: deny not from a worker") })
	w.waitState(wk.Id, v1.AgentState_AGENT_STATE_IDLE)

	// A root's own tool prompt too.
	w.typeInto(w.lead, "/perm Bash rm -rf build\r")
	p = w.pendingFor(t, w.lead.Id)
	if !p.ForUser || p.Tool != "Bash" {
		t.Fatalf("the root's prompt = %v", p)
	}
	if err := w.decide(t, w.device.Mesh, p.Id, "allow", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "allowed", func() bool { return strings.Contains(w.screen(t, w.lead), "permission: allow") })
}

// Answered at the terminal first: the CLI reports the call's end (its tool_use_id), the prompt ends, and the app's
// late answer finds nothing. A prompt of another call is left.
func TestAQuestionAnsweredAtTheTerminalEndsByItsToolUseID(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.typeInto(w.lead, "/question\r")
	p := w.pendingKind(t, w.lead.Id, v1.PermissionKind_PERMISSION_KIND_QUESTION)
	rootHook := w.as(w.token(t, w.lead.Id))
	report := func(payload string) {
		t.Helper()
		if _, err := rootHook.ReportHook(ctx, connect.NewRequest(&v1.ReportHookRequest{Meta: meta(""), Kind: "claude", EventName: "PostToolUse", Payload: []byte(payload)})); err != nil {
			t.Fatal(err)
		}
	}
	report(`{"hook_event_name":"PostToolUse","tool_name":"AskUserQuestion","tool_use_id":"toolu_other","tool_input":{}}`)
	time.Sleep(300 * time.Millisecond)
	if res, _ := w.device.Mesh.ListPermissions(ctx, connect.NewRequest(&v1.ListPermissionsRequest{})); len(res.Msg.Permissions) != 1 {
		t.Fatalf("another call's end ended the prompt: %v", res.Msg.Permissions)
	}
	report(`{"hook_event_name":"PostToolUse","tool_name":"AskUserQuestion","tool_use_id":"toolu_fake_ask","tool_input":{}}`)
	waitFor(t, "the prompt to end", func() bool {
		res, err := w.device.Mesh.ListPermissions(ctx, connect.NewRequest(&v1.ListPermissionsRequest{}))
		return err == nil && len(res.Msg.Permissions) == 0
	})
	if err := w.decideForm(w.device.Mesh, &v1.DecidePermissionRequest{Id: p.Id, Decision: "deny", Why: "late"}); code(err) != connect.CodeNotFound {
		t.Errorf("a late answer: %v, want NotFound", err)
	}
}

// A form the daemon can't read (a CLI whose shape changed), or no hold for the user: nothing is held, the dialog stays
// with the person at the terminal.
func TestAFormThatCantBeReadIsLeftToTheTerminal(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	for name, req := range map[string]*v1.RequestPermissionRequest{
		"not json":                   {Tool: "AskUserQuestion", Input: "{"},
		"no questions":               {Tool: "AskUserQuestion", Input: `{"questions":[]}`},
		"a question twice":           {Tool: "AskUserQuestion", Input: `{"questions":[{"question":"a","options":[{"label":"x"}]},{"question":"a","options":[{"label":"x"}]}]}`},
		"a question with no options": {Tool: "AskUserQuestion", Input: `{"questions":[{"question":"a","options":[]}]}`},
		"an empty plan":              {Tool: "ExitPlanMode", Input: `{"plan":" "}`},
	} {
		start := time.Now()
		res, err := w.asRoot.RequestPermission(ctx, connect.NewRequest(req))
		if err != nil || res.Msg.Decision != "" || time.Since(start) > 3*time.Second {
			t.Errorf("%s: %v, %v after %s", name, res, err, time.Since(start))
		}
	}
	w.set(t, "permission.user_wait", 0)
	start := time.Now()
	res, err := w.asRoot.RequestPermission(ctx, connect.NewRequest(&v1.RequestPermissionRequest{Tool: "AskUserQuestion", Input: `{"questions":[{"question":"a","options":[{"label":"x"}]}]}`}))
	if err != nil || res.Msg.Decision != "" || time.Since(start) > 3*time.Second {
		t.Errorf("no hold for the user: %v, %v after %s", res, err, time.Since(start))
	}
}
