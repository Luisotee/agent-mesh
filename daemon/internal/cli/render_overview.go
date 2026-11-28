package cli

import (
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// renderOverview prints a run's overview as the sections the user reads in order: what waits, what broke, the lead's
// headline, who works on what, what was merged, what is next, the decisions.
func renderOverview(m proto.Message, w io.Writer) {
	o := m.(*v1.GetRunOverviewResponse).Overview
	title := o.Title
	if title == "" {
		title = "untitled"
	}
	fmt.Fprintf(w, "run %s (%s), %s\n", o.RunId, title, enumLower(o.Status, "RUN_STATUS_"))
	if s := o.Since; s != nil {
		fmt.Fprintf(w, "since you looked: %d merged, %d closed, %d started, %d done, %d failed\n", s.Merged, s.Closed, s.Started, s.Done, s.Failed)
	}
	section := func(name string) { fmt.Fprintf(w, "\n%s\n", name) }
	agent := func(a *v1.OverviewAgent) {
		fmt.Fprintf(w, "  %s", a.Name)
		if a.About != "" {
			fmt.Fprintf(w, " — %s", a.About)
		}
		fmt.Fprintf(w, "\n      %s", a.Now)
		for _, p := range a.PullRequests {
			fmt.Fprintf(w, "; PR #%d %s%s", p.Number, p.State, checksText(p))
		}
		if a.Report != "" {
			fmt.Fprintf(w, "; %s: %s", enumLower(a.ReportType, "MESSAGE_TYPE_"), a.Report)
		}
		fmt.Fprintln(w)
	}
	pr := func(p *v1.OverviewPR) {
		fmt.Fprintf(w, "  #%d %s%s  %s", p.PullRequest.Number, p.PullRequest.State, checksText(p.PullRequest), p.AgentName)
		if p.About != "" {
			fmt.Fprintf(w, " — %s", p.About)
		}
		fmt.Fprintln(w)
	}

	if len(o.NeedsYou) > 0 {
		section("NEEDS YOU")
		for _, n := range o.NeedsYou {
			who := "the lead"
			if n.ForUser {
				who = "you"
			}
			fmt.Fprintf(w, "  %s: %s waits on %s", enumLower(n.Kind, "WAITING_KIND_"), n.AgentId, who)
			if n.Detail != "" {
				fmt.Fprintf(w, " (%s)", n.Detail)
			}
			fmt.Fprintln(w)
		}
	}
	if len(o.Trouble) > 0 {
		section("TROUBLE")
		for _, a := range o.Trouble {
			agent(a)
		}
	}
	section("HEADLINE")
	switch {
	case o.Headline == "":
		fmt.Fprintln(w, "  the lead wrote none yet")
	default:
		for _, ln := range strings.Split(o.Headline, "\n") {
			fmt.Fprintf(w, "  %s\n", ln)
		}
		stale := ""
		if o.EventsSinceHeadline > 0 {
			stale = fmt.Sprintf(", %d things happened since", o.EventsSinceHeadline)
		}
		fmt.Fprintf(w, "  (written %s%s)\n", o.HeadlineAt.AsTime().UTC().Format("2006-01-02 15:04Z"), stale)
	}
	if len(o.Working) > 0 {
		section("WORKING")
		for _, a := range o.Working {
			agent(a)
		}
	}
	if len(o.Idle) > 0 {
		section("IDLE")
		for _, a := range o.Idle {
			agent(a)
		}
	}
	if len(o.DonePrs) > 0 || len(o.DoneReports) > 0 {
		section("DONE")
		for _, p := range o.DonePrs {
			pr(p)
		}
		for _, a := range o.DoneReports {
			agent(a)
		}
	}
	if len(o.OpenPrs) > 0 || len(o.Held) > 0 {
		section("NEXT")
		for _, p := range o.OpenPrs {
			pr(p)
		}
		for _, h := range o.Held {
			fmt.Fprintf(w, "  held for %s: %s\n", h.After, heldLine(h.Body, 100))
		}
	}
	if len(o.Decisions) > 0 {
		section("DECISIONS")
		for _, d := range o.Decisions {
			fmt.Fprintf(w, "  %s  %s\n", d.At.AsTime().UTC().Format("01-02 15:04Z"), strings.ReplaceAll(d.Text, "\n", " / "))
		}
	}
}

func checksText(p *v1.PullRequest) string {
	if p.Checks == "" {
		return ""
	}
	return ", checks " + p.Checks
}

func heldLine(s string, limit int) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			if r := []rune(ln); len(r) > limit {
				return string(r[:limit]) + "…"
			}
			return ln
		}
	}
	return ""
}
