package daemon

// Usage limits: an agent whose CLI reported one (a `RateLimited` event: agy's 429 `Stop`, Claude's `StopFailure`)
// sits at LIMITED, where doorbells wait because the turn they start would fail on the same quota. The daemon
// remembers which models are held (until when) and acts on it twice:
//
//   - A tier's chain is walked past held models: a new agent, an approve and a switch to a tier take the first
//     model with quota left (bin/mesh's plan_model), and `SwitchAgent next` the first free one after the agent's own.
//   - limitPass moves a LIMITED agent on its own (setting `limits.auto`): to the next free model of its chain when
//     the limit is its model's (`switch`), else, once the limit is over, it reopens the agent on the same model with
//     a note to carry on (`switch` and `continue`). Both are reopens of the agent's own conversation.
//
// The holds are in memory: a restart forgets them, and a pass that finds a LIMITED agent with no hold holds its
// model for `limits.hold` seconds, then continues it.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// holdAll is the model key of a hold on every model of a kind: an account-wide limit.
const holdAll = "*"

// holds are the models at a limit, by kind and model (or holdAll), until when.
type holds struct {
	mu    sync.Mutex
	until map[string]map[string]time.Time
}

func (h *holds) set(kind, model string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil {
		h.until = map[string]map[string]time.Time{}
	}
	if h.until[kind] == nil {
		h.until[kind] = map[string]time.Time{}
	}
	if until.After(h.until[kind][model]) {
		h.until[kind][model] = until
	}
}

// of is when a model is free again, counting a hold on every model of its kind; zero when it is free now.
func (h *holds) of(kind, model string, now time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	var latest time.Time
	for _, key := range []string{model, holdAll} {
		if t := h.until[kind][key]; t.After(now) && t.After(latest) {
			latest = t
		}
	}
	return latest
}

// holdTime is how long a limit that named no reset holds its model.
func (d *Daemon) holdTime(r store.Reader) time.Duration {
	v, err := d.Setting(r, "limits.hold")
	if err != nil {
		return 30 * time.Minute
	}
	return time.Duration(v.(float64) * float64(time.Second))
}

// noteLimit records the limit an agent's CLI reported: its model is held until the reset the CLI named, else for
// `limits.hold`; an account-wide limit holds every model of the kind.
func (d *Daemon) noteLimit(r store.Reader, a *v1.Agent, rl *adapterv1.RateLimited, at time.Time) {
	until := at.Add(d.holdTime(r))
	if rl.ResetAt != nil {
		until = rl.ResetAt.AsTime()
	}
	model := a.Model
	if rl.AllModels || model == "" {
		model = holdAll
	}
	d.rt.holds.set(a.Kind, model, until)
}

// firstFree is the first model of a chain with quota left; with every one held, the one freed first.
func (d *Daemon) firstFree(kind string, chain []string) string {
	now := d.now()
	best, bestAt := chain[0], time.Time{}
	for i, m := range chain {
		until := d.rt.holds.of(kind, m, now)
		if until.IsZero() {
			return m
		}
		if i == 0 || until.Before(bestAt) {
			best, bestAt = m, until
		}
	}
	return best
}

// nextFree is the first model after position i of a chain with quota left, or "".
func (d *Daemon) nextFree(kind string, chain []string, i int) string {
	now := d.now()
	for _, m := range chain[i+1:] {
		if d.rt.holds.of(kind, m, now).IsZero() {
			return m
		}
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
	for _, a := range agents {
		if a.State != v1.AgentState_AGENT_STATE_LIMITED || a.Role == v1.AgentRole_AGENT_ROLE_ROOT ||
			a.PendingReopen != nil || a.SessionId == "" {
			continue
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
	err := d.store.Write(ctx, meshdActor, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.State != v1.AgentState_AGENT_STATE_LIMITED || a.PendingReopen != nil || a.SessionId == "" {
			return nil
		}
		now := tx.Now()
		held := d.rt.holds.of(a.Kind, a.Model, now)
		if held.IsZero() && !d.rt.holds.known(a.Kind, a.Model) {
			// A limit from before a restart: its reset is unknown, so hold the model a while.
			d.rt.holds.set(a.Kind, a.Model, now.Add(d.holdTime(tx.Reader)))
			held = d.rt.holds.of(a.Kind, a.Model, now)
		}
		model, effort, deny := target(a)
		var body, why string
		switch {
		case auto == "switch" && !held.IsZero() && d.rt.holds.of(a.Kind, holdAll, now).IsZero():
			// The limit is the model's own: the next model of the chain that has quota left.
			next, err := d.nextOnChain(tx.Reader, a, model)
			if err != nil || next == "" {
				return err // no chain, or nothing free after it: wait for the reset
			}
			why = "quota"
			body = fmt.Sprintf("Your previous model (%s) hit its usage limit; you are now on %s, in the same "+
				"conversation. Continue exactly where you stopped.", model, next)
			model = next
		case held.IsZero():
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
		moved = true
		return tx.PutAgent(a)
	})
	if err != nil || !moved {
		return err
	}
	d.deliverSoon(agentID)
	return nil
}

// known reports whether a model has a hold on record, over or not.
func (h *holds) known(kind, model string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.until[kind][model]
	_, all := h.until[kind][holdAll]
	return ok || all
}

func limitDetail(a *v1.Agent) string {
	if s := strings.TrimSpace(a.StateDetail); s != "" {
		return s
	}
	return "usage limit"
}

// nextOnChain is the model after the agent's own on its phase tier's chain that has quota left, "" when there is
// none, and an error only for a store failure; an agent on a named model has no chain.
func (d *Daemon) nextOnChain(r store.Reader, a *v1.Agent, model string) (string, error) {
	tier := a.Tier
	if a.Phase == v1.AgentPhase_AGENT_PHASE_EXECUTING {
		tier = a.ExecTier
	}
	if tier == "" {
		return "", nil
	}
	chain, _, err := d.tierChain(r, a.Kind, tier)
	if err != nil {
		return "", nil // a tier the settings no longer have: nothing to walk
	}
	i := index(chain, model)
	if i < 0 {
		return "", nil
	}
	return d.nextFree(a.Kind, chain, i), nil
}
