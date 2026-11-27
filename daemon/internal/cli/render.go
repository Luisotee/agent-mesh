package cli

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/skip2/go-qrcode"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A few commands are read by agents whose context is the scarce thing (an inbox, the policy): their response
// prints as short text by default, and `--format json` prints the whole response as every other command does.
// This is the CLI's own view of the response: the API and the other clients are untouched.
var renderers = map[string]struct {
	into func() proto.Message
	text func(proto.Message, io.Writer)
}{
	"inbox":           {func() proto.Message { return &v1.ReadInboxResponse{} }, renderInbox},
	"settings":        {func() proto.Message { return &v1.ListSettingsResponse{} }, renderSettings},
	"usage":           {func() proto.Message { return &v1.GetUsageResponse{} }, renderUsage},
	"cost":            {func() proto.Message { return &v1.GetCostResponse{} }, renderCost},
	"models":          {func() proto.Message { return &v1.CheckModelsResponse{} }, renderModels},
	"model":           {func() proto.Message { return &v1.ResolveModelResponse{} }, renderModel},
	"import-settings": {func() proto.Message { return &v1.ImportSettingsResponse{} }, renderImportSettings},
	"conflicts":       {func() proto.Message { return &v1.CheckConflictsResponse{} }, renderConflicts},
	"tools":           {func() proto.Message { return &v1.ListToolsResponse{} }, renderTools},
	"tools-servers":   {func() proto.Message { return &v1.ListMCPServersResponse{} }, renderToolsServers},
	"brief":           {func() proto.Message { return &v1.GetBriefResponse{} }, renderBrief},
	"reset":           {func() proto.Message { return &v1.ResetRootResponse{} }, renderReset},
	"ring":            {func() proto.Message { return &v1.RingAgentResponse{} }, renderRing},
	"permissions":     {func() proto.Message { return &v1.ListPermissionsResponse{} }, renderPermissions},
	"permission":      {func() proto.Message { return &v1.DecidePermissionResponse{} }, renderPermission},
	"check":           {func() proto.Message { return &v1.CheckAgentResponse{} }, renderCheck},
	"pair":            {func() proto.Message { return &v1.CreatePairingResponse{} }, renderPair},
	"ask":             {func() proto.Message { return &v1.AskResponse{} }, renderAsk},
	"asks":            {func() proto.Message { return &v1.ListAsksResponse{} }, renderAsks},
}

type streamState struct {
	lastDay string
}

var streamRenderers = map[string]struct {
	into func() proto.Message
	text func(proto.Message, io.Writer, *streamState)
}{
	"log": {func() proto.Message { return &v1.StreamLogResponse{} }, renderLog},
}

// hasText reports whether a command has a text form (and so a --format flag).
func hasText(cli string) bool {
	if cli == "open" {
		return true
	}
	if _, ok := renderers[cli]; ok {
		return true
	}
	_, ok := streamRenderers[cli]
	return ok
}

// renderStream prints one streamed dynamic response in its command's text form.
func renderStream(cli string, res proto.Message, w io.Writer, st *streamState) error {
	r := streamRenderers[cli]
	raw, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	typed := r.into()
	if err := proto.Unmarshal(raw, typed); err != nil {
		return err
	}
	r.text(typed, w, st)
	return nil
}

// renderText prints res, a dynamic response, in its command's text form.
func renderText(cli string, res proto.Message, w io.Writer) error {
	r := renderers[cli]
	raw, err := proto.Marshal(res)
	if err != nil {
		return err
	}
	typed := r.into()
	if err := proto.Unmarshal(raw, typed); err != nil {
		return err
	}
	r.text(typed, w)
	return nil
}

// renderInbox prints each message as a header line, then its body: who, what kind, when, and its id.
func renderInbox(m proto.Message, w io.Writer) {
	msgs := m.(*v1.ReadInboxResponse).Messages
	if len(msgs) == 0 {
		fmt.Fprintln(w, "no messages")
		return
	}
	for i, msg := range msgs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		id := msg.Id
		if msg.Re != "" {
			id += ", on task " + msg.Re // a report names the task it answers
		}
		fmt.Fprintf(w, "── %s from %s at %s (%s) ──\n", enumLower(msg.Type, "MESSAGE_TYPE_"), msg.From.GetName(),
			msg.CreatedAt.AsTime().UTC().Format("2006-01-02T15:04:05Z"), id)
		fmt.Fprintln(w, strings.TrimRight(msg.Body, "\n"))
	}
}

// renderSettings prints one `key = value` line per setting, and the default after an override.
func renderSettings(m proto.Message, w io.Writer) {
	settings := m.(*v1.ListSettingsResponse).Settings
	if len(settings) == 0 {
		fmt.Fprintln(w, "no settings")
		return
	}
	for _, s := range settings {
		line := s.Key + " = " + jsonOf(s.Value)
		if s.IsSet {
			line += "   (set; default " + jsonOf(s.Default) + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func jsonOf(m proto.Message) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return "null"
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return "?"
	}
	return string(b)
}

func enumLower(e interface{ String() string }, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(e.String(), prefix))
}

func renderUsage(m proto.Message, w io.Writer) {
	u := m.(*v1.GetUsageResponse)
	if c := u.Claude; c != nil && len(c.Windows) > 0 {
		parts := make([]string, 0, len(c.Windows))
		for _, win := range c.Windows {
			res := "?"
			if win.ResetsAt != nil {
				res = win.ResetsAt.AsTime().Local().Format("15:04")
			}
			parts = append(parts, fmt.Sprintf("%s %.0f%% (resets %s)", win.Label, win.Used, res))
		}
		line := "Claude: " + strings.Join(parts, " · ")
		if c.Source != "" && c.Source != "live" {
			line += " [" + c.Source + "]"
		}
		if len(c.AtReserve) > 0 {
			line += " — at the reserve: " + strings.Join(c.AtReserve, ", ")
		}
		if len(c.ReviewsOnly) > 0 {
			line += " — takes only PR reviews: " + strings.Join(c.ReviewsOnly, ", ")
		}
		fmt.Fprintln(w, line)
	} else {
		fmt.Fprintln(w, "Claude: unknown (no reading)")
	}
	if len(u.AgyRows) > 0 {
		fmt.Fprintln(w)
		for _, r := range u.AgyRows {
			mark := ""
			switch {
			case r.AtReserve:
				mark = "  at the reserve"
			case r.ReviewsOnly:
				mark = "  only PR reviews"
			}
			fmt.Fprintf(w, "  %-24s %-16s %3d%% left  (%s)%s\n", r.Group, r.Label, r.Left, r.Resets, mark)
		}
	} else if len(u.Notes) == 0 {
		fmt.Fprintln(w, "agy: `agy -p /usage` gave no quota rows")
	}
	for _, n := range u.Notes {
		fmt.Fprintln(w, n)
	}
	if len(u.AgyTiers) > 0 {
		fmt.Fprintln(w)
		for _, t := range u.AgyTiers {
			model := t.Model
			if model == "" {
				model = "nothing (every model of it is at the reserve)"
			}
			extra := ""
			if t.HasLeft {
				extra = fmt.Sprintf(" (%d%% left)", t.Left)
			}
			fmt.Fprintf(w, "agy %s: %s%s\n", t.Tier, model, extra)
			for _, note := range t.Notes {
				fmt.Fprintf(w, "  %s\n", note)
			}
		}
	}
	weekly := u.QuotaStopWeekly
	if weekly == 0 {
		weekly = u.QuotaStop // a host that predates the weekly line
	}
	fmt.Fprintf(w, "\nreserve: mesh starts nothing new on a quota window once %d%% of a five-hour one (quota_stop) or %d%% of a weekly one (quota_stop_weekly) is used\n", u.QuotaStop, weekly)
	for _, o := range []struct {
		kind string
		at   int32
	}{{"claude", u.ReviewsOnlyClaude}, {"agy", u.ReviewsOnlyAgy}} {
		if o.at > 0 && o.at < 100 {
			fmt.Fprintf(w, "reviews only: %s starts only PR reviews once %d%% of a window is used (quota_reviews_only.%s)\n", o.kind, o.at, o.kind)
		}
	}
}

// renderBrief prints the brief as the text it is.
func renderBrief(m proto.Message, w io.Writer) {
	fmt.Fprintln(w, strings.TrimRight(m.(*v1.GetBriefResponse).Text, "\n"))
}

// renderReset says a reset is pending, and what the root does next.
func renderReset(m proto.Message, w io.Writer) {
	res := m.(*v1.ResetRootResponse)
	r := res.RootReset
	fmt.Fprintf(w, "reset %s pending (%s)", r.GetId(), r.GetVia())
	if r.GetCompact() {
		fmt.Fprint(w, ", compacting")
	}
	fmt.Fprintln(w)
	for _, n := range res.Notes {
		fmt.Fprintln(w, n)
	}
}

func printTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	writeRow := func(cols []string) {
		for i, c := range cols {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			if i == len(cols)-1 {
				fmt.Fprint(w, c)
			} else {
				fmt.Fprintf(w, "%-*s", widths[i], c)
			}
		}
		fmt.Fprintln(w)
	}
	writeRow(headers)
	seps := make([]string, len(headers))
	for i, wid := range widths {
		seps[i] = strings.Repeat("-", wid)
	}
	writeRow(seps)
	for _, r := range rows {
		writeRow(r)
	}
}

func tokensText(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	// The unit is chosen after rounding: 999_600 is 1.0M, not 1000k.
	// Half rounds up, as the desktop's Math.round and toFixed do (Go's %.0f rounds half to even: 2500 would be 2k).
	if math.Round(float64(n)/1e3) >= 1000 {
		return fmt.Sprintf("%.1fM", math.Round(float64(n)/1e5)/10)
	}
	return fmt.Sprintf("%.0fk", math.Round(float64(n)/1e3))
}

func trimRound2(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "0"
	}
	return s
}

func renderCost(m proto.Message, w io.Writer) {
	r := m.(*v1.GetCostResponse)
	var in, wr, out int64
	for _, c := range r.Clis {
		in += c.Input
		wr += c.W1 + c.W5
		out += c.Output
	}
	fmt.Fprintf(w, "%s: %s tokens (input %s, cache write %s, output %s); cache reads %s apart; ≈ $%.2f at API prices for Claude (%s)\n",
		r.Scope, tokensText(r.TotalTokens), tokensText(in), tokensText(wr), tokensText(out), tokensText(r.TotalRead), r.Usd, r.PricesAsOf)
	fmt.Fprintln(w, "total = input + cache write + output; cache reads are shown apart (most of a Claude session, far cheaper on a plan)")
	singleRun := strings.HasPrefix(r.Scope, "run ")
	if len(r.Clis) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(r.Clis))
		for _, c := range r.Clis {
			rows = append(rows, []string{c.Cli, fmt.Sprint(c.Calls), tokensText(c.Input), tokensText(c.W1 + c.W5), tokensText(c.Output), tokensText(c.Total), tokensText(c.Read), fmt.Sprintf("%.2f", c.Usd)})
		}
		printTable(w, []string{"cli", "calls", "input", "cache write", "output", "total", "cache read", "≈ $"}, rows)
		for _, c := range r.Clis {
			fmt.Fprintf(w, "  %s: %s\n", c.Cli, c.Basis)
		}
	}
	if len(r.Agents) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(r.Agents))
		for _, a := range r.Agents {
			agent := a.Name
			if !singleRun && a.Run != "" {
				agent = a.Run + ":" + a.Name
			}
			cold := ""
			if a.Cold > 0 {
				cold = fmt.Sprintf("%d (≈ $%.2f)", a.Cold, a.ColdUsd)
			}
			rows = append(rows, []string{
				agent, a.Kind, fmt.Sprint(a.Calls),
				tokensText(a.Input), tokensText(a.W1 + a.W5), tokensText(a.Output), tokensText(a.Total), tokensText(a.Read),
				fmt.Sprintf("%.2f", a.Usd), fmt.Sprintf("%.2f/%.2f", a.PlanUsd, a.ExecUsd),
				fmt.Sprintf("%.2f", a.SubUsd), cold,
			})
		}
		printTable(w, []string{"agent", "kind", "calls", "input", "cache write", "output", "total", "cache read", "≈ $", "plan/exec $", "sub $", "cold"}, rows)
	}
	if len(r.Kinds) > 0 {
		fmt.Fprintln(w)
		rows := make([][]string, 0, len(r.Kinds))
		for _, k := range r.Kinds {
			prefix := ""
			if k.HasFirstCtxMedian {
				prefix = tokensText(k.FirstCtxMedian)
			}
			cold := ""
			if k.Cold > 0 {
				cold = fmt.Sprintf("%d (≈ $%.2f)", k.Cold, k.ColdUsd)
			}
			rows = append(rows, []string{
				k.Kind, fmt.Sprint(k.Sessions), fmt.Sprint(k.Calls),
				tokensText(k.Input), tokensText(k.W1 + k.W5), tokensText(k.Output), tokensText(k.Total), tokensText(k.Read), fmt.Sprintf("%.1f", k.TokenShare),
				fmt.Sprintf("%.2f", k.Usd),
				tokensText(k.CtxPerCall), fmt.Sprint(k.OutPerCall), fmt.Sprintf("%.1f", k.Write_5MShare),
				prefix, cold,
			})
		}
		printTable(w, []string{"kind", "sessions", "calls", "input", "cache write", "output", "total", "cache read", "% of tokens", "≈ $", "ctx/call", "out/call", "5m %", "prefix", "cold"}, rows)
	}
	if len(r.ByModel) > 0 {
		fmt.Fprintln(w, "\nby model and phase:")
		rows := make([][]string, 0, len(r.ByModel))
		for _, bm := range r.ByModel {
			phase := bm.Phase
			if phase == "" {
				phase = "-"
			}
			rows = append(rows, []string{
				bm.Model, phase, fmt.Sprint(bm.Calls),
				tokensText(bm.Input), tokensText(bm.W1 + bm.W5), tokensText(bm.Output), tokensText(bm.Total), tokensText(bm.Read),
				fmt.Sprintf("%.2f", bm.Usd), tokensText(bm.CtxPerCall), fmt.Sprint(bm.OutPerCall),
			})
		}
		printTable(w, []string{"model", "phase", "calls", "input", "cache write", "output", "total", "cache read", "≈ $", "ctx/call", "out/call"}, rows)
	}
	if len(r.Verdicts) > 0 {
		fmt.Fprintln(w, "\nreview findings, by the judge's verdict (`mesh verdict`; the PR author's unless the root changed it):")
		rows := make([][]string, 0, len(r.Verdicts))
		for _, v := range r.Verdicts {
			agreed := ""
			if v.HasAgreedPct {
				agreed = fmt.Sprintf("%d%%", v.AgreedPct)
			}
			rows = append(rows, []string{v.Kind, v.Model, v.Tier, fmt.Sprint(v.Reviews), fmt.Sprint(v.ByAuthor), fmt.Sprint(v.Findings), agreed, fmt.Sprint(v.Blocking)})
		}
		printTable(w, []string{"kind", "model", "tier", "reviews", "by author", "findings", "agreed", "blocking"}, rows)
	}
	if rev := r.Reviews; rev != nil && (rev.ReviewedPrs > 0 || rev.Prs > 0) {
		fmt.Fprintln(w)
		if rev.ReviewedPrs > 0 {
			clis := make([]string, 0, len(rev.ByCli))
			for k := range rev.ByCli {
				clis = append(clis, k)
			}
			sort.Strings(clis)
			parts := make([]string, 0, len(clis))
			for _, k := range clis {
				parts = append(parts, fmt.Sprintf("%s %s", k, trimRound2(rev.ByCli[k])))
			}
			fmt.Fprintf(w, "reviews: %d PRs, %s reviewers per PR (%s)\n", rev.ReviewedPrs, trimRound2(rev.ReviewersPerPr), strings.Join(parts, ", "))
		}
		if rev.Prs > 0 {
			fmt.Fprintf(w, "Claude reviews: %d PRs, ≈ $%.2f at API prices (≈ $%.2f per PR)\n", rev.Prs, rev.Usd, rev.UsdPerPr)
		}
	}
	fmt.Fprintf(w, "root resets: %d\n", r.Resets)
}

func renderModels(m proto.Message, w io.Writer) {
	r := m.(*v1.CheckModelsResponse)
	for _, k := range r.Kinds {
		if len(k.Listed) == 0 {
			continue
		}
		parts := make([]string, 0, len(k.Listed))
		for _, e := range k.Listed {
			if e.InTiers {
				parts = append(parts, e.Family+" (a tier)")
			} else {
				parts = append(parts, e.Family)
			}
		}
		fmt.Fprintf(w, "%s: %s\n", k.Kind, strings.Join(parts, ", "))
	}
	if r.Notice != "" {
		fmt.Fprintln(w, r.Notice)
	} else {
		fmt.Fprintln(w, "mesh: nothing new or retired since the tiers were set")
	}
}

func renderModel(m proto.Message, w io.Writer) {
	r := m.(*v1.ResolveModelResponse)
	fmt.Fprintf(w, "%s %s\n", r.Model, r.Effort)
	for _, c := range r.Chain {
		line := "  " + c.Model
		if c.Standing != "" {
			line += "   (" + c.Standing + ")"
		}
		fmt.Fprintln(w, line)
	}
}

func renderImportSettings(m proto.Message, w io.Writer) {
	r := m.(*v1.ImportSettingsResponse)
	verb := "imported"
	if r.DryRun {
		verb = "would import"
	}
	fmt.Fprintf(w, "%s: %s %d setting(s)\n", r.File, verb, len(r.Imported))
	for _, s := range r.Imported {
		line := "  " + s.Key + " = " + jsonOf(s.Value)
		if s.From != "" {
			line += "   (from " + s.From + ")"
		}
		fmt.Fprintln(w, line)
	}
	for _, s := range r.Skipped {
		fmt.Fprintln(w, "  left alone: "+s)
	}
}

func renderConflicts(m proto.Message, w io.Writer) {
	r := m.(*v1.CheckConflictsResponse)
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, warn)
	}
	if len(r.Conflicts) > 0 {
		rows := make([][]string, 0, len(r.Conflicts))
		for _, c := range r.Conflicts {
			rows = append(rows, []string{c.Repo, c.Branch, c.Against, strings.Join(c.Files, " ")})
		}
		printTable(w, []string{"repo", "branch", "against", "files"}, rows)
	} else {
		fmt.Fprintf(w, "No conflicts among %d branch(es) with commits.\n", r.CheckedBranches)
	}
	if len(r.Overlaps) > 0 {
		fmt.Fprintln(w, "\nNote: these workers change the same files (committed or not); nothing to do now; after a merge, the root has each affected worker rebase:")
		rows := make([][]string, 0, len(r.Overlaps))
		for _, o := range r.Overlaps {
			rows = append(rows, []string{o.Repo, o.Branch, o.Against, strings.Join(o.Files, " ")})
		}
		printTable(w, []string{"repo", "branch", "against", "files"}, rows)
	}
}

func renderTools(m proto.Message, w io.Writer) {
	t := m.(*v1.ListToolsResponse)
	if !t.Lean {
		fmt.Fprintln(w, "claude.lean is off: this session runs with everything")
		return
	}
	line := func(label string, xs []string) {
		if len(xs) > 0 {
			fmt.Fprintf(w, "%s: %s\n", label, strings.Join(xs, ", "))
		}
	}
	line("tools cut", t.Tools)
	line("skills cut", t.Skills)
	line("plugins cut", t.Plugins)
	if t.ClaudeaiSkills != "" {
		fmt.Fprintf(w, "claude.ai skills cut: %s\n", t.ClaudeaiSkills)
	}
	fmt.Fprintf(w, "mcp (%s): loaded [%s]", t.McpMode, strings.Join(t.McpLoaded, ", "))
	if len(t.McpCut) > 0 {
		fmt.Fprintf(w, "; cut [%s]", strings.Join(t.McpCut, ", "))
	}
	fmt.Fprintln(w)
	if t.ClaudeaiConnectors != "" {
		fmt.Fprintf(w, "connectors cut: %s\n", t.ClaudeaiConnectors)
	}
	line("added back", t.Added)
}

func renderToolsServers(m proto.Message, w io.Writer) {
	servers := m.(*v1.ListMCPServersResponse).Servers
	if len(servers) == 0 {
		fmt.Fprintln(w, "no MCP servers in the user's config")
		return
	}
	width := 0
	for _, s := range servers {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	for _, s := range servers {
		var marks []string
		if s.Always {
			marks = append(marks, "always loaded")
		}
		if s.Exclusive {
			marks = append(marks, "exclusive")
		}
		line := fmt.Sprintf("%-*s  %s", width, s.Name, s.About)
		if len(marks) > 0 {
			line += "  [" + strings.Join(marks, ", ") + "]"
		}
		if s.HeldBy != "" {
			line += "  held by " + s.HeldBy
		}
		fmt.Fprintln(w, line)
	}
}

func renderLog(m proto.Message, w io.Writer, st *streamState) {
	e := m.(*v1.StreamLogResponse).Entry
	if e == nil {
		return
	}
	when := "--:--:--"
	if e.Time != nil && e.Time.IsValid() {
		local := e.Time.AsTime().Local()
		day := local.Format("2006-01-02")
		if day != st.lastDay {
			st.lastDay = day
			fmt.Fprintf(w, "── %s ──\n", day)
		}
		when = local.Format("15:04:05")
	}
	var parts []string
	parts = append(parts, when, fmt.Sprintf("%-15s", e.Kind))
	if e.Who != "" {
		parts = append(parts, e.Who)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
}

func renderRing(m proto.Message, w io.Writer) {
	r := m.(*v1.RingAgentResponse)
	name := r.Agent.GetName()
	if name == "" {
		name = r.Agent.GetId()
	}
	switch {
	case r.Unread == 0:
		fmt.Fprintf(w, "%s has no unread messages.\n", name)
	case r.Rung:
		fmt.Fprintf(w, "rung %s (%d unread via %s)\n", name, r.Unread, r.Via)
	case r.Error != "":
		fmt.Fprintf(w, "not rung %s (%d unread): %s\n", name, r.Unread, r.Error)
	default:
		fmt.Fprintf(w, "not rung %s (%d unread)\n", name, r.Unread)
	}
}

func renderPermissions(m proto.Message, w io.Writer) {
	ps := m.(*v1.ListPermissionsResponse).Permissions
	if len(ps) == 0 {
		fmt.Fprintln(w, "no permission prompts waiting")
		return
	}
	for _, p := range ps {
		fmt.Fprintf(w, "%s  %s  %s", p.Id, p.AgentName, p.Tool)
		if p.Summary != "" {
			fmt.Fprintf(w, ": %q", p.Summary) // the agent's text, quoted: it is not for the reader to obey
		}
		fmt.Fprintln(w)
	}
}

func renderPermission(m proto.Message, w io.Writer) {
	if p := m.(*v1.DecidePermissionResponse).Permission; p != nil {
		fmt.Fprintf(w, "answered %s (%s: %s)\n", p.Id, p.AgentName, p.Tool)
	}
}

// renderPair prints a pairing as a QR code of its link (two rows of modules to a line, half blocks) and the
// link itself, for a phone's camera or for copying. `--format json` prints the whole response.
func renderPair(m proto.Message, w io.Writer) {
	p := m.(*v1.CreatePairingResponse)
	if code, err := qrcode.New(p.Uri, qrcode.Medium); err == nil {
		fmt.Fprint(w, code.ToSmallString(false))
	} else {
		fmt.Fprintf(w, "(no QR code: %v)\n", err)
	}
	fmt.Fprintf(w, "\nScan it with the app, or open the link:\n%s\n", p.Uri)
	if p.HostName != "" {
		fmt.Fprintf(w, "host: %s\n", p.HostName)
	}
	if p.CertFingerprint != "" {
		fmt.Fprintf(w, "certificate: %s\n", p.CertFingerprint)
	}
	if p.ExpiresAt != nil {
		fmt.Fprintf(w, "expires: %s\n", p.ExpiresAt.AsTime().UTC().Format("2006-01-02T15:04:05Z"))
	}
}

// renderCheck prints a worker's check report as it is.
func renderCheck(m proto.Message, w io.Writer) {
	fmt.Fprintln(w, strings.TrimRight(m.(*v1.CheckAgentResponse).Text, "\n"))
}

// askHead is an ask's one line: id, kind, status and what it is about (the PR and round, or the size).
func askHead(a *v1.Ask) string {
	line := fmt.Sprintf("%s  %s  %s", a.Id, enumLower(a.Kind, "ASK_KIND_"), enumLower(a.Status, "ASK_STATUS_"))
	if a.Pr != "" {
		line += fmt.Sprintf("  PR #%d", a.PrNumber)
		if a.RelayStep > 0 { // a relay step is no round, though it carries the number
			line += fmt.Sprintf(" relay step %d", a.RelayStep)
		} else if a.Round > 0 {
			line += fmt.Sprintf(" round %d", a.Round)
		}
		if a.Size != "" {
			line += " (" + a.Size + ")"
		}
	}
	return line
}

// answered counts the members that answered, of those asked (a member not asked is left out).
func answered(a *v1.Ask) (done, total int) {
	for _, m := range a.Members {
		switch m.Status {
		case v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED:
		case v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED:
			done++
			total++
		default:
			total++
		}
	}
	return done, total
}

// renderAsk prints what `mesh ask` started: the ask, who was asked and how, what to know, and where the answers go.
func renderAsk(m proto.Message, w io.Writer) {
	r := m.(*v1.AskResponse)
	if a := r.Ask; a != nil {
		fmt.Fprintln(w, askHead(a))
		for _, mem := range a.Members {
			how := mem.Kind
			if mem.How != "" {
				how += ", " + mem.How
			}
			line := fmt.Sprintf("  %s (%s): %s", mem.Name, how, enumLower(mem.Status, "ASK_MEMBER_STATUS_"))
			if mem.Line != "" {
				line += ": " + mem.Line
			}
			fmt.Fprintln(w, line)
		}
		if a.AnswersPath != "" {
			fmt.Fprintln(w, "answers: "+a.AnswersPath)
		}
	} else {
		for _, ag := range r.Agents {
			fmt.Fprintf(w, "asked %s (%s)\n", ag.Name, ag.Kind)
		}
		if r.Message != nil {
			fmt.Fprintln(w, "task "+r.Message.Id)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "note: "+n)
	}
	if r.Hint != "" {
		fmt.Fprintln(w, r.Hint)
	}
}

// renderAsks prints one line per ask (how many members answered, the verdicts given) and its answers file.
func renderAsks(m proto.Message, w io.Writer) {
	asks := m.(*v1.ListAsksResponse).Asks
	if len(asks) == 0 {
		fmt.Fprintln(w, "no asks")
		return
	}
	for _, a := range asks {
		done, total := answered(a)
		line := fmt.Sprintf("%s  by %s  %d/%d answered", askHead(a), a.CallerName, done, total)
		if len(a.Verdicts) > 0 {
			var parts []string
			for _, v := range a.Verdicts {
				p := fmt.Sprintf("%s %d/%d/%d", v.Reviewer, v.Agree, v.Other, v.Disagree)
				if v.Blocking > 0 {
					p += fmt.Sprintf(" (%d blocking)", v.Blocking)
				}
				if v.DeclinedBlocking > 0 {
					p += fmt.Sprintf(" (%d blocking DECLINED)", v.DeclinedBlocking)
				}
				if v.Judge != "" {
					p += " by " + v.Judge
				}
				parts = append(parts, p)
			}
			line += "  verdicts (agree/other/disagree): " + strings.Join(parts, ", ")
		}
		fmt.Fprintln(w, line)
		var who []string
		for _, mem := range a.Members {
			who = append(who, mem.Name+" "+enumLower(mem.Status, "ASK_MEMBER_STATUS_"))
		}
		if len(who) > 0 {
			fmt.Fprintln(w, "    "+strings.Join(who, ", "))
		}
		switch {
		case a.AnswersPath != "" && a.Status == v1.AskStatus_ASK_STATUS_FINISHED:
			fmt.Fprintln(w, "    "+a.AnswersPath)
		case a.Collected && a.Status == v1.AskStatus_ASK_STATUS_FINISHED:
			fmt.Fprintln(w, "    no answers file: the answers are in `mesh asks --format json` (each member's text)")
		}
	}
}
