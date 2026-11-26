package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	pricesAsOf   = "2026-11-26, platform.claude.com pricing"
	coldWriteMin = int64(20_000)
	costCacheTTL = time.Hour
	costTTL5m    = 5 * time.Minute
)

// priceTable maps Claude model family -> [input, w5, w1, read, output] USD per 1M tokens.
var priceOrder = []struct {
	family string
	rates  [5]float64
}{
	{"fable", [5]float64{10.0, 12.5, 20.0, 0.25, 50.0}},
	{"opus", [5]float64{4.0, 5.0, 8.0, 0.20, 20.0}},
	{"sonnet", [5]float64{2.0, 2.5, 4.0, 0.20, 10.0}},
	{"haiku", [5]float64{0.10, 0.125, 0.20, 0.01, 0.50}},
}

// haikuLongPrompt is the prompt size (input + cache writes + cache reads) above which Haiku 5.5
// bills every rate at 5x: $0.50 / $0.625 / $1 / $0.05 / $2.50 per MTok.
// (The table's Haiku row is 5.5's: only that model has the long-prompt tier.)
const haikuLongPrompt = int64(100_000)

var (
	spanRE        = regexp.MustCompile(`^(\d+)([dhm])$`)
	rootCommandRE = regexp.MustCompile(`(?m)(?:^|[;&|(]|\b(?:do|then|else|time)\b)\s*(?:timeout\s+\S+\s+)?(?:\w+=\S*\s+)*mesh (?:spawn|init|ask|approve|resume|switch|watch|start-run)\b`)
	sanitizeCwdRE = regexp.MustCompile(`[^A-Za-z0-9]`)
	reviewPRRE    = regexp.MustCompile(`^\s*Review \(read-only\) of (.+?)(?: at [0-9a-f]{7,40})?: review this pull request`)
	pullNumRE     = regexp.MustCompile(`/pull/(\d+)(?:$|[/?#])`)
)

func claudeCallCost(model string, input, w5, w1, read, output int64) float64 {
	if model == "agy" {
		return 0
	}
	ml := strings.ToLower(model)
	rates := priceOrder[1].rates // default opus
	for _, p := range priceOrder {
		if strings.Contains(ml, p.family) {
			rates = p.rates
			break
		}
	}
	if strings.Contains(ml, "fable-5-2") || strings.Contains(ml, "fable-5.2") {
		rates = [5]float64{8.0, 10.0, 16.0, 0.20, 40.0} // Fable 5.2; the table's fable row is 5.1's
	}
	if strings.Contains(ml, "sonnet-5-5") || strings.Contains(ml, "sonnet-5.5") {
		rates[3] = 0.10 // Sonnet 5.5 cache hits bill 0.05x its input price
	}
	if (strings.Contains(ml, "haiku-5-5") || strings.Contains(ml, "haiku-5.5")) && input+w5+w1+read > haikuLongPrompt {
		for i := range rates {
			rates[i] *= 5
		}
	}
	return (float64(input)*rates[0] +
		float64(w5)*rates[1] +
		float64(w1)*rates[2] +
		float64(read)*rates[3] +
		float64(output)*rates[4]) / 1e6
}

func costModelFamily(model string) string {
	if model == "" {
		return "?"
	}
	if fam, ok := agy.ModelFamily(model); ok {
		return fam
	}
	ml := strings.ToLower(model)
	for _, p := range priceOrder {
		if strings.Contains(ml, p.family) {
			return p.family
		}
	}
	return model
}

// ParseSince parses a window edge: relative span (`7d`, `36h`, `90m`) or ISO 8601 (local time when no offset).
func ParseSince(text string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(text)
	if m := spanRE.FindStringSubmatch(strings.ToLower(s)); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}, invalid("%s: invalid span", text)
		}
		var unit time.Duration
		switch m[2] {
		case "d":
			unit = 24 * time.Hour
		case "h":
			unit = time.Hour
		case "m":
			unit = time.Minute
		}
		if n < 0 || time.Duration(n) > math.MaxInt64/2/unit {
			return time.Time{}, invalid("%s: span too large", text)
		}
		return now.Add(-time.Duration(n) * unit), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, invalid("%s: give a span back from now (7d, 36h) or ISO 8601 (2026-11-04, 2026-11-04T09:00)", text)
}

func (d *Daemon) costEnv() map[string]string {
	if testing.Testing() {
		env := map[string]string{}
		if d != nil && d.rt != nil {
			for k, v := range d.rt.cfg.Env {
				env[k] = v
			}
		}
		return env
	}
	return d.cliBaseEnv()
}

func (d *Daemon) claudeProjectsDir() string {
	env := d.costEnv()
	if dir := env["CLAUDE_CONFIG_DIR"]; dir != "" {
		return filepath.Join(dir, "projects")
	}
	home := env["HOME"]
	if home == "" && !testing.Testing() {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func (d *Daemon) agyConversationsDir() string {
	env := d.costEnv()
	if testing.Testing() && env["AGENT_MESH_AGY_CONVERSATIONS"] == "" && env["MESH_AGY_CONVERSATIONS"] == "" && env["HOME"] == "" {
		return ""
	}
	return agy.ConversationsDir(env)
}

func (d *Daemon) opencodeDBPathForCost() string {
	env := d.costEnv()
	if testing.Testing() && env["AGENT_MESH_OPENCODE_DB"] == "" && env["MESH_OPENCODE_DB"] == "" && env["XDG_DATA_HOME"] == "" && env["HOME"] == "" {
		return ""
	}
	return opencode.DBPath(env)
}

func sanitizeClaudeCwd(cwd string) string {
	return sanitizeCwdRE.ReplaceAllString(cwd, "-")
}

// ── Claude JSONL reader ──

type costCall struct {
	id     string
	t      time.Time
	model  string
	input  int64
	w5     int64
	w1     int64
	read   int64
	output int64
	usd    float64 // used for paid OpenCode calls; Claude prices via claudeCallCost
}

type claudeSessionInfo struct {
	entrypoint   string
	cwd          string
	doorbell     bool
	rootCommands bool
}

type rawClaudeRecord struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp"`
	Entrypoint string `json:"entrypoint"`
	Cwd        string `json:"cwd"`
	IsMeta     bool   `json:"isMeta"`
	RequestID  string `json:"requestId"`
	Message    *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreation            *struct {
				Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

func extractPromptText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, " "), true
}

func readClaudeCalls(path string) ([]costCall, claudeSessionInfo) {
	var info claudeSessionInfo
	f, err := os.Open(path)
	if err != nil {
		return nil, info
	}
	defer f.Close()

	var calls []costCall
	seen := map[string]bool{}
	prompted := false
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			hasUsage := bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"assistant"`))
			if hasUsage || (!prompted && bytes.Contains(line, []byte(`"user"`))) ||
				(info.entrypoint == "" && bytes.Contains(line, []byte(`"entrypoint"`))) ||
				(info.cwd == "" && bytes.Contains(line, []byte(`"cwd"`))) ||
				(!info.rootCommands && bytes.Contains(line, []byte(`"Bash"`))) {
				var rec rawClaudeRecord
				if json.Unmarshal(line, &rec) == nil {
					if info.entrypoint == "" && rec.Entrypoint != "" {
						info.entrypoint = rec.Entrypoint
					}
					if info.cwd == "" && rec.Cwd != "" {
						info.cwd = rec.Cwd
					}
					if rec.Message != nil {
						switch rec.Type {
						case "user":
							if !prompted && !rec.IsMeta {
								if text, ok := extractPromptText(rec.Message.Content); ok {
									prompted = true
									info.doorbell = strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "📬 [mesh]")
								}
							}
						case "assistant":
							if !info.rootCommands && len(rec.Message.Content) > 0 && bytes.Contains(rec.Message.Content, []byte(`"Bash"`)) {
								var blocks []struct {
									Name  string `json:"name"`
									Input struct {
										Command string `json:"command"`
									} `json:"input"`
								}
								if json.Unmarshal(rec.Message.Content, &blocks) == nil {
									for _, b := range blocks {
										if b.Name == "Bash" && rootCommandRE.MatchString(b.Input.Command) {
											info.rootCommands = true
											break
										}
									}
								}
							}
							if rec.Message.Usage != nil && rec.Message.Model != "<synthetic>" {
								key := rec.Message.ID
								if key == "" {
									key = rec.RequestID
								}
								t, terr := parseClaudeTimestamp(rec.Timestamp)
								if terr == nil && (key == "" || !seen[key]) {
									if key != "" {
										seen[key] = true
									}
									u := rec.Message.Usage
									var w5, w1 int64
									if u.CacheCreation != nil {
										w5 = u.CacheCreation.Ephemeral5mInputTokens
										w1 = u.CacheCreation.Ephemeral1hInputTokens
									}
									if extra := u.CacheCreationInputTokens - w5 - w1; extra > 0 {
										w1 += extra
									}
									calls = append(calls, costCall{
										id:     key,
										t:      t,
										model:  rec.Message.Model,
										input:  u.InputTokens,
										w5:     w5,
										w1:     w1,
										read:   u.CacheReadInputTokens,
										output: u.OutputTokens,
									})
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return calls, info
		}
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].t.Before(calls[j].t) })
	return calls, info
}

func parseClaudeTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// ── Segment attribution ──

type phaseMark struct {
	at    time.Time
	phase string
}

type modelMark struct {
	at    time.Time
	model string
}

type costSegment struct {
	run     string
	name    string
	kind    string
	root    bool
	start   time.Time
	end     time.Time
	marks   []phaseMark
	tally   string // "claude", "agy", "opencode"
	cli     string
	prs     []string
	cwds    []string
	who     string
	models  []modelMark
	agentID string
}

type reviewTaskRecord struct {
	at   time.Time
	pr   string
	who  string
	kind string
}

type verdictRecord struct {
	at      time.Time
	verdict *v1.Verdict
}

type runWindow struct {
	start    time.Time
	end      time.Time
	stopped  bool
	rootName string
	cwds     []string
	resets   []time.Time
	reviews  []reviewTaskRecord
	verdicts []verdictRecord
}

func phaseLabel(p v1.AgentPhase) string {
	switch p {
	case v1.AgentPhase_AGENT_PHASE_PLANNING:
		return "plan"
	case v1.AgentPhase_AGENT_PHASE_EXECUTING:
		return "exec"
	default:
		return ""
	}
}

func costKind(a *v1.Agent, isReviewer bool) string {
	var base string
	switch {
	case a.Role == v1.AgentRole_AGENT_ROLE_ROOT:
		base = "root"
	case a.Peer:
		base = "peer"
	case a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER || isReviewer:
		base = "reviewer"
	case a.Role == v1.AgentRole_AGENT_ROLE_ASKER:
		base = "asker"
	default:
		base = "worker"
	}
	switch a.Kind {
	case agy.Kind:
		return "agy " + base
	case opencode.Kind:
		return "opencode " + base
	default:
		return base
	}
}

func reviewPRKey(repo, prURL string, prNum int32) string {
	num := prNum
	if num == 0 && prURL != "" {
		if m := pullNumRE.FindStringSubmatch(prURL); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				num = int32(n)
			}
		}
	}
	repoName := filepath.Base(repo)
	if repoName != "" && repoName != "." && num > 0 {
		return fmt.Sprintf("%s#%d", repoName, num)
	}
	if prURL != "" {
		return prURL
	}
	if num > 0 {
		return fmt.Sprintf("#%d", num)
	}
	return ""
}

func reviewTaskPRFromBody(repo, body string) string {
	trimmed := strings.TrimSpace(body)
	m := reviewPRRE.FindStringSubmatch(trimmed)
	if m == nil {
		return ""
	}
	ref := m[1]
	if n, err := strconv.Atoi(ref); err == nil && n > 0 {
		return reviewPRKey(repo, "", int32(n))
	}
	if pm := pullNumRE.FindStringSubmatch(ref); pm != nil {
		if n, err := strconv.Atoi(pm[1]); err == nil {
			// Extract repo name from URL `/owner/repo/pull/N` if repo empty.
			r := repo
			if r == "" {
				parts := strings.Split(strings.TrimPrefix(ref, "https://"), "/")
				for i := 0; i+2 < len(parts); i++ {
					if parts[i+1] == "pull" {
						r = parts[i]
						break
					}
				}
			}
			return reviewPRKey(r, ref, int32(n))
		}
	}
	return ref
}

func cliCostTally(kind string) (tally string, ok bool) {
	switch kind {
	case "", "claude":
		return "claude", true
	case agy.Kind:
		return "agy", true
	case opencode.Kind:
		return "opencode", true
	default:
		return "", false
	}
}

func (d *Daemon) buildCostSegments(r store.Reader) (map[string][]*costSegment, map[string]*runWindow, error) {
	runs, err := r.ListRuns(true)
	if err != nil {
		return nil, nil, err
	}
	agents, err := r.AllAgents()
	if err != nil {
		return nil, nil, err
	}
	events, err := r.ListEvents("")
	if err != nil {
		return nil, nil, err
	}
	asks, err := r.RunAsks("")
	if err != nil {
		return nil, nil, err
	}

	byID := make(map[string]*v1.Agent, len(agents))
	agentsByRun := map[string][]*v1.Agent{}
	for _, a := range agents {
		byID[a.Id] = a
		agentsByRun[a.RunId] = append(agentsByRun[a.RunId], a)
	}

	oneAgent := func(id string) string {
		cur := id
		for steps := 0; steps <= len(byID); steps++ {
			a := byID[cur]
			if a == nil || a.ResumedFromAgentId == "" || a.ResumedFromAgentId == cur {
				break
			}
			cur = a.ResumedFromAgentId
		}
		if a := byID[cur]; a != nil && a.SessionId != "" {
			return a.SessionId
		}
		return cur
	}

	eventsByRun := map[string][]*v1.Event{}
	for _, ev := range events {
		if ev.RunId != "" {
			eventsByRun[ev.RunId] = append(eventsByRun[ev.RunId], ev)
		}
	}
	asksByRun := map[string][]*v1.Ask{}
	for _, ask := range asks {
		asksByRun[ask.RunId] = append(asksByRun[ask.RunId], ask)
	}

	segments := map[string][]*costSegment{}
	windows := map[string]*runWindow{}

	for _, run := range runs {
		runEvents := eventsByRun[run.Id]
		runAgents := agentsByRun[run.Id]
		runAsks := asksByRun[run.Id]

		start := run.CreatedAt.AsTime()
		lastEv := start
		var stopTime time.Time
		var resets []time.Time
		agentEvents := map[string][]*v1.Event{}

		for _, ev := range runEvents {
			if ev.Time != nil {
				et := ev.Time.AsTime()
				if et.After(lastEv) {
					lastEv = et
				}
				if ev.GetRunStopped() != nil && et.After(stopTime) {
					stopTime = et
				}
				if r := ev.GetAgentReopened(); r != nil && r.Reopen != nil && r.Reopen.Why == "reset" {
					resets = append(resets, et)
				}
			}
			if ev.AgentId != "" {
				agentEvents[ev.AgentId] = append(agentEvents[ev.AgentId], ev)
			}
		}

		var rootAgent *v1.Agent
		for _, a := range runAgents {
			if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
				rootAgent = a
				break
			}
		}
		var rootName string
		var rootCwds []string
		if rootAgent != nil {
			rootName = rootAgent.Name
			if rootAgent.Worktree != "" {
				rootCwds = append(rootCwds, rootAgent.Worktree)
			}
			if rootAgent.Repo != "" && rootAgent.Repo != rootAgent.Worktree {
				rootCwds = append(rootCwds, rootAgent.Repo)
			}
		}

		winEnd := stopTime
		stopped := !stopTime.IsZero()
		if !stopped {
			winEnd = lastEv.Add(costCacheTTL)
		}

		// Collect review tasks and verdicts from Asks, plus manual `Review (read-only)` tasks from MessageSent.
		var reviews []reviewTaskRecord
		var verdicts []verdictRecord
		agentPRs := map[string]map[string]bool{}
		askReviewByAgent := map[string]bool{}

		for _, ask := range runAsks {
			if ask.Kind != v1.AskKind_ASK_KIND_REVIEW {
				continue
			}
			pk := reviewPRKey(ask.Repo, ask.Pr, ask.PrNumber)
			askAt := ask.CreatedAt.AsTime()
			for _, mem := range ask.Members {
				if mem.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED || mem.AgentId == "" {
					continue
				}
				askReviewByAgent[mem.AgentId] = true
				if pk != "" {
					if agentPRs[mem.AgentId] == nil {
						agentPRs[mem.AgentId] = map[string]bool{}
					}
					agentPRs[mem.AgentId][pk] = true
					reviews = append(reviews, reviewTaskRecord{
						at:   askAt,
						pr:   pk,
						who:  oneAgent(mem.AgentId),
						kind: mem.Kind,
					})
				}
			}
			for _, v := range ask.Verdicts {
				vat := askAt
				if v.At != nil {
					vat = v.At.AsTime()
				}
				verdicts = append(verdicts, verdictRecord{at: vat, verdict: v})
			}
		}

		for _, ev := range runEvents {
			ms := ev.GetMessageSent()
			if ms == nil || ms.Message == nil || ms.Message.Type != v1.MessageType_MESSAGE_TYPE_TASK {
				continue
			}
			to := byID[ms.Message.ToId]
			if to == nil || askReviewByAgent[to.Id] {
				continue
			}
			if pk := reviewTaskPRFromBody(to.Repo, ms.Message.Body); pk != "" {
				if agentPRs[to.Id] == nil {
					agentPRs[to.Id] = map[string]bool{}
				}
				agentPRs[to.Id][pk] = true
				reviews = append(reviews, reviewTaskRecord{
					at:   ms.Message.CreatedAt.AsTime(),
					pr:   pk,
					who:  oneAgent(to.Id),
					kind: orClaude(to.Kind),
				})
			}
		}

		w := &runWindow{
			start:    start,
			end:      winEnd,
			stopped:  stopped,
			rootName: rootName,
			cwds:     rootCwds,
			resets:   resets,
			reviews:  reviews,
			verdicts: verdicts,
		}
		windows[run.Id] = w

		for _, a := range runAgents {
			tallyName, ok := cliCostTally(a.Kind)
			if !ok {
				continue
			}
			isRoot := a.Role == v1.AgentRole_AGENT_ROLE_ROOT
			evs := agentEvents[a.Id]

			sessSet := map[string]bool{}
			var sessions []string
			addSess := func(s string) {
				if s != "" && !sessSet[s] {
					sessSet[s] = true
					sessions = append(sessions, s)
				}
			}
			addSess(a.SessionId)

			segStart := start
			if !isRoot && a.CreatedAt != nil {
				segStart = a.CreatedAt.AsTime()
			}

			var initialModel string
			var marks []phaseMark
			var models []modelMark
			var firstPhaseFrom string
			hasPhaseChange := false

			for _, ev := range evs {
				et := ev.Time.AsTime()
				switch p := ev.Payload.(type) {
				case *v1.Event_AgentSpawned:
					if !isRoot && et.Before(segStart) {
						segStart = et
					}
					if p.AgentSpawned.Agent != nil {
						if initialModel == "" {
							initialModel = p.AgentSpawned.Agent.Model
						}
						addSess(p.AgentSpawned.Agent.SessionId)
					}
				case *v1.Event_AgentReopened:
					addSess(p.AgentReopened.SessionId)
					if p.AgentReopened.Reopen != nil && p.AgentReopened.Reopen.Model != "" {
						models = append(models, modelMark{at: et, model: p.AgentReopened.Reopen.Model})
					}
				case *v1.Event_AgentPhaseChanged:
					if !hasPhaseChange {
						firstPhaseFrom = phaseLabel(p.AgentPhaseChanged.From)
						hasPhaseChange = true
					}
					marks = append(marks, phaseMark{at: et, phase: phaseLabel(p.AgentPhaseChanged.To)})
				}
			}

			if initialModel == "" {
				initialModel = a.Model
			}
			if initialModel != "" {
				models = append([]modelMark{{at: time.Time{}, model: initialModel}}, models...)
			}

			var phaseMarksOut []phaseMark
			if !isRoot {
				firstPhase := phaseLabel(a.Phase)
				if hasPhaseChange {
					firstPhase = firstPhaseFrom
				}
				phaseMarksOut = append([]phaseMark{{at: segStart, phase: firstPhase}}, marks...)
				sort.SliceStable(phaseMarksOut, func(i, j int) bool {
					return phaseMarksOut[i].at.Before(phaseMarksOut[j].at)
				})
			}

			var cwds []string
			if a.Worktree != "" {
				cwds = append(cwds, a.Worktree)
			}
			if a.Repo != "" && a.Repo != a.Worktree {
				cwds = append(cwds, a.Repo)
			}

			prs := sortedKeys(agentPRs[a.Id])
			isRev := a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER || len(prs) > 0
			kindLabel := costKind(a, isRev)

			seg := &costSegment{
				run:     run.Id,
				name:    a.Name,
				kind:    kindLabel,
				root:    isRoot,
				start:   segStart,
				end:     winEnd,
				marks:   phaseMarksOut,
				tally:   tallyName,
				cli:     orClaude(a.Kind),
				prs:     prs,
				cwds:    cwds,
				who:     oneAgent(a.Id),
				models:  models,
				agentID: a.Id,
			}
			for _, s := range sessions {
				segments[s] = append(segments[s], seg)
			}
		}
	}

	// Unstopped runs end at CACHE_TTL after their last event, or where the next run in the same root cwd starts.
	for _, w := range windows {
		if w.stopped {
			continue
		}
		for _, other := range windows {
			if sharesCwd(w.cwds, other.cwds) && other.start.After(w.start) && other.start.Before(w.end) {
				w.end = other.start
			}
		}
	}
	for _, segs := range segments {
		for _, seg := range segs {
			if w := windows[seg.run]; w != nil {
				seg.end = w.end
			}
		}
	}
	return segments, windows, nil
}

func sharesCwd(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x != "" && x == y {
				return true
			}
		}
	}
	return false
}

func segmentAt(segs []*costSegment, t time.Time) *costSegment {
	for _, s := range segs {
		if s.root && !t.Before(s.start) && (s.end.IsZero() || !t.After(s.end)) {
			return s
		}
	}
	var workers []*costSegment
	for _, s := range segs {
		if !s.root {
			workers = append(workers, s)
		}
	}
	sort.SliceStable(workers, func(i, j int) bool { return workers[i].start.Before(workers[j].start) })
	var chosen *costSegment
	for _, s := range workers {
		if !s.start.After(t) {
			chosen = s
		} else if chosen == nil {
			return nil
		} else if !chosen.end.IsZero() && t.After(chosen.end) {
			return s
		} else {
			break
		}
	}
	return chosen
}

func rootFallback(windows map[string]*runWindow, cwd string, t time.Time) *costSegment {
	if cwd == "" {
		return nil
	}
	ids := make([]string, 0, len(windows))
	for id := range windows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := windows[id]
		match := false
		for _, c := range w.cwds {
			if c == cwd {
				match = true
				break
			}
		}
		if match && !t.Before(w.start) && (w.end.IsZero() || !t.After(w.end)) {
			return &costSegment{run: id, name: w.rootName, kind: "root", root: true}
		}
	}
	return nil
}

func phaseAt(marks []phaseMark, t time.Time) string {
	phase := ""
	for _, m := range marks {
		if !m.at.After(t) {
			phase = m.phase
		}
	}
	return phase
}

func modelAt(seg *costSegment, t time.Time) string {
	found := ""
	for _, m := range seg.models {
		if !m.at.After(t) || found == "" {
			found = m.model
		}
	}
	return found
}

// ── Tally ──

type tallyBucket struct {
	kind     string
	sessions map[string]bool
	calls    int64
	input    int64
	w5       int64
	w1       int64
	read     int64
	output   int64
	usd      float64
	ctx      int64
	plan     float64
	exec     float64
	cold     int64
	coldUSD  float64
	subUSD   float64
	firsts   []int64
}

func newBucket(kind string) *tallyBucket {
	return &tallyBucket{kind: kind, sessions: map[string]bool{}}
}

type agentKey struct{ run, name string }
type modelPhaseKey struct{ model, phase string }

type costTally struct {
	agents map[agentKey]*tallyBucket
	kinds  map[string]*tallyBucket
	models map[modelPhaseKey]*tallyBucket
	priced bool
}

func newCostTally(priced bool) *costTally {
	return &costTally{
		agents: map[agentKey]*tallyBucket{},
		kinds:  map[string]*tallyBucket{},
		models: map[modelPhaseKey]*tallyBucket{},
		priced: priced,
	}
}

func (ct *costTally) add(session string, call costCall, kind string, ag agentKey, phase string, sub, cold, first bool, overrideModel string) {
	usd := call.usd
	if ct.priced {
		usd = claudeCallCost(call.model, call.input, call.w5, call.w1, call.read, call.output)
	}
	kindKey := kind
	if sub {
		kindKey += " subagents"
	}
	kb := ct.kinds[kindKey]
	if kb == nil {
		kb = newBucket(kindKey)
		ct.kinds[kindKey] = kb
	}
	modelName := overrideModel
	if modelName == "" {
		modelName = call.model
	}
	mp := modelPhaseKey{model: costModelFamily(modelName), phase: phase}
	mb := ct.models[mp]
	if mb == nil {
		mb = newBucket("")
		ct.models[mp] = mb
	}
	ab := ct.agents[ag]
	if ab == nil {
		ab = newBucket(kind)
		ct.agents[ag] = ab
	}
	targets := []*tallyBucket{kb, mb}
	if sub {
		ab.subUSD += usd
	} else {
		targets = append(targets, ab)
	}
	ctx := call.input + call.w5 + call.w1 + call.read
	for _, b := range targets {
		b.sessions[session] = true
		b.calls++
		b.usd += usd
		b.ctx += ctx
		b.input += call.input
		b.w5 += call.w5
		b.w1 += call.w1
		b.read += call.read
		b.output += call.output
		if phase == "plan" {
			b.plan += usd
		} else if phase == "exec" {
			b.exec += usd
		}
		if cold {
			b.cold++
			b.coldUSD += usd
		}
		if first {
			b.firsts = append(b.firsts, ctx)
		}
	}
}

type placedCall struct {
	kind  string
	agent agentKey
	phase string
	model string
}

func tallySession(ct *costTally, session string, calls []costCall, sub bool, place func(costCall) *placedCall, since, until time.Time, seen map[string]bool) {
	ttl := costCacheTTL
	var last time.Time
	for i, call := range calls {
		cold := !sub && !last.IsZero() && call.t.Sub(last) > ttl && (call.w5+call.w1) >= coldWriteMin
		last = call.t
		if call.w1 > 0 || call.w5 > 0 {
			if call.w1 > 0 {
				ttl = costCacheTTL
			} else {
				ttl = costTTL5m
			}
		}
		if call.id != "" {
			if seen[call.id] {
				continue
			}
			seen[call.id] = true
		}
		if !since.IsZero() && call.t.Before(since) {
			continue
		}
		if !until.IsZero() && !call.t.Before(until) {
			continue
		}
		if p := place(call); p != nil {
			ct.add(session, call, p.kind, p.agent, p.phase, sub, cold, i == 0 && !sub, p.model)
		}
	}
}

// ── File discovery & GetCost ──

func claudeSessionFiles(projectsDir, session string, cwds []string) []string {
	if projectsDir == "" || session == "" {
		return nil
	}
	for _, cwd := range cwds {
		if cwd == "" {
			continue
		}
		p := filepath.Join(projectsDir, sanitizeClaudeCwd(cwd), session+".jsonl")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return []string{p}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(projectsDir, "*", session+".jsonl"))
	sort.Strings(matches)
	if len(matches) > 0 {
		return matches[:1]
	}
	return nil
}

func recentFiles(paths []string, since time.Time) []string {
	var out []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if since.IsZero() || !st.ModTime().Before(since) {
			out = append(out, p)
		}
	}
	return out
}

func subagentFiles(sessionJSONL string) []string {
	base := strings.TrimSuffix(sessionJSONL, filepath.Ext(sessionJSONL))
	matches, _ := filepath.Glob(filepath.Join(base, "subagents", "*.jsonl"))
	sort.Strings(matches)
	return matches
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func withinWindow(t, since, until time.Time) bool {
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !until.IsZero() && !t.Before(until) {
		return false
	}
	return true
}

// GetCost computes spend per agent, kind, model, phase, reviews, and verdicts for one run or a time window.
func (d *Daemon) GetCost(ctx context.Context, req *connect.Request[v1.GetCostRequest]) (*connect.Response[v1.GetCostResponse], error) {
	m := req.Msg
	actor := auth.ActorFrom(ctx)
	now := d.nowTime()

	var since, until time.Time
	var err error
	if m.Since != "" {
		if since, err = ParseSince(m.Since, now); err != nil {
			return nil, err
		}
	}
	if m.Until != "" {
		if until, err = ParseSince(m.Until, now); err != nil {
			return nil, err
		}
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return nil, invalid("--since %s is not before --until %s", m.Since, m.Until)
	}

	hasWindow := !since.IsZero() || !until.IsZero()
	runID := m.RunId
	// A root is exempt from scopedRun on purpose (bin/mesh parity: a root's `mesh cost --since` reads every run and
	// the sessions outside any run); workers, askers and reviewers read only their own run.
	if IsAgent(actor.GetClass()) && actor.GetClass() != v1.ActorClass_ACTOR_CLASS_ROOT {
		if hasWindow && runID == "" {
			return nil, denied("an agent reads only its own run")
		}
		if runID, err = scopedRun(actor, runID); err != nil {
			return nil, err
		}
	} else if runID == "" && !hasWindow {
		if IsAgent(actor.GetClass()) && actor.GetRunId() != "" {
			runID = actor.GetRunId()
		} else {
			return nil, invalid("name a run, or a window with --since (`mesh cost --since 7d`)")
		}
	}

	r := d.store.Read(ctx)
	segments, windows, err := d.buildCostSegments(r)
	if err != nil {
		return nil, err
	}
	if runID != "" && windows[runID] == nil {
		return nil, notFound("unknown run %s (`mesh agents` lists runs)", runID)
	}

	projectsDir := d.claudeProjectsDir()
	worktreesSanitized := sanitizeClaudeCwd(d.worktrees())

	claudeFiles := map[string]bool{}
	agySessions := map[string]string{}    // session -> kind ("agy")
	openCodeSessions := map[string]bool{} // session -> true

	if runID != "" {
		w := windows[runID]
		for sess, segs := range segments {
			for _, seg := range segs {
				if seg.run != runID {
					continue
				}
				switch seg.tally {
				case "claude":
					for _, p := range claudeSessionFiles(projectsDir, sess, seg.cwds) {
						claudeFiles[p] = true
					}
				case "agy":
					agySessions[sess] = seg.cli
				case "opencode":
					openCodeSessions[sess] = true
				}
			}
		}
		if projectsDir != "" {
			for _, cwd := range w.cwds {
				if cwd == "" {
					continue
				}
				glob, _ := filepath.Glob(filepath.Join(projectsDir, sanitizeClaudeCwd(cwd), "*.jsonl"))
				for _, p := range recentFiles(glob, w.start) {
					claudeFiles[p] = true
				}
			}
		}
	} else {
		if projectsDir != "" {
			glob, _ := filepath.Glob(filepath.Join(projectsDir, "*", "*.jsonl"))
			for _, p := range recentFiles(glob, since) {
				claudeFiles[p] = true
			}
		}
		if convDir := d.agyConversationsDir(); convDir != "" {
			for _, s := range agy.RecentConversations(convDir, since) {
				agySessions[s] = agy.Kind
			}
		}
		if dbPath := d.opencodeDBPathForCost(); dbPath != "" {
			for _, s := range opencode.RecentSessions(dbPath, since) {
				openCodeSessions[s] = true
			}
		}
	}

	type fileRead struct {
		path    string
		calls   []costCall
		info    claudeSessionInfo
		sub     bool
		session string
		inSegs  bool
	}
	var reads []fileRead
	for p := range claudeFiles {
		sess := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		calls, info := readClaudeCalls(p)
		_, inSegs := segments[sess]
		reads = append(reads, fileRead{path: p, calls: calls, info: info, sub: false, session: sess, inSegs: inSegs})
		for _, sp := range subagentFiles(p) {
			scalls, _ := readClaudeCalls(sp)
			reads = append(reads, fileRead{path: sp, calls: scalls, info: info, sub: true, session: sess, inSegs: inSegs})
		}
	}
	sort.Slice(reads, func(i, j int) bool {
		a, b := reads[i], reads[j]
		if a.inSegs != b.inSegs {
			return a.inSegs
		}
		var aFirst, aLast, bFirst, bLast time.Time
		if len(a.calls) > 0 {
			aFirst, aLast = a.calls[0].t, a.calls[len(a.calls)-1].t
		}
		if len(b.calls) > 0 {
			bFirst, bLast = b.calls[0].t, b.calls[len(b.calls)-1].t
		}
		if !aFirst.Equal(bFirst) {
			return aFirst.Before(bFirst)
		}
		if !aLast.Equal(bLast) {
			return aLast.Before(bLast)
		}
		return a.path < b.path
	})

	tally := newCostTally(true)
	tokenTally := newCostTally(false)
	reviewed := map[string]map[string]bool{}
	seenClaudeIDs := map[string]bool{}

	for _, rd := range reads {
		mine := segments[rd.session]
		place := func(c costCall) *placedCall {
			var seg *costSegment
			if len(mine) > 0 {
				seg = segmentAt(mine, c.t)
			}
			if seg == nil && rd.info.rootCommands {
				seg = rootFallback(windows, rd.info.cwd, c.t)
			}
			if seg != nil {
				if runID != "" && seg.run != runID {
					return nil
				}
				for _, pr := range seg.prs {
					if reviewed[pr] == nil {
						reviewed[pr] = map[string]bool{}
					}
					who := seg.who
					if who == "" {
						who = seg.name
					}
					reviewed[pr][who] = true
				}
				return &placedCall{
					kind:  seg.kind,
					agent: agentKey{run: seg.run, name: seg.name},
					phase: phaseAt(seg.marks, c.t),
				}
			}
			if runID != "" {
				return nil
			}
			kind := "user"
			if (worktreesSanitized != "" && strings.Contains(filepath.Base(filepath.Dir(rd.path)), worktreesSanitized)) || rd.info.doorbell {
				kind = "worker (unmapped)"
			} else if rd.info.entrypoint == "sdk-cli" {
				kind = "headless"
			}
			return &placedCall{kind: kind, agent: agentKey{name: kind}}
		}
		tallySession(tally, rd.session, rd.calls, rd.sub, place, since, until, seenClaudeIDs)
	}

	if convDir := d.agyConversationsDir(); convDir != "" {
		sessList := make([]string, 0, len(agySessions))
		for s := range agySessions {
			sessList = append(sessList, s)
		}
		sort.Strings(sessList)
		for _, sess := range sessList {
			rawCalls := agy.ConversationCalls(convDir, sess)
			calls := make([]costCall, len(rawCalls))
			for i, rc := range rawCalls {
				calls[i] = costCall{t: rc.Time, model: "agy", input: rc.Input, read: rc.Read, output: rc.Output}
			}
			mine := segments[sess]
			place := func(c costCall) *placedCall {
				var seg *costSegment
				if len(mine) > 0 {
					seg = segmentAt(mine, c.t)
				}
				if seg != nil {
					if runID != "" && seg.run != runID {
						return nil
					}
					return &placedCall{
						kind:  seg.kind,
						agent: agentKey{run: seg.run, name: seg.name},
						phase: phaseAt(seg.marks, c.t),
						model: modelAt(seg, c.t),
					}
				}
				if runID != "" {
					return nil
				}
				return &placedCall{kind: "agy user", agent: agentKey{name: "agy user"}}
			}
			tallySession(tokenTally, sess, calls, false, place, since, until, map[string]bool{})
		}
	}

	if dbPath := d.opencodeDBPathForCost(); dbPath != "" {
		sessList := make([]string, 0, len(openCodeSessions))
		for s := range openCodeSessions {
			sessList = append(sessList, s)
		}
		sort.Strings(sessList)
		for _, sess := range sessList {
			rawCalls := opencode.SessionCalls(dbPath, sess)
			calls := make([]costCall, len(rawCalls))
			for i, rc := range rawCalls {
				calls[i] = costCall{id: rc.ID, t: rc.Time, model: rc.Model, input: rc.Input, read: rc.Read, output: rc.Output, usd: rc.USD}
			}
			mine := segments[sess]
			place := func(c costCall) *placedCall {
				var seg *costSegment
				if len(mine) > 0 {
					seg = segmentAt(mine, c.t)
				}
				if seg != nil {
					if runID != "" && seg.run != runID {
						return nil
					}
					m := c.model
					if m == "" {
						m = modelAt(seg, c.t)
					}
					return &placedCall{
						kind:  seg.kind,
						agent: agentKey{run: seg.run, name: seg.name},
						phase: phaseAt(seg.marks, c.t),
						model: m,
					}
				}
				if runID != "" {
					return nil
				}
				return &placedCall{kind: "opencode user", agent: agentKey{name: "opencode user"}, model: c.model}
			}
			tallySession(tokenTally, sess, calls, false, place, since, until, map[string]bool{})
		}
	}

	var resets int32
	reviewers := map[string]map[string]string{} // pr -> who -> kind
	var verdicts []*v1.Verdict

	for rid, w := range windows {
		if runID != "" && rid != runID {
			continue
		}
		for _, rt := range w.resets {
			if withinWindow(rt, since, until) {
				resets++
			}
		}
		for _, rv := range w.reviews {
			if withinWindow(rv.at, since, until) {
				if reviewers[rv.pr] == nil {
					reviewers[rv.pr] = map[string]string{}
				}
				reviewers[rv.pr][rv.who] = rv.kind
			}
		}
		for _, vr := range w.verdicts {
			if withinWindow(vr.at, since, until) {
				verdicts = append(verdicts, vr.verdict)
			}
		}
	}

	scope := "every session"
	if runID != "" {
		scope = "run " + runID
	}
	if !since.IsZero() || !until.IsZero() {
		edge := func(t time.Time, fallback string) string {
			if t.IsZero() {
				return fallback
			}
			return t.Local().Format("2006-01-02 15:04 -0700")
		}
		scope += fmt.Sprintf(", %s → %s", edge(since, "start"), edge(until, "now"))
	}

	resp := buildCostReport(scope, tally, tokenTally, reviewed, resets, reviewers, verdicts)
	if runID == "" && !m.Agents {
		resp.Agents = nil
	}
	return connect.NewResponse(resp), nil
}

func buildCostReport(scope string, tally, tokenTally *costTally, reviewed map[string]map[string]bool,
	resets int32, reviewers map[string]map[string]string, verdicts []*v1.Verdict) *v1.GetCostResponse {

	var total float64
	for _, b := range tally.kinds {
		total += b.usd
	}

	agents := make([]*v1.AgentCost, 0, len(tally.agents))
	for ak, b := range tally.agents {
		agents = append(agents, &v1.AgentCost{
			Run:     ak.run,
			Name:    ak.name,
			Kind:    b.kind,
			Calls:   b.calls,
			Input:   b.input,
			W1:      b.w1,
			W5:      b.w5,
			Read:    b.read,
			Output:  b.output,
			Usd:     round2(b.usd),
			PlanUsd: round2(b.plan),
			ExecUsd: round2(b.exec),
			SubUsd:  round2(b.subUSD),
			Cold:    b.cold,
			ColdUsd: round2(b.coldUSD),
		})
	}
	sort.Slice(agents, func(i, j int) bool {
		si := agents[i].Usd + agents[i].SubUsd
		sj := agents[j].Usd + agents[j].SubUsd
		if si != sj {
			return si > sj
		}
		if agents[i].Run != agents[j].Run {
			return agents[i].Run < agents[j].Run
		}
		return agents[i].Name < agents[j].Name
	})

	kinds := make([]*v1.KindCost, 0, len(tally.kinds))
	var reviewUSD float64
	for kind, b := range tally.kinds {
		if strings.HasPrefix(kind, "reviewer") {
			reviewUSD += b.usd
		}
		writes := b.w5 + b.w1
		var share, usdPerCall, write5mShare float64
		var ctxPerCall, outPerCall int64
		if total > 0 {
			share = round1(100 * b.usd / total)
		}
		if b.calls > 0 {
			usdPerCall = round4(b.usd / float64(b.calls))
			ctxPerCall = b.ctx / b.calls
			outPerCall = b.output / b.calls
		}
		if writes > 0 {
			write5mShare = round1(100 * float64(b.w5) / float64(writes))
		}
		var firstMed int64
		hasMed := len(b.firsts) > 0
		if hasMed {
			sortedFirsts := append([]int64(nil), b.firsts...)
			sort.Slice(sortedFirsts, func(i, j int) bool { return sortedFirsts[i] < sortedFirsts[j] })
			firstMed = sortedFirsts[len(sortedFirsts)/2]
		}
		kinds = append(kinds, &v1.KindCost{
			Kind:              kind,
			Sessions:          int64(len(b.sessions)),
			Calls:             b.calls,
			Usd:               round2(b.usd),
			Share:             share,
			UsdPerCall:        usdPerCall,
			CtxPerCall:        ctxPerCall,
			OutPerCall:        outPerCall,
			Input:             b.input,
			W1:                b.w1,
			W5:                b.w5,
			Read:              b.read,
			Output:            b.output,
			Write_5MShare:     write5mShare,
			FirstCtxMedian:    firstMed,
			HasFirstCtxMedian: hasMed,
			Cold:              b.cold,
			ColdUsd:           round2(b.coldUSD),
		})
	}
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i].Usd != kinds[j].Usd {
			return kinds[i].Usd > kinds[j].Usd
		}
		return kinds[i].Kind < kinds[j].Kind
	})

	tokenKinds := make([]*v1.TokenKindCost, 0, len(tokenTally.kinds))
	for kind, b := range tokenTally.kinds {
		tokenKinds = append(tokenKinds, &v1.TokenKindCost{
			Kind:     kind,
			Sessions: int64(len(b.sessions)),
			Calls:    b.calls,
			Input:    b.input,
			Read:     b.read,
			Output:   b.output,
			Usd:      round2(b.usd),
		})
	}
	sort.Slice(tokenKinds, func(i, j int) bool {
		ti := tokenKinds[i].Input + tokenKinds[i].Read
		tj := tokenKinds[j].Input + tokenKinds[j].Read
		if ti != tj {
			return ti > tj
		}
		return tokenKinds[i].Kind < tokenKinds[j].Kind
	})

	byModel := make([]*v1.ModelPhaseCost, 0, len(tally.models))
	for mp, b := range tally.models {
		var ctxPerCall, outPerCall int64
		if b.calls > 0 {
			ctxPerCall = b.ctx / b.calls
			outPerCall = b.output / b.calls
		}
		byModel = append(byModel, &v1.ModelPhaseCost{
			Model:      mp.model,
			Phase:      mp.phase,
			Calls:      b.calls,
			Usd:        round2(b.usd),
			CtxPerCall: ctxPerCall,
			OutPerCall: outPerCall,
		})
	}
	sort.Slice(byModel, func(i, j int) bool {
		if byModel[i].Usd != byModel[j].Usd {
			return byModel[i].Usd > byModel[j].Usd
		}
		if byModel[i].Model != byModel[j].Model {
			return byModel[i].Model < byModel[j].Model
		}
		return byModel[i].Phase < byModel[j].Phase
	})

	tokensByModel := make([]*v1.TokenModelPhaseCost, 0, len(tokenTally.models))
	for mp, b := range tokenTally.models {
		tokensByModel = append(tokensByModel, &v1.TokenModelPhaseCost{
			Model:  mp.model,
			Phase:  mp.phase,
			Calls:  b.calls,
			Input:  b.input,
			Read:   b.read,
			Output: b.output,
			Usd:    round2(b.usd),
		})
	}
	sort.Slice(tokensByModel, func(i, j int) bool {
		ti := tokensByModel[i].Input + tokensByModel[i].Read
		tj := tokensByModel[j].Input + tokensByModel[j].Read
		if ti != tj {
			return ti > tj
		}
		if tokensByModel[i].Model != tokensByModel[j].Model {
			return tokensByModel[i].Model < tokensByModel[j].Model
		}
		return tokensByModel[i].Phase < tokensByModel[j].Phase
	})

	byCLI := map[string]int{}
	totalReviewers := 0
	for _, whoMap := range reviewers {
		totalReviewers += len(whoMap)
		for _, k := range whoMap {
			byCLI[k]++
		}
	}
	revSummary := &v1.ReviewCostSummary{
		Prs:         int64(len(reviewed)),
		Usd:         round2(reviewUSD),
		ReviewedPrs: int64(len(reviewers)),
		ByCli:       map[string]float64{},
	}
	if len(reviewed) > 0 {
		revSummary.UsdPerPr = round2(reviewUSD / float64(len(reviewed)))
		revSummary.HasUsdPerPr = true
	}
	if len(reviewers) > 0 {
		revSummary.ReviewersPerPr = round2(float64(totalReviewers) / float64(len(reviewers)))
		revSummary.HasReviewersPerPr = true
		for k, n := range byCLI {
			revSummary.ByCli[k] = round2(float64(n) / float64(len(reviewers)))
		}
	}

	return &v1.GetCostResponse{
		Scope:         scope,
		PricesAsOf:    pricesAsOf,
		Usd:           round2(total),
		Agents:        agents,
		Kinds:         kinds,
		TokenKinds:    tokenKinds,
		ByModel:       byModel,
		TokensByModel: tokensByModel,
		Reviews:       revSummary,
		Verdicts:      verdictRows(verdicts),
		Resets:        resets,
	}
}

func verdictRows(verdicts []*v1.Verdict) []*v1.VerdictCostSummary {
	type vKey struct{ kind, model, tier string }
	rows := map[vKey]*v1.VerdictCostSummary{}
	for _, v := range verdicts {
		k := vKey{kind: v.Kind, model: costModelFamily(v.Model), tier: v.Tier}
		row := rows[k]
		if row == nil {
			row = &v1.VerdictCostSummary{Kind: k.kind, Model: k.model, Tier: k.tier}
			rows[k] = row
		}
		row.Reviews++
		row.Agree += int64(v.Agree)
		row.Other += int64(v.Other)
		row.Disagree += int64(v.Disagree)
		row.Blocking += int64(v.Blocking)
	}
	out := make([]*v1.VerdictCostSummary, 0, len(rows))
	for _, row := range rows {
		row.Findings = row.Agree + row.Other + row.Disagree
		if row.Findings > 0 {
			row.AgreedPct = int32(math.Round(100 * float64(row.Agree+row.Other) / float64(row.Findings)))
			row.HasAgreedPct = true
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Findings != out[j].Findings {
			return out[i].Findings > out[j].Findings
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Model < out[j].Model
	})
	return out
}
