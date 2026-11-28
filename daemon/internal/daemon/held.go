package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Held tasks: `mesh send --type task --after <task id>` stores the request (held_tasks, migration 0015) instead of a
// message, and the daemon sends it, through the whole SendMessage path and as its sender, once a `done` answers the task
// it names. Nothing but this file reads the table, so a held task is in no inbox, no doorbell and no event until it is
// sent, and the send rules (the reserve, takeChange, a stopped agent's resume) apply at the moment it is sent.

type taskOutcome int

const (
	taskOpen taskOutcome = iota
	taskDone
	taskFailed
)

// outcomeOf is how a task stands: answered by a done (that wins), by a failed, or neither. Reports name their task in
// `Message.re`, and name the recipient's newest task when it was sent another before it reported (a follow-up), so a
// report of the same recipient that names a task its sender sent it at or after this one answers this one too (a
// follow-up; a task from anyone else, the daemon's feedback say, is other work and answers nothing here).
func outcomeOf(r store.Reader, runID, taskID string) (taskOutcome, error) {
	msgs, err := r.RunMessages(runID, "", -1)
	if err != nil {
		return taskOpen, err
	}
	var task *v1.Message
	for _, m := range msgs {
		if m.Id == taskID {
			task = m
			break
		}
	}
	mine := map[string]bool{taskID: true} // the task, and the tasks its recipient was sent after it
	if task != nil {
		for _, m := range msgs {
			if m.Type == v1.MessageType_MESSAGE_TYPE_TASK && m.ToId == task.ToId && m.From.GetId() == task.From.GetId() &&
				!m.CreatedAt.AsTime().Before(task.CreatedAt.AsTime()) {
				mine[m.Id] = true
			}
		}
	}
	out := taskOpen
	for _, m := range msgs {
		if !mine[m.Re] || task != nil && m.From.GetId() != task.ToId {
			continue
		}
		switch m.Type {
		case v1.MessageType_MESSAGE_TYPE_DONE:
			return taskDone, nil
		case v1.MessageType_MESSAGE_TYPE_FAILED:
			out = taskFailed
		}
	}
	return out, nil
}

// holdTask is SendMessage for a task with `after`. It answers (done true) when the task is held; it does not (done
// false) when the task it waits for is already done, which then goes the plain way.
func (d *Daemon) holdTask(ctx context.Context, req *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], bool, error) {
	m := req.Msg
	actor := auth.ActorFrom(ctx)
	if m.Type != v1.MessageType_MESSAGE_TYPE_TASK {
		return nil, true, invalid("--after only goes with a task (--type task)")
	}
	if IsAgent(actor.Class) && actor.Class != v1.ActorClass_ACTOR_CLASS_ROOT {
		return nil, true, denied("only a run's lead or a device holds a task for another")
	}
	if m.To == "" || m.To == "parent" {
		return nil, true, invalid("name the agent the task is for (--to <agent>)")
	}
	after, err := d.store.Read(ctx).MessageByID(m.After)
	if errors.Is(err, store.ErrNotFound) {
		if held, herr := d.store.Read(ctx).GetHeldTask(m.After); herr == nil && (runScope(actor) == "" || runScope(actor) == held.RunId) {
			return nil, true, invalid("%s is a held task: it has no task id until it is sent (send this one after that is sent)", m.After)
		}
		return nil, true, notFound("no task %q (the id in its `mesh inbox` header)", m.After)
	} else if err != nil {
		return nil, true, err
	}
	if after.Type != v1.MessageType_MESSAGE_TYPE_TASK {
		return nil, true, invalid("%s is not a task", m.After)
	}
	if own := runScope(actor); own != "" && own != after.RunId {
		return nil, true, notFound("no task %q", m.After)
	}
	if depends, err := d.store.Read(ctx).GetAgent(after.ToId); err == nil && depends.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		return nil, true, invalid("%s is a task for the run's root, which reports to nobody: there is no done to wait for", m.After)
	}
	if o, err := outcomeOf(d.store.Read(ctx), after.RunId, after.Id); err != nil {
		return nil, true, err
	} else if o == taskDone {
		return nil, false, nil // answered already: nothing to wait for
	} else if o == taskFailed {
		return nil, true, failedPrecondition("the task %s was reported failed: nothing to wait for", m.After)
	}
	out := &v1.SendMessageResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceSendMessageProcedure, out, func(t *store.Tx) error {
		to, err := d.FindAgent(t.Reader, t.Actor, m.To)
		if err != nil {
			return err
		}
		if to.RunId != after.RunId {
			return invalid("%s is in another run than the task %s waits for", to.Name, m.After)
		}
		if IsAgent(t.Actor.Class) && to.ParentId != t.Actor.Id {
			return denied("an agent sends only to its parent or its children; %s is neither", to.Name)
		}
		h := &v1.HeldTask{Id: ids.New("h", 10), RunId: to.RunId, After: after.Id, To: to.Id, Body: m.Body,
			About: capAbout(m.About), From: t.Actor, CreatedAt: tsOf(t.Now())}
		if err := t.PutHeldTask(h); err != nil {
			return err
		}
		out.Held = h.Id
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	// The task may have been answered between the check above and the commit, and its release would have found
	// nothing: look again now the row is there.
	d.releaseHeldSoon(after.RunId)
	return connect.NewResponse(out), true, nil
}

// releaseHeldSoon settles a run's held tasks, in the background: it is called after a report is stored (which task it
// answers is the outcome's to say: a follow-up's report answers the task before it too).
func (d *Daemon) releaseHeldSoon(runID string) {
	d.goBG(func() {
		ctx, cancel := d.stopCtx()
		defer cancel()
		held, err := d.store.Read(ctx).HeldTasks(runID)
		if err != nil {
			d.log.Error("reading held tasks", "run", runID, "err", err)
			return
		}
		for _, h := range held {
			d.settleHeld(ctx, h.Id)
		}
	})
}

// refusal is an error that says the task can't be sent as it is (an invalid, denied, missing or precondition-failed
// request, the reserve), as opposed to one that may clear by itself.
func refusal(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeFailedPrecondition,
		connect.CodeResourceExhausted, connect.CodeAlreadyExists:
		return true
	}
	return false
}

// heldSweepEvery is how often every held task is looked at again: one a restart or a busy host left, one whose run
// closed or whose agent went.
const heldSweepEvery = time.Minute

// sweepHeld settles every held task now and then, until the daemon stops.
func (d *Daemon) sweepHeld(ctx context.Context) {
	t := time.NewTicker(heldSweepEvery)
	defer t.Stop()
	for {
		d.scanHeld(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// scanHeld settles every held task: a release the daemon stopped before it finished, one whose run closed.
func (d *Daemon) scanHeld(ctx context.Context) {
	held, err := d.store.Read(ctx).HeldTasks("")
	if err != nil {
		d.log.Error("reading held tasks", "err", err)
		return
	}
	for _, h := range held {
		d.settleHeld(ctx, h.Id)
	}
}

// settleHeld sends or drops one held task if what it waits for is settled: a done sends it, a failed drops it and
// tells the run's lead, a closed run drops it. Serialized per task: two callers (a report and the start scan) settle it once.
func (d *Daemon) settleHeld(ctx context.Context, id string) {
	mu := d.agentLock("held:" + id)
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	r := d.store.Read(ctx)
	h, err := r.GetHeldTask(id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			d.log.Error("reading a held task", "id", id, "err", err)
		}
		return
	}
	run, err := r.GetRun(h.RunId)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		d.log.Error("reading a held task's run", "id", id, "err", err)
		return
	}
	if run == nil || run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		d.endHeld(ctx, h, "", false) // the run is over: nobody to tell
		return
	}
	if _, err := r.GetAgent(h.To); errors.Is(err, store.ErrNotFound) {
		d.endHeld(ctx, h, "the agent it is for is gone", false)
		return
	}
	if after, err := r.MessageByID(h.After); err == nil {
		if _, err := r.GetAgent(after.ToId); errors.Is(err, store.ErrNotFound) {
			d.endHeld(ctx, h, "the agent that had the task it waited for ("+h.After+") is gone, so no done can come", false)
			return
		}
	}
	if h.From.GetClass() == v1.ActorClass_ACTOR_CLASS_DEVICE {
		// What a device queued does not outlive its token: revokeDevice drops it, and this covers a revoke that landed
		// while this release was under way.
		if dev, err := r.GetDevice(h.From.Id); errors.Is(err, store.ErrNotFound) || err == nil && dev.RevokedAt != nil {
			d.endHeld(ctx, h, "", false)
			return
		}
	}
	o, err := outcomeOf(r, h.RunId, h.After)
	if err != nil {
		d.log.Error("reading a held task's dependency", "id", id, "err", err)
		return
	}
	switch o {
	case taskOpen:
		return
	case taskFailed:
		d.endHeld(ctx, h, fmt.Sprintf("the task it waited for (%s) was reported failed", h.After), false)
		return
	}
	res, err := d.SendMessage(auth.WithActor(ctx, h.From), connect.NewRequest(&v1.SendMessageRequest{
		Meta: &v1.RequestMeta{MutationId: "held:" + h.Id}, To: h.To, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: h.Body, About: h.About}))
	if err == nil && res.Msg.ResumeError != "" {
		// Stored for the agent's next resume, but it stayed stopped: the lead has to know.
		d.endHeld(ctx, h, "the agent could not be resumed ("+clip(res.Msg.ResumeError, 300)+"); the task waits in its inbox for a resume (`mesh resume`)", true)
		return
	}
	if err != nil {
		if ctx.Err() != nil {
			return // the daemon is stopping: the start scan sends it
		}
		msg := err.Error()
		var ce *connect.Error
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		if !refusal(err) {
			// Not a refusal of the task (the host busy, a call that timed out): the row stays and the sweep sends it.
			d.log.Warn("sending a held task failed; it stays held", "id", h.Id, "err", err)
			return
		}
		d.endHeld(ctx, h, "it was refused when it was due: "+clip(msg, 300), false)
		return
	}
	d.endHeld(ctx, h, "", false)
}

// endHeld drops a held task's row; a non-empty why tells the run's lead why the task was not sent, or, with sent, that
// it was sent and what then went wrong.
func (d *Daemon) endHeld(ctx context.Context, h *v1.HeldTask, why string, sent bool) {
	var ring string
	_, err := d.write(auth.WithActor(ctx, meshActor), &v1.RequestMeta{MutationId: "held-end:" + h.Id},
		agentmeshv1connect.MeshServiceCancelHeldTaskProcedure, &v1.CancelHeldTaskResponse{}, func(t *store.Tx) error {
			if err := t.DeleteHeldTask(h.Id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil // cancelled meanwhile
				}
				return err
			}
			if why == "" {
				return nil
			}
			root, err := d.runRoot(t.Reader, h.RunId)
			if err != nil {
				return nil // no lead to tell
			}
			to := h.To
			if a, err := t.GetAgent(h.To); err == nil {
				to = a.Name
			}
			line := fmt.Sprintf("[mesh held %s] your task for %s was not sent: %s.", h.Id, to, why)
			if sent {
				line = fmt.Sprintf("[mesh held %s] your task for %s was sent, but %s.", h.Id, to, why)
			}
			if _, err := tellTx(t, root, line); err != nil {
				return err
			}
			ring = root.Id
			return nil
		})
	if err != nil && ctx.Err() == nil {
		d.log.Error("ending a held task", "id", h.Id, "err", err)
		return
	}
	if ring != "" {
		d.deliverSoon(ring)
	}
}

// ListHeldTasks lists what is held, oldest first: a lead's own run, or a device's run asked for (every run when none).
func (d *Daemon) ListHeldTasks(ctx context.Context, req *connect.Request[v1.ListHeldTasksRequest]) (*connect.Response[v1.ListHeldTasksResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	held, err := d.store.Read(ctx).HeldTasks(runID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListHeldTasksResponse{Held: held}), nil
}

// CancelHeldTask drops a held task: it is never sent, and nobody is told (the caller knows).
func (d *Daemon) CancelHeldTask(ctx context.Context, req *connect.Request[v1.CancelHeldTaskRequest]) (*connect.Response[v1.CancelHeldTaskResponse], error) {
	out := &v1.CancelHeldTaskResponse{}
	if _, err := d.store.Read(ctx).GetHeldTask(req.Msg.Id); errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no held task %q (`mesh held`)", req.Msg.Id) // no lock made for an id that is none
	}
	mu := d.agentLock("held:" + req.Msg.Id) // not while it is being sent
	mu.Lock()
	defer mu.Unlock()
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceCancelHeldTaskProcedure, out, func(t *store.Tx) error {
		h, err := t.GetHeldTask(req.Msg.Id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no held task %q (`mesh held`)", req.Msg.Id)
		} else if err != nil {
			return err
		}
		if own := runScope(t.Actor); own != "" && own != h.RunId {
			return notFound("no held task %q (`mesh held`)", req.Msg.Id)
		}
		return t.DeleteHeldTask(h.Id)
	})
	return connect.NewResponse(out), err
}
