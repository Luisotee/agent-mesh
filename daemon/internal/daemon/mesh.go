package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
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

// FindAgent resolves an agent reference: an id, else a name within the caller's run (an agent) or unique
// across the open runs (a device).
func (d *Daemon) FindAgent(r store.Reader, actor *v1.Actor, ref string) (*v1.Agent, error) {
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
	runID := ""
	if actor != nil && IsAgent(actor.Class) {
		runID = actor.RunId
	}
	found, err := r.AgentsNamed(runID, ref)
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
		Actor: auth.ActorFrom(ctx),
	}), nil
}

// StartRun starts a run.
func (d *Daemon) StartRun(ctx context.Context, req *connect.Request[v1.StartRunRequest]) (*connect.Response[v1.StartRunResponse], error) {
	out := &v1.StartRunResponse{}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceStartRunProcedure, out, func(t *store.Tx) error {
		title := strings.TrimSpace(req.Msg.Title)
		if title == "" {
			title = "untitled"
		}
		run := &v1.Run{Id: ids.New("r", 6), Title: title, Status: v1.RunStatus_RUN_STATUS_OPEN,
			CreatedAt: tsOf(t.Now()), CreatedBy: t.Actor}
		if err := t.PutRun(run); err != nil {
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

// ListRuns lists runs.
func (d *Daemon) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	runs, err := d.store.Read(ctx).ListRuns(req.Msg.All)
	return connect.NewResponse(&v1.ListRunsResponse{Runs: runs}), err
}

// ListAgents lists agents with their derived state.
func (d *Daemon) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	agents, err := d.store.Read(ctx).ListAgents(req.Msg.RunId)
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
	a, err := d.FindAgent(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
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
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSendMessageProcedure, out, func(t *store.Tx) error {
		actor := t.Actor
		var to *v1.Agent
		var err error
		if req.Msg.To == "parent" {
			if !IsAgent(actor.Class) {
				return invalid("only an agent has a parent")
			}
			self, err := t.GetAgent(actor.Id)
			if err != nil {
				return err
			}
			if self.ParentId == "" {
				return connect.NewError(connect.CodeFailedPrecondition,
					errors.New("you have no parent agent: a device started you"))
			}
			to, err = t.GetAgent(self.ParentId)
			if err != nil {
				return err
			}
		} else if to, err = d.FindAgent(t.Reader, actor, req.Msg.To); err != nil {
			return err
		}
		if IsAgent(actor.Class) {
			self, err := t.GetAgent(actor.Id)
			if err != nil {
				return err
			}
			if to.Id != self.ParentId && to.ParentId != self.Id {
				return denied("an agent sends only to its parent or its children; %s is neither", to.Name)
			}
		}
		msg := &v1.Message{Id: ids.New("m", 10), RunId: to.RunId, From: actor, ToId: to.Id, ToName: to.Name,
			Type: req.Msg.Type, Body: req.Msg.Body, CreatedAt: tsOf(t.Now())}
		if err := t.PutMessage(msg); err != nil {
			return err
		}
		if _, err := t.AppendEvent(&v1.Event{RunId: to.RunId, AgentId: to.Id,
			Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: msg}}}); err != nil {
			return err
		}
		out.Message = msg
		return nil
	})
	if err != nil {
		return nil, err
	}
	if d.notify != nil {
		d.notify(out.Message.ToId)
	}
	return connect.NewResponse(out), nil
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
	return d.bus.Serve(ctx, req.Msg, d.stopping, stream.Send)
}
