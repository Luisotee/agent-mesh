package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Compaction (docs/design.md §12): a conversation a resume, an approve or a follow-up round would re-read in full is
// compacted first when it pays. The request (a PendingCompaction on the agent) fixes the thresholds; whatever runs
// it measures the conversation then and decides. A stopped conversation is compacted headless, before its reopen;
// a live Claude one by typing `/compact <focus>` at its quiet prompt, which the CLI's own hooks confirm
// (PreCompact, then SessionStart of source compact). It is never a failure: whatever goes wrong, the agent goes
// on as it was.

const (
	// The defaults of AgentConfig's CompactQuietWait (how long a live compaction waits for a user to stop typing
	// before it gives up), CompactStartWait (how long after `/compact` was typed its PreCompact hook may take) and
	// CompactTimeout (what bounds a compaction, headless or live).
	compactQuietWait = 60 * time.Second
	compactStartWait = 60 * time.Second
	compactTimeout   = 900 * time.Second
	// compactSettle is how long after the CLI's own hook said a typed `/compact` is over the daemon waits for the
	// compaction to show in the conversation's transcript before it goes on (AgentConfig's CompactSettle).
	compactSettle = 20 * time.Second

	capContext     = "context"      // the adapter can measure a conversation and plan a headless compaction
	capCompactLive = "compact-live" // Claude: a warm prompt cache whose compaction can be typed into the live session
)

// The focus of a compaction: what its summary keeps (bin/mesh's COMPACT_FOCUS, PEER_FOCUS and APPROVE_FOCUS).
const (
	compactFocusText = "Keep what the next task needs: %s. Keep the repo facts, the decisions and their reasons, the branch and the PR; drop tool output and dead ends."
	peerFocusText    = "Keep what you learned about %s: its architecture and mechanisms with file:line references, the answers you gave and the upstream commit you last read; drop tool output and dead ends."
	approveFocusText = "Your plan was just approved, and you carry it out next in this conversation. Keep the approved plan (%s) verbatim, the files it changes with the facts you read from them (file:line), the decisions and their reasons, and the branch; drop tool output and dead ends."
)

// contextRuntime is the daemon's in-memory state of context management; its zero value is ready.
type contextRuntime struct {
	mu         sync.Mutex
	compacting map[string]bool // agent id → a headless compaction is running for it
}

func (c *contextRuntime) begin(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compacting[id] {
		return false
	}
	if c.compacting == nil {
		c.compacting = map[string]bool{}
	}
	c.compacting[id] = true
	return true
}

func (c *contextRuntime) end(id string) {
	c.mu.Lock()
	delete(c.compacting, id)
	c.mu.Unlock()
}

func (c *contextRuntime) running(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.compacting[id]
}

// ── focus ──

var paragraphSplit = regexp.MustCompile(`\n\s*\n`)

const directPrefix = "Direct task (no plan): research, make the change, verify it, open the PR and report `done`."

var directLead = regexp.MustCompile(`^Direct task(?:\s*\(no plan\))?[:.]?\s*`)

func standardStart(p string) bool {
	return isReadOnlyTask(p) || strings.HasPrefix(p, "Plan review by the user") || isDirectTask(p)
}

// taskGist is the line that says what a task is: the first one past the standard prefixes, without a heading's #
// (bin/mesh's task_gist).
func taskGist(task string) string {
	for _, p := range paragraphSplit.Split(task, -1) {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, directPrefix) || (standardStart(p) && !isDirectTask(p)) {
			continue
		}
		if isDirectTask(p) { // a root's own `Direct task: <the task>`: the task is what follows
			p = directLead.ReplaceAllString(p, "")
		}
		if p == "" {
			continue
		}
		for _, ln := range strings.Split(strings.TrimSpace(strings.TrimLeft(p, "#")), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				if r := []rune(ln); len(r) > 160 {
					ln = string(r[:160])
				}
				return ln
			}
		}
	}
	return ""
}

func resumeFocus(a *v1.Agent, task string) string {
	if a.Peer {
		repo := filepath.Base(a.Repo)
		if a.Repo == "" {
			repo = filepath.Base(a.Worktree)
		}
		return fmt.Sprintf(peerFocusText, repo)
	}
	g := taskGist(task)
	if g == "" {
		g = "the follow-up task you are about to get"
	}
	return fmt.Sprintf(compactFocusText, g)
}

// ── requests ──

// nonzeroMin is the smaller of the thresholds that are on (0 is off), or 0.
func nonzeroMin(xs ...int) int {
	out := 0
	for _, x := range xs {
		if x > 0 && (out == 0 || x < out) {
			out = x
		}
	}
	return out
}

// requestResumeCompaction asks for the compaction a resume does before it reopens a stopped conversation, when it
// pays (bin/mesh's should_compact, design §12): a warm conversation above `compact.<kind>`, a cold one (its prompt
// cache expired, so the first turn writes it all again) above the lower of that and `compact.claude_cold`, twice
// as much for a read-only task but not a standing peer's; an agy conversation of unreadable size only when it
// comes back from another run and the task isn't read-only. A switch never compacts. mode is "auto" (or empty),
// "yes" or "no". The caller stores the agent.
func (d *Daemon) requestResumeCompaction(tx *store.Tx, a *v1.Agent, spec reviveSpec) error {
	mode := spec.compact
	if mode == "" {
		mode = "auto"
	}
	if mode == "no" || !d.rt.adapters.HasCapability(a.Kind, capContext) {
		return nil
	}
	readOnly := adapter.ReadOnly(a.Role) || isReadOnlyTask(spec.taskText)
	pc := &v1.PendingCompaction{Id: ids.New("c", 10), Why: "resume", Focus: spec.focus, RequestedAt: tsOf(tx.Now())}
	if pc.Focus == "" {
		pc.Focus = resumeFocus(a, spec.taskText)
	}
	if mode == "yes" {
		pc.Always = true
		a.PendingCompaction = pc
		return nil
	}
	scale := 1
	if readOnly && !a.Peer {
		scale = 2
	}
	if d.rt.adapters.HasCapability(a.Kind, capCompactLive) {
		warm, err := d.settingInt(tx.Reader, "compact.claude")
		if err != nil {
			return err
		}
		cold, err := d.settingInt(tx.Reader, "compact.claude_cold")
		if err != nil {
			return err
		}
		pc.WarmAt, pc.ColdAt = int64(warm*scale), int64(nonzeroMin(warm, cold)*scale)
	} else {
		warm, err := d.settingInt(tx.Reader, "compact.agy")
		if err != nil {
			return err
		}
		pc.WarmAt = int64(warm * scale)
		pc.Unmeasured = spec.crossRun && !readOnly
	}
	if pc.WarmAt == 0 && pc.ColdAt == 0 && !pc.Unmeasured {
		return nil
	}
	a.PendingCompaction = pc
	return nil
}

// requestApproveCompaction asks for the compaction `approve` does before the reopen that lifts a planner's deny
// list or moves it to its execution model: over `compact.approve_switch` when the model differs (the new model
// writes the whole context anyway, a prompt cache being per model), over `compact.approve` when it stays (the
// reopen keeps the cache, so only a big context repays it). Claude only. The caller stores the agent.
func (d *Daemon) requestApproveCompaction(tx *store.Tx, a *v1.Agent, model string) error {
	if !d.rt.adapters.HasCapability(a.Kind, capCompactLive) {
		return nil
	}
	key := "compact.approve"
	if model != a.Model {
		key = "compact.approve_switch"
	}
	at, err := d.settingInt(tx.Reader, key)
	if err != nil || at <= 0 {
		return err
	}
	plan := filepath.Join(d.paths.RunDir(a.RunId), "plans", a.Name+".md")
	a.PendingCompaction = &v1.PendingCompaction{Id: ids.New("c", 10), Why: "approve", Focus: fmt.Sprintf(approveFocusText, plan),
		RequestedAt: tsOf(tx.Now()), WarmAt: int64(at), ColdAt: int64(at)}
	return nil
}

// requestFollowupCompaction asks for the compaction before a follow-up round: a task or answer for a live Claude
// change worker that has reported done or failed since its last one. Over `compact.followup` while its cache is
// warm, over the lower of that and `compact.followup_cold` once it is cold. The caller stores the agent.
func (d *Daemon) requestFollowupCompaction(tx *store.Tx, a *v1.Agent, typ v1.MessageType, body string) error {
	if typ != v1.MessageType_MESSAGE_TYPE_TASK && typ != v1.MessageType_MESSAGE_TYPE_ANSWER {
		return nil
	}
	if a.Role != v1.AgentRole_AGENT_ROLE_WORKER || a.Peer || a.PendingCompaction != nil || a.TerminalId == "" ||
		stopped(a) || !d.rt.adapters.HasCapability(a.Kind, capCompactLive) {
		return nil
	}
	report, err := lastReport(tx.Reader, a)
	if err != nil || report == nil ||
		(report.Type != v1.MessageType_MESSAGE_TYPE_DONE && report.Type != v1.MessageType_MESSAGE_TYPE_FAILED) {
		return err
	}
	warm, err := d.settingInt(tx.Reader, "compact.followup")
	if err != nil {
		return err
	}
	cold, err := d.settingInt(tx.Reader, "compact.followup_cold")
	if err != nil {
		return err
	}
	if warm == 0 && cold == 0 {
		return nil
	}
	g := taskGist(body)
	if g == "" {
		g = "the follow-up you are about to get"
	}
	a.PendingCompaction = &v1.PendingCompaction{Id: ids.New("c", 10), Why: "follow-up", Focus: fmt.Sprintf(compactFocusText, g),
		RequestedAt: tsOf(tx.Now()), WarmAt: int64(warm), ColdAt: int64(nonzeroMin(warm, cold))}
	return tx.PutAgent(a)
}

// wants reports whether a conversation of this size is worth compacting now.
func wants(pc *v1.PendingCompaction, u *v1.ContextUsage) bool {
	if pc.Always {
		return true
	}
	if !u.GetMeasured() {
		return pc.Unmeasured
	}
	at := pc.WarmAt
	if u.Cold && pc.ColdAt > 0 {
		at = pc.ColdAt
	}
	return at > 0 && u.Tokens > at
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// ── measuring ──

// measureContext reads an agent's conversation size through its adapter; an adapter without it, or a conversation
// it can't read, is unmeasured.
func (d *Daemon) measureContext(ctx context.Context, a *v1.Agent) *v1.ContextUsage {
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil || a.SessionId == "" {
		return &v1.ContextUsage{}
	}
	res, err := ad.MeasureContext(ctx, connect.NewRequest(&adapterv1.MeasureContextRequest{
		SessionId: a.SessionId, Cwd: a.Worktree, Env: d.cliBaseEnv()}))
	if err != nil || res.Msg.Usage == nil {
		return &v1.ContextUsage{}
	}
	return res.Msg.Usage
}

// ── carrying a compaction out ──

// quietToType reports whether a line may be typed into an agent's terminal now: nobody is typing into it, and its
// input box holds no draft (a user who typed and detached leaves one: the line would be typed after it, and
// submitted with it). Every typed `/compact`, `/clear` and bootstrap line asks here, as a doorbell does before it
// types. A box it can't read, or reads as empty, doesn't hold the line.
func (d *Daemon) quietToType(ctx context.Context, a *v1.Agent) (bool, error) {
	typing, err := d.userTyping(ctx, a.TerminalId)
	if err != nil || typing {
		return false, err
	}
	return d.readInputBox(ctx, a, "").GetBox() != adapterv1.InputBox_INPUT_BOX_TYPED, nil
}

// noteHookEvents is what an agent's hooks tell the context machinery: its turn began with a mesh doorbell or not
// (a root's reset waits longer after the user's), and a typed `/compact` began (PreCompact) and is over (the
// session restarted with source compact).
func noteHookEvents(a *v1.Agent, evs []*adapterv1.AgentEvent, now time.Time) {
	for _, ev := range evs {
		if ts := ev.GetTurnStarted(); ts != nil {
			a.LastTurnByMesh = ts.ByMesh
		}
		pc := a.PendingCompaction
		if pc == nil || pc.TypedAt == nil {
			continue
		}
		if ev.GetCompacting() != nil && pc.StartedAt == nil {
			pc.StartedAt = timestamppb.New(now)
		}
		if ss := ev.GetSessionStarted(); ss != nil && ss.Source == "compact" && pc.DoneAt == nil {
			pc.DoneAt = timestamppb.New(now)
		}
	}
}

// compactStep is the compaction part of deliver, under the agent's lock: it reports whether the compaction holds
// the agent for now (nothing else is typed or reopened until it is done or given up).
func (d *Daemon) compactStep(ctx context.Context, a *v1.Agent) (bool, error) {
	pc := a.PendingCompaction
	if pc == nil {
		return false, nil
	}
	if d.rt.ctxwork.running(a.Id) {
		return true, nil
	}
	if pc.TypedAt == nil && pc.StartedAt != nil {
		// A headless compaction was started and nothing here is running it: meshd restarted in the middle. Its CLI
		// may still be running or may have finished, and running it again could compact twice: the request ends.
		return false, d.endCompaction(ctx, a, pc, "meshd restarted while it was compacting; it was not run again", pc.TokensBefore, 0)
	}
	if !d.rt.adapters.HasCapability(a.Kind, capContext) {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0)
	}
	leftover := a.State == v1.AgentState_AGENT_STATE_RETIRED && a.TerminalId != ""
	gone := a.TerminalId == "" || a.State == v1.AgentState_AGENT_STATE_EXITED || (stopped(a) && !leftover)
	now := d.now()

	if pc.TypedAt != nil { // live: waiting for the CLI to say it is over
		switch {
		case pc.DoneAt != nil:
			// Claude Code fires SessionStart (compact) before the compacted transcript is on disk: reopening now
			// (approve, a model switch) stops the process before its write, and the next `--resume` reads the old,
			// uncompacted conversation (found live: two approves at once both left their 65k workers whole, one
			// alone landed; the 215k worker of bin/mesh was the same race). So the compaction is done when the
			// transcript says so (a size below the one before, or a boundary), or after compactSettle.
			after := d.measureContext(ctx, a)
			if !compactLanded(after, pc.TokensBefore) {
				if now.Sub(pc.DoneAt.AsTime()) < orDuration(d.rt.cfg.CompactSettle, compactSettle) {
					return true, nil // looked at again by the next contextPass
				}
				return false, d.endCompaction(ctx, a, pc, "the CLI reported the compaction done, but its transcript showed no smaller size in time",
					pc.TokensBefore, tokensOf(after))
			}
			return false, d.endCompaction(ctx, a, pc, "done", pc.TokensBefore, tokensOf(after))
		case gone || leftover:
			return false, d.endCompaction(ctx, a, pc, "its process ended first", pc.TokensBefore, 0)
		case pc.StartedAt == nil && now.Sub(pc.TypedAt.AsTime()) > orDuration(d.rt.cfg.CompactStartWait, compactStartWait):
			return false, d.endCompaction(ctx, a, pc, "the CLI did not start compacting", pc.TokensBefore, 0)
		case now.Sub(pc.TypedAt.AsTime()) > orDuration(d.rt.cfg.CompactTimeout, compactTimeout):
			return false, d.endCompaction(ctx, a, pc, "the compaction did not finish", pc.TokensBefore, 0)
		}
		return true, nil
	}

	if leftover {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0) // a process of unknown state: the reopen stops it
	}
	if gone {
		if a.PendingReopen == nil && pc.Why != "resume" {
			// Asked for a live agent (approve, follow-up) whose process ended before it was typed: nothing reopens it
			// now, and a later resume asks for its own.
			return false, d.endCompaction(ctx, a, pc, "", 0, 0)
		}
		return d.compactHeadless(ctx, a, pc)
	}
	if !atPrompt(a.State) {
		return true, nil // between turns only
	}
	if a.State == v1.AgentState_AGENT_STATE_LIMITED {
		return false, d.endCompaction(ctx, a, pc, "it is at a usage limit", 0, 0)
	}
	if !d.rt.adapters.HasCapability(a.Kind, capCompactLive) {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0)
	}
	usage := d.measureContext(ctx, a)
	if !wants(pc, usage) {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0)
	}
	quiet, qerr := d.quietToType(ctx, a)
	if qerr != nil || !quiet {
		if pc.WaitedSince == nil {
			return true, d.updateCompaction(ctx, a.Id, pc.Id, func(p *v1.PendingCompaction) { p.WaitedSince = timestamppb.New(now) })
		}
		if now.Sub(pc.WaitedSince.AsTime()) > orDuration(d.rt.cfg.CompactQuietWait, compactQuietWait) {
			why := "a user was typing, so it was not compacted"
			if qerr != nil { // can't tell: the same wait, so a terminal host that stays down doesn't hold the agent for good
				why = "its terminal could not be checked, so it was not compacted"
			}
			return false, d.endCompaction(ctx, a, pc, why, usage.Tokens, 0)
		}
		return true, qerr
	}
	line := "/compact"
	if focus := oneLine(pc.Focus); focus != "" { // one line: a newline would be a keystroke
		line += " " + focus
	}
	// Recorded before the line is typed: the CLI's PreCompact can arrive before typeLine returns, and a hook for a
	// compaction not yet marked typed is ignored.
	if err := d.updateCompaction(ctx, a.Id, pc.Id, func(p *v1.PendingCompaction) {
		p.TypedAt, p.TokensBefore = timestamppb.New(d.now()), usage.Tokens
	}); err != nil {
		return true, err
	}
	if err := d.typeLine(ctx, a.TerminalId, line); err != nil {
		// Ended, not retried: a failed Enter can leave the text in the input box, and typing it again would double
		// the line (and hold every doorbell meanwhile).
		return false, d.endCompaction(ctx, a, pc, "could not type /compact: "+err.Error(), usage.Tokens, 0)
	}
	return true, nil
}

// compactLanded reports whether a measurement of the conversation shows a compaction that began at before tokens:
// a size below it, or a boundary (0 tokens: nothing has replied since). A transcript that can't be read at all,
// or one that wasn't measured before, can't say, and isn't waited on.
func compactLanded(after *v1.ContextUsage, before int64) bool {
	if !after.GetMeasured() || before <= 0 {
		return true
	}
	return after.Tokens == 0 || after.Tokens < before
}

// tokensOf is a measured size, 0 when unknown.
func tokensOf(u *v1.ContextUsage) int64 {
	if !u.GetMeasured() {
		return 0
	}
	return u.Tokens
}

// updateCompaction changes the agent's pending compaction when it is still the one named.
func (d *Daemon) updateCompaction(ctx context.Context, agentID, id string, f func(*v1.PendingCompaction)) error {
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if cur.PendingCompaction == nil || cur.PendingCompaction.Id != id {
			return nil
		}
		f(cur.PendingCompaction)
		return tx.PutAgent(cur)
	})
}

// endCompaction clears a pending compaction. An outcome (not empty) is recorded as an event with the sizes
// before and after (0: unknown); no outcome is the quiet case of a conversation that wasn't worth
// compacting.
func (d *Daemon) endCompaction(ctx context.Context, a *v1.Agent, pc *v1.PendingCompaction, outcome string, before, after int64) error {
	via := "live" // typed into its terminal, or asked of a live agent and never typed (a user was typing)
	if pc.TypedAt == nil && (pc.StartedAt != nil || a.TerminalId == "" || stopped(a)) {
		via = "headless" // a stopped conversation's
	}
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		if cur.PendingCompaction == nil || cur.PendingCompaction.Id != pc.Id {
			return nil
		}
		cur.PendingCompaction = nil
		if pc.Why == "reset" && outcome != "done" && cur.PendingReset != nil {
			// A compacting reset needs its compaction done: the reset fails with why, and types no bootstrap.
			cur.PendingReset.Reason = outcome
			if outcome == "" {
				cur.PendingReset.Reason = "the compaction was not done"
			}
		}
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		if outcome == "" {
			return nil
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id, Payload: &v1.Event_AgentCompacted{
			AgentCompacted: &v1.AgentCompacted{Why: pc.Why, Via: via, TokensBefore: before, TokensAfter: after, Outcome: outcome}}})
		return err
	})
}

// compactTarget is the model and effort a headless compaction runs on: the agent's, each replaced by what a pending
// reopen names (a reopen may change only one of them).
func compactTarget(a *v1.Agent) (model, effort string) {
	model, effort = a.Model, a.Effort
	if p := a.PendingReopen; p != nil {
		if p.Model != "" {
			model = p.Model
		}
		if p.Effort != "" {
			effort = p.Effort
		}
	}
	return model, effort
}

// compactHeadless compacts a stopped conversation before its reopen, off the agent's lock (it takes minutes): the
// agent's own CLI, `-p /compact <focus>` on its conversation, in its worktree, on the model it will resume on,
// with the agent's lean session but none of its identity or hooks.
func (d *Daemon) compactHeadless(ctx context.Context, a *v1.Agent, pc *v1.PendingCompaction) (bool, error) {
	usage := d.measureContext(ctx, a)
	if !wants(pc, usage) {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0)
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return false, d.endCompaction(ctx, a, pc, "", 0, 0)
	}
	model, effort := compactTarget(a)
	env := d.cliBaseEnv()
	lean, err := d.leanLaunch(ctx, a, env)
	if err != nil {
		return false, d.endCompaction(ctx, a, pc, "planning its lean session failed: "+err.Error(), usage.Tokens, 0)
	}
	req := &adapterv1.PlanCompactRequest{Binary: d.rt.cfg.Binaries[a.Kind], SessionId: a.SessionId, Cwd: a.Worktree,
		Env: env, Model: model, Effort: effort, Focus: pc.Focus, Lean: lean}
	if a.Kind == claude.Kind {
		if adapter.ReadOnly(a.Role) || a.DenyEdits {
			req.DenyTools = claude.DenyRules(a.Worktree, a.Repo)
		}
		if lean != nil {
			req.DenyTools = append(req.DenyTools, lean.DenyTools...)
		}
	}
	plan, err := ad.PlanCompact(ctx, connect.NewRequest(req))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return false, d.endCompaction(ctx, a, pc, "", 0, 0) // the adapter has no headless compaction
		}
		return false, d.endCompaction(ctx, a, pc, "planning the compaction failed: "+err.Error(), usage.Tokens, 0)
	}
	if len(plan.Msg.Argv) > 0 && !strings.Contains(plan.Msg.Argv[0], "/") {
		if path, ok := lookPath(plan.Msg.Argv[0], plan.Msg.Env["PATH"]); ok {
			plan.Msg.Argv[0] = path
		}
	}
	if !d.rt.ctxwork.begin(a.Id) {
		return true, nil
	}
	// The marker is stored before the CLI starts, so a restart of meshd does not run it again (compactStep).
	if err := d.updateCompaction(ctx, a.Id, pc.Id, func(p *v1.PendingCompaction) {
		p.StartedAt, p.TokensBefore = timestamppb.New(d.now()), usage.Tokens
	}); err != nil {
		d.rt.ctxwork.end(a.Id)
		return true, err
	}
	snapshot := a
	go func() {
		defer d.deliverSoon(snapshot.Id)
		defer d.rt.ctxwork.end(snapshot.Id)
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orDuration(d.rt.cfg.CompactTimeout, compactTimeout))
		defer cancel()
		run := d.rt.cfg.RunCLI
		if run == nil {
			run = runCLI
		}
		out, err := run(rctx, plan.Msg.Argv, plan.Msg.Env, snapshot.Worktree)
		outcome, after := "done", int64(0)
		if err != nil {
			outcome = "compaction failed, so it goes on as it was: " + lastChars(strings.TrimSpace(out+" "+err.Error()), 300)
		} else {
			after = tokensOf(d.measureContext(rctx, snapshot))
		}
		if err := d.endCompaction(context.WithoutCancel(ctx), snapshot, pc, outcome, usage.Tokens, after); err != nil {
			d.log.Warn("recording a compaction", "agent", snapshot.Id, "err", err)
		}
	}()
	return true, nil
}

func lastChars(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// runCLI runs a command to the end with exactly the environment given and returns its output.
func runCLI(ctx context.Context, argv []string, env map[string]string, cwd string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = envSlice(env)
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok && os.IsNotExist(err) {
			return string(out), fmt.Errorf("%s: %w", argv[0], err)
		}
	}
	return string(out), err
}

// contextEvery is how often agents with a compaction (or a reset) still to carry out are looked for: what holds
// one back (a user typing, a turn that has not ended, a timeout) raises no hook of its own.
const contextEvery = 5 * time.Second

// contextPass rings deliver for every agent with a pending compaction, so a timeout or a quiet prompt is noticed
// without waiting for a hook.
func (d *Daemon) contextPass(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	for _, a := range agents {
		if a.PendingCompaction != nil || a.PendingReset != nil {
			d.deliverSoon(a.Id)
		}
	}
	d.autoReset(ctx)
}
