package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

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
		fmt.Fprintf(w, "── %s from %s at %s (%s) ──\n", enumLower(msg.Type, "MESSAGE_TYPE_"), msg.From.GetName(),
			msg.CreatedAt.AsTime().UTC().Format("2006-01-02T15:04:05Z"), msg.Id)
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
		fmt.Fprintln(w, line)
	} else {
		fmt.Fprintln(w, "Claude: unknown (no reading)")
	}
	if len(u.AgyRows) > 0 {
		fmt.Fprintln(w)
		for _, r := range u.AgyRows {
			fmt.Fprintf(w, "  %-24s %-16s %3d%% left  (%s)\n", r.Group, r.Label, r.Left, r.Resets)
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
	fmt.Fprintf(w, "\nreserve: mesh starts nothing new on a quota once %d%% of it is used (quota_stop)\n", u.QuotaStop)
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
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%.0fk", float64(n)/1e3)
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
	fmt.Fprintf(w, "%s: $%.2f at API prices (%s)\n", r.Scope, r.Usd, r.PricesAsOf)
	singleRun := strings.HasPrefix(r.Scope, "run ")
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
				cold = fmt.Sprintf("%d ($%.2f)", a.Cold, a.ColdUsd)
			}
			rows = append(rows, []string{
				agent, a.Kind, fmt.Sprint(a.Calls),
				tokensText(a.Input), tokensText(a.W1), tokensText(a.W5), tokensText(a.Read), tokensText(a.Output),
				fmt.Sprintf("%.2f", a.Usd), fmt.Sprintf("%.2f/%.2f", a.PlanUsd, a.ExecUsd),
				fmt.Sprintf("%.2f", a.SubUsd), cold,
			})
		}
		printTable(w, []string{"agent", "kind", "calls", "input", "w1", "w5", "read", "output", "$", "plan/exec", "sub $", "cold"}, rows)
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
				cold = fmt.Sprintf("%d ($%.2f)", k.Cold, k.ColdUsd)
			}
			rows = append(rows, []string{
				k.Kind, fmt.Sprint(k.Sessions), fmt.Sprint(k.Calls),
				fmt.Sprintf("%.2f", k.Usd), fmt.Sprintf("%.1f", k.Share), fmt.Sprintf("%.3f", k.UsdPerCall),
				tokensText(k.CtxPerCall), fmt.Sprint(k.OutPerCall), fmt.Sprintf("%.1f", k.Write_5MShare),
				prefix, cold,
			})
		}
		printTable(w, []string{"kind", "sessions", "calls", "$", "%", "$/call", "ctx/call", "out/call", "5m %", "prefix", "cold"}, rows)
	}
	if len(r.TokenKinds) > 0 {
		hasUSD := false
		for _, tk := range r.TokenKinds {
			if tk.Usd > 0 {
				hasUSD = true
				break
			}
		}
		fmt.Fprintln(w, "\nagy & OpenCode (tokens):")
		headers := []string{"kind", "sessions", "calls", "input", "read", "output"}
		if hasUSD {
			headers = append(headers, "$")
		}
		rows := make([][]string, 0, len(r.TokenKinds))
		for _, tk := range r.TokenKinds {
			row := []string{tk.Kind, fmt.Sprint(tk.Sessions), fmt.Sprint(tk.Calls), tokensText(tk.Input), tokensText(tk.Read), tokensText(tk.Output)}
			if hasUSD {
				row = append(row, fmt.Sprintf("%.2f", tk.Usd))
			}
			rows = append(rows, row)
		}
		printTable(w, headers, rows)
	}
	if len(r.ByModel) > 0 {
		fmt.Fprintln(w, "\nby model and phase:")
		rows := make([][]string, 0, len(r.ByModel))
		for _, bm := range r.ByModel {
			phase := bm.Phase
			if phase == "" {
				phase = "-"
			}
			rows = append(rows, []string{bm.Model, phase, fmt.Sprint(bm.Calls), fmt.Sprintf("%.2f", bm.Usd), tokensText(bm.CtxPerCall), fmt.Sprint(bm.OutPerCall)})
		}
		printTable(w, []string{"model", "phase", "calls", "$", "ctx/call", "out/call"}, rows)
	}
	if len(r.TokensByModel) > 0 {
		hasUSD := false
		for _, tm := range r.TokensByModel {
			if tm.Usd > 0 {
				hasUSD = true
				break
			}
		}
		fmt.Fprintln(w, "\nagy and OpenCode by model and phase (tokens):")
		headers := []string{"model", "phase", "calls", "input", "read", "output"}
		if hasUSD {
			headers = append(headers, "$")
		}
		rows := make([][]string, 0, len(r.TokensByModel))
		for _, tm := range r.TokensByModel {
			phase := tm.Phase
			if phase == "" {
				phase = "-"
			}
			row := []string{tm.Model, phase, fmt.Sprint(tm.Calls), tokensText(tm.Input), tokensText(tm.Read), tokensText(tm.Output)}
			if hasUSD {
				row = append(row, fmt.Sprintf("%.2f", tm.Usd))
			}
			rows = append(rows, row)
		}
		printTable(w, headers, rows)
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
			fmt.Fprintf(w, "Claude reviews: %d PRs, $%.2f ($%.2f per PR)\n", rev.Prs, rev.Usd, rev.UsdPerPr)
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
			fmt.Fprintf(w, ": %s", p.Summary)
		}
		fmt.Fprintln(w)
	}
}

func renderPermission(m proto.Message, w io.Writer) {
	if p := m.(*v1.DecidePermissionResponse).Permission; p != nil {
		fmt.Fprintf(w, "answered %s (%s: %s)\n", p.Id, p.AgentName, p.Tool)
	}
}
