package daemon

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// ResolveModel (`mesh model`, bin/mesh's `mesh model`) says what a spawn of a kind and tier would run now. It
// asks `pick`, the function a spawn calls, so the two cannot disagree; the chain lists each model's standing so
// the ones a spawn would skip are visible.
func (d *Daemon) ResolveModel(ctx context.Context, req *connect.Request[v1.ResolveModelRequest]) (*connect.Response[v1.ResolveModelResponse], error) {
	m := req.Msg
	kind := m.Kind
	if kind == "" {
		kind = "claude"
	}
	out := &v1.ResolveModelResponse{Kind: kind, Tier: m.Tier}
	d.warmForPick(ctx, kind, m)
	err := d.store.Snapshot(ctx, func(r store.Reader) error {
		model, effort, err := d.pick(r, kind, m.Tier, m.Model, m.Effort, "the model a spawn would use")
		if err != nil {
			return err
		}
		out.Model, out.Effort = model, effort
		if m.Model != "" {
			return nil
		}
		chain, _, err := d.tierChain(r, kind, m.Tier)
		if err != nil {
			return err
		}
		stop := d.quotaStop(r)
		now := d.now()
		for _, c := range d.expandChain(kind, chain) {
			if c == "" || (kind == opencode.Kind && c == opencode.FreeFamily) {
				continue
			}
			out.Chain = append(out.Chain, &v1.ChainModel{Model: c, Standing: d.standing(r, kind, c, effort, stop, now)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// warmForPick does what SpawnAgent does before it picks (`warmUsage`, `warmOpenCodeModels`), outside the snapshot,
// so the answer is the spawn's: agy's usage and display names are read (a model at the reserve is skipped) and
// OpenCode's `free` family is expanded. warmUsage resolves each agy tier's chain at the tier's own effort, so
// when another effort is asked for the tier's chain is warmed at that one too.
func (d *Daemon) warmForPick(ctx context.Context, kind string, m *v1.ResolveModelRequest) {
	r := d.store.Read(ctx)
	extra := []agyResolveKey{{model: m.Model, effort: m.Effort}}
	if m.Tier != "" && m.Effort != "" {
		if chain, _, err := d.tierChain(r, kind, m.Tier); err == nil {
			for _, c := range chain {
				extra = append(extra, agyResolveKey{model: c, effort: m.Effort})
			}
		}
	}
	d.warmUsage(ctx, r, kind, extra...)
	d.warmOpenCodeModels(ctx, kind)
}

// standing is why a spawn would skip a model of a chain: held at a limit, or (agy) at the quota reserve; empty
// when it has quota.
func (d *Daemon) standing(r store.Reader, kind, model, effort string, stop float64, now time.Time) string {
	if until, _, err := r.ModelHold(kind, model, now); err == nil && !until.IsZero() {
		return "held until " + until.Local().Format("15:04")
	}
	if kind == "agy" && stop < 100 {
		if hit := d.agyModelAtReserveCached(model, effort, stop); hit != nil {
			return fmt.Sprintf("at the reserve (%d%% left)", hit.left)
		}
	}
	return ""
}
