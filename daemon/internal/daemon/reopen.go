package daemon

// Reopens: the core rule of agent-mesh is that a model change reopens the agent's own conversation (`claude
// --resume <session> --model … --effort …`), never a fresh agent briefed again, so the agent keeps what it
// learned. Approve (plan → execution tier), switch (another tier, model or effort, or the next model of a chain
// on a quota) and resume (a stopped agent) all come down to one path: a Reopen stored on the agent, carried out
// by reopen at its next IDLE (or at once when it is idle or stopped), before any doorbell.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/reducer"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// StopWait bounds how long a reopen waits for the old process to exit after SIGTERM (the host sends SIGKILL
// after its own grace).
var StopWait = 15 * time.Second

// Test seams (nil in production): called at the points a race needs to be forced at.
var (
	spawnPlannedHook      func()               // SpawnAgent: planned, before the agent is stored
	sendPlannedHook       func()               // SendMessage: prepared, before the write that stores the message
	spawnCommittedHook    func(agentID string) // SpawnAgent: stored, before its terminal starts (its lock held)
	stopRunLockedHook     func()               // StopRun: the run's agents locked, before the run is closed
	stopAgentLockedHook   func()               // StopAgent: the agent locked, before cancelling its pending reopen
	permissionPlannedHook func()               // RequestPermission: the parent read, before the message to it is stored
)

// launchedHook is a test seam: called once a reopen's terminal is started and before it is recorded, with a
// function that kills that process and reports whether its exit was seen.
var launchedHook func(agentID string, die func() bool)

// namedEffort is the effort of a model named without one (bin/mesh's NAMED_EFFORT).
const namedEffort = "high"

func failedPrecondition(format string, args ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
}

// ── tiers ──

// tierChain is a tier's model chain and effort for a kind, from the settings. A tier the kind doesn't have is
// InvalidArgument, naming the ones it has.
func (d *Daemon) tierChain(r store.Reader, kind, tier string) ([]string, string, error) {
	key := "tiers." + kind + "." + tier + "."
	if _, ok := d.schema.Lookup(key + "model"); !ok {
		var have []string
		for _, k := range d.schema.Keys("tiers." + kind + ".") {
			if t, ok := strings.CutSuffix(strings.TrimPrefix(k, "tiers."+kind+"."), ".model"); ok {
				have = append(have, t)
			}
		}
		if len(have) == 0 {
			return nil, "", invalid("%s has no tiers in the settings: name a model", kind)
		}
		return nil, "", invalid("%s has no %q tier: one of %s", kind, tier, strings.Join(have, ", "))
	}
	models, err := d.Setting(r, key+"model")
	if err != nil {
		return nil, "", err
	}
	effort, err := d.Setting(r, key+"effort")
	if err != nil {
		return nil, "", err
	}
	var chain []string
	for _, m := range models.([]any) {
		chain = append(chain, m.(string))
	}
	return chain, effort.(string), nil
}

// pick resolves what an agent runs on: a tier's first model with quota left (limits.go) at its effort, or a named model (at the effort
// named, else the one its name fixes, as an exact agy id does, else the tier's, else high). There is no default:
// neither is InvalidArgument.
func (d *Daemon) pick(r store.Reader, kind, tier, model, effort, what string, reviewer bool) (string, string, error) {
	return d.pickIn(r, "", "", kind, tier, model, effort, what, reviewer)
}

// pickIn is pick for work in a repo that may be set to keep its code from OpenCode's free models (freemodels.go):
// `freeOff` is the setting's entry that lists the repo ("" when it may use them), and `repo` names it in the error.
// Only the zero-cost models are held back: a named one that is free (or can't be told from one) is refused, and a
// tier's chain loses its free models.
func (d *Daemon) pickIn(r store.Reader, repo, freeOff, kind, tier, model, effort, what string, reviewer bool) (string, string, error) {
	if freeOff != "" && kind == opencode.Kind && model != "" {
		if err := d.checkNotFree(repo, freeOff, model); err != nil {
			return "", "", err
		}
	}
	if effort != "" && !validEffort(effort) {
		return "", "", invalid("effort %q: one of %s", effort, strings.Join(settings.Efforts, ", "))
	}
	if model != "" {
		if effort == "" && !(kind == opencode.Kind && model == opencode.FreeFamily) {
			effort = d.rt.adapters.ModelEffort(context.Background(), kind, model)
		}
		if tier != "" {
			_, e, err := d.tierChain(r, kind, tier)
			if err != nil {
				return "", "", err
			}
			if effort == "" {
				effort = e
			}
		}
		if effort == "" {
			effort = namedEffort
		}
		if kind == opencode.Kind && model == opencode.FreeFamily {
			expanded := d.expandChain(kind, []string{model})
			chosen := d.firstFree(r, kind, effort, expanded, reviewer)
			if chosen == "" {
				return "", "", invalid("%s", d.openCodeNoFreeError())
			}
			return chosen, effort, nil
		}
		return model, effort, nil
	}
	if tier == "" {
		return "", "", invalid("give a tier for %s (light, mid, strong, …: `mesh settings --prefix tiers.%s.` shows "+
			"what each is for), or a model if the user named one: there is no default model", what, kind)
	}
	chain, e, err := d.tierChain(r, kind, tier)
	if err != nil {
		return "", "", err
	}
	chain = d.expandChain(kind, chain)
	if freeOff != "" && kind == opencode.Kind {
		if chain, err = d.dropFree(repo, freeOff, chain); err != nil {
			return "", "", err
		}
	}
	if effort == "" {
		effort = e
	}
	chosen := d.firstFree(r, kind, effort, chain, reviewer)
	if chosen == "" {
		return "", "", invalid("%s", d.openCodeNoFreeError())
	}
	return chosen, effort, nil
}

func validEffort(e string) bool {
	for _, x := range settings.Efforts {
		if e == x {
			return true
		}
	}
	return false
}

// ── the agent's target ──

// target is what an agent runs on once any pending reopen is done: a second request builds on the first.
func target(a *v1.Agent) (model, effort string, deny bool) {
	if p := a.PendingReopen; p != nil {
		return p.Model, p.Effort, p.DenyEdits
	}
	return a.Model, a.Effort, a.DenyEdits
}

// setPhase changes an agent's phase, with its event.
func setPhase(tx *store.Tx, a *v1.Agent, to v1.AgentPhase, why string) error {
	if a.Phase == to {
		return nil
	}
	from := a.Phase
	a.Phase = to
	_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentPhaseChanged{
		AgentPhaseChanged: &v1.AgentPhaseChanged{From: from, To: to, Why: why}}})
	return err
}

// endExecution settles an agent's tier when its execution phase ends (a new task, or a plan): the model it
// runs on is its execution tier's, so that tier is the chain `switch --next` walks, never the planning tier's.
func endExecution(a *v1.Agent) {
	if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING {
		a.Tier = a.ExecTier
	}
	a.ExecTier = ""
}

// endPlanPhase is a direct task: the plan phase, if any, is over (setPhase to UNSPECIFIED, with its tier settled).
func endPlanPhase(tx *store.Tx, a *v1.Agent, why string) error {
	endExecution(a)
	return setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_UNSPECIFIED, why)
}

// backTier is the tier an agent goes back to when it executes after a re-plan: the one it executes on now, or has run
// on (none for a named model, which approve then names). A planner keeps the one it has.
func backTier(a *v1.Agent) string {
	switch a.Phase {
	case v1.AgentPhase_AGENT_PHASE_EXECUTING, v1.AgentPhase_AGENT_PHASE_PLANNING:
		return a.ExecTier
	}
	return a.Tier
}

// replanTier is the tier a re-plan runs on when none is named: the one the agent planned on (still its Tier while it
// executes), else strong, which is where research and design are planned.
func replanTier(a *v1.Agent) string {
	if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING && a.Tier != "" {
		return a.Tier
	}
	return "strong"
}

// replanTarget is the model, effort and tier an agent plans on again. A model or tier named is taken as given; an
// effort alone is a step up on the model it is on (the tier it runs on, kept for `next`); nothing named is its plan
// tier (replanTier).
func (d *Daemon) replanTarget(r store.Reader, a *v1.Agent, tier, model, effort string) (string, string, string, error) {
	what := a.Name + "'s planning"
	switch {
	case model != "":
		m, e, err := d.pickFor(r, a, "", model, effort, what)
		return m, e, "", err
	case tier == "" && effort != "":
		cm, _, _ := target(a)
		if err := d.checkKept(r, a, cm); err != nil {
			return "", "", "", err
		}
		keep := a.Tier
		if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING {
			keep = a.ExecTier
		}
		return cm, effort, keep, nil
	}
	if tier == "" {
		tier = replanTier(a)
	}
	m, e, err := d.pickFor(r, a, tier, "", effort, what)
	return m, e, tier, err
}

// replanTask puts a live worker back into planning in its own conversation: its plan phase, the planning deny list
// and the plan tier's model (or the effort given), by a reopen; ApproveAgent then moves it to execution as after a
// first plan. A worker already planning with nothing to change takes the task as more work for its plan.
func (d *Daemon) replanTask(tx *store.Tx, a *v1.Agent, tier, effort string) error {
	if a.Phase == v1.AgentPhase_AGENT_PHASE_PLANNING && tier == "" && effort == "" {
		return nil
	}
	cm, ce, cd := target(a)
	back := backTier(a)
	model, eff, planTier, err := d.replanTarget(tx.Reader, a, tier, "", effort)
	if err != nil {
		return err
	}
	if err := d.reserveCheck(tx.Reader, a.RunId, a.Kind, model, eff, "re-planning "+a.Name, isReviewer(a)); err != nil {
		return err
	}
	if (model != cm || eff != ce || !cd) && a.SessionId == "" {
		return failedPrecondition("%s's conversation id is not known yet, so it can't be reopened into planning: "+
			"send the task again once it has started", a.Name)
	}
	a.Tier, a.ExecTier, a.Pinned = planTier, back, false
	if err := setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_PLANNING, "re-plan"); err != nil {
		return err
	}
	if model != cm || eff != ce || !cd {
		if _, err := requestReopen(tx, a, model, eff, true, "re-plan", ""); err != nil {
			return err
		}
	}
	return tx.PutAgent(a)
}

// directStartRe is how a direct task begins (bin/mesh's DIRECT_RE, DIRECT_PREFIX): "Direct task" and then
// "(no plan)", a colon, a period before whitespace or the end, or the end of its line. The daemon reads it as
// bin/mesh does, to tell a task with no plan phase from one the agent plans: "Direct tasks are…" and "Direct
// task.md…" are planned.
var directStartRe = regexp.MustCompile(`^Direct task(?:\s*\(no plan\)|:|\.(?:\s|$)|[ \t\r]*(?:\n|$))`)

func isDirectTask(body string) bool { return directStartRe.MatchString(strings.TrimSpace(body)) }

// readOnlyStartRe is how a read-only task begins (bin/mesh's ASK_PREFIX and REVIEW_PREFIX): PROTOCOL.md rule 6.
var readOnlyStartRe = regexp.MustCompile(`^(?:Question|Review) \(read-only\)`)

func isReadOnlyTask(body string) bool { return readOnlyStartRe.MatchString(strings.TrimSpace(body)) }

// becomesWorker reports whether a task with this body, sent to this agent, gives an asker or reviewer a change
// to make: any task that is not a read-only one, unless a worker started the agent. A worker never gets a change
// worker (mayStart), so a plain task to its asker stays a read-only follow-up, as it was before roles changed.
func (d *Daemon) becomesWorker(r store.Reader, a *v1.Agent, body string) bool {
	if !adapter.ReadOnly(a.Role) || isReadOnlyTask(body) || a.Peer {
		return false // a peer's asker only answers, whatever it is sent
	}
	parent := v1.ActorClass_ACTOR_CLASS_DEVICE // started by a device: no parent agent
	if a.ParentId != "" {
		p, err := d.getAgent(r, a.ParentId)
		if err != nil {
			return false
		}
		parent = ClassOf(p.Role)
	}
	return mayStart(parent, v1.AgentRole_AGENT_ROLE_WORKER)
}

// takeChange turns an asker or reviewer that was given a change task into a worker, in the same conversation
// (bin/mesh does the same: the role is the agent's, not its start): its role and its live tokens' class change,
// and a planned task puts it in the plan phase, still under the planning deny list its read-only start gave it
// (approve then reopens it without, on the execution tier; a direct task lifts it through directTask or
// revive). It stores the agent. A task that is read-only, one sent to any other role, and one to an agent a
// worker started change nothing. On a stopped agent whose resume is then refused, the conversion stays with the
// stored task: the next resume brings it back as the worker the task is for.
func (d *Daemon) takeChange(tx *store.Tx, a *v1.Agent, body string, planned bool) error {
	if !d.becomesWorker(tx.Reader, a, body) {
		return nil
	}
	from := a.Role
	why := "change task"
	if planned {
		why = "planned change task"
	}
	a.Role = v1.AgentRole_AGENT_ROLE_WORKER
	a.DenyEdits = true // it was launched under the read-only list: until it is lifted, the list is its own
	if err := tx.SetTokenClass(a.Id, ClassOf(a.Role)); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentRoleChanged{
		AgentRoleChanged: &v1.AgentRoleChanged{From: from, To: a.Role, Why: why}}}); err != nil {
		return err
	}
	if planned {
		a.Pinned = false // a named model covers the whole task, but a plan phase now exists to approve into a tier
		endExecution(a)
		if err := setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_PLANNING, why); err != nil {
			return err
		}
	}
	return tx.PutAgent(a)
}

// directTask applies a direct task sent to an agent outside resume: the plan phase it was in ends, and the
// planning deny list goes unless the role is read-only. A running process can't change its deny list, so that
// is a reopen of the same model and effort; a stopped agent just launches without it when it next starts.
func (d *Daemon) directTask(tx *store.Tx, a *v1.Agent) error {
	model, effort, deny := target(a)
	lift := deny && !adapter.ReadOnly(a.Role)
	if a.Phase == v1.AgentPhase_AGENT_PHASE_UNSPECIFIED && !lift {
		return nil
	}
	// Lifting a running planner's deny list reopens it on the model it has: not onto a free one its repo now refuses
	// (a stopped one is checked when it is resumed). Before anything is written.
	if lift && (a.PendingReopen != nil || !stopped(a)) {
		if err := d.checkKept(tx.Reader, a, model); err != nil {
			return err
		}
	}
	if err := endPlanPhase(tx, a, "direct task"); err != nil {
		return err
	}
	if lift {
		switch {
		case a.PendingReopen != nil || !stopped(a):
			if a.SessionId == "" {
				return failedPrecondition("%s's conversation id is not known yet, so it can't be reopened without its "+
					"planning deny list: send the task again once it has started", a.Name)
			}
			if _, err := requestReopen(tx, a, model, effort, false, "direct task", ""); err != nil {
				return err
			}
		default:
			a.DenyEdits = false
		}
	}
	return tx.PutAgent(a)
}

// requestReopen stores a reopen on the agent (replacing one not done yet), with its event. The caller stores the
// agent and, after the commit, calls deliverSoon.
func requestReopen(tx *store.Tx, a *v1.Agent, model, effort string, deny bool, why, reason string) (*v1.Reopen, error) {
	return requestReopenWith(tx, a, model, effort, deny, why, reason, false)
}

// requestReopenWith is requestReopen for a reopen that starts a fresh conversation (a root's reset) when fresh: the
// event it records says so.
func requestReopenWith(tx *store.Tx, a *v1.Agent, model, effort string, deny bool, why, reason string, fresh bool) (*v1.Reopen, error) {
	r := &v1.Reopen{Id: ids.New("o", 10), Model: model, Effort: effort, DenyEdits: deny, Why: why, Reason: reason,
		RequestedAt: timestamppb.New(tx.Now()), RequestedBy: tx.Actor, Fresh: fresh}
	a.PendingReopen = r
	_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentReopenRequested{
		AgentReopenRequested: &v1.AgentReopenRequested{Reopen: r}}})
	return r, err
}

// storeMessage stores a message to an agent from the transaction's actor, with its event.
func storeMessage(tx *store.Tx, a *v1.Agent, typ v1.MessageType, body string) (*v1.Message, error) {
	m := &v1.Message{Id: ids.New("m", 10), RunId: a.RunId, From: tx.Actor, ToId: a.Id, ToName: a.Name, Type: typ,
		Body: body, CreatedAt: timestamppb.New(tx.Now())}
	if err := tx.PutMessage(m); err != nil {
		return nil, err
	}
	_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_MessageSent{
		MessageSent: &v1.MessageSent{Message: m}}})
	return m, err
}

// isReport is a message type an agent reports with; info and wait don't answer a task.
func isReport(t v1.MessageType) bool {
	switch t {
	case v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED:
		return true
	}
	return false
}

// lastReport is an agent's newest report to its parent since its last task or answer, or nil (a question to
// one of its own children is no report).
func lastReport(r store.Reader, a *v1.Agent) (*v1.Message, error) {
	msgs, err := r.RunMessages(a.RunId, "", -1) // -1: no limit
	if err != nil {
		return nil, err
	}
	var report *v1.Message
	for _, m := range msgs {
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			report = nil
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && isReport(m.Type):
			report = m
		}
	}
	return report, nil
}

// plannerOwesItsPlan reports whether the agent is a planner at work on its plan: in its plan phase, with no report
// (plan, question, done or failed) since its last task or answer, and either starting, in a turn (WORKING) or held at a
// prompt (NEEDS_INPUT: a permission dialog is part of the turn), or idle
// with that task still unread (it has not seen it yet). A task sent to it now is an addition to the plan it is
// making. One that finds it idle with its task read and no plan (its turn ended: a silent stop), or after its
// plan, is not: that is the parent's own choice to skip the plan.
func plannerOwesItsPlan(r store.Reader, a *v1.Agent) (bool, error) {
	if a.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING {
		return false, nil
	}
	switch a.State {
	case v1.AgentState_AGENT_STATE_STARTING, v1.AgentState_AGENT_STATE_WORKING, v1.AgentState_AGENT_STATE_NEEDS_INPUT,
		v1.AgentState_AGENT_STATE_IDLE:
	default:
		return false, nil
	}
	report, err := lastReport(r, a)
	if err != nil || report != nil {
		return false, err
	}
	if a.State != v1.AgentState_AGENT_STATE_IDLE {
		return true, nil
	}
	unread, err := r.Inbox(a.Id, false)
	if err != nil {
		return false, err
	}
	for _, m := range unread {
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER {
			return true, nil
		}
	}
	return false, nil
}

// additionNote is what a planner hears when a `Direct task` reaches it mid-plan: the task is more work for its plan.
const additionNote = "The task you were just sent says \"Direct task (no plan)\", but you are still planning: it is an addition to " +
	"your task, not a decision to skip the plan. Cover it in your plan and send the plan; editing stays off until your " +
	"parent approves it."

// mayMove is who may approve, switch or resume an agent: a device, or the agent's parent.
func mayMove(actor *v1.Actor, a *v1.Agent, what string) error {
	if IsAgent(actor.Class) && a.ParentId != actor.Id {
		return denied("only %s's parent or a device may %s it", a.Name, what)
	}
	return nil
}

// ── the actions ──

// ApproveAgent approves an agent's plan: the answer is stored, the agent executes, and it is reopened on its
// execution tier without the planning deny list.
func (d *Daemon) ApproveAgent(ctx context.Context, req *connect.Request[v1.ApproveAgentRequest]) (*connect.Response[v1.ApproveAgentResponse], error) {
	m := req.Msg
	if m.KeepModel && (m.Tier != "" || m.Model != "") {
		return nil, invalid("keep_model executes on the planning model: give no tier or model with it")
	}
	if m.Effort != "" && !validEffort(m.Effort) {
		return nil, invalid("effort %q: one of %s", m.Effort, strings.Join(settings.Efforts, ", "))
	}
	if err := checkReviewBy(m.Reviews); err != nil {
		return nil, err
	}
	out := &v1.ApproveAgentResponse{}
	actor := auth.ActorFrom(ctx)
	{
		r := d.store.Read(ctx)
		if a, err := d.FindAgent(r, actor, m.Agent); err == nil {
			d.warmUsage(ctx, r, a.Kind, agyResolveKey{model: m.Model, effort: m.Effort})
			d.warmOpenCodeModels(ctx, a.Kind)
		}
	}
	d.learnSession(ctx, actor, m.Agent, func(a *v1.Agent) bool { return mayMove(actor, a, "approve") == nil && !stopped(a) })
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceApproveAgentProcedure, out, func(tx *store.Tx) error {
		a, err := d.FindAgent(tx.Reader, tx.Actor, m.Agent)
		if err != nil {
			return err
		}
		if err := mayMove(tx.Actor, a, "approve"); err != nil {
			return err
		}
		if stopped(a) {
			// Approving must not undo a stop: resume brings it back planning, then approve it.
			return failedPrecondition("%s has stopped: resume it, then approve its plan", a.Name)
		}
		report, err := lastReport(tx.Reader, a)
		if err != nil {
			return err
		}
		// A planner with no plan yet (none since its last task or answer) can still be approved: that is the
		// parent skipping its plan, and the answer says so. An agent with no plan phase has nothing to skip.
		skipped := report == nil && a.Phase == v1.AgentPhase_AGENT_PHASE_PLANNING
		switch {
		case skipped:
		case report == nil:
			return failedPrecondition("%s has sent no plan since its last task or answer: nothing to approve", a.Name)
		case report.Type != v1.MessageType_MESSAGE_TYPE_PLAN:
			return failedPrecondition("%s's newest report since its last task or answer is a %s, not a plan",
				a.Name, strings.ToLower(strings.TrimPrefix(report.Type.String(), "MESSAGE_TYPE_")))
		}
		model, effort, _ := target(a)
		executes := a.Phase == v1.AgentPhase_AGENT_PHASE_PLANNING
		execTier := a.ExecTier
		switch {
		case !executes && (m.Tier != "" || m.Model != "" || m.Effort != "" || m.KeepModel):
			return failedPrecondition("%s has no plan phase (it started on a named model or effort, which covers the "+
				"whole task): approve it without a tier, model or effort, or switch it", a.Name)
		case !executes:
		case m.KeepModel:
			// Executes on what it planned on; only the deny list goes, which still takes a reopen.
			if m.Effort != "" {
				effort = m.Effort
			}
			if err := d.checkKept(tx.Reader, a, model); err != nil {
				return err
			}
			execTier = a.Tier
		default:
			tier := m.Tier
			if tier == "" && m.Model == "" {
				tier = a.ExecTier
			}
			if tier == "" && m.Model == "" {
				return invalid("name %s's execution tier (read its plan first), or a model", a.Name)
			}
			// A tier picked with no effort executes at its own execution effort (Opus after a plan at medium).
			named := m.Effort
			if named == "" && m.Model == "" {
				if named, err = d.execEffort(tx.Reader, a.Kind, tier); err != nil {
					return err
				}
			}
			if model, effort, err = d.pickFor(tx.Reader, a, tier, m.Model, named, a.Name+"'s execution"); err != nil {
				return err
			}
			execTier = tier
			if m.Model != "" {
				execTier = "" // a named model has no chain to walk
			}
		}
		deny := adapter.ReadOnly(a.Role) // the planning reason goes; a read-only role keeps its list
		cm, ce, cd := target(a)
		reopen := model != cm || effort != ce || deny != cd
		if reopen && a.SessionId == "" {
			return failedPrecondition("%s's conversation id is not known yet, so it can't be reopened on its "+
				"execution model: approve it again once it has started", a.Name)
		}
		if err := d.reserveCheck(tx.Reader, a.RunId, a.Kind, model, effort, "approving "+a.Name, isReviewer(a)); err != nil {
			return err
		}
		body := "approved"
		if skipped {
			body += ": you had not sent a plan, so your plan phase is skipped. Make the change, verify it, open the PR and report `done`."
		}
		if extra := strings.TrimSpace(m.Body); extra != "" {
			body += "\n\n" + extra
		}
		if out.Message, err = storeMessage(tx, a, v1.MessageType_MESSAGE_TYPE_ANSWER, body); err != nil {
			return err
		}
		if executes {
			a.ExecTier = execTier
			if err := setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_EXECUTING, "plan approved"); err != nil {
				return err
			}
		}
		if reopen {
			if out.Reopen, err = requestReopen(tx, a, model, effort, deny, "plan approved", m.Why); err != nil {
				return err
			}
			if err := d.requestApproveCompaction(tx, a, model, skipped); err != nil {
				return err
			}
		}
		if m.Reviews != "" {
			a.ReviewBy = m.Reviews
		}
		out.Agent = a
		return tx.PutAgent(a)
	})
	if err != nil {
		return nil, err
	}
	d.deliverSoon(out.Agent.Id)
	out.Agent = d.view(out.Agent)
	return connect.NewResponse(out), nil
}

// SwitchAgent moves an agent to another tier, model or effort, or to the next model of its chain, in its own
// conversation, and tells it to carry on.
func (d *Daemon) SwitchAgent(ctx context.Context, req *connect.Request[v1.SwitchAgentRequest]) (*connect.Response[v1.SwitchAgentResponse], error) {
	m := req.Msg
	picks := 0
	for _, set := range []bool{m.Tier != "", m.Model != "", m.Next} {
		if set {
			picks++
		}
	}
	if picks > 1 {
		return nil, invalid("give one of a tier, a model or next")
	}
	if picks == 0 && m.Effort == "" {
		return nil, invalid("give a tier, a model, an effort, or next (the next model of its chain)")
	}
	if m.Effort != "" && !validEffort(m.Effort) {
		return nil, invalid("effort %q: one of %s", m.Effort, strings.Join(settings.Efforts, ", "))
	}
	out := &v1.SwitchAgentResponse{}
	actor := auth.ActorFrom(ctx)
	{
		r := d.store.Read(ctx)
		if a, err := d.FindAgent(r, actor, m.Agent); err == nil {
			d.warmUsage(ctx, r, a.Kind, agyResolveKey{model: m.Model, effort: m.Effort})
			d.warmOpenCodeModels(ctx, a.Kind)
		}
	}
	d.learnSession(ctx, actor, m.Agent, func(a *v1.Agent) bool { return mayMove(actor, a, "switch") == nil && !stopped(a) })
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceSwitchAgentProcedure, out, func(tx *store.Tx) error {
		a, err := d.FindAgent(tx.Reader, tx.Actor, m.Agent)
		if err != nil {
			return err
		}
		if err := mayMove(tx.Actor, a, "switch"); err != nil {
			return err
		}
		if stopped(a) {
			return failedPrecondition("%s has stopped: resume it (with a tier or model) instead", a.Name)
		}
		if a.SessionId == "" {
			return failedPrecondition("%s's conversation id is not known yet, so it can't be reopened", a.Name)
		}
		was, wasEffort, deny := target(a)
		model, effort := was, wasEffort
		executing := a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING
		phaseTier := a.Tier
		if executing {
			phaseTier = a.ExecTier
		}
		switch {
		case m.Tier != "":
			if model, effort, err = d.pickFor(tx.Reader, a, m.Tier, "", m.Effort, a.Name); err != nil {
				return err
			}
			if executing {
				a.ExecTier = m.Tier
			} else {
				a.Tier = m.Tier
			}
		case m.Model != "":
			model = m.Model
			// A repo set against free models (freemodels.go) is moved onto none, named or by the `free` family.
			if off := d.freeOffFor(tx.Reader, a.Kind, a.Repo); off != "" {
				if err := d.checkNotFree(a.Repo, off, model); err != nil {
					return err
				}
			}
			if a.Kind == opencode.Kind && model == opencode.FreeFamily {
				if m.Effort != "" {
					effort = m.Effort
				} else if effort == "" {
					effort = namedEffort
				}
				expanded := d.expandChain(a.Kind, []string{model})
				model = d.firstFree(tx.Reader, a.Kind, effort, expanded, isReviewer(a))
				if model == "" {
					return failedPrecondition("%s", d.openCodeNoFreeError())
				}
			} else if m.Effort != "" {
				effort = m.Effort
			} else if own := d.rt.adapters.ModelEffort(ctx, a.Kind, model); own != "" {
				effort = own // an exact id names its effort: the old one would move it to another model
			}
			// A named model has no chain: `next` from it would go back to the tier's first model.
			if executing {
				a.ExecTier = ""
			} else {
				a.Tier = ""
			}
		case m.Next:
			if phaseTier == "" {
				return failedPrecondition("%s runs on a named model, with no chain to walk: switch it to a tier or a model", a.Name)
			}
			chain, _, err := d.tierChain(tx.Reader, a.Kind, phaseTier)
			if err != nil {
				return err
			}
			chain = d.expandChain(a.Kind, chain)
			if len(chain) == 0 {
				return failedPrecondition("%s", d.openCodeNoFreeError())
			}
			// A repo set against free models (freemodels.go) walks only the paid part of the chain.
			if off := d.freeOffFor(tx.Reader, a.Kind, a.Repo); off != "" {
				if chain, err = d.dropFree(a.Repo, off, chain); err != nil {
					return err
				}
			}
			if m.Effort != "" {
				effort = m.Effort
			}
			i := index(chain, was)
			switch {
			case i < 0:
				if a.Kind == opencode.Kind {
					// OpenCode's expanded `free` chain can change while running: take the first unheld model,
					// or refuse with the soonest reset when every model in the chain is held.
					if model = d.nextFree(tx.Reader, a.Kind, effort, chain, -1, isReviewer(a)); model == "" {
						if soonest := d.soonestHold(tx.Reader, a.Kind, chain); !soonest.IsZero() {
							return failedPrecondition("every model of the %s %s chain (%s) is held; soonest reset at %s",
								a.Kind, phaseTier, strings.Join(chain, ", "), soonest.UTC().Format(time.RFC3339))
						}
						return failedPrecondition("no free model in the %s %s chain (%s) to move %s to",
							a.Kind, phaseTier, strings.Join(chain, ", "), a.Name)
					}
				} else {
					model = chain[0] // the chain changed since it started: its first model
				}
			case i+1 < len(chain):
				// Never back to one before: they were tried. The first with quota left, else the next one
				// (OpenCode refuses instead of reopening onto a held model).
				if model = d.nextFree(tx.Reader, a.Kind, effort, chain, i, isReviewer(a)); model == "" {
					if a.Kind == opencode.Kind {
						if soonest := d.soonestHold(tx.Reader, a.Kind, chain[i+1:]); !soonest.IsZero() {
							return failedPrecondition("every remaining model of the %s %s chain (%s) is held; soonest reset at %s",
								a.Kind, phaseTier, strings.Join(chain[i+1:], ", "), soonest.UTC().Format(time.RFC3339))
						}
					}
					model = chain[i+1]
				}
			default:
				return failedPrecondition("%s is the last model of the %s %s chain (%s): nothing to move %s to",
					was, a.Kind, phaseTier, strings.Join(chain, ", "), a.Name)
			}
		default:
			effort = m.Effort
			// The same model, so a free one its repo now refuses stays refused.
			if err := d.checkKept(tx.Reader, a, model); err != nil {
				return err
			}
		}
		if model == was && effort == wasEffort {
			return failedPrecondition("%s already runs on %s at %s effort", a.Name, model, effort)
		}
		why, body := "moved", fmt.Sprintf("You are now on %s at %s effort, in the same conversation (your parent "+
			"moved you). Continue exactly where you stopped.", model, effort)
		if m.Next {
			why, body = "quota", fmt.Sprintf("Your previous model (%s) hit its quota; you are now on %s, in the same "+
				"conversation. Continue exactly where you stopped.", was, model)
		}
		// An info, not a task: it doesn't end a plan phase's wait for approval (a planner switched before it is
		// approved is still approved on its plan).
		if out.Message, err = storeMessage(tx, a, v1.MessageType_MESSAGE_TYPE_INFO, body); err != nil {
			return err
		}
		if out.Reopen, err = requestReopen(tx, a, model, effort, deny, why, m.Why); err != nil {
			return err
		}
		out.Agent = a
		return tx.PutAgent(a)
	})
	if err != nil {
		return nil, err
	}
	d.deliverSoon(out.Agent.Id)
	out.Agent = d.view(out.Agent)
	return connect.NewResponse(out), nil
}

func index(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// ResumeAgent reopens a stopped agent's conversation: with a task (planned under the deny list on a tier, or
// direct), or as it was.
func (d *Daemon) hasProcessOnSessionOrWorktree(r store.Reader, a *v1.Agent) error {
	all, err := r.AllAgents()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.Id == a.Id || !hasProcess(other) {
			continue
		}
		if a.SessionId != "" && other.SessionId == a.SessionId {
			return failedPrecondition("conversation %s is already running in %s (run %s): stop it before resuming",
				a.SessionId, other.Name, other.RunId)
		}
		// Askers of one peer share its checkout (their conversations differ).
		if a.Worktree != "" && other.Worktree == a.Worktree && !(a.Peer && other.Peer && !(a.Kind == "agy" && other.Kind == "agy")) { // agy writes its hooks per checkout: one agent id
			return failedPrecondition("worktree %s is already in use by %s (run %s): stop it before resuming",
				a.Worktree, other.Name, other.RunId)
		}
	}
	return nil
}

func uniqueAgentName(r store.Reader, runID, base string) (string, error) {
	same, err := r.AgentsNamed(runID, base)
	if err != nil {
		return "", err
	}
	if len(same) == 0 {
		return base, nil
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		same, err := r.AgentsNamed(runID, cand)
		if err != nil {
			return "", err
		}
		if len(same) == 0 {
			return cand, nil
		}
	}
}

func canResume(r store.Reader, actor *v1.Actor, a *v1.Agent, targetRunID string) error {
	if targetRunID == "" || a.RunId == targetRunID {
		return mayMove(actor, a, "resume")
	}
	if IsAgent(actor.Class) && (actor.Class != v1.ActorClass_ACTOR_CLASS_ROOT || actor.RunId == "") {
		return denied("only a root or device may resume an agent across runs")
	}
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		return denied("only non-root agents may be resumed across runs")
	}
	src, err := r.GetRun(a.RunId)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("no run %q", a.RunId)
	}
	if err != nil {
		return err
	}
	if src.Status == v1.RunStatus_RUN_STATUS_OPEN {
		return denied("cannot resume %s from open run %s into %s: cross-run resume is only from closed runs",
			a.Name, a.RunId, targetRunID)
	}
	return nil
}

func (d *Daemon) previewReviveTarget(r store.Reader, a *v1.Agent, spec reviveSpec) (string, string, error) {
	model, effort, _ := target(a)
	kept := func() (string, string, error) { return model, effort, d.checkKept(r, a, model) }
	switch {
	case spec.plan:
		m, e, _, err := d.replanTarget(r, a, spec.tier, spec.model, spec.effort)
		return m, e, err
	case spec.task:
		if spec.tier != "" || spec.model != "" {
			return d.pickFor(r, a, spec.tier, spec.model, spec.effort, a.Name)
		}
		if spec.effort != "" {
			effort = spec.effort
		}
		return kept()
	case spec.tier != "" || spec.model != "":
		return d.pickFor(r, a, spec.tier, spec.model, spec.effort, a.Name)
	case spec.effort != "":
		effort = spec.effort
	}
	return kept()
}

// ResumeAgent brings an exited or retired agent back, in its own conversation. A task gives it new work (under
// the planning deny list when `plan`, else direct); a tier, model or effort moves it without changing phase.
// A root may resume a stopped non-root worker from an earlier run into its own open run.
func (d *Daemon) ResumeAgent(ctx context.Context, req *connect.Request[v1.ResumeAgentRequest]) (*connect.Response[v1.ResumeAgentResponse], error) {
	m := req.Msg
	task := strings.TrimSpace(m.Task)
	if m.Plan && task == "" {
		return nil, invalid("plan is for a task: give the task it plans")
	}
	if task == "" && strings.TrimSpace(m.About) != "" {
		return nil, invalid("--about goes with a task (to change an agent's line alone: `mesh about <agent> --text …`)")
	}
	if m.Effort != "" && !validEffort(m.Effort) {
		return nil, invalid("effort %q: one of %s", m.Effort, strings.Join(settings.Efforts, ", "))
	}
	reqMCP, clearMCP, err := parseMCPArg(m.Mcp)
	if err != nil {
		return nil, err
	}
	hasMCPFlag := len(reqMCP) > 0 || clearMCP // an empty or comma-only value is no flag
	if m.Compact != "" && m.Compact != "auto" && m.Compact != "yes" && m.Compact != "no" {
		return nil, invalid("compact %q: auto, yes or no", m.Compact)
	}
	actor := auth.ActorFrom(ctx)
	if IsAgent(actor.Class) {
		if actor.RunId == "" {
			return nil, denied("caller has no run")
		}
		if m.RunId != "" && m.RunId != actor.RunId {
			return nil, denied("a root resumes agents only into its own run")
		}
	}
	r0 := d.store.Read(ctx)
	if mid := m.Meta.GetMutationId(); mid != "" {
		if _, resp, err := r0.Mutation(actor.Id, mid); err == nil {
			out := &v1.ResumeAgentResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	a0, err := d.findAgentCrossRun(r0, actor, m.Agent)
	if err != nil {
		return nil, err
	}
	targetRunID := m.RunId
	if IsAgent(actor.Class) {
		targetRunID = actor.RunId
	} else if targetRunID == "" {
		targetRunID = a0.RunId
	}
	if err := canResume(r0, actor, a0, targetRunID); err != nil {
		return nil, err
	}
	if !stopped(a0) {
		return nil, failedPrecondition("%s is running: resume is for a stopped agent (switch moves a running one)", a0.Name)
	}
	if hasMCPFlag {
		if !clearMCP {
			if err := d.checkMCP(ctx, a0.Kind, a0.Repo, reqMCP); err != nil {
				return nil, err
			}
			if err := d.checkMCPLease(r0, reqMCP, a0.Id, auth.ActorFrom(ctx)); err != nil {
				return nil, err
			}
		} else if a0.Kind != claude.Kind {
			return nil, invalid("mcp is for Claude agents: %s has no lean session to clear servers on", a0.Kind)
		}
	}
	if task != "" {
		d.warmUsage(ctx, r0, a0.Kind, agyResolveKey{model: a0.Model, effort: a0.Effort}, agyResolveKey{model: m.Model, effort: m.Effort})
	}
	d.warmOpenCodeModels(ctx, a0.Kind)
	d.learnSession(ctx, actor, a0.Id, func(a *v1.Agent) bool {
		return canResume(d.store.Read(ctx), actor, a, targetRunID) == nil && stopped(a)
	})
	r1 := d.store.Read(ctx)
	a0, err = d.findAgentCrossRun(r1, actor, a0.Id)
	if err != nil {
		return nil, err
	}
	if err := canResume(r1, actor, a0, targetRunID); err != nil {
		return nil, err
	}
	if !stopped(a0) {
		return nil, failedPrecondition("%s is running: resume is for a stopped agent (switch moves a running one)", a0.Name)
	}
	if a0.SessionId == "" {
		return nil, failedPrecondition("%s never reported a conversation id, so there is nothing to resume", a0.Name)
	}
	run, err := r1.GetRun(targetRunID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no run %q", targetRunID)
	}
	if err != nil {
		return nil, err
	}
	if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, failedPrecondition("run %s is closed", targetRunID)
	}
	if err := d.hasProcessOnSessionOrWorktree(r1, a0); err != nil {
		return nil, err
	}
	spec := reviveSpec{task: task != "", plan: m.Plan, tier: m.Tier, model: m.Model, effort: m.Effort, why: m.Why,
		compact: m.Compact, focus: strings.TrimSpace(m.Focus), taskText: task}
	// An asker or reviewer that this task makes a worker (takeChange, or the cross-run follow-up below) is prepared as
	// one: its worktree recreated if StopRun removed it, moved off a finished PR's branch.
	probe := a0
	if targetRunID != a0.RunId {
		probe = proto.Clone(a0).(*v1.Agent) // the follow-up's parent is whoever resumes it
		probe.ParentId = ""
		if IsAgent(actor.Class) {
			probe.ParentId = actor.Id
		}
	}
	if task != "" && a0.Role != v1.AgentRole_AGENT_ROLE_ROOT {
		pm, pe, err := d.previewReviveTarget(r1, a0, spec)
		if err != nil {
			return nil, err
		}
		// Held as the work it will be: a change task makes a reviewer a worker, so past the reviews-only line it is refused.
		if err := d.reserveCheck(r1, targetRunID, a0.Kind, pm, pe, "resuming "+a0.Name, d.reviewWork(r1, probe, task)); err != nil {
			return nil, err
		}
	}
	if err := d.capGuard(r1, "resuming "+a0.Name, a0.Id); err != nil {
		return nil, err
	}
	if err := d.checkMemoryGuard(r1, "resuming "+a0.Name); err != nil {
		return nil, err
	}

	out := &v1.ResumeAgentResponse{}
	err = func() error {
		if a0.Worktree != "" {
			mu := d.worktreeLock(a0.Worktree)
			mu.Lock()
			defer mu.Unlock()
		}
		asWorker := a0
		if task != "" && d.becomesWorker(r1, probe, task) {
			asWorker = proto.Clone(a0).(*v1.Agent)
			asWorker.Role = v1.AgentRole_AGENT_ROLE_WORKER
		}
		prep, perr := d.prepareWorktreeForTask(ctx, asWorker, targetRunID, task != "")
		if perr != nil {
			return failedPrecondition("preparing worktree for %s: %v", a0.Name, perr)
		}
		_, werr := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceResumeAgentProcedure, out, func(tx *store.Tx) error {
			a, err := d.findAgentCrossRun(tx.Reader, tx.Actor, a0.Id)
			if err != nil {
				return err
			}
			if err := canResume(tx.Reader, tx.Actor, a, targetRunID); err != nil {
				return err
			}
			if !stopped(a) {
				return failedPrecondition("%s is running: resume is for a stopped agent (switch moves a running one)", a.Name)
			}
			if err := d.hasProcessOnSessionOrWorktree(tx.Reader, a); err != nil {
				return err
			}
			if tr, err := tx.GetRun(targetRunID); errors.Is(err, store.ErrNotFound) {
				return notFound("no run %q", targetRunID)
			} else if err != nil {
				return err
			} else if tr.Status != v1.RunStatus_RUN_STATUS_OPEN {
				return failedPrecondition("run %s is closed", targetRunID)
			}
			taskBody := withOpenCodeBodyNote(a.Kind, task)
			if taskBody != "" && prep.taskNote != "" {
				taskBody += "\n\n" + prep.taskNote
			}
			if prep.note != "" {
				out.Notes = []string{prep.note}
			}
			out.LeftPr = prep.leftPR

			if targetRunID == a.RunId {
				// A change task makes an asker or reviewer the worker for it, before it is revived (takeChange).
				if task != "" {
					if err := d.takeChange(tx, a, task, m.Plan); err != nil {
						return err
					}
				}
				if hasMCPFlag {
					if clearMCP {
						a.Mcp, a.McpAll = nil, false
					} else {
						if err := d.checkMCPLease(tx.Reader, reqMCP, a.Id, tx.Actor); err != nil {
							return err
						}
						a.Mcp, a.McpAll = reqMCP, false
					}
				} else if dropNote, dropped, err := d.dropHeldExclusiveMCP(tx.Reader, a); err != nil {
					return err
				} else if dropped {
					out.Notes = append(out.Notes, dropNote)
				}
				if prep.newBranch != "" {
					a.Branch = prep.newBranch
					a.BaseCommit = prep.newBaseCommit
				}
				if taskBody != "" {
					if out.Message, err = storeMessage(tx, a, v1.MessageType_MESSAGE_TYPE_TASK, taskBody); err != nil {
						return err
					}
					if out.AboutNote, err = taskAboutOn(tx, a, m.About, taskBody); err != nil {
						return err
					}
				}
				if out.Reopen, err = d.revive(tx, a, spec); err != nil {
					return err
				}
				out.Agent = a
				return tx.PutAgent(a)
			}

			// Cross-run resume: keep the earlier run's agent intact and spawn a follow-up agent in targetRunID
			// pointing at the same conversation session and worktree.
			name, err := uniqueAgentName(tx.Reader, targetRunID, a.Name)
			if err != nil {
				return err
			}
			parentID := ""
			if IsAgent(tx.Actor.Class) {
				parentID = tx.Actor.Id
			}
			branch, baseCommit := a.Branch, a.BaseCommit
			if prep.newBranch != "" {
				branch = prep.newBranch
				baseCommit = prep.newBaseCommit
			}
			now := tsOf(tx.Now())
			created := &v1.Agent{
				Id:                 ids.New("a", 10),
				RunId:              targetRunID,
				ParentId:           parentID,
				Name:               name,
				Kind:               a.Kind,
				Role:               a.Role,
				Model:              a.Model,
				Effort:             a.Effort,
				Tier:               a.Tier,
				ExecTier:           a.ExecTier,
				Phase:              a.Phase,
				Peer:               a.Peer, // a peer stays a peer in the new run: read-only, its worktree kept
				Pinned:             a.Pinned,
				About:              a.About, // the job it was started for goes on, unless this task says otherwise
				AboutSource:        a.AboutSource,
				Mcp:                slices.Clone(a.Mcp), // the servers its spawn named: the new launch loads them too
				AddedTools:         slices.Clone(a.AddedTools),
				ToolsAdded:         a.ToolsAdded,
				McpAll:             a.McpAll,
				DenyEdits:          a.DenyEdits,
				State:              a.State,
				Repo:               a.Repo,
				Worktree:           a.Worktree,
				Branch:             branch,
				BaseCommit:         baseCommit,
				SessionId:          a.SessionId,
				CreatedAt:          now,
				LastActivity:       now,
				ResumedFromRunId:   a.RunId,
				ResumedFromAgentId: a.Id,
			}
			// The follow-up of an asker or reviewer given a change task is a worker from its start (reviveAs then
			// lifts or keeps its list as the task says).
			if task != "" && d.becomesWorker(tx.Reader, created, task) {
				created.Role = v1.AgentRole_AGENT_ROLE_WORKER
			}
			if hasMCPFlag {
				if clearMCP {
					created.Mcp, created.McpAll = nil, false
				} else {
					if err := d.checkMCPLease(tx.Reader, reqMCP, a.Id, tx.Actor); err != nil {
						return err
					}
					created.Mcp, created.McpAll = reqMCP, false
				}
			} else if dropNote, dropped, err := d.dropHeldExclusiveMCP(tx.Reader, created); err != nil {
				return err
			} else if dropped {
				out.Notes = append(out.Notes, dropNote)
			}
			out.Agent = created
			if err := d.mayLaunch(tx.Reader, "resuming "+created.Name, created.Id); err != nil {
				return err
			}
			crossSpec := spec
			crossSpec.crossRun = true // it comes back from another run
			rv, err := d.reviveAs(tx, created, crossSpec)
			if err != nil {
				d.rt.guard.release(created.Id)
				return err
			}
			out.Reopen = rv
			if err := tx.PutAgent(created); err != nil {
				d.rt.guard.release(created.Id)
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{
				RunId:   created.RunId,
				AgentId: created.Id,
				Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: created}},
			}); err != nil {
				d.rt.guard.release(created.Id)
				return err
			}
			if taskBody != "" {
				if out.Message, err = storeMessage(tx, created, v1.MessageType_MESSAGE_TYPE_TASK, taskBody); err != nil {
					d.rt.guard.release(created.Id)
					return err
				}
				if out.AboutNote, err = taskAboutOn(tx, created, m.About, taskBody); err != nil {
					d.rt.guard.release(created.Id)
					return err
				}
			}
			return nil
		})
		if werr != nil {
			prep.rollback(ctx, a0.Repo, a0.Worktree)
		}
		return werr
	}()
	if err != nil {
		if out.Reopen != nil {
			d.rt.guard.release(out.Agent.GetId()) // the write failed after the guard passed: nothing was requested
		}
		return nil, err
	}
	d.deliverSoon(out.Agent.Id)
	out.Agent = d.view(out.Agent)
	return connect.NewResponse(out), nil
}

// atPrompt reports whether an agent's CLI is at its input box between turns: idle, or stopped by a usage limit or
// an API error (agy's Stop with terminationReason ERROR). All three take a reopen and an idle retire; only a turn
// in progress (or a prompt it waits on) holds them.
func atPrompt(s v1.AgentState) bool {
	return s == v1.AgentState_AGENT_STATE_IDLE || s == v1.AgentState_AGENT_STATE_LIMITED ||
		s == v1.AgentState_AGENT_STATE_ERROR
}

// takesDoorbell reports whether a doorbell may be typed into an agent: at its prompt, but not at a usage limit,
// where the turn it starts would fail on the same quota and use the doorbell up. Its messages wait for the switch
// (or the `continue` at the reset) that moves it off the limit.
func takesDoorbell(s v1.AgentState) bool {
	return atPrompt(s) && s != v1.AgentState_AGENT_STATE_LIMITED
}

// stopped reports whether an agent has no process and may be resumed: it exited, or was retired.
func stopped(a *v1.Agent) bool {
	return a.State == v1.AgentState_AGENT_STATE_EXITED || a.State == v1.AgentState_AGENT_STATE_RETIRED
}

// reviveSpec says how a stopped agent comes back. Nothing set brings it back as it was; a task decides its phase
// and deny list (plan: it plans, under the list, on the tier given or the one it had; else a direct task, which
// ends a plan phase), as `mesh resume --task` does; a tier, model or effort alone moves it.
type reviveSpec struct {
	task                bool
	plan                bool
	tier, model, effort string
	why                 string
	// compact is `--compact`: "" or "auto" (when it pays), "yes" or "no"; focus overrides what it keeps. taskText
	// is the task the resume is for, which the default focus names; crossRun says it comes back from another run.
	compact, focus, taskText string
	crossRun                 bool
	// capShared are the agents the launch shares its place in the host's cap with (a review round's members).
	capShared []string
}

// revive requests the reopen that brings a stopped agent back, applying spec to the agent (the caller stores
// it). The launch is refused (ResourceExhausted) when it would pass the host's agent cap or its memory floor.
func (d *Daemon) revive(tx *store.Tx, a *v1.Agent, spec reviveSpec) (*v1.Reopen, error) {
	if isAdopted(a) {
		return nil, failedPrecondition("%s was your own Claude Code session (process %d): it is not the daemon's to resume; resume it yourself "+
			"(`claude --resume %s`) and adopt it again", a.Name, a.AdoptedPid, a.SessionId)
	}
	if a.SessionId == "" {
		return nil, failedPrecondition("%s never reported a conversation id, so there is nothing to resume", a.Name)
	}
	if run, err := tx.GetRun(a.RunId); err == nil && run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, failedPrecondition("run %s is closed", a.RunId)
	}
	if err := d.hasProcessOnSessionOrWorktree(tx.Reader, a); err != nil {
		return nil, err
	}
	if err := d.mayLaunch(tx.Reader, "resuming "+a.Name, a.Id, spec.capShared...); err != nil {
		return nil, err
	}
	rv, err := d.reviveAs(tx, a, spec)
	if err != nil {
		d.rt.guard.release(a.Id) // the guard passed but nothing was requested
		return rv, err
	}
	d.bootRelease(a.Id) // a resume, by hand or by a message, ends the hold a machine restart put on its exit alert (restart.go)
	return rv, err
}

// reviveAs is revive past its guards.
func (d *Daemon) reviveAs(tx *store.Tx, a *v1.Agent, spec reviveSpec) (*v1.Reopen, error) {
	model, effort, deny := target(a)
	var err error
	// A resume that names no tier or model keeps the model the agent had (a task, or an effort, on it): checked first,
	// before the phase changes below write anything, so a refusal leaves the agent exactly as it was. A tier or a
	// model named is checked where it is picked (pickFor).
	if !spec.plan && spec.tier == "" && spec.model == "" {
		if err := d.checkKept(tx.Reader, a, model); err != nil {
			return nil, err
		}
	}
	switch {
	case spec.plan:
		// A task it plans: on the tier or model given, else the tier it planned on (replanTarget).
		back := backTier(a)
		var tier string
		if model, effort, tier, err = d.replanTarget(tx.Reader, a, spec.tier, spec.model, spec.effort); err != nil {
			return nil, err
		}
		a.Tier, a.ExecTier, a.Pinned, deny = tier, back, false, true
		if a.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			if err := d.reserveCheck(tx.Reader, a.RunId, a.Kind, model, effort, "resuming "+a.Name, isReviewer(a)); err != nil {
				return nil, err
			}
		}
		if err := setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_PLANNING, "planned task"); err != nil {
			return nil, err
		}
	case spec.task:
		// The phase ends: the chain `next` walks is the one of the tier it runs on (the execution tier, if it
		// was executing), or none when the model is named.
		endExecution(a)
		if spec.tier != "" || spec.model != "" {
			if model, effort, err = d.pickFor(tx.Reader, a, spec.tier, spec.model, spec.effort, a.Name); err != nil {
				return nil, err
			}
			a.Tier = spec.tier
			if spec.model != "" {
				a.Tier = ""
			}
		} else if spec.effort != "" {
			effort = spec.effort
		}
		a.Pinned, deny = spec.model != "" || spec.effort != "", adapter.ReadOnly(a.Role)
		if a.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			if err := d.reserveCheck(tx.Reader, a.RunId, a.Kind, model, effort, "resuming "+a.Name, isReviewer(a)); err != nil {
				return nil, err
			}
		}
		if err := setPhase(tx, a, v1.AgentPhase_AGENT_PHASE_UNSPECIFIED, "direct task"); err != nil {
			return nil, err
		}
	case spec.tier != "" || spec.model != "":
		if model, effort, err = d.pickFor(tx.Reader, a, spec.tier, spec.model, spec.effort, a.Name); err != nil {
			return nil, err
		}
		tier := spec.tier
		if spec.model != "" {
			tier = "" // a named model has no chain to walk
		}
		if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING {
			a.ExecTier = tier
		} else {
			a.Tier = tier
		}
	case spec.effort != "":
		effort = spec.effort
	}
	rv, err := requestReopen(tx, a, model, effort, deny, "resumed", spec.why)
	if err != nil {
		return nil, err
	}
	return rv, d.requestResumeCompaction(tx, a, spec) // a resume re-reads the conversation: compact it first when it pays
}

// ── carrying a reopen out ──

// reopen carries out an agent's pending reopen, under its agent lock: once it is idle (or its process is gone),
// and no user is typing into it, the old process is stopped, a new token minted, and the conversation
// relaunched with --resume in a new terminal labelled with the reopen's id. The agent keeps its id, name,
// worktree and session.
func (d *Daemon) reopen(ctx context.Context, a *v1.Agent) error {
	r := a.PendingReopen
	// A retired agent is marked before its process is stopped, so one that still has a terminal may have a live
	// process (its retire failed, or its caller gave up): it is stopped here, whatever it is doing.
	leftover := a.State == v1.AgentState_AGENT_STATE_RETIRED && a.TerminalId != ""
	gone := a.TerminalId == "" || a.State == v1.AgentState_AGENT_STATE_EXITED || (stopped(a) && !leftover)
	if !gone && !leftover && !atPrompt(a.State) {
		return nil // at its next IDLE (or once stopped on a limit or an error): never cut a turn short
	}
	if d.rt.term == nil {
		return nil
	}
	if !gone {
		if !leftover {
			if typing, err := d.userTyping(ctx, a.TerminalId); err != nil || typing {
				return err
			}
		}
		if err := d.stopTerminal(ctx, a.TerminalId); err != nil {
			return fmt.Errorf("stopping %s for its reopen: %w", a.Name, err)
		}
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return d.dropReopen(ctx, a.Id, r, err.Error())
	}
	token := auth.NewToken()
	var cur *v1.Agent
	cancelledClosed := false
	err = d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		if cur, err = tx.GetAgent(a.Id); err != nil {
			return err
		}
		if !gone && cur.TerminalId == a.TerminalId {
			cur.TerminalId = "" // stopped above: no process to wait for any more
		}
		if cur.PendingReopen == nil || cur.PendingReopen.Id != r.Id {
			// Replaced by a newer request meanwhile: that one runs next, from here.
			err := tx.PutAgent(cur)
			cur = nil
			return err
		}
		if run, err := tx.GetRun(cur.RunId); err == nil && run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			// Nothing starts in a closed run (StopRun drops the requests it finds; this one slipped past).
			err := cancelReopen(tx, cur, "its run is closed")
			if err == nil {
				err = tx.PutAgent(cur)
			}
			cancelledClosed = err == nil
			cur = nil
			return err
		}
		// No terminal until the new one is recorded: a new process that dies at once is then found by its
		// labels, and its exit recorded.
		cur.TerminalId = ""
		if err := tx.RevokeTokens(cur.Id); err != nil {
			return err
		}
		if err := tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(cur.Role), SubjectID: cur.Id,
			Name: cur.Name, RunID: cur.RunId}); err != nil {
			return err
		}
		// STARTING before the launch: the new process's first hook (its resumed session, idle) can come before
		// its terminal is recorded, and must find it starting.
		before := cur.State
		st := reducer.Start(tx.Now())
		cur.State, cur.NeedsInput, cur.StateDetail, cur.ExitCode = st.State, 0, "", 0
		cur.StateBeforeCompact = v1.AgentState_AGENT_STATE_UNSPECIFIED
		if r.Fresh {
			cur.SessionId = ""
			cur.HadTurn = false // a new conversation has had no turn yet
		}
		cur.LastActivity = timestamppb.New(tx.Now())
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		if before == cur.State {
			return nil
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id, Payload: &v1.Event_AgentStateChanged{
			AgentStateChanged: &v1.AgentStateChanged{From: before, To: cur.State}}})
		return err
	})
	if err != nil || cur == nil {
		if cancelledClosed {
			d.rt.guard.release(a.Id)
		} else if err == nil {
			d.deliverSoon(a.Id)
		}
		return err
	}
	next := proto.Clone(cur).(*v1.Agent)
	next.Model, next.Effort, next.DenyEdits = r.Model, r.Effort, r.DenyEdits
	resume := cur.SessionId
	if r.Fresh {
		resume = "" // a root's reset: a new conversation; the new process records its own id
	}
	plan, err := d.planLaunch(ctx, ad, next, token, resume)
	if err != nil {
		return d.dropReopen(ctx, a.Id, r, "planning the launch: "+err.Error())
	}
	// Registered before the process exists: if it dies at once while a newer request has replaced this one,
	// recordExit marks it so finishReopen doesn't record a terminal that is already gone.
	d.rt.mu.Lock()
	d.rt.launching[r.Id] = false
	d.rt.mu.Unlock()
	defer func() {
		d.rt.mu.Lock()
		delete(d.rt.launching, r.Id)
		d.rt.mu.Unlock()
	}()
	d.rt.guard.refresh(a.Id, d.now())
	tid, err := d.startTerminal(ctx, next, token, plan, r.Id)
	if err != nil {
		return d.dropReopen(ctx, a.Id, r, "starting its terminal: "+err.Error())
	}
	if launchedHook != nil {
		launchedHook(a.Id, func() bool {
			d.rt.term.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: tid}))
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				d.rt.mu.Lock()
				died := d.rt.launching[r.Id]
				d.rt.mu.Unlock()
				cur, err := d.store.Read(ctx).GetAgent(a.Id)
				if died || (err == nil && cur.TerminalId == tid && cur.State == v1.AgentState_AGENT_STATE_EXITED) {
					return true // the exit was seen: marked for the replaced reopen, or recorded as the agent's
				}
			}
			return false
		})
	}
	return d.finishReopen(ctx, a.Id, r, tid, plan)
}

// finishReopen records a reopen's new terminal: the agent starts again on the reopen's model, effort and deny
// list. Also run at boot for a terminal a reopen started just before the daemon died.
func (d *Daemon) finishReopen(ctx context.Context, agentID string, r *v1.Reopen, tid string, plan *adapterv1.PlanLaunchResponse) error {
	stale, died := false, false
	var told []string // agents told their context is gone, to ring
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.PendingReopen == nil || a.PendingReopen.Id != r.Id {
			if a.TerminalId == tid && a.State == v1.AgentState_AGENT_STATE_EXITED {
				return nil // its process died at once, and that exit is recorded (a newer request may be pending)
			}
			stale = a.PendingReopen == nil // stopped meanwhile: the new process goes too
			if stale {
				return nil
			}
			// Replaced by a newer request, and the new process died at once: recordExit found no terminal to
			// match and left a mark. Recording it would leave the agent starting on a process that is gone,
			// and the newer reopen never run.
			d.rt.mu.Lock()
			died = d.rt.launching[r.Id]
			d.rt.mu.Unlock()
			if died {
				_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentReopenDropped{
					AgentReopenDropped: &v1.AgentReopenDropped{Reopen: r, Reason: "its process exited before it was recorded"}}})
				return err
			}
		} else {
			a.PendingReopen = nil
		}
		// The state is what the new process's hooks made it: it was set STARTING before the launch.
		a.TerminalId, a.Model, a.Effort, a.DenyEdits = tid, r.Model, r.Effort, r.DenyEdits
		a.Archived = false // its worktree was made again before the launch
		if plan != nil && plan.SessionId != "" {
			a.SessionId = plan.SessionId
		}
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentReopened{
			AgentReopened: &v1.AgentReopened{Reopen: r, TerminalId: tid, SessionId: a.SessionId}}})
		if err != nil {
			return err
		}
		if plan != nil && plan.ResumeMissing && a.HadTurn && !r.Fresh {
			told, err = d.tellContextLost(tx, a)
		}
		return err
	})
	if err != nil {
		// Without its terminal recorded the agent would sit starting for good: give the reopen up (its event
		// says why) before the process goes, so its exit isn't taken for the agent's own.
		// Not on ctx: a failed write may be a daemon shutting down, which the drop must outlive.
		if derr := d.dropReopen(context.WithoutCancel(ctx), agentID, r, "recording its terminal: "+err.Error()); derr != nil {
			d.log.Error("giving up a reopen", "agent", agentID, "err", derr)
		}
	}
	if err != nil || stale {
		// A terminal no agent records would run on unseen.
		d.rt.term.API.KillTerminal(context.Background(), connect.NewRequest(&termv1.KillTerminalRequest{Id: tid}))
	}
	if died {
		d.deliverSoon(agentID) // the newer reopen runs now
	}
	for _, id := range told {
		d.deliverSoon(id)
	}
	return err
}

// tellContextLost tells an agent whose conversation Claude Code no longer has (it deletes transcripts older than
// `cleanupPeriodDays`, 30 by default) that it starts with no memory, and its parent. Its adapter launched the same id
// fresh. A plain agent is the user's own chat: it is told nothing (its chat shows an empty conversation). Returns the
// agents to ring.
func (d *Daemon) tellContextLost(tx *store.Tx, a *v1.Agent) ([]string, error) {
	if a.Plain {
		return nil, nil
	}
	var ring []string
	text := "Your earlier conversation (session " + a.SessionId + ") was not found where Claude Code keeps it (it deletes transcripts " +
		"after 30 days, by default), so you start fresh with no memory of it. Run `mesh inbox` and re-read your task and the messages you have; " +
		"look at your worktree (`git log`, `git status`) for what is already done, and carry on from there."
	if _, err := tellTx(tx, a, text); err != nil {
		return nil, err
	}
	ring = append(ring, a.Id)
	if alert, err := d.alertParentTx(tx, a, "context", a.Name+" was resumed with no memory: its earlier Claude conversation was not found "+
		"(Claude Code deletes transcripts after 30 days, by default). It was told, and starts from its task and its worktree. "+
		"Check what it has before relying on it.", tx.Now()); err != nil {
		return nil, err
	} else if alert != nil {
		ring = append(ring, alert.ToId)
	}
	return ring, nil
}

// dropReopen gives up a reopen whose relaunch failed: the old process is already gone, so the agent is EXITED,
// with its token revoked; ResumeAgent tries again.
func (d *Daemon) dropReopen(ctx context.Context, agentID string, r *v1.Reopen, reason string) error {
	d.log.Error("reopen failed", "agent", agentID, "reason", reason)
	d.rt.guard.release(agentID)
	var alert *v1.Message
	cur, _ := d.store.Read(ctx).GetAgent(agentID)
	words := d.exitWords(ctx, cur) // read before the write that records the exit
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.PendingReopen == nil || a.PendingReopen.Id != r.Id {
			return nil
		}
		a.PendingReopen = nil
		lastSaid := a.StateDetail // the exit event below overwrites it
		if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentReopenDropped{
			AgentReopenDropped: &v1.AgentReopenDropped{Reopen: r, Reason: reason}}}); err != nil {
			return err
		}
		ev := &adapterv1.AgentEvent{Time: timestamppb.New(tx.Now()),
			Kind: &adapterv1.AgentEvent_ProcessExited{ProcessExited: &adapterv1.ProcessExited{ExitCode: -1}}}
		if err := d.applyEvents(tx, a, []*adapterv1.AgentEvent{ev}); err != nil {
			return err
		}
		if err := setExitDetail(tx, a, "reopen failed: "+reason); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentExited{
			AgentExited: &v1.AgentExited{ExitCode: -1, Reason: "reopen failed: " + reason}}}); err != nil {
			return err
		}
		if alert, err = d.alertExitTx(tx, a, lastSaid, words, "", -1, 0, "reopen failed: "+reason); err != nil {
			return err
		}
		return tx.RevokeTokens(a.Id)
	})
	if err == nil && alert != nil && d.notify != nil {
		d.notify(alert.ToId)
	}
	return err
}

// cancelReopen drops a pending reopen because the agent is being stopped on purpose.
func cancelReopen(tx *store.Tx, a *v1.Agent, reason string) error {
	a.PendingCompaction = nil // a compaction asked for with it goes too
	r := a.PendingReopen
	if r == nil {
		return nil
	}
	a.PendingReopen = nil
	_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentReopenDropped{
		AgentReopenDropped: &v1.AgentReopenDropped{Reopen: r, Reason: reason}}})
	return err
}

// stopTerminal stops a terminal's process group (SIGTERM, then the host's SIGKILL), waits until it has exited,
// and lets the host forget it.
func (d *Daemon) stopTerminal(ctx context.Context, id string) error {
	api := d.rt.term.API
	if _, err := api.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: id})); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(StopWait)
	for {
		res, err := api.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		if err != nil {
			return err
		}
		exited := true
		for _, t := range res.Msg.Terminals {
			if t.Id == id && !t.Exited {
				exited = false
			}
		}
		if exited {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("its process did not exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := api.AckExit(ctx, connect.NewRequest(&termv1.AckExitRequest{Id: id})); err != nil &&
		connect.CodeOf(err) != connect.CodeNotFound {
		return err
	}
	return nil
}

// adoptReopens finishes, at boot, the reopens a daemon died in the middle of: a running terminal labelled with
// the pending reopen is the relaunch (recorded, never started twice); an exited one is the relaunch that died.
// The rest run from the background loop.
func (d *Daemon) adoptReopens(ctx context.Context, terminals []*termv1.Terminal, claimed map[string]bool) error {
	pending, err := d.store.Read(ctx).AgentsWithPendingReopen()
	if err != nil {
		return err
	}
	for _, a := range pending {
		for _, t := range terminals {
			if t.Labels["agent"] != a.Id || t.Labels["reopen"] != a.PendingReopen.Id {
				continue
			}
			claimed[t.Id] = true
			if t.Exited {
				err = d.recordExit(ctx, t.Id, a.Id, t.Labels["reopen"], t.ExitCode, t.Signal, "")
			} else {
				err = d.finishReopen(ctx, a.Id, a.PendingReopen, t.Id, nil)
				if err == nil {
					if t.StartedAt != nil {
						d.rt.guard.seed(a.Id, t.StartedAt.AsTime(), d.now())
					}
					if t.Pid > 0 {
						d.adoptCPUHolder(ctx, a, int(t.Pid), t.PidStartTicks)
					}
				}
				d.log.Info("adopted a reopen's terminal", "agent", a.Name, "terminal", t.Id)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// reportTask is the task a report of agent a to its parent answers: its current task, which the daemon stamps on the
// report (Message.re). A report that names a task (--re) must name that one: a late or retried report for an older
// task is refused, and so is any report after a done for it, or a second failed (a task or answer since starts the
// work again; a done after a failed goes through). A report that names none is stamped and let through, as every report was before.
func reportTask(r store.Reader, a *v1.Agent, typ v1.MessageType, re string) (string, error) {
	cur, _, err := currentTask(r, a)
	if err != nil || cur == nil {
		if err == nil && re != "" {
			return "", failedPrecondition("you named task %s, but no task was sent to you", re)
		}
		return "", err
	}
	if re == "" {
		return cur.Id, nil
	}
	if re != cur.Id {
		return "", failedPrecondition("your current task is %s (%s), not %s: that report would answer an older task "+
			"(read it with `mesh inbox --id %s`; nothing was sent)", cur.Id, firstLineOf(cur.Body, 60), re, cur.Id)
	}
	sent, err := reportsSinceWork(r, a)
	if err != nil {
		return "", err
	}
	// A done is final: no report follows it. A failed may be followed by a done (the work was finished after
	// all), a plan or a question, not by another failed.
	if sent[v1.MessageType_MESSAGE_TYPE_DONE] || sent[v1.MessageType_MESSAGE_TYPE_FAILED] && typ == v1.MessageType_MESSAGE_TYPE_FAILED {
		last := v1.MessageType_MESSAGE_TYPE_DONE
		if !sent[last] {
			last = v1.MessageType_MESSAGE_TYPE_FAILED
		}
		return "", failedPrecondition("task %s was already reported %s: nothing was sent (a new task or an answer opens it again)",
			cur.Id, strings.ToLower(strings.TrimPrefix(last.String(), "MESSAGE_TYPE_")))
	}
	return cur.Id, nil
}

// reportsSinceWork is the types of the reports an agent sent to its parent since its last task, answer or PR feedback.
func reportsSinceWork(r store.Reader, a *v1.Agent) (map[v1.MessageType]bool, error) {
	msgs, err := r.RunMessages(a.RunId, "", -1) // -1: no limit
	if err != nil {
		return nil, err
	}
	sent := map[v1.MessageType]bool{}
	for _, m := range msgs {
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			clear(sent)
		case m.ToId == a.Id && m.From.GetId() == meshActor.Id && strings.HasPrefix(m.Body, "[mesh pr-feedback"):
			clear(sent) // feedback on its PR asks for a new report on the same task (prfeedback.go)
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && isReport(m.Type):
			sent[m.Type] = true
		}
	}
	return sent, nil
}
