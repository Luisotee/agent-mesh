package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The run overview (`GetRunOverview`): one run as the user who comes back to it needs it. Everything in it is read from
// stored state (agents, messages, pull requests the refresher already keeps, held tasks, decisions, events): no GitHub
// call, no transcript read, no terminal read. The lead writes only what needs judgment (the headline, the decisions).
const (
	overviewReportChars = 160  // characters of a report's first line
	overviewDonePRs     = 30   // merged or closed pull requests listed
	overviewEventLimit  = 5000 // events read after a cursor
)

// overviewAgent is one agent's row: its about line, what it does now, its pull requests and its last report.
func (d *Daemon) overviewAgent(a *v1.Agent, wait *v1.AgentWait, prs []*v1.PullRequest, report *v1.Message) *v1.OverviewAgent {
	row := &v1.OverviewAgent{AgentId: a.Id, Name: a.Name, Role: a.Role, Kind: a.Kind, State: a.State, About: a.About,
		Now: nowText(a, wait), PullRequests: prs, LastActivity: a.LastActivity}
	if report != nil && (report.Type == v1.MessageType_MESSAGE_TYPE_DONE || report.Type == v1.MessageType_MESSAGE_TYPE_FAILED) {
		row.Report, row.ReportType = firstLineOf(report.Body, overviewReportChars), report.Type
	}
	return row
}

// nowText is what an agent does or waits for, as one sentence (the apps' status sentence, from the host's own facts).
func nowText(a *v1.Agent, w *v1.AgentWait) string {
	detail := func(prefix string) string {
		if a.StateDetail == "" {
			return prefix
		}
		return prefix + ": " + a.StateDetail
	}
	switch a.State {
	case v1.AgentState_AGENT_STATE_STARTING:
		return "Starting up"
	case v1.AgentState_AGENT_STATE_WORKING:
		return "Working"
	case v1.AgentState_AGENT_STATE_NEEDS_INPUT:
		return detail("Needs you")
	case v1.AgentState_AGENT_STATE_IDLE:
		if w == nil {
			return "Idle"
		}
		switch w.Kind {
		case v1.AgentWaitKind_AGENT_WAIT_KIND_DECLARED:
			if w.Detail != "" {
				return "Waiting: " + w.Detail
			}
			return "Waiting"
		case v1.AgentWaitKind_AGENT_WAIT_KIND_ANSWER:
			return "Asked " + orDefault(w.From, "its parent") + ", waiting for the answer"
		case v1.AgentWaitKind_AGENT_WAIT_KIND_REPORTED:
			return "Reported to " + orDefault(w.From, "its parent") + ", nothing owed"
		}
		return "Idle"
	case v1.AgentState_AGENT_STATE_LIMITED:
		return detail("Hit a usage limit")
	case v1.AgentState_AGENT_STATE_ERROR:
		return detail("Its last turn ended in an error")
	case v1.AgentState_AGENT_STATE_UNKNOWN:
		return "Quiet for a while: state unknown"
	case v1.AgentState_AGENT_STATE_EXITED:
		if a.ExitCode == 0 && (a.StateDetail == "" || a.StateDetail == "exited") {
			return "Finished"
		}
		if a.StateDetail == "lost" || a.ExitCode < 0 {
			return "Lost: its terminal is gone"
		}
		return fmt.Sprintf("Exited with code %d", a.ExitCode)
	case v1.AgentState_AGENT_STATE_RETIRED:
		return "Retired"
	}
	return ""
}

// inTrouble says whether an agent's state asks the user to look: it failed, was lost, hit a usage limit or ended a turn
// in an error. (A pull request with failing checks shows in `open_prs`.)
func inTrouble(a *v1.Agent) bool {
	switch a.State {
	case v1.AgentState_AGENT_STATE_LIMITED, v1.AgentState_AGENT_STATE_ERROR, v1.AgentState_AGENT_STATE_UNKNOWN:
		return true
	case v1.AgentState_AGENT_STATE_EXITED:
		return a.ExitCode != 0 || a.StateDetail == "lost"
	}
	return false
}

// matters says whether an event is one of the few the overview counts as something that happened: an agent started, a
// pull request changed state, a plan, question, done or failed.
func matters(ev *v1.Event) bool {
	switch p := ev.Payload.(type) {
	case *v1.Event_AgentSpawned:
		return !p.AgentSpawned.GetAgent().GetPeer()
	case *v1.Event_PullRequestChanged:
		return p.PullRequestChanged.GetPullRequest().GetState() != p.PullRequestChanged.GetFromState()
	case *v1.Event_MessageSent:
		switch p.MessageSent.GetMessage().GetType() {
		case v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
			v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED:
			return true
		}
	}
	return false
}

// GetRunOverview builds the overview of one run (see the constants above for what it reads).
func (d *Daemon) GetRunOverview(ctx context.Context, req *connect.Request[v1.GetRunOverviewRequest]) (*connect.Response[v1.GetRunOverviewResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("name the run (run_id)")
	}
	r := d.store.Read(ctx)
	run, err := r.GetRun(runID)
	if err != nil {
		return nil, notFound("no run %q", runID)
	}
	agents, err := r.ListAgents(runID)
	if err != nil {
		return nil, err
	}
	for i, a := range agents {
		agents[i] = d.agentView(a)
	}
	leadFirst(agents)
	byID := map[string]*v1.Agent{}
	for _, a := range agents {
		byID[a.Id] = a
	}
	msgs, err := r.RunMessages(runID, "", -1) // once for every agent's last report and wait
	if err != nil {
		return nil, err
	}
	reports := lastReports(msgs)
	var waitMax, unseen time.Duration
	if v, err := d.Setting(r, "accountability.wait_max"); err == nil {
		waitMax = time.Duration(v.(float64) * float64(time.Second))
	}
	if v, err := d.Setting(r, "accountability.wait_unseen"); err == nil {
		unseen = time.Duration(v.(float64) * float64(time.Second))
	}
	now := d.now()
	seq, err := r.LastSeq()
	if err != nil {
		return nil, err
	}
	out := &v1.RunOverview{RunId: run.Id, Title: run.Title, Status: run.Status, Seq: seq, Headline: run.Headline, HeadlineAt: run.HeadlineAt}

	w, err := d.ListWaiting(ctx, connect.NewRequest(&v1.ListWaitingRequest{RunId: runID}))
	if err != nil {
		return nil, err
	}
	out.NeedsYou = w.Msg.Waiting

	for _, a := range agents {
		prs, err := r.AgentPRs(a.Id)
		if err != nil {
			return nil, err
		}
		parent := "user"
		if p := byID[a.ParentId]; p != nil {
			parent = p.Name
		}
		wait := waitFrom(a, parent, msgs, now, waitMax, unseen, func(at time.Time) bool { return d.waits.peek(a, at) })
		row := d.overviewAgent(a, wait, prs, reports[a.Id])
		for _, p := range prs {
			op := &v1.OverviewPR{PullRequest: p, AgentName: a.Name, About: a.About}
			switch p.State {
			case "merged", "closed":
				out.DonePrs = append(out.DonePrs, op)
			default:
				out.OpenPrs = append(out.OpenPrs, op)
			}
		}
		switch {
		case a.State == v1.AgentState_AGENT_STATE_RETIRED || a.State == v1.AgentState_AGENT_STATE_EXITED && !inTrouble(a):
			// Over: what it did shows in the pull requests and reports below.
		case inTrouble(a):
			out.Trouble = append(out.Trouble, row)
		case a.Peer && a.State == v1.AgentState_AGENT_STATE_IDLE:
			// A peer between asks is standing by, not part of the run's work.
		case a.State == v1.AgentState_AGENT_STATE_IDLE:
			out.Idle = append(out.Idle, row)
		default:
			out.Working = append(out.Working, row)
		}
		if a.Role == v1.AgentRole_AGENT_ROLE_WORKER && len(prs) == 0 && row.ReportType == v1.MessageType_MESSAGE_TYPE_DONE {
			out.DoneReports = append(out.DoneReports, row)
		}
	}
	sort.SliceStable(out.DonePrs, func(i, j int) bool {
		return out.DonePrs[i].PullRequest.CheckedAt.AsTime().After(out.DonePrs[j].PullRequest.CheckedAt.AsTime())
	})
	if len(out.DonePrs) > overviewDonePRs {
		out.DonePrs = out.DonePrs[:overviewDonePRs]
	}
	if out.Held, err = r.HeldTasks(runID); err != nil {
		return nil, err
	}
	if out.Decisions, err = r.Decisions(runID, overviewDecide); err != nil {
		return nil, err
	}
	if run.HeadlineSeq > 0 {
		if err := eachEventAfter(r, run.HeadlineSeq, runID, func(ev *v1.Event) {
			if matters(ev) {
				out.EventsSinceHeadline++
			}
		}); err != nil {
			return nil, err
		}
	}
	if since := req.Msg.SinceSeq; since > 0 {
		out.Since = &v1.OverviewSince{}
		if err := eachEventAfter(r, since, runID, func(ev *v1.Event) {
			switch p := ev.Payload.(type) {
			case *v1.Event_AgentSpawned:
				if !p.AgentSpawned.GetAgent().GetPeer() {
					out.Since.Started++
				}
			case *v1.Event_PullRequestChanged:
				if pr := p.PullRequestChanged.GetPullRequest(); pr.GetState() != p.PullRequestChanged.GetFromState() {
					switch pr.GetState() {
					case "merged":
						out.Since.Merged++
					case "closed":
						out.Since.Closed++
					}
				}
			case *v1.Event_MessageSent:
				switch p.MessageSent.GetMessage().GetType() {
				case v1.MessageType_MESSAGE_TYPE_DONE:
					out.Since.Done++
				case v1.MessageType_MESSAGE_TYPE_FAILED:
					out.Since.Failed++
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&v1.GetRunOverviewResponse{Overview: out}), nil
}

// eachEventAfter calls fn for every event of the run after seq, a page at a time: a busy run holds more events than
// one page, and a count of what happened since must not stop at the first.
func eachEventAfter(r store.Reader, seq int64, runID string, fn func(*v1.Event)) error {
	for {
		evs, err := r.EventsAfter(seq, runID, overviewEventLimit)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.RunId == runID {
				fn(ev)
			}
			seq = ev.Seq
		}
		if len(evs) < overviewEventLimit {
			return nil
		}
	}
}

// lastReports is each agent's newest report to its parent (plan, question, done or failed) since the last task or answer
// it got, read in one pass over the run's messages.
func lastReports(msgs []*v1.Message) map[string]*v1.Message {
	out := map[string]*v1.Message{}
	for _, m := range msgs {
		if (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER) && m.ToId != "" {
			delete(out, m.ToId)
		}
		if id := m.From.GetId(); id != "" && isReport(m.Type) {
			out[id] = m
		}
	}
	return out
}

// overviewBrief is the part of a lead's brief that the overview adds: the headline with its age, and the decisions.
func (d *Daemon) overviewBrief(r store.Reader, run *v1.Run) []string {
	var out []string
	if run.Headline != "" {
		age := ago(d.now().Sub(run.HeadlineAt.AsTime()))
		stale := ""
		if run.HeadlineSeq > 0 {
			n := 0
			if err := eachEventAfter(r, run.HeadlineSeq, run.Id, func(ev *v1.Event) {
				if matters(ev) {
					n++
				}
			}); err == nil {
				if n > 0 {
					stale = fmt.Sprintf(", %d things happened since: update it with `mesh headline`", n)
				}
			}
		}
		out = append(out, fmt.Sprintf("headline (what the user reads first; written %s ago%s): %s", age, stale, strings.ReplaceAll(run.Headline, "\n", " / ")))
	} else {
		out = append(out, "headline: none yet. The user reads it first when they come back: `mesh headline --text \"<where the run is; what comes next>\"` at each phase boundary.")
	}
	if decs, err := r.Decisions(run.Id, overviewDecide); err == nil && len(decs) > 0 {
		out = append(out, "decisions you recorded for the user (`mesh decision`; newest first):")
		for _, dec := range decs {
			out = append(out, fmt.Sprintf("- %s: %s", dec.At.AsTime().UTC().Format("01-02 15:04Z"), strings.ReplaceAll(dec.Text, "\n", " / ")))
		}
	}
	return out
}
