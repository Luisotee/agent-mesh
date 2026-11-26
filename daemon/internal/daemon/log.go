package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/events"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

var reviewGistRE = regexp.MustCompile(`^(Review \(read-only\) of (\S+?)(?: at \S+)?):\s`)

// StreamLog streams a run's events and messages as a chronological timeline (`mesh log`), with optional
// --agent filtering, --lines tailing, and --follow live streaming until the run stops.
func (d *Daemon) StreamLog(ctx context.Context, req *connect.Request[v1.StreamLogRequest],
	stream *connect.ServerStream[v1.StreamLogResponse]) error {
	m := req.Msg
	if m.Lines < 0 {
		return invalid("--lines takes a count of events (0: all)")
	}
	actor := auth.ActorFrom(ctx)
	r := d.store.Read(ctx)

	var runID string
	var err error
	if IsAgent(actor.Class) {
		runID, err = scopedReadRun(actor, m.RunId)
		if err != nil {
			return err
		}
	} else {
		runID = m.RunId
		if runID == "" && m.Agent != "" {
			a, err := d.findAgentCrossRun(r, actor, m.Agent)
			if err != nil {
				return err
			}
			runID = a.RunId
		}
	}
	if runID == "" {
		return invalid("give --run-id (or --agent)")
	}
	run, err := r.GetRun(runID)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("no run %q", runID)
	}
	if err != nil {
		return err
	}

	agents, err := r.ListAgents(runID)
	if err != nil {
		return err
	}
	agentNames := make(map[string]string, len(agents))
	var filterID, filterName string
	for _, a := range agents {
		agentNames[a.Id] = a.Name
		if m.Agent != "" && (a.Id == m.Agent || a.Name == m.Agent) {
			filterID, filterName = a.Id, a.Name
		}
	}
	if m.Agent != "" && filterID == "" {
		filterID, filterName = m.Agent, m.Agent
	}

	var ch <-chan *v1.Event
	var unsub func()
	if m.Follow && run.Status != v1.RunStatus_RUN_STATUS_CLOSED {
		ch, unsub = d.bus.SubscribeRun(runID, false)
		defer unsub()
	}

	backlog, err := r.ListEvents(runID)
	if err != nil {
		return err
	}
	var entries []*v1.LogEntry
	var lastSeq int64
	sawStop := false
	for _, ev := range backlog {
		lastSeq = ev.Seq
		if sp := ev.GetAgentSpawned(); sp != nil && sp.Agent != nil {
			agentNames[sp.Agent.Id] = sp.Agent.Name
		}
		if ev.GetRunStopped() != nil {
			sawStop = true
		}
		if entry, ok := logEntryFromEvent(ev, agentNames, filterID, filterName); ok {
			entries = append(entries, entry)
		}
	}
	if m.Lines > 0 && len(entries) > int(m.Lines) {
		entries = entries[len(entries)-int(m.Lines):]
	}
	for _, entry := range entries {
		if err := stream.Send(&v1.StreamLogResponse{Entry: entry}); err != nil {
			return err
		}
	}
	if !m.Follow || sawStop || run.Status == v1.RunStatus_RUN_STATUS_CLOSED {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.stopping:
			return nil
		case ev, ok := <-ch:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted, events.ErrSlow)
			}
			if ev.Seq <= lastSeq || ev.RunId != runID {
				continue
			}
			lastSeq = ev.Seq
			if sp := ev.GetAgentSpawned(); sp != nil && sp.Agent != nil {
				agentNames[sp.Agent.Id] = sp.Agent.Name
			}
			if entry, ok := logEntryFromEvent(ev, agentNames, filterID, filterName); ok {
				if err := stream.Send(&v1.StreamLogResponse{Entry: entry}); err != nil {
					return err
				}
			}
			if ev.GetRunStopped() != nil {
				return nil
			}
		}
	}
}

func logEntryFromEvent(ev *v1.Event, agentNames map[string]string, filterID, filterName string) (*v1.LogEntry, bool) {
	nameOf := func(id string) string {
		if id == "" {
			return ""
		}
		if n := agentNames[id]; n != "" {
			return n
		}
		return id
	}
	matches := func(ids ...string) bool {
		if filterID == "" && filterName == "" {
			return true
		}
		for _, id := range ids {
			if id != "" && (id == filterID || id == filterName || nameOf(id) == filterName) {
				return true
			}
		}
		return false
	}

	entry := &v1.LogEntry{
		Seq:     ev.Seq,
		Time:    ev.Time,
		RunId:   ev.RunId,
		AgentId: ev.AgentId,
	}

	switch p := ev.Payload.(type) {
	case *v1.Event_RunStarted:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "init"
		if p.RunStarted.GetRun() != nil {
			entry.Detail = p.RunStarted.Run.Title
		}
	case *v1.Event_WorktreeCreated:
		wt := p.WorktreeCreated.GetWorktree()
		if !matches(ev.AgentId) || wt == nil {
			return nil, false
		}
		entry.Kind = "worktree"
		entry.Who = wt.Slug
		entry.Detail = joinFields(wt.Branch, wt.Path)
	case *v1.Event_PullRequestChanged:
		pr := p.PullRequestChanged.GetPullRequest()
		if !matches(ev.AgentId) || pr == nil {
			return nil, false
		}
		entry.Kind = "pr"
		entry.Who = nameOf(ev.AgentId)
		ch := p.PullRequestChanged
		change := fmt.Sprintf("#%d %s", pr.Number, arrow(ch.FromState, pr.State))
		if pr.Checks != "" || ch.FromChecks != "" {
			change += ", checks " + arrow(ch.FromChecks, pr.Checks)
		}
		entry.Detail = joinFields(change, pr.Url)
	case *v1.Event_AgentSpawned:
		a := p.AgentSpawned.GetAgent()
		if a == nil || !matches(a.Id, a.Name) {
			return nil, false
		}
		entry.Kind = "spawn"
		entry.Who = a.Name
		entry.AgentId = a.Id
		entry.Detail = kvLine("role", enumShort(a.Role.String(), "AGENT_ROLE_"),
			"kind", a.Kind, "model", a.Model, "effort", a.Effort, "branch", a.Branch)
	case *v1.Event_MessageSent:
		msg := p.MessageSent.GetMessage()
		if msg == nil {
			return nil, false
		}
		if msg.From.GetId() == meshActor.Id {
			if strings.HasPrefix(msg.Body, nudgeTag) {
				if !matches(msg.ToId, msg.ToName) {
					return nil, false
				}
				entry.Kind = "nudge"
				entry.Who = msg.ToName
				if entry.Who == "" {
					entry.Who = nameOf(msg.ToId)
				}
				entry.AgentId = msg.ToId
				entry.Detail = logGist(strings.TrimPrefix(msg.Body, nudgeTag), 100)
				return entry, true
			}
			if alertKind, subjID, rest, ok := parseAlertMessage(msg.Body); ok {
				if !matches(subjID, msg.ToId, msg.ToName) {
					return nil, false
				}
				entry.Kind = "alert"
				entry.Who = nameOf(subjID)
				entry.AgentId = subjID
				entry.Detail = alertKind + ": " + logGist(rest, 100)
				return entry, true
			}
		}
		if !matches(msg.From.GetId(), msg.From.GetName(), msg.ToId, msg.ToName) {
			return nil, false
		}
		fromName := msg.From.GetName()
		if fromName == "" {
			fromName = nameOf(msg.From.GetId())
		}
		toName := msg.ToName
		if toName == "" {
			toName = nameOf(msg.ToId)
		}
		entry.Kind = "send"
		entry.Who = fmt.Sprintf("%s → %s  %s", fromName, toName, enumShort(msg.Type.String(), "MESSAGE_TYPE_"))
		if rm := reviewGistRE.FindStringSubmatch(strings.TrimSpace(msg.Body)); rm != nil {
			entry.Detail = rm[1]
		} else {
			entry.Detail = logGist(msg.Body, 100)
		}
	case *v1.Event_MessageDelivered:
		if !matches(ev.AgentId) {
			return nil, false
		}
		md := p.MessageDelivered
		entry.Kind = "delivered"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = kvLine("msg", md.MessageId, "via", md.Via, "tries", fmt.Sprint(md.Tries))
	case *v1.Event_DoorbellFailed:
		if !matches(ev.AgentId) {
			return nil, false
		}
		df := p.DoorbellFailed
		entry.Kind = "doorbell_failed"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = joinFields(fmt.Sprintf("tries=%d", df.Tries), df.Reason)
	case *v1.Event_DoorbellHeld:
		if !matches(ev.AgentId) {
			return nil, false
		}
		dh := p.DoorbellHeld
		entry.Kind = "doorbell_held"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = joinFields(dh.Reason, fmt.Sprintf("draft=%q", dh.Draft))
	case *v1.Event_AgentStateChanged:
		if !matches(ev.AgentId) {
			return nil, false
		}
		sc := p.AgentStateChanged
		entry.Kind = "state"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = enumShort(sc.From.String(), "AGENT_STATE_") + " → " + enumShort(sc.To.String(), "AGENT_STATE_")
		if sc.Detail != "" {
			entry.Detail += " (" + logGist(sc.Detail, 80) + ")"
		}
	case *v1.Event_AgentReopenRequested:
		if !matches(ev.AgentId) {
			return nil, false
		}
		r := p.AgentReopenRequested.GetReopen()
		entry.Kind = "swap_requested"
		entry.Who = nameOf(ev.AgentId)
		if r != nil {
			entry.Detail = kvLine("why", r.Why, "model", r.Model, "effort", r.Effort, "reason", r.Reason)
		}
	case *v1.Event_AgentReopened:
		if !matches(ev.AgentId) {
			return nil, false
		}
		r := p.AgentReopened.GetReopen()
		entry.Kind = "swap"
		entry.Who = nameOf(ev.AgentId)
		if r != nil {
			entry.Detail = kvLine("why", r.Why, "model", r.Model, "effort", r.Effort, "reason", r.Reason)
		}
	case *v1.Event_AgentReopenDropped:
		if !matches(ev.AgentId) {
			return nil, false
		}
		d := p.AgentReopenDropped
		entry.Kind = "swap_dropped"
		entry.Who = nameOf(ev.AgentId)
		why := ""
		if d.Reopen != nil {
			why = d.Reopen.Why
		}
		entry.Detail = kvLine("why", why, "reason", d.Reason)
	case *v1.Event_AgentPhaseChanged:
		if !matches(ev.AgentId) {
			return nil, false
		}
		pc := p.AgentPhaseChanged
		entry.Kind = "phase"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = enumShort(pc.From.String(), "AGENT_PHASE_") + " → " + enumShort(pc.To.String(), "AGENT_PHASE_")
		if pc.Why != "" {
			entry.Detail += " (" + pc.Why + ")"
		}
	case *v1.Event_AgentRoleChanged:
		if !matches(ev.AgentId) {
			return nil, false
		}
		rc := p.AgentRoleChanged
		entry.Kind = "role"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = enumShort(rc.From.String(), "AGENT_ROLE_") + " → " + enumShort(rc.To.String(), "AGENT_ROLE_")
		if rc.Why != "" {
			entry.Detail += " (" + rc.Why + ")"
		}
	case *v1.Event_AgentRetired:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "retire"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = p.AgentRetired.Reason
	case *v1.Event_AgentExited:
		if !matches(ev.AgentId) {
			return nil, false
		}
		ex := p.AgentExited
		entry.Kind = "exited"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = kvLine("code", fmt.Sprint(ex.ExitCode), "signal", fmt.Sprint(ex.Signal), "reason", ex.Reason)
	case *v1.Event_AgentAdopted:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "adopted"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = "terminal=" + p.AgentAdopted.TerminalId
	case *v1.Event_AgentCompacted:
		if !matches(ev.AgentId) {
			return nil, false
		}
		c := p.AgentCompacted
		entry.Kind = "compacted"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = fmt.Sprintf("%s %s %s %d→%d", c.Outcome, c.Via, c.Why, c.TokensBefore, c.TokensAfter)
	case *v1.Event_RootResetRequested:
		if !matches(ev.AgentId) {
			return nil, false
		}
		rr := p.RootResetRequested.GetRootReset()
		entry.Kind = "reset_requested"
		entry.Who = nameOf(ev.AgentId)
		if rr != nil {
			entry.Detail = kvLine("via", rr.Via, "mode", rr.Mode, "next", rr.Next)
		}
	case *v1.Event_RootResetFinished:
		if !matches(ev.AgentId) {
			return nil, false
		}
		rr := p.RootResetFinished.GetRootReset()
		entry.Kind = "reset"
		entry.Who = nameOf(ev.AgentId)
		if rr != nil {
			entry.Detail = kvLine("outcome", rr.Outcome, "via", rr.Via, "mode", rr.Mode, "reason", rr.Reason)
		}
	case *v1.Event_HookDecodeFailed:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "hook_failed"
		entry.Who = nameOf(ev.AgentId)
		entry.Detail = p.HookDecodeFailed.Error
	case *v1.Event_ReserveChanged:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "reserve"
		entry.Detail = fmt.Sprintf("over_reserve=%v", p.ReserveChanged.OverReserve)
	case *v1.Event_RunStopped:
		if !matches(ev.AgentId) {
			return nil, false
		}
		entry.Kind = "stop"
		entry.Detail = "run stopped"
	default:
		return nil, false
	}
	return entry, true
}

func parseAlertMessage(body string) (kind, agentID, rest string, ok bool) {
	if !strings.HasPrefix(body, alertTag) {
		return "", "", "", false
	}
	after := strings.TrimPrefix(body, alertTag)
	end := strings.Index(after, alertEnds)
	if end < 0 {
		return "", "", "", false
	}
	head := after[:end]
	parts := strings.SplitN(head, " ", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	return parts[0], parts[1], strings.TrimSpace(after[end+len(alertEnds):]), true
}

func logGist(s string, limit int) string {
	line := firstLineOf(s, limit)
	return strings.TrimSpace(line)
}

func enumShort(s, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s, prefix))
}

// arrow is `old → new`, or just `new` when nothing changed or there was no old.
func arrow(from, to string) string {
	if from == "" || from == to {
		return to
	}
	return from + " → " + to
}

func kvLine(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			parts = append(parts, kv[i]+"="+kv[i+1])
		}
	}
	return strings.Join(parts, " ")
}

func joinFields(xs ...string) string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, " ")
}
