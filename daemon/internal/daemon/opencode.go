package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

const opencodeBodyNote = "\n\nWhen sending a report (`plan`, `question`, `done`, `failed`), write the body to a file under `$(mesh dir)/reports/` (or `plans/`) and pass `--body @\"<path>\"` (never pass backticks inside double-quoted `--body \"...\"` in bash, which expands them as commands)."

func withOpenCodeBodyNote(kind, body string) string {
	if kind != opencode.Kind || strings.TrimSpace(body) == "" || strings.Contains(body, "--body @") {
		return body
	}
	return body + opencodeBodyNote
}

type openCodePollState struct {
	terminalID    string
	lastOutput    time.Time
	since         time.Time
	lastMsgID     string
	lastMsgStatus string
	stallChecked  time.Time // the last time a quiet terminal's open reply was looked at for a stall
	stalledFor    time.Time // the reply's UpdatedAt a stall was last told for: not looked at again until it moves
}

type openCodeEvidenceKey struct {
	agentID    string
	terminalID string
	model      string
	evidence   string
}

const openCodeWarmRetryAfter = 60 * time.Second

type openCodeRuntime struct {
	mu           sync.Mutex
	polled       map[string]*openCodePollState
	seenEvidence map[openCodeEvidenceKey]bool
	freeModels   []string
	// costs is each listed model's cost class (opencode.CostClasses); nil until a listing was read.
	costs        map[string]string
	lastWarmFail time.Time
	lastWarmErr  string
}

func (d *Daemon) nowTime() time.Time {
	if d != nil && d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Daemon) opencodeAdapter() *opencode.Adapter {
	if d == nil || d.rt == nil || d.rt.adapters == nil {
		return nil
	}
	ad, err := d.rt.adapters.Get(opencode.Kind)
	if err != nil {
		return nil
	}
	oc, _ := ad.(*opencode.Adapter)
	return oc
}

func (d *Daemon) opencodeBaseEnv() map[string]string {
	env := d.cliBaseEnv()
	for _, k := range opencodeInheritedEnv {
		if _, set := env[k]; !set {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
	}
	return env
}

func (d *Daemon) opencodeBinary() (string, bool) {
	return d.cliBinary(opencode.Kind)
}

func (d *Daemon) cliBinary(kind string) (string, bool) {
	if bin := d.rt.cfg.Binaries[kind]; bin != "" {
		st, err := os.Stat(bin)
		return bin, err == nil && !st.IsDir() && st.Mode()&0o111 != 0
	}
	if d.rt.cfg.LookPath != nil {
		return d.rt.cfg.LookPath(kind)
	}
	if pathList := d.cliBaseEnv()["PATH"]; pathList != "" {
		if bin, ok := lookPath(kind, pathList); ok {
			return bin, true
		}
	}
	bin, err := exec.LookPath(kind)
	return bin, err == nil
}

// warmOpenCodeModels resolves OpenCode's `free` family outside any store.Write transaction so pick() and
// nextOnChain() can expand tier chains in memory without shelling out under SQLite's writer lock. Failed
// listings are remembered for openCodeWarmRetryAfter so repeated sends do not each wait on the failure.
func (d *Daemon) warmOpenCodeModels(ctx context.Context, kind string) {
	if kind != opencode.Kind {
		return
	}
	oc := d.opencodeAdapter()
	if oc == nil {
		return
	}
	bin, have := d.opencodeBinary()
	if !have && oc.ListModels == nil {
		return
	}
	now := d.nowTime()
	d.rt.openCode.mu.Lock()
	if !d.rt.openCode.lastWarmFail.IsZero() && now.Sub(d.rt.openCode.lastWarmFail) < openCodeWarmRetryAfter {
		d.rt.openCode.mu.Unlock()
		return
	}
	d.rt.openCode.mu.Unlock()

	listing, err := oc.Listing(ctx, bin, d.opencodeBaseEnv())
	var expanded []string
	if err == nil {
		expanded = opencode.FreeModels(listing)
	} else {
		err = fmt.Errorf("listing opencode models: %w", err)
	}
	d.rt.openCode.mu.Lock()
	defer d.rt.openCode.mu.Unlock()
	if err == nil {
		// What OpenCode reports as each model's cost, for `repos.no_free_models` (freemodels.go).
		d.rt.openCode.costs = opencode.CostClasses(listing)
	}
	if err != nil || len(expanded) == 0 {
		d.rt.openCode.lastWarmFail = d.nowTime()
		if err != nil {
			d.rt.openCode.lastWarmErr = err.Error()
		} else {
			d.rt.openCode.lastWarmErr = "no free models returned"
		}
		return
	}
	d.rt.openCode.freeModels = expanded
	d.rt.openCode.lastWarmFail = time.Time{}
	d.rt.openCode.lastWarmErr = ""
}

func (d *Daemon) openCodeNoFreeError() string {
	d.rt.openCode.mu.Lock()
	warmErr := d.rt.openCode.lastWarmErr
	d.rt.openCode.mu.Unlock()
	if warmErr != "" {
		return fmt.Sprintf("no active zero-cost %s models available (%s)", opencode.Kind, warmErr)
	}
	return fmt.Sprintf("no active zero-cost %s models found (`opencode models opencode`)", opencode.Kind)
}

// expandChain replaces any `free` entry in an OpenCode tier chain with the cached expanded `opencode/*` free
// models, deduplicating while preserving order. When the cache is empty, `free` is dropped rather than
// returning the literal family name so no agent ever launches on `"free"`.
func (d *Daemon) expandChain(kind string, chain []string) []string {
	if kind != opencode.Kind {
		return chain
	}
	d.rt.openCode.mu.Lock()
	free := append([]string(nil), d.rt.openCode.freeModels...)
	d.rt.openCode.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, item := range chain {
		cands := []string{item}
		if item == opencode.FreeFamily {
			cands = free
		}
		for _, m := range cands {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

func (d *Daemon) seenOpenCodeEvidence(agentID, terminalID, model, evidence string) bool {
	if evidence == "" {
		return false
	}
	d.rt.openCode.mu.Lock()
	defer d.rt.openCode.mu.Unlock()
	return d.rt.openCode.seenEvidence[openCodeEvidenceKey{
		agentID:    agentID,
		terminalID: terminalID,
		model:      model,
		evidence:   evidence,
	}]
}

func (d *Daemon) markOpenCodeEvidence(agentID, terminalID, model, evidence string) {
	if evidence == "" {
		return
	}
	d.rt.openCode.mu.Lock()
	if d.rt.openCode.seenEvidence == nil {
		d.rt.openCode.seenEvidence = map[openCodeEvidenceKey]bool{}
	}
	d.rt.openCode.seenEvidence[openCodeEvidenceKey{
		agentID:    agentID,
		terminalID: terminalID,
		model:      model,
		evidence:   evidence,
	}] = true
	d.rt.openCode.mu.Unlock()
}

func (d *Daemon) clearOpenCodeEvidence(agentID string) {
	if d == nil || d.rt == nil {
		return
	}
	d.rt.openCode.mu.Lock()
	d.clearOpenCodeEvidenceLocked(agentID)
	d.rt.openCode.mu.Unlock()
}

func (d *Daemon) clearOpenCodeEvidenceLocked(agentID string) {
	for k := range d.rt.openCode.seenEvidence {
		if k.agentID == agentID {
			delete(d.rt.openCode.seenEvidence, k)
		}
	}
}

func (d *Daemon) clearOpenCodeTerminal(terminalID string) {
	if d == nil || d.rt == nil || terminalID == "" {
		return
	}
	d.rt.openCode.mu.Lock()
	for id, ps := range d.rt.openCode.polled {
		if ps.terminalID == terminalID {
			delete(d.rt.openCode.polled, id)
		}
	}
	for k := range d.rt.openCode.seenEvidence {
		if k.terminalID == terminalID {
			delete(d.rt.openCode.seenEvidence, k)
		}
	}
	d.rt.openCode.mu.Unlock()
}

func (d *Daemon) readScreen(ctx context.Context, terminalID string) ([]byte, error) {
	if d.rt.term == nil {
		return nil, fmt.Errorf("no terminal host")
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s, err := termclient.Attach(rctx, d.rt.term.HTTP, client.BaseURL+"/term/"+terminalID, d.rt.term.Token,
		&termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT})
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var buf []byte
	for {
		f, err := s.Recv(rctx)
		if err != nil {
			return buf, err
		}
		if sc := f.GetScreen(); sc != nil {
			buf = append(buf, sc.Data...)
		}
		if o := f.GetOutput(); o != nil {
			buf = append(buf, o.Data...)
		}
		if f.GetReplayDone() != nil {
			return buf, nil
		}
	}
}

// pollOpenCode inspects live OpenCode agents whose terminal output moved since the last poll: it records
// their session IDs, drives WORKING / IDLE / ERROR / LIMITED transitions via the reducer, and records model
// limit holds from `opencode.db` and in-flight retry banners.
func (d *Daemon) pollOpenCode(ctx context.Context) {
	oc := d.opencodeAdapter()
	if d.rt == nil || d.rt.term == nil || oc == nil {
		return
	}
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	var live []*v1.Agent
	for _, a := range agents {
		if a.Kind == opencode.Kind && !stopped(a) && a.TerminalId != "" && a.State != v1.AgentState_AGENT_STATE_STARTING {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return
	}
	byID := make(map[string]*termv1.Terminal, len(res.Msg.Terminals))
	for _, tm := range res.Msg.Terminals {
		byID[tm.Id] = tm
	}

	stallEvery := d.openCodeStallRecheckEvery(ctx)
	for _, a := range live {
		if ctx.Err() != nil {
			return
		}
		tm := byID[a.TerminalId]
		if tm == nil || tm.Exited || tm.LastOutput == nil {
			continue
		}
		lastOut := tm.LastOutput.AsTime()

		d.rt.openCode.mu.Lock()
		if d.rt.openCode.polled == nil {
			d.rt.openCode.polled = map[string]*openCodePollState{}
		}
		ps := d.rt.openCode.polled[a.Id]
		if ps == nil || ps.terminalID != a.TerminalId {
			d.clearOpenCodeEvidenceLocked(a.Id)
			var since time.Time
			if tm.StartedAt != nil {
				since = tm.StartedAt.AsTime()
			}
			if a.State != v1.AgentState_AGENT_STATE_WORKING && a.LastActivity != nil && a.LastActivity.AsTime().After(since) {
				since = a.LastActivity.AsTime()
			}
			ps = &openCodePollState{terminalID: a.TerminalId, since: since}
			d.rt.openCode.polled[a.Id] = ps
		} else if a.State != v1.AgentState_AGENT_STATE_LIMITED && ps.lastMsgStatus == "limited" {
			d.clearOpenCodeEvidenceLocked(a.Id)
		}
		if !lastOut.After(ps.lastOutput) {
			// A terminal with nothing new on it can still be a stalled reply: look at the DB once the screen has been
			// quiet for openCodeStallRecheck (or the stall time, when that is shorter), and then that often.
			quiet := stallEvery > 0 && ps.lastMsgStatus == "working" && a.State == v1.AgentState_AGENT_STATE_WORKING &&
				d.now().Sub(lastOut) >= stallEvery && d.now().Sub(ps.stallChecked) >= stallEvery
			if quiet {
				ps.stallChecked = d.now()
			}
			since := ps.since
			d.rt.openCode.mu.Unlock()
			if quiet {
				var notBefore time.Time
				if a.CreatedAt != nil {
					notBefore = a.CreatedAt.AsTime()
				}
				if info, err := opencode.InspectSession(oc.DBPath(d.agentEnv(a, "")), a.SessionId, a.Worktree, a.Repo, a.Model, since, notBefore); err == nil {
					d.openCodeStalled(ctx, a, info)
				}
			}
			continue
		}
		since, prevMsgID, prevMsgStatus := ps.since, ps.lastMsgID, ps.lastMsgStatus
		d.rt.openCode.mu.Unlock()

		var notBefore time.Time
		if a.CreatedAt != nil {
			notBefore = a.CreatedAt.AsTime()
		}
		dbPath := oc.DBPath(d.agentEnv(a, ""))
		info, dbErr := opencode.InspectSession(dbPath, a.SessionId, a.Worktree, a.Repo, a.Model, since, notBefore)

		now := d.now()
		var events []*adapterv1.AgentEvent
		if dbErr == nil && info.SessionID != "" && a.SessionId == "" {
			events = append(events, &adapterv1.AgentEvent{
				Time: timestamppb.New(now),
				Kind: &adapterv1.AgentEvent_SessionStarted{
					SessionStarted: &adapterv1.SessionStarted{SessionId: info.SessionID, Source: "turn"},
				},
			})
		}

		nextMsgID, nextMsgStatus := prevMsgID, prevMsgStatus
		paneHit := false
		var markedEvidence string
		if a.State != v1.AgentState_AGENT_STATE_LIMITED && (dbErr != nil || info.Status == "" || info.Status == "working") {
			if raw, serr := d.readScreen(ctx, a.TerminalId); serr == nil {
				if hit, ok := opencode.MatchPaneLimit(opencode.ScreenLines(raw)); ok {
					if (dbErr != nil || info.HasModelReply) && !d.seenOpenCodeEvidence(a.Id, a.TerminalId, a.Model, hit.Evidence) {
						markedEvidence = hit.Evidence
						events = append(events, &adapterv1.AgentEvent{
							Time: timestamppb.New(now),
							Kind: &adapterv1.AgentEvent_RateLimited{
								RateLimited: &adapterv1.RateLimited{Message: hit.Message, ResetIn: hit.ResetIn},
							},
						})
						if dbErr == nil && info.MessageID != "" {
							nextMsgID = info.MessageID
							if info.LastRole == "user" {
								// Its reply has no row yet: the one that comes is this turn's, still at its limit.
								nextMsgID = ""
							}
						}
						nextMsgStatus = "limited"
						paneHit = true
					}
				}
			}
		}

		if !paneHit && dbErr == nil {
			switch info.Status {
			case "working":
				if a.State == v1.AgentState_AGENT_STATE_LIMITED && info.MessageID == prevMsgID {
					break
				}
				// The limit was read off the screen before the DB showed the reply (no DB yet, or no message): the
				// reply the DB now shows is that turn, still at its limit, not a new turn that ends it.
				if a.State == v1.AgentState_AGENT_STATE_LIMITED && prevMsgID == "" && prevMsgStatus == "limited" {
					if info.LastRole != "user" { // the user's message is not its reply: wait for that
						nextMsgID = info.MessageID
					}
					break
				}
				if a.State != v1.AgentState_AGENT_STATE_WORKING {
					events = append(events, &adapterv1.AgentEvent{
						Time: timestamppb.New(now),
						Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}},
					})
				} else if info.MessageID != "" && info.MessageID != prevMsgID {
					events = append(events, &adapterv1.AgentEvent{
						Time: timestamppb.New(now),
						Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: "opencode"}},
					})
				}
				nextMsgID, nextMsgStatus = info.MessageID, "working"
			case "idle":
				if info.MessageID != prevMsgID || a.State == v1.AgentState_AGENT_STATE_WORKING {
					if a.State == v1.AgentState_AGENT_STATE_IDLE && prevMsgStatus != "working" {
						events = append(events, &adapterv1.AgentEvent{
							Time: timestamppb.New(now),
							Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}},
						})
					}
					events = append(events, &adapterv1.AgentEvent{
						Time: timestamppb.New(now),
						Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}},
					})
				}
				nextMsgID, nextMsgStatus = info.MessageID, "idle"
			case "limited":
				if info.MessageID != prevMsgID && !d.seenOpenCodeEvidence(a.Id, a.TerminalId, a.Model, info.Evidence) {
					markedEvidence = info.Evidence
					events = append(events, &adapterv1.AgentEvent{
						Time: timestamppb.New(now),
						Kind: &adapterv1.AgentEvent_RateLimited{
							RateLimited: &adapterv1.RateLimited{Message: info.LimitMessage, ResetIn: info.ResetIn},
						},
					})
				}
				nextMsgID, nextMsgStatus = info.MessageID, "limited"
			case "error":
				if info.MessageID != prevMsgID {
					events = append(events, &adapterv1.AgentEvent{
						Time: timestamppb.New(now),
						Kind: &adapterv1.AgentEvent_TurnFailed{TurnFailed: &adapterv1.TurnFailed{Error: info.ErrorText}},
					})
				}
				nextMsgID, nextMsgStatus = info.MessageID, "error"
			}
		}

		if !paneHit && dbErr == nil {
			d.openCodeStalled(ctx, a, info)
		}
		var applyErr error
		if len(events) > 0 {
			applyErr = d.applyOpenCodeEvents(ctx, a.Id, a.TerminalId, events)
		}
		if applyErr == nil && markedEvidence != "" {
			d.markOpenCodeEvidence(a.Id, a.TerminalId, a.Model, markedEvidence)
		}
		if dbErr == nil && applyErr == nil {
			d.rt.openCode.mu.Lock()
			if curPS := d.rt.openCode.polled[a.Id]; curPS != nil && curPS.terminalID == a.TerminalId {
				curPS.lastOutput = lastOut
				curPS.lastMsgID = nextMsgID
				curPS.lastMsgStatus = nextMsgStatus
			}
			d.rt.openCode.mu.Unlock()
		} else if dbErr != nil && paneHit && applyErr == nil {
			// The limit is recorded though the DB could not be read (the output stays unread, so the poll comes
			// back): remember that this turn is at its limit, or the DB's first sight of it would start a turn.
			d.rt.openCode.mu.Lock()
			if curPS := d.rt.openCode.polled[a.Id]; curPS != nil && curPS.terminalID == a.TerminalId {
				curPS.lastMsgStatus = "limited"
				curPS.lastMsgID = "" // whatever it was, the reply that comes is this turn's
			}
			d.rt.openCode.mu.Unlock()
		}
	}
}

func (d *Daemon) applyOpenCodeEvents(ctx context.Context, agentID, terminalID string, evs []*adapterv1.AgentEvent) error {
	var becameDoorbellReady bool
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil || stopped(cur) || cur.TerminalId != terminalID {
			return err
		}
		before := cur.State
		tx.Actor = &v1.Actor{Class: ClassOf(cur.Role), Id: cur.Id, Name: cur.Name, RunId: cur.RunId}
		if err := d.applyEvents(tx, cur, evs); err != nil {
			return err
		}
		becameDoorbellReady = takesDoorbell(cur.State) && cur.State != before
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("opencode poll", "agent", agentID, "err", err)
		}
		return err
	}
	if becameDoorbellReady {
		d.deliverSoon(agentID)
	}
	return nil
}

// openCodeStallRecheck is how often a quiet terminal's open reply is looked at again (a spinner that draws makes
// the poll look at every change instead).
const openCodeStallRecheck = time.Minute

// openCodeStalled tells the agent's parent, once per stretch, that its open reply has not changed for
// `opencode.stall_after` seconds: no new part and no token. A stream that fails silently looks like this, and
// without it the agent stays WORKING until the ask times out (a limit is read from the DB or the screen before
// this). One long tool call looks the same, which the text says.
func (d *Daemon) openCodeStalled(ctx context.Context, a *v1.Agent, info opencode.SessionInfo) {
	if !owesParent(a) || info.Status != "working" || info.UpdatedAt.IsZero() || a.State != v1.AgentState_AGENT_STATE_WORKING ||
		d.view(a).State == v1.AgentState_AGENT_STATE_UNKNOWN {
		return // a root has no parent to tell, and a hang the view calls UNKNOWN is told by that alert: nothing is read or written
	}
	d.rt.openCode.mu.Lock()
	told := d.rt.openCode.polled[a.Id] != nil && !info.UpdatedAt.After(d.rt.openCode.polled[a.Id].stalledFor)
	d.rt.openCode.mu.Unlock()
	if told {
		return // this stretch of the reply was told (or found told): the next poll of it reads nothing
	}
	r := d.store.Read(ctx)
	after := d.openCodeStallAfter(r)
	if after <= 0 {
		return
	}
	if idle := d.now().Sub(info.UpdatedAt); idle < after {
		return
	}
	inStore, err := d.alertOnceSince(ctx, r, a, "stalled", info.UpdatedAt, func(ctx context.Context) string {
		return fmt.Sprintf("%s's OpenCode reply has been open for %s with no new part or token (a stalled or silently failing stream, "+
			"or one long tool call). Look at its terminal; if it is stuck, interrupt it (esc) and send it a task to go on.",
			a.Name, ago(d.now().Sub(info.UpdatedAt))) + d.afterwards(ctx, a)
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("opencode stall alert", "agent", a.Name, "err", err)
		}
		return
	}
	if !inStore {
		return // the write's re-check found the agent moved on (say, UNKNOWN by then) and left nothing: not told, look again
	}
	d.rt.openCode.mu.Lock()
	if ps := d.rt.openCode.polled[a.Id]; ps != nil {
		ps.stalledFor = info.UpdatedAt
	}
	d.rt.openCode.mu.Unlock()
}

// openCodeStallAfter is `opencode.stall_after` as a duration; 0 when it is off (or can't be read).
func (d *Daemon) openCodeStallAfter(r store.Reader) time.Duration {
	v, err := d.Setting(r, "opencode.stall_after")
	if err != nil || v.(float64) <= 0 {
		return 0
	}
	return time.Duration(v.(float64) * float64(time.Second))
}

// openCodeStallRecheckEvery is how often a quiet terminal's open reply is looked at: openCodeStallRecheck, or the
// stall time when that is shorter; 0 when stall alerts are off.
func (d *Daemon) openCodeStallRecheckEvery(ctx context.Context) time.Duration {
	after := d.openCodeStallAfter(d.store.Read(ctx))
	if after <= 0 {
		return 0
	}
	return min(after, openCodeStallRecheck)
}
