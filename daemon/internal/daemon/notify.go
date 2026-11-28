package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/notice"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The daemon decides once what deserves telling the user (docs/apps.md, "Notifications"): `decide` turns a committed
// event into notices, each is stored as a UserNotice event, and the phones' pushes and the apps' notifications and
// Activity list all follow that event. A notice with ForUser false is for the Activity list only.

// permNoticeAfter is how long a permission prompt held for a lead waits for an answer before the user is told
// (a variable for tests).
var permNoticeAfter = time.Minute

// runState is what decide remembers of a run: the lead's finish, told once until the run gets work again.
type runState struct{ finished bool }

// decide returns the notices an event makes. lookup finds an agent (nil when unknown); r reads the store as of now.
func (p *pusher) decide(r store.Reader, e *v1.Event, lookup func(string) *v1.Agent) []*notice.Notice {
	d := p.d
	switch pl := e.Payload.(type) {
	case *v1.Event_AgentSpawned:
		p.runOf(e.RunId).finished = false
	case *v1.Event_AgentStateChanged:
		a := lookup(e.AgentId)
		ch := pl.AgentStateChanged
		if a == nil || ch.To == ch.From {
			return nil
		}
		body := notice.Brief(ch.Detail)
		at := func(kind, title, def string) []*notice.Notice {
			if body == "" {
				body = def
			}
			return []*notice.Notice{{Kind: kind, Title: title, Body: body, AgentID: a.Id, Tab: notice.TabTerminal, ForUser: true, Tag: a.Id}}
		}
		switch ch.To {
		case v1.AgentState_AGENT_STATE_NEEDS_INPUT:
			return at(notice.Input, a.Name+" needs you", "It is stopped at a prompt in its terminal.")
		case v1.AgentState_AGENT_STATE_LIMITED:
			return at(notice.Limited, a.Name+" hit its usage limit", "Its terminal says more.")
		case v1.AgentState_AGENT_STATE_ERROR:
			return at(notice.Error, a.Name+" is in error", "Its terminal says more.")
		case v1.AgentState_AGENT_STATE_WORKING:
			if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
				p.runOf(a.RunId).finished = false // the lead has work again: its next finish is news
			}
		case v1.AgentState_AGENT_STATE_IDLE:
			// Only a turn that ended: an agent coming up to its first prompt (STARTING) or back from unknown is no finish.
			if ch.From != v1.AgentState_AGENT_STATE_WORKING {
				return nil
			}
			switch {
			case a.Role == v1.AgentRole_AGENT_ROLE_ROOT:
				if n := d.runFinished(r, p.runOf(a.RunId), a); n != nil {
					return []*notice.Notice{n}
				}
			case a.ParentId == "":
				// A plain agent (nobody to report to) ended a turn.
				return []*notice.Notice{{Kind: notice.Finished, Title: a.Name + " finished", AgentID: a.Id, Tab: notice.TabChat, ForUser: true, Tag: a.Id}}
			}
		}
	case *v1.Event_MessageSent:
		m := pl.MessageSent.GetMessage()
		if m == nil || m.GetFrom().GetClass() == v1.ActorClass_ACTOR_CLASS_DEVICE {
			return nil
		}
		from := lookup(m.GetFrom().GetId())
		if from == nil {
			return nil
		}
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK {
			p.runOf(e.RunId).finished = false
		}
		base := notice.Notice{Body: notice.Brief(m.Body), AgentID: from.Id, Tab: notice.TabChat, Tag: from.Id}
		if m.ToId == userRecipient {
			// What a lead puts to the user: always for them.
			n := base
			n.ForUser = true
			switch m.Type {
			case v1.MessageType_MESSAGE_TYPE_QUESTION:
				n.Kind, n.Title = notice.Question, from.Name+" asks you"
			case v1.MessageType_MESSAGE_TYPE_PLAN:
				n.Kind, n.Title = notice.Plan, from.Name+" has a plan for you"
			default:
				n.Kind, n.Title = notice.Report, from.Name+" has news for you"
			}
			return []*notice.Notice{&n}
		}
		to := lookup(m.ToId)
		// What reaches a run's lead is what the user may be waiting for; talk among workers isn't.
		if to == nil || to.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			return nil
		}
		run, _ := r.GetRun(e.RunId)
		n := base
		switch m.Type {
		case v1.MessageType_MESSAGE_TYPE_PLAN:
			// A plan is the user's to approve only where the user approves plans: elsewhere the lead decides it.
			n.Kind, n.Title, n.ForUser = notice.Plan, from.Name+" sent a plan", d.userApprovesPlans(r, run, from.Repo)
		case v1.MessageType_MESSAGE_TYPE_QUESTION:
			n.Kind, n.Title = notice.Question, from.Name+" has a question"
		case v1.MessageType_MESSAGE_TYPE_DONE:
			n.Kind, n.Title = notice.Report, from.Name+" reported done"
		case v1.MessageType_MESSAGE_TYPE_FAILED:
			n.Kind, n.Title = notice.Failed, from.Name+" failed"
		default:
			return nil
		}
		return []*notice.Notice{&n}
	case *v1.Event_AgentExited:
		a := lookup(e.AgentId)
		x := pl.AgentExited
		// The user stopping an agent isn't news.
		if a == nil || x.Reason == "killed" {
			return nil
		}
		// Only a lead's or a plain agent's end is the user's: a worker's goes to its lead, which handles it.
		mine := a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.ParentId == ""
		n := &notice.Notice{AgentID: a.Id, Tab: notice.TabChat, ForUser: mine, Tag: a.Id}
		switch {
		case x.ExitCode == 0 && x.Reason != "lost":
			n.Kind, n.Title = notice.Exited, a.Name+" finished"
		case x.Reason == "lost":
			n.Kind, n.Title = notice.Failed, a.Name+" was lost"
		default:
			n.Kind, n.Title = notice.Failed, a.Name+" exited with code "+strconv.Itoa(int(x.ExitCode))
		}
		return []*notice.Notice{n}
	case *v1.Event_PermissionChanged:
		pc := pl.PermissionChanged
		if pc.State != "asked" {
			return nil
		}
		// A prompt held for a lead that nobody answers in a minute is the user's.
		id, agentID := pc.Id, e.AgentId
		d.goBG(func() {
			ctx, cancel := d.stopCtx()
			defer cancel()
			select {
			case <-time.After(permNoticeAfter):
			case <-ctx.Done():
				return
			}
			held, ok := d.perms.get(id)
			if !ok {
				return
			}
			if parent := d.getAgentOrNil(held.ParentId); parent == nil || parent.Role != v1.AgentRole_AGENT_ROLE_ROOT {
				return
			}
			p.announce(ctx, e, &notice.Notice{Kind: notice.Input, Title: held.AgentName + " waits for your permission", Body: notice.Brief(held.Tool + " " + held.Summary),
				AgentID: agentID, Tab: notice.TabTerminal, ForUser: true, Tag: "perm:" + id})
		})
	case *v1.Event_PullRequestChanged:
		pr := pl.PullRequestChanged.GetPullRequest()
		if pr == nil {
			return nil
		}
		was := pl.PullRequestChanged
		// Ready now (open, not a draft, its checks pass, read fresh) and not before: the event's `from` fields say what
		// it was. A draft taken out of draft with passing checks is missed (the event has no earlier draft flag).
		if pr.State != "open" || pr.Draft || pr.Checks != "pass" || pr.Stale || (was.FromState == "open" && was.FromChecks == "pass") {
			return nil
		}
		author := lookup(pr.AgentId)
		if author == nil {
			return nil
		}
		run, _ := r.GetRun(e.RunId)
		if !pr.Held && !d.userMerges(r, run, author.Repo) {
			return nil
		}
		return []*notice.Notice{{Kind: notice.Ready, Title: "PR #" + strconv.Itoa(int(pr.Number)) + " is ready to merge", Body: author.Name + "'s pull request: " + pr.Url, AgentID: author.Id, Tab: notice.TabChat, ForUser: true, Tag: "pr:" + pr.Url}}
	}
	return nil
}

// userRecipient is the recipient `user`: a message to the person, kept in the run, never delivered to an agent.
const userRecipient = "user"

func (p *pusher) runOf(id string) *runState {
	if p.runs == nil {
		p.runs = map[string]*runState{}
	}
	s := p.runs[id]
	if s == nil {
		s = &runState{}
		p.runs[id] = s
	}
	return s
}

// runFinished is the notice for a run whose lead just ended a turn, when the whole run is done: every other agent is
// exited or retired, or idle having reported done or failed to its parent; nothing of it waits on the user; and no
// prompt or message to the lead is still to be taken. It fires once, until the run gets work again.
func (d *Daemon) runFinished(r store.Reader, st *runState, lead *v1.Agent) *notice.Notice {
	if st.finished {
		return nil
	}
	agents, err := r.ListAgents(lead.RunId)
	if err != nil {
		return nil
	}
	// An idle agent counts as done once it told its own parent so after the last task it was given: the lead for a
	// worker, the worker for the reviewer or asker it started.
	reported := map[string]bool{}
	for _, a := range agents {
		if a.Id == lead.Id || a.State != v1.AgentState_AGENT_STATE_IDLE || a.ParentId == "" {
			continue
		}
		inbox, err := r.Inbox(a.ParentId, true)
		if err != nil {
			return nil
		}
		mine, err := r.Inbox(a.Id, true)
		if err != nil {
			return nil
		}
		// The store's order: time to the nanosecond, then id.
		var lastTask *v1.Message
		for _, m := range mine {
			if m.Type == v1.MessageType_MESSAGE_TYPE_TASK && (lastTask == nil || laterThan(m, lastTask)) {
				lastTask = m
			}
		}
		for _, m := range inbox {
			if m.GetFrom().GetId() == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_DONE || m.Type == v1.MessageType_MESSAGE_TYPE_FAILED) &&
				(lastTask == nil || laterThan(m, lastTask)) {
				reported[a.Id] = true
			}
		}
	}
	for _, a := range agents {
		if a.Id == lead.Id {
			continue
		}
		switch a.State {
		case v1.AgentState_AGENT_STATE_EXITED, v1.AgentState_AGENT_STATE_RETIRED:
		case v1.AgentState_AGENT_STATE_IDLE:
			if !reported[a.Id] {
				return nil
			}
		default:
			return nil
		}
	}
	// What the lead put to the user and nobody answered is still open.
	// (as `ListWaiting` lists it: not one from an agent that has exited).
	asked, err := r.Waiting(userRecipient)
	if err != nil {
		return nil
	}
	for _, m := range asked {
		for _, a := range agents {
			if m.RunId == lead.RunId && a.Id == m.GetFrom().GetId() && a.State != v1.AgentState_AGENT_STATE_EXITED {
				return nil
			}
		}
	}
	// A plan or question to the lead still open, as `ListWaiting` lists it: not one from an agent that has exited.
	open, err := r.Waiting(lead.Id)
	if err != nil {
		return nil
	}
	for _, m := range open {
		for _, a := range agents {
			if a.Id == m.GetFrom().GetId() && a.State != v1.AgentState_AGENT_STATE_EXITED {
				return nil
			}
		}
	}
	if und, err := r.Undelivered(lead.Id); err != nil || len(und) > 0 {
		return nil
	}
	if q, err := r.QueuedPrompts(lead.Id); err != nil || len(q) > 0 {
		return nil
	}
	st.finished = true
	run, _ := r.GetRun(lead.RunId)
	name := lead.Name
	if run != nil && run.Title != "" {
		name = run.Title
	}
	body := "Its lead is idle and nothing of the run is left to do."
	// What the user comes back to: how many pull requests the run merged.
	merged := 0
	for _, a := range agents {
		if prs, err := r.AgentPRs(a.Id); err == nil {
			for _, p := range prs {
				if p.State == "merged" {
					merged++
				}
			}
		}
	}
	if merged > 0 {
		noun := "pull requests"
		if merged == 1 {
			noun = "pull request"
		}
		body = fmt.Sprintf("%d %s merged. %s", merged, noun, body)
	}
	return &notice.Notice{Kind: notice.Finished, Title: name + " finished", Body: body, AgentID: lead.Id, Tab: notice.TabChat, ForUser: true, Tag: "run:" + lead.RunId}
}

// laterThan reports whether a came after b in the store's order: time to the nanosecond, then id.
func laterThan(a, b *v1.Message) bool {
	ta, tb := a.CreatedAt.AsTime(), b.CreatedAt.AsTime()
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.Id > b.Id
}

// record stores a notice as a UserNotice event of the run and returns the event's number.
func (d *Daemon) record(ctx context.Context, runID string, n *notice.Notice) (int64, error) {
	var seq int64
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		ev, err := tx.AppendEvent(&v1.Event{RunId: runID, AgentId: n.AgentID, Payload: &v1.Event_UserNotice{UserNotice: &v1.UserNotice{
			Kind: n.Kind, Title: n.Title, Body: n.Body, AgentId: n.AgentID, Tab: n.Tab, ForUser: n.ForUser, Tag: n.Tag}}})
		if ev != nil {
			seq = ev.Seq
		}
		return err
	})
	return seq, err
}

// getAgentOrNil is an agent as the store has it, or nil.
func (d *Daemon) getAgentOrNil(id string) *v1.Agent {
	if id == "" {
		return nil
	}
	a, err := d.store.Read(context.Background()).GetAgent(id)
	if err != nil {
		return nil
	}
	return a
}

// sendToUser stores a message of a run's lead to the person (`mesh send user --type question|plan|info`): in the
// run's messages, never rung to an agent. A question or plan waits (ListWaiting) until the user answers the lead,
// as for any lead's inbox. Only a lead may: a worker's word to the user goes through its lead.
func (d *Daemon) sendToUser(ctx context.Context, req *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error) {
	switch req.Msg.Type {
	case v1.MessageType_MESSAGE_TYPE_QUESTION, v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_INFO:
	default:
		return nil, invalid("a message to the user is a question, a plan or an info")
	}
	if strings.TrimSpace(req.Msg.Body) == "" {
		return nil, invalid("a message to the user needs a body")
	}
	if req.Msg.Until != "" || req.Msg.Re != "" {
		return nil, invalid("a message to the user takes no --until or --re")
	}
	out := &v1.SendMessageResponse{}
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSendMessageProcedure, out, func(t *store.Tx) error {
		if !IsAgent(t.Actor.Class) {
			return invalid("the user is who a device is: only a run's lead sends to `user`")
		}
		self, err := d.getAgent(t.Reader, t.Actor.Id)
		if err != nil {
			return err
		}
		if self.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			return connect.NewError(connect.CodePermissionDenied, errors.New("only a run's lead sends to the user: send to your parent, and it decides what the user sees"))
		}
		msg := &v1.Message{Id: ids.New("m", 10), RunId: self.RunId, From: t.Actor, ToId: userRecipient, ToName: userRecipient,
			Type: req.Msg.Type, Body: req.Msg.Body, CreatedAt: tsOf(t.Now()), DeliveredAt: tsOf(t.Now())}
		if err := t.PutMessage(msg); err != nil {
			return err
		}
		if _, err := t.AppendEvent(&v1.Event{RunId: self.RunId, AgentId: self.Id,
			Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: msg}}}); err != nil {
			return err
		}
		out.Message = msg
		return nil
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
