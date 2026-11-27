package daemon

import (
	"context"
	"slices"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// agentWait is what an agent waits on now, for the apps to word an idle state truthfully: "waiting for a message" is only
// one of five things. First match wins: a background command it started (Claude only), a wait it declared, an answer to
// its newest question or plan, a report it sent for its current task, else a message.
func (d *Daemon) agentWait(ctx context.Context, a *v1.Agent) *v1.AgentWait {
	if cmd, ok := d.backgroundCommandOf(ctx, a); ok {
		return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_BACKGROUND, Detail: cmd}
	}
	r := d.store.Read(ctx)
	parent := "user"
	if a.ParentId != "" {
		parent = a.ParentId
		if p, err := r.GetAgent(a.ParentId); err == nil && p != nil {
			parent = p.Name
		}
	}
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, From: parent}
	}
	return waitFrom(a, parent, msgs)
}

// waitFrom reads the wait off the run's messages: the newest one that settles it, scanning from the end. A task or an
// answer to the agent ends every wait before it; a message of the agent to its parent starts the one it names.
func waitFrom(a *v1.Agent, parent string, msgs []*v1.Message) *v1.AgentWait {
	to := a.ParentId // a lead has no parent: what it sends up goes to the user
	if to == "" {
		to = userRecipient
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			// What it was told last: nothing it sent before this is still open.
			return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, From: parent}
		case m.From.GetId() != a.Id || m.ToId != to:
			continue
		case m.Type == v1.MessageType_MESSAGE_TYPE_WAIT:
			return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_DECLARED, Detail: statusLine(m.Body), From: parent, Until: m.WaitUntil}
		case m.Type == v1.MessageType_MESSAGE_TYPE_QUESTION || m.Type == v1.MessageType_MESSAGE_TYPE_PLAN:
			return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_ANSWER, Detail: statusLine(m.Body), From: parent}
		case m.Type == v1.MessageType_MESSAGE_TYPE_DONE || m.Type == v1.MessageType_MESSAGE_TYPE_FAILED:
			return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_REPORTED, From: parent}
		}
	}
	return &v1.AgentWait{Kind: v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, From: parent}
}

// statusLine is a message body's first non-empty line, cut at 140 characters: what a status sentence can hold.
func statusLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > 140 {
				return string(r[:139]) + "…"
			}
			return l
		}
	}
	return ""
}

// contextMarks are the points of the context meter where the daemon acts on this agent, lowest first and below the window
// only: a lead's reset (when it resets by itself), a planner's compaction when approved, a worker's compactions. A plain
// agent is the user's own conversation: nothing is done to it, so it has none.
func (d *Daemon) contextMarks(r store.Reader, a *v1.Agent, window int64) []*v1.ContextMark {
	if a.Plain || !d.rt.adapters.HasCapability(a.Kind, capContext) {
		return nil
	}
	var marks []*v1.ContextMark
	add := func(key, label string) {
		if at, err := d.settingInt(r, key); err == nil && at > 0 && int64(at) < window {
			marks = append(marks, &v1.ContextMark{Tokens: int64(at), Label: label, Setting: key})
		}
	}
	switch {
	case a.Role == v1.AgentRole_AGENT_ROLE_ROOT:
		if auto, _ := d.Setting(r, "root_reset.auto"); auto == true {
			add("root_reset.at", "The mesh resets this lead")
		}
	case a.Phase == v1.AgentPhase_AGENT_PHASE_PLANNING:
		add("compact.approve", "The mesh compacts this plan when it is approved")
	default:
		add("compact.followup", "The mesh compacts before a follow-up task")
		add("compact.claude", "The mesh compacts this conversation")
	}
	slices.SortFunc(marks, func(x, y *v1.ContextMark) int { return int(x.Tokens - y.Tokens) })
	return marks
}
