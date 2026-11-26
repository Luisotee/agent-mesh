package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
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
}

type openCodeRuntime struct {
	mu           sync.Mutex
	polled       map[string]*openCodePollState
	seenEvidence map[string]bool
	freeModels   []string
}

func (d *Daemon) opencodeBinary() (string, bool) {
	return d.cliBinary(opencode.Kind)
}

func (d *Daemon) cliBinary(kind string) (string, bool) {
	if bin := d.rt.cfg.Binaries[kind]; bin != "" {
		st, err := os.Stat(bin)
		return bin, err == nil && !st.IsDir() && st.Mode()&0o111 != 0
	}
	if testing.Testing() {
		return "", false
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
// nextOnChain() can expand tier chains in memory without shelling out under SQLite's writer lock.
func (d *Daemon) warmOpenCodeModels(ctx context.Context, kind string) {
	if kind != opencode.Kind {
		return
	}
	ad, err := d.rt.adapters.Get(opencode.Kind)
	if err != nil {
		return
	}
	oc, ok := ad.(*opencode.Adapter)
	if !ok {
		return
	}
	bin, have := d.opencodeBinary()
	if !have && oc.ListModels == nil {
		return
	}
	expanded, err := oc.ExpandChain(ctx, bin, d.cliBaseEnv(), []string{opencode.FreeFamily})
	if err != nil || len(expanded) == 0 {
		return
	}
	d.rt.openCode.mu.Lock()
	d.rt.openCode.freeModels = expanded
	d.rt.openCode.mu.Unlock()
}

// expandChain replaces any `free` entry in an OpenCode tier chain with the cached expanded `opencode/*` free
// models, deduplicating while preserving order. Non-OpenCode chains are returned unchanged.
func (d *Daemon) expandChain(kind string, chain []string) []string {
	if kind != opencode.Kind {
		return chain
	}
	d.rt.openCode.mu.Lock()
	free := append([]string(nil), d.rt.openCode.freeModels...)
	d.rt.openCode.mu.Unlock()
	if len(free) == 0 {
		return chain
	}
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
	if len(out) == 0 {
		return chain
	}
	return out
}

func (d *Daemon) seenOpenCodeEvidence(model, evidence string) bool {
	if evidence == "" {
		return false
	}
	d.rt.openCode.mu.Lock()
	defer d.rt.openCode.mu.Unlock()
	return d.rt.openCode.seenEvidence[model+"|"+evidence]
}

func (d *Daemon) markOpenCodeEvidence(model, evidence string) {
	if evidence == "" {
		return
	}
	d.rt.openCode.mu.Lock()
	if d.rt.openCode.seenEvidence == nil {
		d.rt.openCode.seenEvidence = map[string]bool{}
	}
	d.rt.openCode.seenEvidence[model+"|"+evidence] = true
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
	if d.rt.term == nil {
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
			var since time.Time
			if tm.StartedAt != nil {
				since = tm.StartedAt.AsTime()
			}
			if a.State != v1.AgentState_AGENT_STATE_WORKING && a.LastActivity != nil && a.LastActivity.AsTime().After(since) {
				since = a.LastActivity.AsTime()
			}
			ps = &openCodePollState{terminalID: a.TerminalId, since: since}
			d.rt.openCode.polled[a.Id] = ps
		}
		if !lastOut.After(ps.lastOutput) {
			d.rt.openCode.mu.Unlock()
			continue
		}
		since, prevMsgID, prevMsgStatus := ps.since, ps.lastMsgID, ps.lastMsgStatus
		d.rt.openCode.mu.Unlock()

		var notBefore time.Time
		if a.CreatedAt != nil {
			notBefore = a.CreatedAt.AsTime()
		}
		dbPath := opencode.DBPath(d.agentEnv(a, ""))
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
		if a.State != v1.AgentState_AGENT_STATE_LIMITED && (dbErr != nil || info.Status == "" || info.Status == "working") {
			if raw, serr := d.readScreen(ctx, a.TerminalId); serr == nil {
				if hit, ok := opencode.MatchPaneLimit(opencode.ScreenLines(raw)); ok {
					if (dbErr != nil || info.HasModelReply) && !d.seenOpenCodeEvidence(a.Model, hit.Evidence) {
						d.markOpenCodeEvidence(a.Model, hit.Evidence)
						events = append(events, &adapterv1.AgentEvent{
							Time: timestamppb.New(now),
							Kind: &adapterv1.AgentEvent_RateLimited{
								RateLimited: &adapterv1.RateLimited{Message: hit.Message, ResetIn: hit.ResetIn},
							},
						})
						nextMsgStatus = "limited"
						paneHit = true
					}
				}
			}
		}

		if !paneHit && dbErr == nil {
			switch info.Status {
			case "working":
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
				if info.MessageID != prevMsgID && !d.seenOpenCodeEvidence(a.Model, info.Evidence) {
					d.markOpenCodeEvidence(a.Model, info.Evidence)
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

		d.rt.openCode.mu.Lock()
		if curPS := d.rt.openCode.polled[a.Id]; curPS != nil && curPS.terminalID == a.TerminalId {
			curPS.lastOutput = lastOut
			curPS.lastMsgID = nextMsgID
			curPS.lastMsgStatus = nextMsgStatus
		}
		d.rt.openCode.mu.Unlock()

		if len(events) > 0 {
			d.applyOpenCodeEvents(ctx, a.Id, a.TerminalId, events)
		}
	}
}

func (d *Daemon) applyOpenCodeEvents(ctx context.Context, agentID, terminalID string, evs []*adapterv1.AgentEvent) {
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
		return
	}
	if becameDoorbellReady {
		d.deliverSoon(agentID)
	}
}
