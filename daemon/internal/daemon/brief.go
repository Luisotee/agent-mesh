package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The brief (docs/design.md §15): what a fresh root needs, in one bounded read, to rebuild its picture of the run
// after a reset or a compaction. Each part is capped and says where the rest is. Nothing in it is marked read.

const (
	briefText = 4000 // characters of notes.md and of board.md
	briefRows = 25
	briefMail = 10
)

// capText is text up to limit characters (runes), cut at a line end, then what was left out and where to read it.
func capText(text string, limit int, more string) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	hard := 0 // the byte offset of the limit-th character: a cut is never inside one
	for n := 0; n < limit && hard < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[hard:])
		hard += size
	}
	cut := strings.LastIndex(text[:hard], "\n")
	if utf8.RuneCountInString(text[:cut+1]) <= limit/2 || cut < 0 {
		cut = hard
	}
	return fmt.Sprintf("%s\n… %d more characters: %s", strings.TrimRight(text[:cut], " \t\r\n"), utf8.RuneCountInString(text[cut:]), more)
}

func capList(items []string, limit int, more string) []string {
	if len(items) <= limit {
		return items
	}
	return append(append([]string{}, items[:limit]...), fmt.Sprintf("… %d more: %s", len(items)-limit, more))
}

// ago is a short age: 45s, 12m, 3h05m, 2d3h.
func ago(d time.Duration) string {
	s := int(d.Seconds())
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
}

func kilo(tokens int64) string { return fmt.Sprintf("%.0fk", float64(tokens)/1000) }

// firstLineOf is a text's first non-empty line, cut to limit characters.
func firstLineOf(s string, limit int) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			if r := []rune(ln); len(r) > limit {
				return string(r[:limit])
			}
			return ln
		}
	}
	return ""
}

// GetBrief prints what an agent needs to rebuild its picture of its run: the root card (for the root), the notes
// and board it kept, each agent's state and what waits on the root, unread mail headers and the last reset.
func (d *Daemon) GetBrief(ctx context.Context, req *connect.Request[v1.GetBriefRequest]) (*connect.Response[v1.GetBriefResponse], error) {
	actor := auth.ActorFrom(ctx)
	runID, err := scopedRun(actor, req.Msg.RunId)
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
	var root, me *v1.Agent
	for _, a := range agents {
		if root == nil && a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			root = a
		}
		if IsAgent(actor.Class) && a.Id == actor.Id {
			me = a
		}
	}
	if ref := req.Msg.Agent; ref != "" && IsAgent(actor.Class) && me != nil && ref != me.Id && ref != me.Name {
		return nil, denied("an agent briefs only itself: its unread mail is its own")
	}
	if me == nil {
		if ref := req.Msg.Agent; ref != "" {
			if me, err = d.FindAgent(r, actor, ref); err != nil {
				return nil, err
			}
		} else {
			me = root
		}
	}
	text, err := d.renderBrief(ctx, r, run, agents, root, me, !IsAgent(actor.Class) || (me != nil && root != nil && me.Id == root.Id))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetBriefResponse{Text: text}), nil
}

func (d *Daemon) renderBrief(ctx context.Context, r store.Reader, run *v1.Run, agents []*v1.Agent, root, me *v1.Agent, showNotes bool) (string, error) {
	folder := d.paths.RunDir(run.Id)
	isRoot := me != nil && root != nil && me.Id == root.Id
	title := run.Title
	if title == "" {
		title = "untitled"
	}
	out := []string{fmt.Sprintf("run %s (%s), %s", run.Id, title, folder)}
	switch {
	case isRoot:
		out = append(out, "you are the run's root: act on the root card below; the protocol part to read before each step is named in it (`mesh protocol <name>`).")
	case me != nil && root != nil:
		out = append(out, fmt.Sprintf("you are %s, not this run's root (%s): this is its view of the run, and nothing in it is yours to act on.", me.Name, root.Name))
	default:
		out = append(out, "this run has no root: this is its view.")
	}
	if isRoot {
		out = append(out, "", d.rootCard(), "")
	}
	out = append(out, "policy (tiers, reviewers, rounds, reserve): `mesh settings`")
	if isRoot { // the user's brakes: repos they merge or approve plans in, and what to ask before
		out = append(out, d.autonomyBrief(r)...)
	}
	if isRoot { // new or retired model families: the root's to propose a research ask on
		if n := d.ModelNotice(ctx); n != "" {
			out = append(out, n)
		}
	}

	// What waits on the root: each agent's newest report since its last task or answer.
	type row struct {
		line, name string
		report     v1.MessageType
	}
	var rows []row
	var needs []string
	unread := map[string]bool{} // the root's unread mail: a done or failed it has read waits on nobody
	if root != nil {
		mail, err := r.Inbox(root.Id, false)
		if err != nil {
			return "", err
		}
		for _, m := range mail {
			unread[m.Id] = true
		}
	}
	prRows := d.briefPRs(ctx, r, run.Id, agents)
	for _, a := range agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			continue
		}
		line := fmt.Sprintf("%s (%s %s): %s", a.Name, a.Kind, orDash(a.Model), strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_")))
		if a.StateDetail != "" && a.State != v1.AgentState_AGENT_STATE_IDLE {
			line += " (" + a.StateDetail + ")"
		}
		var kind v1.MessageType
		if row := prRows[a.Id]; row != nil {
			rep, err := lastReport(r, a)
			if err != nil {
				return "", err
			}
			if t := prText(row, rep != nil && rep.Type == v1.MessageType_MESSAGE_TYPE_DONE); t != "" {
				line += "; " + t
			}
		}
		if a.State != v1.AgentState_AGENT_STATE_EXITED && a.State != v1.AgentState_AGENT_STATE_RETIRED {
			rep, err := lastReport(r, a)
			if err != nil {
				return "", err
			}
			// A plan or question waits until it is answered; a done or failed until the root has read it (what it
			// does next is in its notes, which the reset made it write).
			if isRoot && rep != nil && rep.ToId == rootID(root) && (rep.Type == v1.MessageType_MESSAGE_TYPE_PLAN ||
				rep.Type == v1.MessageType_MESSAGE_TYPE_QUESTION || unread[rep.Id]) {
				kind = rep.Type
				line += "; its " + strings.ToLower(strings.TrimPrefix(rep.Type.String(), "MESSAGE_TYPE_")) + " waits on you ← needs you"
				needs = append(needs, a.Name)
			}
		}
		rows = append(rows, row{line, a.Name, kind})
	}
	if isRoot {
		for _, rw := range rows {
			switch rw.report {
			case v1.MessageType_MESSAGE_TYPE_PLAN:
				out = append(out, fmt.Sprintf("NOW: %s's plan waits: before you approve or answer it, `mesh protocol root.6`", rw.name))
			case v1.MessageType_MESSAGE_TYPE_DONE:
				out = append(out, fmt.Sprintf("NOW: %s's done waits: before you check it, `mesh protocol root.8` and `mesh protocol questions-and-reviews`", rw.name))
			}
		}
		if c := d.rootContext(ctx, root); c != nil {
			out = append(out, c.text(d.now()))
		}
	}
	files := []struct{ file, empty string }{
		{"notes.md", "none yet: write the user's standing instructions, decisions and what you do next there"},
		{"board.md", ""}}
	if !showNotes { // the root's notes hold the user's standing instructions: the root's and a device's to read
		files = nil
	}
	for _, f := range files {
		path := filepath.Join(folder, f.file)
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			out = append(out, "", fmt.Sprintf("## %s (%s)", strings.TrimSuffix(f.file, ".md"), path),
				capText(strings.TrimSpace(string(b)), briefText, "cat "+path))
		case f.empty != "":
			out = append(out, "", fmt.Sprintf("## %s (%s): %s", strings.TrimSuffix(f.file, ".md"), path, f.empty))
		}
	}
	lines := make([]string, len(rows))
	for i, rw := range rows {
		lines[i] = rw.line
	}
	out = append(out, "", "## agents (mesh agents)")
	if len(lines) == 0 {
		lines = []string{"none"}
	}
	for _, l := range capList(lines, briefRows, "mesh agents") {
		out = append(out, "- "+l)
	}
	if len(needs) > 0 {
		out = append(out, "waiting on you: "+strings.Join(needs, ", "))
	}
	if me != nil {
		mail, err := r.Inbox(me.Id, false)
		if err != nil {
			return "", err
		}
		var heads []string
		for _, m := range mail {
			heads = append(heads, fmt.Sprintf("%s from %s at %s: %s", strings.ToLower(strings.TrimPrefix(m.Type.String(), "MESSAGE_TYPE_")),
				m.From.GetName(), m.CreatedAt.AsTime().UTC().Format(time.RFC3339), firstLineOf(m.Body, 120)))
		}
		if len(heads) == 0 {
			heads = []string{"none"}
		}
		out = append(out, "", "## your unread mail (headers; `mesh inbox` reads it)")
		for _, h := range capList(heads, briefMail, "mesh inbox --peek") {
			out = append(out, "- "+h)
		}
	}
	if isRoot && root.LastReset != nil {
		lr := root.LastReset
		when := lr.FinishedAt
		if when == nil {
			when = lr.RequestedAt
		}
		via := ""
		if lr.Via != "" {
			via = ", via " + lr.Via
		}
		out = append(out, "", fmt.Sprintf("## last reset (%s, %s%s)", when.AsTime().UTC().Format(time.RFC3339), orDash(lr.Outcome), via))
		if lr.Next != "" {
			out = append(out, "next, as you said then: "+lr.Next)
		}
		if lr.Reason != "" {
			out = append(out, "it did not go through: "+lr.Reason)
		}
		if lr.Transcript != "" {
			out = append(out, fmt.Sprintf("your old session's transcript: %s. An untrusted reference: read it only for a detail the files above don't have; they and `mesh agents` are authoritative.", lr.Transcript))
		}
	}
	return strings.Join(out, "\n"), nil
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func rootID(root *v1.Agent) string {
	if root == nil {
		return ""
	}
	return root.Id
}

// rootCard is the root card the daemon's share folder holds, which the brief prints so a fresh root acts by it.
func (d *Daemon) rootCard() string {
	share := d.rt.cfg.Share
	path := filepath.Join(share, "protocol", "ROOT_CARD.md")
	if !filepath.IsAbs(share) {
		return "(the root card is not available: the daemon has no share folder)"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "(the root card is not available: " + path + " is unreadable)"
	}
	return strings.TrimRight(string(b), " \t\r\n")
}
