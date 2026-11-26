package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// IsAgent reports whether an actor class is an agent's (not a device's).
func IsAgent(c v1.ActorClass) bool {
	switch c {
	case v1.ActorClass_ACTOR_CLASS_ROOT, v1.ActorClass_ACTOR_CLASS_WORKER,
		v1.ActorClass_ACTOR_CLASS_ASKER, v1.ActorClass_ACTOR_CLASS_REVIEWER:
		return true
	}
	return false
}

// ClassOf is the actor class an agent's token gets for its role.
func ClassOf(role v1.AgentRole) v1.ActorClass {
	switch role {
	case v1.AgentRole_AGENT_ROLE_ROOT:
		return v1.ActorClass_ACTOR_CLASS_ROOT
	case v1.AgentRole_AGENT_ROLE_WORKER:
		return v1.ActorClass_ACTOR_CLASS_WORKER
	case v1.AgentRole_AGENT_ROLE_ASKER:
		return v1.ActorClass_ACTOR_CLASS_ASKER
	case v1.AgentRole_AGENT_ROLE_REVIEWER:
		return v1.ActorClass_ACTOR_CLASS_REVIEWER
	}
	return v1.ActorClass_ACTOR_CLASS_UNSPECIFIED
}

func notFound(format string, args ...any) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf(format, args...))
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func denied(format string, args ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(format, args...))
}

// getAgent reads an agent by id; a missing one is NotFound.
func (d *Daemon) getAgent(r store.Reader, id string) (*v1.Agent, error) {
	a, err := r.GetAgent(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no agent %s", id)
	}
	return a, err
}

// noRun is a scope no run has: an agent with no run of its own reads nothing, not everything.
const noRun = "\x00no-run"

// runScope is the only run an agent may read; empty (any run) for a device.
func runScope(actor *v1.Actor) string {
	if actor != nil && IsAgent(actor.Class) {
		if actor.RunId == "" {
			return noRun
		}
		return actor.RunId
	}
	return ""
}

// scopedRun returns the run a read covers: an agent's own (asking for another is refused), or what a
// device asked for.
func scopedRun(actor *v1.Actor, asked string) (string, error) {
	own := runScope(actor)
	if own == "" {
		return asked, nil
	}
	if asked != "" && asked != own {
		return "", denied("an agent reads only its own run")
	}
	return own, nil
}

// FindAgent resolves an agent reference: an id, else a name within the caller's run (an agent) or unique
// across the open runs (a device). An agent finds only agents of its own run.
func (d *Daemon) FindAgent(r store.Reader, actor *v1.Actor, ref string) (*v1.Agent, error) {
	if ref == "" {
		return nil, invalid("no agent given")
	}
	a, err := r.GetAgent(ref)
	if err == nil {
		if own := runScope(actor); own != "" && a.RunId != own {
			return nil, notFound("no agent %q", ref)
		}
		return a, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	found, err := r.AgentsNamed(runScope(actor), ref)
	if err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, notFound("no agent %q", ref)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, a := range found {
		names[i] = a.Id + " (run " + a.RunId + ")"
	}
	return nil, connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%q names several agents: %s; give an id", ref, strings.Join(names, ", ")))
}

// Hello implements the handshake.
func (d *Daemon) Hello(ctx context.Context, req *connect.Request[v1.HelloRequest]) (*connect.Response[v1.HelloResponse], error) {
	if v := req.Msg.ProtocolVersion; v != 0 && v != ProtocolVersion {
		side := "the daemon"
		if v < ProtocolVersion {
			side = "the client"
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("protocol %d is not this daemon's %d: upgrade %s", v, ProtocolVersion, side))
	}
	return connect.NewResponse(&v1.HelloResponse{
		DaemonId: d.daemonID, InstanceId: d.instanceID, StartedAt: tsOf(d.startedAt),
		ProtocolVersion: ProtocolVersion, Version: Version, Capabilities: d.capabilities,
		Actor: auth.ActorFrom(ctx), ServerTime: tsOf(d.now()),
	}), nil
}

// StartRun starts a run.
func (d *Daemon) StartRun(ctx context.Context, req *connect.Request[v1.StartRunRequest]) (*connect.Response[v1.StartRunResponse], error) {
	notice := d.ModelNotice(ctx)
	out := &v1.StartRunResponse{ModelNotice: notice}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceStartRunProcedure, out, func(t *store.Tx) error {
		title := strings.TrimSpace(req.Msg.Title)
		if title == "" {
			title = "untitled"
		}
		var run *v1.Run
		for try := 0; ; try++ {
			run = &v1.Run{Id: ids.New("r", 6), Title: title, Status: v1.RunStatus_RUN_STATUS_OPEN,
				CreatedAt: tsOf(t.Now()), CreatedBy: t.Actor}
			err := t.PutRun(run)
			if err == nil {
				break
			}
			if !errors.Is(err, store.ErrExists) || try == 5 {
				return err
			}
		}
		if err := d.ensureRunDir(run.Id); err != nil {
			return err
		}
		if _, err := t.AppendEvent(&v1.Event{RunId: run.Id,
			Payload: &v1.Event_RunStarted{RunStarted: &v1.RunStarted{Run: run}}}); err != nil {
			return err
		}
		out.Run = run
		return nil
	})
	return connect.NewResponse(out), err
}

// mayCrossRunRead reports whether an actor may read runs/agents outside its own run: a device, or a root
// with a valid run id (never an empty-run root or a worker/asker/reviewer).
func mayCrossRunRead(actor *v1.Actor) bool {
	if actor == nil {
		return false
	}
	if !IsAgent(actor.Class) {
		return true
	}
	return actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && actor.RunId != ""
}

// scopedReadRun resolves `asked` for read RPCs (ListAgents): a root defaults to its own run when empty but
// may name another run; workers/askers/reviewers remain strictly confined to their own run.
func scopedReadRun(actor *v1.Actor, asked string) (string, error) {
	if mayCrossRunRead(actor) {
		if asked == "" && actor != nil && IsAgent(actor.Class) {
			return actor.RunId, nil
		}
		return asked, nil
	}
	return scopedRun(actor, asked)
}

// findAgentCrossRun resolves an agent reference for GetAgent / ResumeAgent: a device or root with valid run_id
// may reach agents in other runs. For a root, it prefers its own run, then closed runs, then other open runs;
// for a device, open runs then closed runs. Workers/askers/reviewers remain strictly confined to FindAgent.
func (d *Daemon) findAgentCrossRun(r store.Reader, actor *v1.Actor, ref string) (*v1.Agent, error) {
	if !mayCrossRunRead(actor) {
		return d.FindAgent(r, actor, ref)
	}
	if ref == "" {
		return nil, invalid("no agent given")
	}
	a, err := r.GetAgent(ref)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	var found []*v1.Agent
	if actor != nil && IsAgent(actor.Class) && actor.RunId != "" {
		found, err = r.AgentsNamed(actor.RunId, ref)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			found, err = r.AgentsNamedClosed(ref)
			if err != nil {
				return nil, err
			}
		}
		if len(found) == 0 {
			found, err = r.AgentsNamed("", ref)
			if err != nil {
				return nil, err
			}
		}
	} else {
		found, err = r.AgentsNamed("", ref)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			found, err = r.AgentsNamedClosed(ref)
			if err != nil {
				return nil, err
			}
		}
	}
	switch len(found) {
	case 0:
		return nil, notFound("no agent %q", ref)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, x := range found {
		names[i] = x.Id + " (run " + x.RunId + ")"
	}
	return nil, connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%q names several agents: %s; give an id", ref, strings.Join(names, ", ")))
}

// ListRuns lists runs.
func (d *Daemon) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	r := d.store.Read(ctx)
	actor := auth.ActorFrom(ctx)
	if !mayCrossRunRead(actor) {
		own := runScope(actor)
		run, err := r.GetRun(own)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFound("no run %s", own)
		}
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&v1.ListRunsResponse{Runs: []*v1.Run{run}}), nil
	}
	runs, err := r.ListRuns(req.Msg.All)
	return connect.NewResponse(&v1.ListRunsResponse{Runs: runs}), err
}

// ListAgents lists agents with their derived state.
func (d *Daemon) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	runID, err := scopedReadRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	agents, err := d.store.Read(ctx).ListAgents(runID)
	if err != nil {
		return nil, err
	}
	for i, a := range agents {
		agents[i] = d.agentView(a)
	}
	return connect.NewResponse(&v1.ListAgentsResponse{Agents: agents}), nil
}

// GetAgent shows one agent.
func (d *Daemon) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	a, err := d.findAgentCrossRun(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: d.agentView(a)}), nil
}

// SendMessage stores a message and asks for its doorbell to be rung.
func (d *Daemon) SendMessage(ctx context.Context, req *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error) {
	if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_UNSPECIFIED {
		return nil, invalid("no message type given")
	}
	out := &v1.SendMessageResponse{}
	// A message to a stopped agent resumes it, and a direct task to a planner reopens it without its deny list:
	// both need its conversation id, found before the write (learnSession).
	actor := auth.ActorFrom(ctx)
	if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && req.Msg.To != "" && req.Msg.To != "parent" {
		r := d.store.Read(ctx)
		if to, err := d.FindAgent(r, actor, req.Msg.To); err == nil && to.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			d.warmUsage(ctx, r, to.Kind, agyResolveKey{model: to.Model, effort: to.Effort})
			d.warmOpenCodeModels(ctx, to.Kind)
		}
	}
	// A worker's done to its parent carries a footer on what its PR's reviews found; whether the worker has made
	// commits is read here, outside the write (reviewFooter).
	hasWork := true
	if IsAgent(actor.Class) && req.Msg.Type == v1.MessageType_MESSAGE_TYPE_DONE {
		if self, err := d.store.Read(ctx).GetAgent(actor.Id); err == nil {
			hasWork = hasBranchWork(ctx, self)
		}
	}
	d.learnSession(ctx, actor, req.Msg.To, func(to *v1.Agent) bool {
		if stopped(to) {
			return resumes(req.Msg.Type)
		}
		if req.Msg.Type != v1.MessageType_MESSAGE_TYPE_TASK || !isDirectTask(req.Msg.Body) ||
			IsAgent(actor.Class) && to.ParentId != actor.Id {
			return false
		}
		_, _, deny := target(to)
		return deny && (!adapter.ReadOnly(to.Role) || d.becomesWorker(d.store.Read(ctx), to, req.Msg.Body))
	})
	var prepCandID string
	var prepTarget *v1.Agent
	var prep *preparedWorktree
	var prepErr error
	var prepLock *sync.Mutex
	var resumedID string
	if req.Msg.To != "" && req.Msg.To != "parent" && resumes(req.Msg.Type) {
		r := d.store.Read(ctx)
		// A stopped asker or reviewer that this task makes a worker (takeChange, in the write below) gets the
		// worktree a worker gets: recreated if StopRun removed it, moved off a finished PR's branch.
		isTask := req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK
		if cand, cerr := d.FindAgent(r, actor, req.Msg.To); cerr == nil && stopped(cand) &&
			(!adapter.ReadOnly(cand.Role) || isTask && d.becomesWorker(r, cand, req.Msg.Body)) &&
			cand.Role != v1.AgentRole_AGENT_ROLE_ROOT &&
			cand.Worktree != "" && cand.Repo != "" &&
			(!IsAgent(actor.Class) || cand.ParentId == actor.Id) {
			mu := d.worktreeLock(cand.Worktree)
			mu.Lock()
			r2 := d.store.Read(ctx)
			if cur, cerr := d.FindAgent(r2, actor, cand.Id); cerr == nil && stopped(cur) && cur.SessionId != "" {
				if run, rerr := r2.GetRun(cur.RunId); rerr == nil && run.Status == v1.RunStatus_RUN_STATUS_OPEN {
					okReserve := true
					if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && cur.Role != v1.AgentRole_AGENT_ROLE_ROOT {
						model, effort, _ := target(cur)
						okReserve = d.reserveCheck(r2, cur.RunId, cur.Kind, model, effort, "") == nil
					}
					if okReserve &&
						d.hasProcessOnSessionOrWorktree(r2, cur) == nil &&
						d.capGuard(r2, "resuming "+cur.Name, cur.Id) == nil &&
						d.checkMemoryGuard(r2, "resuming "+cur.Name) == nil {
						prepCandID = cur.Id
						asWorker := proto.Clone(cur).(*v1.Agent)
						if isTask && d.becomesWorker(r2, cur, req.Msg.Body) {
							asWorker.Role = v1.AgentRole_AGENT_ROLE_WORKER
						}
						if p, perr := d.prepareWorktreeForTask(ctx, asWorker, cur.RunId, isTask); perr != nil {
							prepErr = perr
						} else {
							prepTarget, prep = cur, p
						}
					}
				}
			}
			prepLock = mu
		}
	}
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSendMessageProcedure, out, func(t *store.Tx) error {
		actor := t.Actor
		var to *v1.Agent
		var err error
		if req.Msg.To == "parent" {
			if !IsAgent(actor.Class) {
				return invalid("only an agent has a parent")
			}
			self, err := d.getAgent(t.Reader, actor.Id)
			if err != nil {
				return err
			}
			if self.ParentId == "" {
				return connect.NewError(connect.CodeFailedPrecondition,
					errors.New("you have no parent agent: a device started you"))
			}
			to, err = d.getAgent(t.Reader, self.ParentId)
			if err != nil {
				return err
			}
		} else if to, err = d.FindAgent(t.Reader, actor, req.Msg.To); err != nil {
			return err
		}
		var asked *collectRef // the round this report answers, when it is a member's
		if IsAgent(actor.Class) {
			self, err := d.getAgent(t.Reader, actor.Id)
			if err != nil {
				return err
			}
			if to.Id == self.ParentId && isReport(req.Msg.Type) {
				if asked, err = d.collectingRef(t.Reader, self, to); err != nil {
					return err
				}
			}
			if to.Id != self.ParentId && to.ParentId != self.Id {
				return denied("an agent sends only to its parent or its children; %s is neither", to.Name)
			}
			// A report to its parent sent while a task or answer is unread would answer instructions the agent hasn't
			// read (a question to its own child is not a report).
			if to.Id == self.ParentId && isReport(req.Msg.Type) {
				if err := unreadInstructions(t.Reader, self); err != nil {
					return err
				}
			}
			// A plan from an agent with no plan phase (a direct task that turned out to need one, or an approved one
			// given a follow-up) enters one, so approving it moves it to an execution tier. It plans on the model
			// it is on: nothing reopens it to add the deny list (design §9). A named model covers the whole task:
			// it never gets the phase.
			if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_PLAN && to.Id == self.ParentId && !self.Pinned &&
				(self.Phase == v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || self.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING) {
				endExecution(self)
				if err := setPhase(t, self, v1.AgentPhase_AGENT_PHASE_PLANNING, "plan sent"); err != nil {
					return err
				}
				if err := t.PutAgent(self); err != nil {
					return err
				}
			}
		}
		if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && to.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			model, effort, _ := target(to)
			if err := d.reserveCheck(t.Reader, to.RunId, to.Kind, model, effort, "sending a task to "+to.Name); err != nil {
				return err
			}
		}
		// A task marked direct ends the plan phase of the agent it is for and lifts its planning deny list (by a
		// reopen, since a running process can't change it), as bin/mesh's `cmd_send` does for a `Direct task`; a
		// planned follow-up leaves a planner planning. From a device as from the agent's parent. A stopped agent
		// gets it from `revive` below instead, behind the same guards as any resume, so that a refusal leaves it
		// as it was and nothing is requested twice.
		fromParent := !IsAgent(actor.Class) || to.ParentId == actor.Id
		direct := req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && isDirectTask(req.Msg.Body) && fromParent
		// A task that is not a read-only one, from the agent's parent or a device, makes an asker or reviewer
		// the worker for it (takeChange), before the direct task lifts its list.
		wasReadOnly := adapter.ReadOnly(to.Role)
		if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && fromParent {
			if err := d.takeChange(t, to, req.Msg.Body, !direct); err != nil {
				return err
			}
		}
		if direct && !stopped(to) {
			if err := d.directTask(t, to); err != nil {
				return err
			}
		}
		if !stopped(to) { // a follow-up round for a finished Claude worker: compacted before the message rings
			if err := d.requestFollowupCompaction(t, to, req.Msg.Type, req.Msg.Body); err != nil {
				return err
			}
		}
		msgBody := req.Msg.Body
		if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_TASK {
			msgBody = withOpenCodeBodyNote(to.Kind, req.Msg.Body)
		}
		willPrep := stopped(to) && resumes(req.Msg.Type) && prep != nil && prepTarget != nil && to.Id == prepTarget.Id
		// A task, answer or question to an agent whose process is gone brings it back, in its own conversation,
		// to read it (design §18). An info is only stored: it is read when the agent is next resumed. A task
		// marked direct is direct, as one sent to a live agent is and as `resume --task` is: it ends a plan phase;
		// any other comes back as it was (a planner still planning), as `send` leaves a live one.
		if stopped(to) && resumes(req.Msg.Type) {
			if prepErr != nil && to.Id == prepCandID {
				out.ResumeError = fmt.Sprintf("preparing worktree for %s: %v", to.Name, prepErr)
			} else {
				spec := reviveSpec{task: direct, taskText: req.Msg.Body}
				// The guards answer before anything is written: a refusal keeps the message and says why the agent
				// stayed stopped (a report to a retired parent must not be lost to the host's cap).
				switch rv, err := d.revive(t, to, spec); {
				case connect.CodeOf(err) == connect.CodeResourceExhausted || connect.CodeOf(err) == connect.CodeFailedPrecondition:
					out.ResumeError = err.Error()
					var ce *connect.Error
					if errors.As(err, &ce) {
						out.ResumeError = ce.Message()
					}
					// The agent stays stopped with the task stored. A planner keeps its phase (a refusal leaves it as
					// it was), but an asker or reviewer this direct task just made a worker has no plan phase and no
					// deny list to come back under: the next resume brings it back to do the task.
					// Its DenyEdits changes with no event: the apps don't show it, and a stopped agent's launch reads
					// the stored value, so the next reopen or snapshot carries it.
					if direct && wasReadOnly && !adapter.ReadOnly(to.Role) {
						if err := d.directTask(t, to); err != nil {
							return err
						}
					}
				case err != nil:
					return err
				default:
					out.Resumed = rv
					resumedID = to.Id
					if dropNote, dropped, err := d.dropHeldExclusiveMCP(t.Reader, to); err != nil {
						return err
					} else if dropped {
						if _, err := tellTx(t, to, dropNote); err != nil {
							return err
						}
					}
					if willPrep && prep.taskNote != "" {
						msgBody += "\n\n" + prep.taskNote
					}
					if willPrep && prep.newBranch != "" {
						to.Branch = prep.newBranch
						to.BaseCommit = prep.newBaseCommit
					}
					if err := t.PutAgent(to); err != nil {
						return err
					}
				}
			}
		}
		if IsAgent(actor.Class) && req.Msg.Type == v1.MessageType_MESSAGE_TYPE_DONE && to.Id != "" {
			// A worker's done to its parent carries what its PR's review rounds found (reviewFooter).
			if self, err := t.Reader.GetAgent(actor.Id); err == nil && to.Id == self.ParentId {
				msgBody += d.reviewFooter(t.Reader, self, req.Msg.Body, hasWork)
			}
		}
		msg := &v1.Message{Id: ids.New("m", 10), RunId: to.RunId, From: actor, ToId: to.Id, ToName: to.Name,
			Type: req.Msg.Type, Body: msgBody, CreatedAt: tsOf(t.Now())}
		if asked != nil && (req.Msg.Type == v1.MessageType_MESSAGE_TYPE_DONE || req.Msg.Type == v1.MessageType_MESSAGE_TYPE_FAILED) {
			// A round's answer is held for its file: stored, but never rung (and read, so the caller's inbox keeps
			// to what it has to act on); the caller gets one message when the round is in (collect.go).
			msg.DeliveredAt, msg.ReadAt = tsOf(t.Now()), tsOf(t.Now())
		}
		if req.Msg.Type == v1.MessageType_MESSAGE_TYPE_WAIT {
			// Not a report: it asks nothing of its parent, who reads it in the inbox, and rings no doorbell
			// (PROTOCOL.md, design "A declared wait"). Marked delivered so the doorbell loop never takes it.
			msg.DeliveredAt = tsOf(t.Now())
		}
		if err := t.PutMessage(msg); err != nil {
			return err
		}
		if _, err := t.AppendEvent(&v1.Event{RunId: to.RunId, AgentId: to.Id,
			Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: msg}}}); err != nil {
			return err
		}
		out.Message = msg
		if asked != nil {
			if err := d.settleReport(t, asked, req.Msg.Type, req.Msg.Body); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || out.ResumeError != "" {
		if prep != nil && prepTarget != nil {
			prep.rollback(ctx, prepTarget.Repo, prepTarget.Worktree)
		}
	}
	if prepLock != nil {
		prepLock.Unlock()
	}
	if err != nil {
		if out.Resumed != nil && resumedID != "" {
			d.rt.guard.release(resumedID) // the write failed after the guard passed: nothing was requested
		}
		return nil, err
	}
	if d.notify != nil && !replayed && req.Msg.Type != v1.MessageType_MESSAGE_TYPE_WAIT {
		d.notify(out.Message.ToId)
	}
	return connect.NewResponse(out), nil
}

// Page sizes of ListMessages.
const (
	defaultMessagePage = 500
	maxMessagePage     = 2000
)

// ListMessages returns a run's stored messages, oldest first, a page at a time.
func (d *Daemon) ListMessages(ctx context.Context, req *connect.Request[v1.ListMessagesRequest]) (*connect.Response[v1.ListMessagesResponse], error) {
	runID, err := scopedReadRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("no run given")
	}
	limit := int(req.Msg.Limit)
	switch {
	case limit < 0:
		return nil, invalid("limit can't be negative")
	case limit == 0:
		limit = defaultMessagePage
	case limit > maxMessagePage:
		limit = maxMessagePage
	}
	r := d.store.Read(ctx)
	if _, err := r.GetRun(runID); errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no run %s", runID)
	} else if err != nil {
		return nil, err
	}
	// One more than asked tells whether another page follows.
	msgs, err := r.RunMessages(runID, req.Msg.AfterId, limit+1)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no message %s in run %s", req.Msg.AfterId, runID)
	}
	if err != nil {
		return nil, err
	}
	more := len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	return connect.NewResponse(&v1.ListMessagesResponse{Messages: msgs, More: more}), nil
}

// resumes reports whether a message of this type reopens a stopped agent.
func resumes(t v1.MessageType) bool {
	return t == v1.MessageType_MESSAGE_TYPE_TASK || t == v1.MessageType_MESSAGE_TYPE_ANSWER ||
		t == v1.MessageType_MESSAGE_TYPE_QUESTION
}

// ReadInbox returns the calling agent's messages.
func (d *Daemon) ReadInbox(ctx context.Context, req *connect.Request[v1.ReadInboxRequest]) (*connect.Response[v1.ReadInboxResponse], error) {
	out := &v1.ReadInboxResponse{}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceReadInboxProcedure, out, func(t *store.Tx) error {
		msgs, err := t.Inbox(t.Actor.Id, req.Msg.All)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.ReadAt == nil && !req.Msg.Peek {
				m.ReadAt = tsOf(t.Now())
				if err := t.PutMessage(m); err != nil {
					return err
				}
			}
		}
		out.Messages = msgs
		return nil
	})
	return connect.NewResponse(out), err
}

// SubscribeEvents streams the event log.
func (d *Daemon) SubscribeEvents(ctx context.Context, req *connect.Request[v1.SubscribeEventsRequest],
	stream *connect.ServerStream[v1.SubscribeEventsResponse]) error {
	actor := auth.ActorFrom(ctx)
	runID, err := scopedRun(actor, req.Msg.RunId)
	if err != nil {
		return err
	}
	msg := proto.Clone(req.Msg).(*v1.SubscribeEventsRequest)
	msg.RunId = runID
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if actor != nil {
		defer d.trackStream(actor.Id, cancel)()
		// Re-check after trackStream so a RevokeDevice racing between auth and registration is not missed.
		active, err := d.store.Read(ctx).HasActiveToken(actor.Id)
		if err != nil {
			return err
		}
		if !active {
			return connect.NewError(connect.CodeUnauthenticated, auth.ErrUnknownToken)
		}
	}
	// Device pairing/revocation events go only to the host's own device, the one that may manage devices
	// (requireHostDevice).
	deviceEvents := actor != nil && actor.Id == localDeviceID
	return d.bus.Serve(sctx, msg, deviceEvents, d.stopping, stream.Send)
}
