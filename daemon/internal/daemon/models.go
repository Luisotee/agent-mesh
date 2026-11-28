package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	modelCheckMetaKey = "model_check"
	modelCheckTTL     = 24 * time.Hour
)

type modelCheckState struct {
	At      int64               `json:"at"`
	Listed  map[string][]string `json:"listed,omitempty"`
	New     map[string][]string `json:"new,omitempty"`
	Retired map[string][]string `json:"retired,omitempty"`
	Acked   map[string][]string `json:"acked,omitempty"`
	// Offered is the efforts each listed model offers, by CLI (offeredEfforts); Efforts are the tier efforts a model of
	// the chain doesn't offer, worked out from it and the settings each time the state is read (withEfforts), so a
	// fixed setting stops being reported at once.
	Offered map[string]map[string][]string `json:"offered,omitempty"`
	Efforts map[string][]string            `json:"-"`
}

// mergeOffered is the efforts each CLI offers: this listing's for the CLIs it read, the last good one's for the
// others (a listing that failed keeps what the last one said).
func mergeOffered(prev, now map[string]map[string][]string) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for k, v := range prev {
		out[k] = v
	}
	for k, v := range now {
		out[k] = v
	}
	return out
}

func (d *Daemon) withEfforts(r store.Reader, s modelCheckState) modelCheckState {
	s.Efforts = d.effortProblems(r, s.Offered)
	return s
}

func loadModelCheckState(r store.Reader) modelCheckState {
	raw, err := r.Meta(modelCheckMetaKey)
	if err != nil || raw == "" {
		return modelCheckState{}
	}
	var s modelCheckState
	if json.Unmarshal([]byte(raw), &s) != nil {
		return modelCheckState{}
	}
	return s
}

func saveModelCheckState(tx *store.Tx, s modelCheckState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return tx.PutMeta(modelCheckMetaKey, string(b))
}

func (d *Daemon) tierFamilies(r store.Reader, kind string) map[string]bool {
	out := map[string]bool{}
	prefix := "tiers." + kind + "."
	for _, k := range d.schema.Keys(prefix) {
		if !strings.HasSuffix(k, ".model") {
			continue
		}
		v, err := d.Setting(r, k)
		if err != nil {
			continue
		}
		items, _ := v.([]any)
		for _, item := range items {
			m, _ := item.(string)
			if m == "" {
				continue
			}
			if kind == agy.Kind {
				if fam, ok := agy.ModelFamily(m); ok {
					out[fam] = true
					continue
				}
			}
			out[m] = true
		}
	}
	return out
}

// modelsEnv builds the environment passed to adapter listing/cache helpers. Under `testing.Testing()`, it
// never falls back to the real host's HOME/PATH/XDG_* so tests stay isolated unless AgentConfig.Env sets them.
func (d *Daemon) modelsEnv() map[string]string {
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

func (d *Daemon) hasModelListingBinary(kind string) bool {
	if d == nil || d.rt == nil {
		return false
	}
	if bin := d.rt.cfg.Binaries[kind]; bin != "" {
		st, err := os.Stat(bin)
		return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
	}
	if d.rt.cfg.LookPath != nil {
		_, ok := d.rt.cfg.LookPath(kind)
		return ok
	}
	if testing.Testing() {
		return false
	}
	_, ok := d.cliBinary(kind)
	return ok
}

// modelKindInstalled says whether a kind's CLI can be listed at all: its binary is found, or a listing hook stands in
// for it.
func (d *Daemon) modelKindInstalled(kind string) bool {
	if d.hasModelListingBinary(kind) {
		return true
	}
	if d == nil || d.rt == nil || d.rt.adapters == nil {
		return false
	}
	ad, err := d.rt.adapters.Get(kind)
	if err != nil {
		return false
	}
	switch a := ad.(type) {
	case *agy.Adapter:
		return a.ListModels != nil || (d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil)
	case *opencode.Adapter:
		return a.ListModels != nil
	}
	return false
}

func (d *Daemon) listedFamilies(ctx context.Context, fresh bool) map[string][]string {
	out := map[string][]string{}
	if d == nil || d.rt == nil || d.rt.adapters == nil {
		return out
	}
	env := d.modelsEnv()

	if ad, err := d.rt.adapters.Get(agy.Kind); err == nil {
		if ag, ok := ad.(*agy.Adapter); ok {
			hasOverride := ag.ListModels != nil || (d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil)
			hasCacheEnv := env["HOME"] != "" || env["XDG_CACHE_HOME"] != ""
			if d.hasModelListingBinary(agy.Kind) || hasOverride || (!fresh && (!testing.Testing() || hasCacheEnv)) {
				if fresh && d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil {
					if listing, err := d.rt.cfg.Usage.ReadAgyModels(ctx); err == nil && listing != "" {
						var fams []string
						seen := map[string]bool{}
						for _, id := range strings.Split(listing, "\n") {
							id, _, _ = strings.Cut(id, "\t")
							if fam, ok := agy.ModelFamily(strings.TrimSpace(id)); ok && !seen[fam] {
								seen[fam] = true
								fams = append(fams, fam)
							}
						}
						sort.Strings(fams)
						if len(fams) > 0 {
							out[agy.Kind] = fams
						}
					}
				} else if !fresh && !testing.Testing() || hasCacheEnv || (fresh && (ag.ListModels != nil || d.hasModelListingBinary(agy.Kind))) {
					if fams, err := ag.ListedFamilies(ctx, d.rt.cfg.Binaries[agy.Kind], env, fresh); err == nil && len(fams) > 0 {
						out[agy.Kind] = fams
					}
				}
			}
		}
	}

	if ad, err := d.rt.adapters.Get(opencode.Kind); err == nil {
		if oc, ok := ad.(*opencode.Adapter); ok {
			hasOverride := oc.ListModels != nil
			hasCacheEnv := env["HOME"] != "" || env["XDG_CACHE_HOME"] != ""
			if d.hasModelListingBinary(opencode.Kind) || hasOverride || (!fresh && (!testing.Testing() || hasCacheEnv)) {
				if !fresh && (!testing.Testing() || hasCacheEnv) || (fresh && (oc.ListModels != nil || d.hasModelListingBinary(opencode.Kind))) {
					if fams, err := oc.ListedFreeModels(ctx, d.rt.cfg.Binaries[opencode.Kind], env, fresh); err == nil && len(fams) > 0 {
						sort.Strings(fams)
						out[opencode.Kind] = fams
					}
				}
			}
		}
	}

	return out
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, x := range items {
		if x != "" {
			s[x] = true
		}
	}
	return s
}

func sortedKeys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) computeModelCheck(r store.Reader, listed, acked map[string][]string, prev modelCheckState, now time.Time) modelCheckState {
	prevListed := prev.Listed
	next := modelCheckState{
		At:      now.Unix(),
		Listed:  map[string][]string{},
		New:     map[string][]string{},
		Retired: map[string][]string{},
		Acked:   map[string][]string{},
	}
	for k, v := range acked {
		if len(v) > 0 {
			next.Acked[k] = sortedKeys(toSet(v))
		}
	}
	for kind, fams := range listed {
		famSet := toSet(fams)
		next.Listed[kind] = sortedKeys(famSet)
		ackSet := toSet(acked[kind])
		tierSet := d.tierFamilies(r, kind)

		known := map[string]bool{}
		for a := range ackSet {
			known[a] = true
		}
		var gone []string
		if kind == opencode.Kind {
			before := famSet
			if prev := prevListed[opencode.Kind]; len(prev) > 0 {
				before = toSet(prev)
			}
			for b := range before {
				known[b] = true
			}
			for tf := range tierSet {
				if strings.HasPrefix(tf, "opencode/") && !famSet[tf] {
					gone = append(gone, tf)
				}
			}
		} else {
			for tf := range tierSet {
				known[tf] = true
			}
			for tf := range tierSet {
				if !famSet[tf] {
					gone = append(gone, tf)
				}
			}
		}
		sort.Strings(gone)
		if len(gone) > 0 {
			next.Retired[kind] = gone
		}
		var fresh []string
		for f := range famSet {
			if !known[f] {
				fresh = append(fresh, f)
			}
		}
		sort.Strings(fresh)
		if len(fresh) > 0 {
			next.New[kind] = fresh
		}
	}
	// A kind that listed nothing this time but is still installed (a failed or timed-out listing) keeps what the
	// last good check found for it: its baseline (OpenCode diffs against it) and its notices, the notices minus
	// what was acked since and with the retired ones recomputed against the current tiers. The result isn't cached
	// for a day then, so the next call looks again rather than trusting a partial listing. A kind whose CLI is gone
	// is dropped: nothing to compare, and the cache applies again.
	carried := len(listed) == 0
	for kind, prevFams := range prev.Listed {
		if _, ok := listed[kind]; ok {
			continue
		}
		if !d.modelKindInstalled(kind) {
			continue
		}
		carried = true
		next.Listed[kind] = prevFams
		famSet := toSet(prevFams)
		ackSet := toSet(acked[kind])
		tierSet := d.tierFamilies(r, kind)
		var fresh, gone []string
		for _, f := range prev.New[kind] {
			if !ackSet[f] && !(kind != opencode.Kind && tierSet[f]) {
				fresh = append(fresh, f)
			}
		}
		for tf := range tierSet {
			if kind == opencode.Kind && !strings.HasPrefix(tf, "opencode/") {
				continue
			}
			if !famSet[tf] {
				gone = append(gone, tf)
			}
		}
		sort.Strings(gone)
		if len(fresh) > 0 {
			next.New[kind] = fresh
		}
		if len(gone) > 0 {
			next.Retired[kind] = gone
		}
	}
	if carried {
		next.At = 0
	}
	return next
}

// formatModelNotice builds the one-line root notice when new or retired model families exist, or "".
func formatModelNotice(s modelCheckState) string {
	kinds := map[string]bool{}
	for k, v := range s.New {
		if len(v) > 0 {
			kinds[k] = true
		}
	}
	for k, v := range s.Retired {
		if len(v) > 0 {
			kinds[k] = true
		}
	}
	for k, v := range s.Efforts {
		if len(v) > 0 && k == "agy" { // agy's launch fails; OpenCode's model runs on its default variant: listed by `mesh models`, not told to the root
			kinds[k] = true
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	ordered := sortedKeys(kinds)
	var parts []string
	for _, k := range ordered {
		if v := s.New[k]; len(v) > 0 {
			parts = append(parts, fmt.Sprintf("new %s models no tier names: %s", k, strings.Join(v, ", ")))
		}
	}
	for _, k := range ordered {
		if v := s.Retired[k]; len(v) > 0 {
			parts = append(parts, fmt.Sprintf("%s no longer lists: %s (a tier names it)", k, strings.Join(v, ", ")))
		}
	}
	for _, k := range ordered {
		if v := s.Efforts[k]; len(v) > 0 && k == "agy" {
			shown := v
			more := ""
			if len(v) > 3 {
				shown, more = v[:3], fmt.Sprintf("; … %d more (`mesh models --check`)", len(v)-3)
			}
			parts = append(parts, fmt.Sprintf("%s settings name an effort a model doesn't offer: %s%s", k, strings.Join(shown, "; "), more))
		}
	}
	hasModels := false
	for k := range kinds {
		hasModels = hasModels || len(s.New[k]) > 0 || len(s.Retired[k]) > 0
	}
	if !hasModels {
		return "mesh: " + strings.Join(parts, "; ") + ". Fix the settings (`mesh settings`) or the tier's models"
	}
	return "mesh: " + strings.Join(parts, "; ") + `. Propose a research ask on them to the user (ORCHESTRATE "New models"), or ` + "`mesh models --ack <kind>:<name>` once decided"
}

// ModelNotice returns the one-line notice for new/retired model families (reusing a result up to 24h old, or
// checking cached CLI model listings without spawning subprocesses). Exported for `StartRun` and `mesh brief`.
func (d *Daemon) ModelNotice(ctx context.Context) string {
	if d == nil || d.store == nil {
		return ""
	}
	saved := d.withEfforts(d.store.Read(ctx), loadModelCheckState(d.store.Read(ctx)))
	now := d.nowTime()
	if saved.At > 0 && now.Sub(time.Unix(saved.At, 0)) < modelCheckTTL {
		return formatModelNotice(saved)
	}
	listed := d.listedFamilies(ctx, false)
	if len(listed) == 0 {
		return formatModelNotice(saved)
	}
	offered := d.offeredEfforts(ctx, false)
	var out modelCheckState
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur := loadModelCheckState(tx.Reader)
		if cur.At > 0 && tx.Now().Sub(time.Unix(cur.At, 0)) < modelCheckTTL {
			out = cur
			return nil
		}
		out = d.computeModelCheck(tx.Reader, listed, cur.Acked, cur, tx.Now())
		out.Offered = mergeOffered(cur.Offered, offered)
		return saveModelCheckState(tx, out)
	})
	if err != nil {
		return formatModelNotice(saved)
	}
	return formatModelNotice(d.withEfforts(d.store.Read(ctx), out))
}

func (d *Daemon) buildCheckModelsResponse(r store.Reader, s modelCheckState) *v1.CheckModelsResponse {
	s = d.withEfforts(r, s)
	kindsSet := map[string]bool{}
	for k := range s.Listed {
		kindsSet[k] = true
	}
	for k := range s.New {
		kindsSet[k] = true
	}
	for k := range s.Retired {
		kindsSet[k] = true
	}
	for k := range s.Acked {
		kindsSet[k] = true
	}
	for k := range s.Efforts {
		kindsSet[k] = true
	}
	out := &v1.CheckModelsResponse{
		Notice: formatModelNotice(s),
	}
	if s.At > 0 {
		out.CheckedAt = timestamppb.New(time.Unix(s.At, 0))
	}
	for _, kind := range sortedKeys(kindsSet) {
		tierSet := d.tierFamilies(r, kind)
		km := &v1.KindModels{
			Kind:            kind,
			NewFamilies:     append([]string(nil), s.New[kind]...),
			RetiredFamilies: append([]string(nil), s.Retired[kind]...),
			AckedFamilies:   append([]string(nil), s.Acked[kind]...),
			EffortProblems:  append([]string(nil), s.Efforts[kind]...),
		}
		for _, fam := range s.Listed[kind] {
			km.Listed = append(km.Listed, &v1.ModelFamilyEntry{
				Family:  fam,
				InTiers: tierSet[fam],
			})
		}
		out.Kinds = append(out.Kinds, km)
	}
	return out
}

// CheckModels lists model families per CLI against configured tiers, flagging new or retired ones and recording
// `--ack <kind>:<family>` decisions.
func (d *Daemon) CheckModels(ctx context.Context, req *connect.Request[v1.CheckModelsRequest]) (*connect.Response[v1.CheckModelsResponse], error) {
	m := req.Msg
	// Validate --ack entries before touching anything.
	type ackItem struct{ kind, name string }
	var acks []ackItem
	for _, item := range m.Ack {
		kind, name, ok := strings.Cut(item, ":")
		if !ok || kind == "" || name == "" {
			return nil, invalid("--ack takes <kind>:<name>, like agy:neon (not %q)", item)
		}
		if _, err := d.rt.adapters.Get(kind); err != nil {
			return nil, invalid("--ack takes a known kind:<name>, like agy:neon (not %q)", item)
		}
		acks = append(acks, ackItem{kind: kind, name: name})
	}

	r := d.store.Read(ctx)
	saved := loadModelCheckState(r)
	force := m.Check || len(acks) > 0
	needCompute := force || saved.At == 0 || d.nowTime().Sub(time.Unix(saved.At, 0)) >= modelCheckTTL

	var listed map[string][]string
	var offered map[string]map[string][]string
	if needCompute {
		// Shell out / read caches outside SQLite's writer lock.
		listed = d.listedFamilies(ctx, force)
		offered = d.offeredEfforts(ctx, force)
	}

	out := &v1.CheckModelsResponse{}
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceCheckModelsProcedure, out, func(tx *store.Tx) error {
		cur := loadModelCheckState(tx.Reader)
		if !force && cur.At > 0 && tx.Now().Sub(time.Unix(cur.At, 0)) < modelCheckTTL {
			*out = *d.buildCheckModelsResponse(tx.Reader, cur)
			return nil
		}
		acked := map[string][]string{}
		for k, v := range cur.Acked {
			acked[k] = append([]string(nil), v...)
		}
		for _, a := range acks {
			set := toSet(acked[a.kind])
			set[a.name] = true
			acked[a.kind] = sortedKeys(set)
		}
		next := d.computeModelCheck(tx.Reader, listed, acked, cur, tx.Now())
		next.Offered = mergeOffered(cur.Offered, offered)
		if err := saveModelCheckState(tx, next); err != nil {
			return err
		}
		*out = *d.buildCheckModelsResponse(tx.Reader, next)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}
	return connect.NewResponse(out), err
}
