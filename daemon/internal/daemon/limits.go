package daemon

// Usage limits: an agent whose CLI reported one (a `RateLimited` event: agy's 429 `Stop`, Claude's `StopFailure`)
// sits at LIMITED, where doorbells wait because the turn they start would fail on the same quota. The daemon
// persists which models are held (until when) in SQLite (`model_limits`) so a restart never forgets them, and
// acts on it twice:
//
//   - A tier's chain is walked past held models (and agy models at the reserve): a new agent, an approve and a
//     switch to a tier take the first model with quota left (bin/mesh's plan_model), and `SwitchAgent next` the
//     first free one after the agent's own.
//   - limitPass moves a LIMITED agent on its own (setting `limits.auto`): to the next free model of its chain when
//     the limit is its model's (`switch`), else, once the limit is over, it reopens the agent on the same model with
//     a note to carry on (`switch` and `continue`). Both are reopens of the agent's own conversation.

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// holdAll is the model key of a hold on every model of a kind: an account-wide limit.
const holdAll = "*"

// holdTime is how long a limit that named no reset holds its model.
func (d *Daemon) holdTime(r store.Reader) time.Duration {
	v, err := d.Setting(r, "limits.hold")
	if err != nil {
		return 30 * time.Minute
	}
	return time.Duration(v.(float64) * float64(time.Second))
}

// limitUntil is when a reported limit ends: the reset time it named (Claude), else its relative reset (agy's
// "Resets in 1h2m3s"), else `hold` after it was reported.
func limitUntil(rl *adapterv1.RateLimited, at time.Time, hold time.Duration) time.Time {
	if rl.ResetAt != nil {
		return rl.ResetAt.AsTime()
	}
	if rl.ResetIn != "" {
		if d, err := time.ParseDuration(rl.ResetIn); err == nil && d > 0 {
			return at.Add(d)
		}
	}
	return at.Add(hold)
}

// noteLimit records the limit an agent's CLI reported: its model is held until the reset the CLI named, else for
// `limits.hold`; an account-wide limit holds every model of the kind.
func (d *Daemon) noteLimit(tx *store.Tx, a *v1.Agent, rl *adapterv1.RateLimited, at time.Time) error {
	until := limitUntil(rl, at, d.holdTime(tx.Reader))
	model := a.Model
	if rl.AllModels || model == "" {
		model = holdAll
	}
	return tx.PutModelLimit(a.Kind, model, until)
}

// firstFree is the first model of a chain with quota left (not held in SQLite and not under agy's reserve); with
// every one held or spent, the one freed first (or with the most quota left). Returns "" when no launchable
// model is in `chain` (never the literal unexpanded `free` family).
func (d *Daemon) firstFree(r store.Reader, kind, effort string, chain []string) string {
	now := d.nowTime()
	var best string
	var bestAt time.Time
	var agySpent *struct {
		left  int
		model string
	}
	stop := d.quotaStop(r)
	for _, m := range chain {
		if m == "" || (kind == opencode.Kind && m == opencode.FreeFamily) {
			continue
		}
		until, _, err := r.ModelHold(kind, m, now)
		if err != nil {
			continue
		}
		if until.IsZero() {
			if kind == "agy" && stop < 100 {
				if hit := d.agyModelAtReserveCached(m, effort, stop); hit != nil {
					if agySpent == nil || hit.left > agySpent.left {
						agySpent = &struct {
							left  int
							model string
						}{hit.left, m}
					}
					continue
				}
			}
			return m
		}
		if best == "" || until.Before(bestAt) {
			best, bestAt = m, until
		}
	}
	if agySpent != nil {
		return agySpent.model
	}
	return best
}

// soonestHold returns the earliest active hold expiry among models in `chain`, or zero if none are held.
func (d *Daemon) soonestHold(r store.Reader, kind string, chain []string) time.Time {
	now := d.now()
	var soonest time.Time
	for _, m := range chain {
		if until, _, err := r.ModelHold(kind, m, now); err == nil && !until.IsZero() {
			if soonest.IsZero() || until.Before(soonest) {
				soonest = until
			}
		}
	}
	return soonest
}

// nextFree is the first model after position i of a chain with quota left (not held and not under agy's reserve), or "".
func (d *Daemon) nextFree(r store.Reader, kind, effort string, chain []string, i int) string {
	now := d.now()
	stop := d.quotaStop(r)
	for _, m := range chain[i+1:] {
		if m == "" || (kind == opencode.Kind && m == opencode.FreeFamily) {
			continue
		}
		until, _, err := r.ModelHold(kind, m, now)
		if err != nil || !until.IsZero() {
			continue
		}
		if kind == "agy" && stop < 100 && d.agyModelAtReserveCached(m, effort, stop) != nil {
			continue
		}
		return m
	}
	return ""
}

// limitsAuto is the `limits.auto` policy: "off", "continue" or "switch"; anything else counts as off.
func (d *Daemon) limitsAuto(r store.Reader) string {
	v, err := d.Setting(r, "limits.auto")
	if err != nil {
		return "off"
	}
	switch s, _ := v.(string); s {
	case "continue", "switch":
		return s
	}
	return "off"
}

// limitPass moves the agents at a usage limit on, as `limits.auto` says. It looks at each non-root LIMITED agent
// with no reopen pending; the root's model is the user's, so the root or the user moves a root.
func (d *Daemon) limitPass(ctx context.Context) {
	r := d.store.Read(ctx)
	auto := d.limitsAuto(r)
	if auto == "off" {
		return
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return
	}
	warmedOpenCode := false
	for _, a := range agents {
		if a.State != v1.AgentState_AGENT_STATE_LIMITED || a.Role == v1.AgentRole_AGENT_ROLE_ROOT ||
			a.PendingReopen != nil || a.SessionId == "" {
			continue
		}
		if a.Kind == opencode.Kind && !warmedOpenCode {
			d.warmOpenCodeModels(ctx, opencode.Kind)
			warmedOpenCode = true
		}
		if err := d.settleLimit(ctx, a.Id, auto); err != nil && ctx.Err() == nil {
			d.log.Warn("limit pass", "agent", a.Name, "err", err)
		}
	}
}

// meshdActor is who the daemon's own moves are made by.
var meshdActor = &v1.Actor{Id: "meshd", Name: "meshd"}

// settleLimit moves one LIMITED agent on, re-checking under its lock and inside the transaction that it is still
// at the limit with nothing pending.
func (d *Daemon) settleLimit(ctx context.Context, agentID, auto string) error {
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	moved := false
	var told *v1.Message
	err := d.store.Write(ctx, meshdActor, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.State != v1.AgentState_AGENT_STATE_LIMITED || a.PendingReopen != nil || a.SessionId == "" {
			return nil
		}
		now := tx.Now()
		held, known, err := tx.ModelHold(a.Kind, a.Model, now)
		if err != nil {
			return err
		}
		if held.IsZero() && !known {
			// A limit with no recorded hold (e.g. a legacy DB before migration): hold it briefly.
			if err := tx.PutModelLimit(a.Kind, a.Model, now.Add(d.holdTime(tx.Reader))); err != nil {
				return err
			}
			held, _, err = tx.ModelHold(a.Kind, a.Model, now)
			if err != nil {
				return err
			}
		}
		allHeld, _, err := tx.ModelHold(a.Kind, holdAll, now)
		if err != nil {
			return err
		}
		model, effort, deny := target(a)
		var body, why string
		switch {
		case auto == "switch" && !held.IsZero() && allHeld.IsZero():
			// The limit is the model's own: the next model of the chain that has quota left.
			next, err := d.nextOnChain(tx.Reader, a, model, effort)
			if err != nil || next == "" {
				return err // no chain, or nothing free after it: wait for the reset
			}
			why = "quota"
			body = fmt.Sprintf("Your previous model (%s) hit its usage limit; you are now on %s, in the same "+
				"conversation. Continue exactly where you stopped.", model, next)
			model = next
		case held.IsZero():
			// Back on the model it had: not when that is a free one and its repo has since been set against them
			// (freemodels.go). It stays stopped at its limit, for its parent or the user to switch or resume.
			if d.checkKept(tx.Reader, a, model) != nil {
				return nil
			}
			why = "limit reset"
			body = fmt.Sprintf("Your usage limit has reset; you are back on %s, in the same conversation. "+
				"Continue exactly where you stopped.", model)
		default:
			return nil // the limit isn't over and no other model is free
		}
		if _, err := storeMessage(tx, a, v1.MessageType_MESSAGE_TYPE_INFO, body); err != nil {
			return err
		}
		if _, err := requestReopen(tx, a, model, effort, deny, why, "automatic: "+limitDetail(a)); err != nil {
			return err
		}
		// Its parent was told it stopped at the limit; now it is told what meshd did about it.
		note := fmt.Sprintf("%s's usage limit on %s was dealt with: it is reopened on %s (%s), in the same conversation, and carries on.",
			a.Name, a.Model, model, why)
		if told, err = d.noteParentTx(tx, a, "moved", note); err != nil {
			return err
		}
		moved = true
		return tx.PutAgent(a)
	})
	if err != nil || !moved {
		return err
	}
	if told != nil && d.notify != nil {
		d.notify(told.ToId)
	}
	d.deliverSoon(agentID)
	return nil
}

func limitDetail(a *v1.Agent) string {
	if s := strings.TrimSpace(a.StateDetail); s != "" {
		return s
	}
	return "usage limit"
}

// nextOnChain is the model after the agent's own on its phase tier's chain that has quota left, "" when there is
// none, and an error only for a store failure; an agent on a named model has no chain.
func (d *Daemon) nextOnChain(r store.Reader, a *v1.Agent, model, effort string) (string, error) {
	tier := a.Tier
	if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING {
		tier = a.ExecTier
	}
	if tier == "" {
		return "", nil
	}
	chain, tierEffort, err := d.tierChain(r, a.Kind, tier)
	if err != nil {
		return "", nil // a tier the settings no longer have: nothing to walk
	}
	chain = d.expandChain(a.Kind, chain)
	// A repo set against free models (freemodels.go) never moves on to one: the next paid model, or none.
	if off := d.freeOffFor(r, a.Kind, a.Repo); off != "" {
		kept, err := d.dropFree(a.Repo, off, chain)
		if err != nil {
			return "", nil
		}
		chain = kept
	}
	if len(chain) == 0 {
		return "", nil
	}
	if effort == "" {
		effort = tierEffort
	}
	i := index(chain, model)
	if i < 0 {
		if a.Kind == opencode.Kind && len(chain) > 0 {
			return d.nextFree(r, a.Kind, effort, chain, -1), nil
		}
		return "", nil
	}
	return d.nextFree(r, a.Kind, effort, chain, i), nil
}
