package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Root reset (docs/design.md §15). A root's conversation grows for the whole run, and every turn re-reads it; once
// it is big and its cache has gone cold, a fresh session that rebuilds from the run's notes (`mesh brief`) is
// cheaper. `mesh reset` asks for one; the keeper asks for it by itself when the root sits idle.
//
// The daemon owns the root's terminal, so a reset is a step of `deliver`, under the agent's lock like a doorbell
// or a reopen: wait until the root is at its prompt with nobody typing, then relaunch it lean in a fresh
// conversation (a reopen with Fresh set), or type `/clear` (or `/compact`), then type the bootstrap line that
// tells it to run `mesh brief`. A reset never leaves the root without a process.

const (
	// DefaultAutoresetEvery is how often the keeper looks at the roots of open runs.
	autoresetEvery = 60 * time.Second
	// autoresetQuiet is how long a root must have been idle after a turn mesh started; autoresetUserQuiet after
	// one the user started or that can't be told apart (it may be waiting on them).
	autoresetQuiet     = 5 * time.Minute
	autoresetUserQuiet = 30 * time.Minute
	// autoresetBackoff is how long after a reset was asked for the keeper asks again, whatever came of it.
	autoresetBackoff = time.Hour
	// resetConfirmWait is how long a reset waits for its `/clear` to show as a new session, or its fresh process
	// to come up at its prompt.
	resetConfirmWait = 120 * time.Second
	// clearSettle is how long after a `/clear` typed into an agy root its bootstrap waits (agy reports no new
	// conversation until its next turn).
	clearSettle = 3 * time.Second

	autoresetNext = "carry on supervising: mesh reset you while you were idle"
	watchNote     = "; if notes.md says you were waiting on an answer from the user, ask the user again rather than going on without it"
)

// ResetTimings overrides the reset's timings (zero: the default); for tests.
type ResetTimings struct {
	Every, Quiet, UserQuiet, Backoff, Confirm, Settle time.Duration
}

func (d *Daemon) resetTimings() ResetTimings {
	t := d.rt.cfg.Reset
	t.Every = orDuration(t.Every, autoresetEvery)
	t.Quiet = orDuration(t.Quiet, autoresetQuiet)
	t.UserQuiet = orDuration(t.UserQuiet, autoresetUserQuiet)
	t.Backoff = orDuration(t.Backoff, autoresetBackoff)
	t.Confirm = orDuration(t.Confirm, resetConfirmWait)
	t.Settle = orDuration(t.Settle, clearSettle)
	return t
}

// ── the root's context ──

// rootCtx is a root's context size and cache state.
type rootCtx struct {
	usage *v1.ContextUsage
	at    int64         // root_reset.at
	idle  time.Duration // since its last turn, when it is idle
}

// rootContext measures a root's conversation, or nil when it can't be read (an agy root before its first turn, a
// transcript not found, a CLI that compacts itself).
func (d *Daemon) rootContext(ctx context.Context, root *v1.Agent) *rootCtx {
	if root == nil || !d.rt.adapters.HasCapability(root.Kind, capContext) {
		return nil
	}
	u := d.measureContext(ctx, root)
	if !u.Measured {
		return nil
	}
	at, _ := d.settingInt(d.store.Read(ctx), "root_reset.at")
	c := &rootCtx{usage: u, at: int64(at)}
	if root.State == v1.AgentState_AGENT_STATE_IDLE && root.LastActivity != nil {
		c.idle = d.now().Sub(root.LastActivity.AsTime())
	} else if u.WrittenAt != nil {
		c.idle = d.now().Sub(u.WrittenAt.AsTime())
	}
	return c
}

// text is `root context 452k, cache cold since 14:02`, or warm with how long it has been still.
func (c *rootCtx) text(now time.Time) string {
	out := "root context " + kilo(c.usage.Tokens)
	if c.usage.WrittenAt == nil {
		return out
	}
	if c.usage.Cold {
		since := c.usage.WrittenAt.AsTime().Add(time.Duration(c.usage.TtlSeconds) * time.Second)
		return fmt.Sprintf("%s, cache cold since %s", out, since.Local().Format("15:04"))
	}
	return fmt.Sprintf("%s, cache warm (%s since its last turn)", out, ago(c.idle))
}

// ── asking for a reset ──

// runRoot is a run's root agent.
func (d *Daemon) runRoot(r store.Reader, runID string) (*v1.Agent, error) {
	agents, err := r.ListAgents(runID)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			return a, nil
		}
	}
	return nil, failedPrecondition("run %s has no root", runID)
}

// unreadBlocking lists the root's unread plans and questions: a worker waits on each, and the root answers them
// before a reset.
func unreadBlocking(r store.Reader, root *v1.Agent) ([]*v1.Message, error) {
	mail, err := r.Inbox(root.Id, false)
	if err != nil {
		return nil, err
	}
	var out []*v1.Message
	for _, m := range mail {
		if m.Type == v1.MessageType_MESSAGE_TYPE_PLAN || m.Type == v1.MessageType_MESSAGE_TYPE_QUESTION {
			out = append(out, m)
		}
	}
	return out, nil
}

func mailText(ms []*v1.Message) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = fmt.Sprintf("a %s from %s", strings.ToLower(strings.TrimPrefix(m.Type.String(), "MESSAGE_TYPE_")), m.From.GetName())
	}
	return strings.Join(parts, ", ")
}

// requestReset stores a reset on the root, with its event. The caller stores the agent and calls deliverSoon.
func (d *Daemon) requestReset(tx *store.Tx, root *v1.Agent, via, next, focus string, compact bool, usage *v1.ContextUsage) (*v1.RootReset, error) {
	if !d.rt.adapters.HasCapability(root.Kind, capContext) {
		return nil, failedPrecondition("%s has no Claude or agy session to reset", root.Name)
	}
	if compact && !d.rt.adapters.HasCapability(root.Kind, capCompactLive) {
		return nil, invalid("compact is for a Claude root: %s runs %s", root.Name, root.Kind)
	}
	if root.PendingReset != nil {
		return nil, failedPrecondition("a reset is already pending for %s", root.Name)
	}
	if stopped(root) || root.TerminalId == "" {
		return nil, failedPrecondition("%s has no process to reset: resume it", root.Name)
	}
	if waits, err := unreadBlocking(tx.Reader, root); err != nil {
		return nil, err
	} else if len(waits) > 0 {
		return nil, failedPrecondition("%s waits on you: answer it first (`mesh inbox`), then reset", mailText(waits))
	}
	rs := &v1.RootReset{Id: ids.New("r", 10), Via: via, Next: strings.TrimSpace(next), Compact: compact, Focus: strings.TrimSpace(focus),
		RequestedAt: tsOf(tx.Now()), OldSession: root.SessionId}
	if usage != nil {
		rs.Tokens, rs.Transcript = usage.Tokens, usage.Transcript
	}
	root.PendingReset = rs
	if _, err := tx.AppendEvent(&v1.Event{RunId: root.RunId, AgentId: root.Id, Payload: &v1.Event_RootResetRequested{
		RootResetRequested: &v1.RootResetRequested{RootReset: rs}}}); err != nil {
		return nil, err
	}
	return rs, tx.PutAgent(root)
}

// ResetRoot asks for the root's context to be reset: once its turn has ended (and nobody is typing) it starts a
// fresh session, which is told to rebuild from `mesh brief`.
func (d *Daemon) ResetRoot(ctx context.Context, req *connect.Request[v1.ResetRootRequest]) (*connect.Response[v1.ResetRootResponse], error) {
	m := req.Msg
	actor := auth.ActorFrom(ctx)
	runID, err := scopedRun(actor, m.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("name the run (run_id)")
	}
	out := &v1.ResetRootResponse{}
	var rootID string
	usage := func() *v1.ContextUsage { // read before the write: a file read, never inside the transaction
		root, err := d.runRoot(d.store.Read(ctx), runID)
		if err != nil {
			return nil
		}
		return d.measureContext(ctx, root)
	}()
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceResetRootProcedure, out, func(tx *store.Tx) error {
		root, err := d.runRoot(tx.Reader, runID)
		if err != nil {
			return err
		}
		if IsAgent(tx.Actor.Class) && tx.Actor.Id != root.Id {
			return denied("only the run's root resets itself")
		}
		if run, err := tx.GetRun(runID); err != nil || run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			return failedPrecondition("run %s is closed", runID)
		}
		rootID = root.Id
		if out.RootReset, err = d.requestReset(tx, root, "command", m.Next, m.Focus, m.Compact, usage); err != nil {
			return err
		}
		out.Notes = []string{"end your turn now: once it has ended (and nobody is typing), mesh starts the fresh session and types the prompt that runs `mesh brief`"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	d.deliverSoon(rootID)
	return connect.NewResponse(out), nil
}

// ── carrying a reset out ──

// bootstrapText is the line a reset root wakes to: a doorbell (it begins like one), but a root's: it has no
// parent to report to. After the keeper's reset the root may have been waiting on the user: its notes say so.
func bootstrapText(root *v1.Agent, pr *v1.RootReset, compacted bool) string {
	what := "gave you a fresh session (your earlier context is gone)"
	if compacted {
		what = "compacted your session"
	}
	then := pr.Next
	if then == "" {
		then = "carry on from the next step in your notes"
	}
	if pr.Via == "keeper" {
		then += watchNote
	}
	return fmt.Sprintf("📬 [mesh] You are %s, the root of agent-mesh run %s, and `mesh reset` just %s. You have no parent to report to. "+
		"Run `mesh brief`, act by the root card it prints, and rebuild your picture from it, not from memory. Then: %s",
		root.Name, root.RunId, what, then)
}

// finishReset ends a reset: done, failed or cancelled. The root's last reset is kept for its brief and its hints.
func (d *Daemon) finishReset(ctx context.Context, agentID string, pr *v1.RootReset, outcome, reason, newSession string) error {
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
			return nil
		}
		done := proto.Clone(cur.PendingReset).(*v1.RootReset)
		done.Outcome, done.Reason, done.NewSession, done.FinishedAt = outcome, reason, newSession, tsOf(tx.Now())
		cur.PendingReset, cur.LastReset = nil, done
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id, Payload: &v1.Event_RootResetFinished{
			RootResetFinished: &v1.RootResetFinished{RootReset: done}}})
		return err
	})
}

// updateReset changes the agent's pending reset (and, with more, the agent) when it is still the one named.
func (d *Daemon) updateReset(ctx context.Context, agentID, id string, f func(*v1.Agent, *v1.RootReset)) error {
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if cur.PendingReset == nil || cur.PendingReset.Id != id {
			return nil
		}
		f(cur, cur.PendingReset)
		return tx.PutAgent(cur)
	})
}

// resetStep is the reset part of deliver, under the root's lock: it reports whether the reset holds the root for
// now (no doorbell is typed and nothing else is reopened until it is done), and when it changed what deliver
// would act on, the caller runs deliver again.
func (d *Daemon) resetStep(ctx context.Context, a *v1.Agent) (bool, error) {
	pr := a.PendingReset
	if pr == nil {
		return false, nil
	}
	now := d.now()
	t := d.resetTimings()
	leftover := a.State == v1.AgentState_AGENT_STATE_RETIRED && a.TerminalId != ""
	gone := a.TerminalId == "" || a.State == v1.AgentState_AGENT_STATE_EXITED || (stopped(a) && !leftover)

	if pr.TypedAt == nil {
		return d.startReset(ctx, a, pr, gone, now)
	}
	switch pr.Mode {
	case "relaunch":
		if a.PendingReopen != nil {
			return false, nil // the reopen runs next in this deliver
		}
		if gone {
			return true, d.resumeAfterFailedRelaunch(ctx, a, pr, "the fresh session did not start; the old one was resumed")
		}
		if a.State != v1.AgentState_AGENT_STATE_IDLE {
			if now.Sub(pr.TypedAt.AsTime()) > t.Confirm {
				// Too slow (a dialog, memory pressure): the process would sit in an empty conversation, unbriefed.
				// It is stopped and the old conversation resumed, as for a launch that failed.
				if err := d.stopTerminal(ctx, a.TerminalId); err != nil {
					return true, err
				}
				return true, d.resumeAfterFailedRelaunch(ctx, a, pr, "the fresh session did not come up at its prompt in time; it was stopped and the old one resumed")
			}
			return true, nil
		}
		return d.ringBootstrap(ctx, a, pr, false, a.SessionId)
	case "clear":
		if gone { // it ended after the /clear: holding it would block its reopen
			return false, d.finishReset(ctx, a.Id, pr, "failed", "its process ended before the prompt to run `mesh brief` was typed", "")
		}
		if a.Kind == claude.Kind {
			if a.SessionId == pr.OldSession || a.State != v1.AgentState_AGENT_STATE_IDLE {
				if now.Sub(pr.TypedAt.AsTime()) > t.Confirm {
					return false, d.finishReset(ctx, a.Id, pr, "failed", "the /clear didn't show up as a new session; no prompt typed", "")
				}
				return true, nil
			}
		} else if now.Sub(pr.TypedAt.AsTime()) < t.Settle || a.State != v1.AgentState_AGENT_STATE_IDLE {
			if now.Sub(pr.TypedAt.AsTime()) > t.Confirm { // never back at its prompt (a limit, an error): not held for good
				return false, d.finishReset(ctx, a.Id, pr, "failed", "the root did not come back to its prompt after /clear; no prompt typed", "")
			}
			return true, nil // agy reports no new conversation before its next turn: settle, then ring
		}
		newSession := ""
		if a.Kind == claude.Kind {
			newSession = a.SessionId // agy's is recorded by its next turn's hooks
		}
		return d.ringBootstrap(ctx, a, pr, false, newSession)
	case "compact":
		if a.PendingCompaction != nil {
			return false, nil // the compaction step runs next in this deliver
		}
		if gone {
			return false, d.finishReset(ctx, a.Id, pr, "failed", "its process ended before the prompt to run `mesh brief` was typed", "")
		}
		if pr.Reason != "" { // its compaction did not complete
			return false, d.finishReset(ctx, a.Id, pr, "failed", pr.Reason, "")
		}
		if now.Sub(pr.TypedAt.AsTime()) > t.Confirm+compactTimeout {
			return false, d.finishReset(ctx, a.Id, pr, "failed", "the compaction did not finish", "")
		}
		if u := d.measureContext(ctx, a); u.Measured && pr.Tokens > 0 && u.Tokens >= pr.Tokens {
			return false, d.finishReset(ctx, a.Id, pr, "failed", "its context did not shrink", "")
		}
		return d.ringBootstrap(ctx, a, pr, true, a.SessionId)
	}
	return false, d.finishReset(ctx, a.Id, pr, "failed", "unknown reset mode "+pr.Mode, "")
}

// startReset is the first stage: wait for a quiet prompt, then begin by the reset's mode.
func (d *Daemon) startReset(ctx context.Context, a *v1.Agent, pr *v1.RootReset, gone bool, now time.Time) (bool, error) {
	r := d.store.Read(ctx)
	if waits, err := unreadBlocking(r, a); err != nil {
		return true, err
	} else if len(waits) > 0 { // a report is on its way: it rings after this
		return false, d.finishReset(ctx, a.Id, pr, "cancelled", mailText(waits)+" arrived", "")
	}
	wait, err := d.Setting(r, "root_reset.wait")
	if err != nil {
		return true, err
	}
	why := "it never read idle"
	if gone {
		return false, d.finishReset(ctx, a.Id, pr, "failed", "its process is gone", "")
	}
	if takesDoorbell(a.State) {
		quiet, err := d.quietToType(ctx, a.TerminalId)
		if err != nil {
			return true, err
		}
		if quiet {
			return d.beginReset(ctx, a, pr)
		}
		why = "someone is typing into it"
	}
	if secs, _ := wait.(float64); now.Sub(pr.RequestedAt.AsTime()) > time.Duration(secs*float64(time.Second)) {
		return false, d.finishReset(ctx, a.Id, pr, "failed", fmt.Sprintf("gave up after waiting %s: %s", ago(time.Duration(secs*float64(time.Second))), why), "")
	}
	return true, nil
}

// beginReset starts the reset at a quiet prompt: relaunch the root lean in a fresh conversation (a Claude root,
// `root_reset.relaunch`), else type /clear, or /compact for a compacting reset.
func (d *Daemon) beginReset(ctx context.Context, a *v1.Agent, pr *v1.RootReset) (bool, error) {
	r := d.store.Read(ctx)
	relaunch, err := d.Setting(r, "root_reset.relaunch")
	if err != nil {
		return true, err
	}
	mode := "clear"
	switch {
	case pr.Compact:
		mode = "compact"
	case relaunch == true && a.Kind == claude.Kind:
		mode = "relaunch"
	}
	now := d.now()
	switch mode {
	case "compact":
		err = d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil || cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
				return err
			}
			cur.PendingReset.Mode, cur.PendingReset.TypedAt = mode, timestamppb.New(now)
			if cur.PendingCompaction == nil {
				cur.PendingCompaction = &v1.PendingCompaction{Id: ids.New("c", 10), Why: "reset", Focus: pr.Focus,
					RequestedAt: timestamppb.New(now), Always: true}
			}
			return tx.PutAgent(cur)
		})
	case "relaunch":
		err = d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil || cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
				return err
			}
			cur.PendingReset.Mode, cur.PendingReset.TypedAt = mode, timestamppb.New(now)
			model, effort, deny := target(cur)
			rv, err := requestReopen(tx, cur, model, effort, deny, "reset", "mesh reset")
			if err != nil {
				return err
			}
			rv.Fresh = true
			return tx.PutAgent(cur)
		})
	default:
		// The mode is recorded first; for agy the old id goes once the clear is typed (below).
		err = d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil || cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
				return err
			}
			cur.PendingReset.Mode, cur.PendingReset.TypedAt = mode, timestamppb.New(now)
			return tx.PutAgent(cur)
		})
		if err == nil {
			if terr := d.typeLine(ctx, a.TerminalId, "/clear"); terr != nil {
				_ = d.finishReset(ctx, a.Id, pr, "failed", "typing /clear failed: "+terr.Error(), "")
				return false, nil
			}
			if a.Kind != claude.Kind {
				// agy drops hooks of a conversation other than its own, so the old id goes once the clear is typed: the
				// next turn's hooks record the new conversation. (A failed clear keeps the id.)
				err = d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
					cur, err := tx.GetAgent(a.Id)
					if err != nil || cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
						return err
					}
					cur.SessionId = ""
					return tx.PutAgent(cur)
				})
			}
		}
	}
	if err != nil {
		return true, err
	}
	d.deliverSoon(a.Id)
	return true, nil
}

// ringBootstrap types the bootstrap line into the reset root and finishes the reset.
func (d *Daemon) ringBootstrap(ctx context.Context, a *v1.Agent, pr *v1.RootReset, compacted bool, newSession string) (bool, error) {
	quiet, err := d.quietToType(ctx, a.TerminalId)
	if err != nil {
		return true, err
	}
	if !quiet {
		// Someone is typing into the root: the line waits, for as long as a reset waits for a quiet prompt, and no
		// longer: the root is not held for good (it holds doorbells while the reset is pending).
		now := d.now()
		if pr.BootstrapWaitedSince == nil {
			return true, d.updateReset(ctx, a.Id, pr.Id, func(_ *v1.Agent, p *v1.RootReset) { p.BootstrapWaitedSince = timestamppb.New(now) })
		}
		wait, err := d.Setting(d.store.Read(ctx), "root_reset.wait")
		if err != nil {
			return true, err
		}
		if secs, _ := wait.(float64); now.Sub(pr.BootstrapWaitedSince.AsTime()) > time.Duration(secs*float64(time.Second)) {
			return false, d.finishReset(ctx, a.Id, pr, "failed", fmt.Sprintf("its session was reset, but the prompt to run `mesh brief` was not typed: someone was typing into it for %s. Run `mesh brief` yourself",
				ago(time.Duration(secs*float64(time.Second)))), newSession)
		}
		return true, nil
	}
	if err := d.typeLine(ctx, a.TerminalId, bootstrapText(a, pr, compacted)); err != nil {
		return false, d.finishReset(ctx, a.Id, pr, "failed", "its session was reset, but the prompt to run `mesh brief` didn't get through: "+err.Error(), newSession)
	}
	d.rt.mu.Lock()
	d.rt.introduced[a.TerminalId] = true
	d.rt.mu.Unlock()
	return true, d.finishReset(ctx, a.Id, pr, "done", "", newSession)
}

// resumeAfterFailedRelaunch brings the old conversation back when the fresh process did not start (or was too slow
// to, and was stopped): a reset never leaves the root without a process, nor with an unbriefed one.
func (d *Daemon) resumeAfterFailedRelaunch(ctx context.Context, a *v1.Agent, pr *v1.RootReset, why string) error {
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil || cur.PendingReset == nil || cur.PendingReset.Id != pr.Id {
			return err
		}
		// The fresh conversation never had a turn (its process died before the bootstrap): the old one is the
		// root's. A launch that was recorded sets its planned id even when the process died at once.
		if pr.OldSession != "" {
			cur.SessionId = pr.OldSession
		}
		if cur.PendingReopen == nil && cur.SessionId != "" {
			model, effort, deny := target(cur)
			if _, err := requestReopen(tx, cur, model, effort, deny, "resumed", "the fresh session of a reset did not start"); err != nil {
				return err
			}
		}
		return tx.PutAgent(cur)
	})
	if err != nil {
		return err
	}
	if err := d.finishReset(ctx, a.Id, pr, "failed", why, pr.OldSession); err != nil {
		return err
	}
	d.deliverSoon(a.Id)
	return nil
}

// ── the hint a root command prints ──

// rootMailReports lists the root's unread reports (a plan, question, done or failed): what it owes an answer to.
func rootMailReports(r store.Reader, root *v1.Agent) ([]*v1.Message, error) {
	mail, err := r.Inbox(root.Id, false)
	if err != nil {
		return nil, err
	}
	var out []*v1.Message
	for _, m := range mail {
		if isReport(m.Type) {
			out = append(out, m)
		}
	}
	return out, nil
}

// rootHint is what a root command says about the root's own context, or "" (design §15). A /clear never re-reads
// the old context, so a reset costs the fresh session's small prefix whenever it happens; what it saves is the
// next turns of the big one: all but its static prefix rewritten at the cache-write price once the cache is cold,
// all of it at a twentieth while warm. Hence: reset now when cold, or when about to wait (the next report may well
// come after the cache expired); while warm, only when very big, at a quiet point. failure says the line reports
// a failed or cancelled reset, which it does once.
func (d *Daemon) rootHint(ctx context.Context, root *v1.Agent, aboutToWait bool) (line string, failure bool) {
	if root == nil || root.PendingReset != nil {
		return "", false
	}
	r := d.store.Read(ctx)
	var notes []string
	if lr := root.LastReset; lr != nil && (lr.Outcome == "failed" || lr.Outcome == "cancelled") && !lr.Noted {
		what := "failed"
		if lr.Outcome == "cancelled" {
			what = "was cancelled"
		}
		notes = append(notes, fmt.Sprintf("the last `mesh reset` %s %s ago: %s", what, ago(d.now().Sub(lr.FinishedAt.AsTime())), lr.Reason))
		failure = true
	}
	c := d.rootContext(ctx, root)
	if c != nil && c.at > 0 && c.usage.Tokens > c.at {
		reset := "write notes.md, then run `mesh reset --next \"<what you do next>\"` and end your turn"
		waits, _ := rootMailReports(r, root)
		switch {
		case c.usage.Cold:
			n := fmt.Sprintf("%s: its next turn re-reads all of it at the cache-write price; %s", c.text(d.now()), reset)
			if len(waits) > 0 {
				n += ", once you've handled " + mailText(waits)
			}
			notes = append(notes, n)
		case aboutToWait && len(waits) == 0:
			notes = append(notes, fmt.Sprintf("%s, and you're about to wait on workers: a report that wakes you after the cache expires (%s) re-reads all of it at the cache-write price; %s",
				c.text(d.now()), ago(time.Duration(c.usage.TtlSeconds)*time.Second), reset))
		case c.usage.Tokens > 2*c.at:
			notes = append(notes, fmt.Sprintf("%s: at your next quiet point (a phase boundary, nothing waiting on you), %s", c.text(d.now()), reset))
		}
	}
	return strings.Join(notes, "; "), failure
}

// HintHeader is the response header that carries a root command's hint to the CLI, which prints it on stderr.
const HintHeader = "Agent-Mesh-Hint"

// hintProcedures are the root's calls that say nothing of its context: hooks fire all the time, and the brief
// carries the same text itself.
var hintSilent = map[string]bool{
	agentmeshv1connect.MeshServiceReportHookProcedure: true,
	agentmeshv1connect.MeshServiceGetBriefProcedure:   true,
	agentmeshv1connect.MeshServiceHelloProcedure:      true,
	agentmeshv1connect.MeshServiceResetRootProcedure:  true,
}

// hintAboutToWait are the calls after which the root ends its turn to wait on workers.
var hintAboutToWait = map[string]bool{
	agentmeshv1connect.MeshServiceSpawnAgentProcedure:   true,
	agentmeshv1connect.MeshServiceApproveAgentProcedure: true,
	agentmeshv1connect.MeshServiceSwitchAgentProcedure:  true,
	agentmeshv1connect.MeshServiceResumeAgentProcedure:  true,
	agentmeshv1connect.MeshServiceAskProcedure:          true,
}

// hintInterceptor adds the root's context hint to the response of a root's call, as a header (no message, so no
// doorbell wakes the root to read it). It never fails the call.
func (d *Daemon) hintInterceptor() connect.Interceptor {
	return hintInterceptor{d}
}

type hintInterceptor struct{ d *Daemon }

func (h hintInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		if err != nil || req.Spec().IsClient || hintSilent[req.Spec().Procedure] {
			return res, err
		}
		actor := auth.ActorFrom(ctx)
		if actor == nil || actor.Class != v1.ActorClass_ACTOR_CLASS_ROOT {
			return res, err
		}
		root, gerr := h.d.store.Read(ctx).GetAgent(actor.Id)
		if gerr != nil || root.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			return res, err
		}
		line, failure := h.d.rootHint(ctx, root, hintAboutToWait[req.Spec().Procedure])
		if line != "" {
			res.Header().Set(HintHeader, headerText(line))
		}
		if failure {
			h.d.noteResetFailure(root)
		}
		return res, err
	}
}

func (h hintInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (h hintInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// noteResetFailure marks the last failed reset as told: a command shows it once.
func (d *Daemon) noteResetFailure(root *v1.Agent) {
	if root.LastReset == nil {
		return
	}
	id := root.LastReset.Id
	err := d.store.Write(context.Background(), nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(root.Id)
		if err != nil || cur.LastReset == nil || cur.LastReset.Id != id || cur.LastReset.Noted {
			return err
		}
		cur.LastReset.Noted = true
		return tx.PutAgent(cur)
	})
	if err != nil {
		d.log.Warn("noting a reset failure", "agent", root.Id, "err", err)
	}
}

// ── the keeper ──

// keeperState is when the keeper last looked.
type keeperState struct {
	mu   sync.Mutex
	last time.Time
}

// autoReset is the keeper's pass: a root over `root_reset.at`, idle, owed nothing and quiet is reset by itself,
// warm or cold (design §15). Quiet is five minutes since its last turn when mesh started that turn, thirty when
// the user did or it can't be told (an agy turn's hooks carry no prompt): a root answering the user may be waiting
// on them. Stricter than `mesh reset`: any unread mail holds it, since it acts without the root's say.
func (d *Daemon) autoReset(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	t := d.resetTimings()
	d.rt.keeper.mu.Lock()
	if d.now().Sub(d.rt.keeper.last) < t.Every {
		d.rt.keeper.mu.Unlock()
		return
	}
	d.rt.keeper.last = d.now()
	d.rt.keeper.mu.Unlock()
	r := d.store.Read(ctx)
	if on, err := d.Setting(r, "root_reset.auto"); err != nil || on != true {
		return
	}
	runs, err := r.ListRuns(false)
	if err != nil {
		return
	}
	for _, run := range runs {
		if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
			continue
		}
		root, err := d.runRoot(r, run.Id)
		if err != nil || root.PendingReset != nil || root.PendingReopen != nil || stopped(root) || root.TerminalId == "" ||
			!takesDoorbell(root.State) || !d.rt.adapters.HasCapability(root.Kind, capContext) {
			continue
		}
		if lr := root.LastReset; lr != nil && lr.RequestedAt != nil && d.now().Sub(lr.RequestedAt.AsTime()) < t.Backoff {
			continue // a failing one shouldn't repeat every minute
		}
		if mail, err := r.Inbox(root.Id, false); err != nil || len(mail) > 0 {
			continue
		}
		c := d.rootContext(ctx, d.agentView(root))
		if c == nil || c.at <= 0 || c.usage.Tokens <= c.at || root.LastActivity == nil {
			continue
		}
		quiet := t.UserQuiet
		if root.LastTurnByMesh {
			quiet = t.Quiet
		}
		if d.now().Sub(root.LastActivity.AsTime()) < quiet {
			continue
		}
		usage := c.usage
		err = d.store.Write(ctx, &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Name: "mesh keeper"}, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(root.Id)
			if err != nil {
				return err
			}
			if cur.PendingReset != nil || !takesDoorbell(cur.State) {
				return nil
			}
			_, err = d.requestReset(tx, cur, "keeper", autoresetNext, "", false, usage)
			return err
		})
		if err != nil {
			d.log.Warn("the keeper's reset", "agent", root.Name, "err", err)
			continue
		}
		d.deliverSoon(root.Id)
	}
}

// headerText is a line made safe for a header value: control characters (a failed reset's reason may hold a
// newline) become spaces, and it is cut to 1000 characters.
func headerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 1000 {
		s = string(r[:1000]) + "…"
	}
	return s
}
