package daemon

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// offeredEfforts is what each listed model of the CLIs that list their efforts offers: agy by family (a level it has
// an id for), OpenCode by model (its variants). Claude takes every level and Copilot has no listing, so neither is here.
func (d *Daemon) offeredEfforts(ctx context.Context, fresh bool) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	if d == nil || d.rt == nil || d.rt.adapters == nil {
		return out
	}
	env := d.modelsEnv()
	hasCacheEnv := env["HOME"] != "" || env["XDG_CACHE_HOME"] != ""
	if ad, err := d.rt.adapters.Get(agy.Kind); err == nil {
		if ag, ok := ad.(*agy.Adapter); ok {
			switch {
			case fresh && d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil:
				if listing, err := d.rt.cfg.Usage.ReadAgyModels(ctx); err == nil && listing != "" {
					if m := agy.ListingEfforts(listing); len(m) > 0 {
						out[agy.Kind] = m
					}
				}
			case !fresh && (!testing.Testing() || hasCacheEnv) || fresh && (ag.ListModels != nil || d.hasModelListingBinary(agy.Kind)):
				if m, err := ag.Efforts(ctx, d.rt.cfg.Binaries[agy.Kind], env, fresh); err == nil && len(m) > 0 {
					out[agy.Kind] = m
				}
			}
		}
	}
	if ad, err := d.rt.adapters.Get(opencode.Kind); err == nil {
		if oc, ok := ad.(*opencode.Adapter); ok {
			if !fresh && (!testing.Testing() || hasCacheEnv) || fresh && (oc.ListModels != nil || d.hasModelListingBinary(opencode.Kind)) {
				if m, err := oc.Variants(ctx, d.rt.cfg.Binaries[opencode.Kind], env, fresh); err == nil && len(m) > 0 {
					out[opencode.Kind] = m
				}
			}
		}
	}
	return out
}

// effortProblems lists, per CLI, each effort a tier names (its own, its execution one, its review one) that a model
// of the tier's chain doesn't offer: agy has no id of that family at that level, so the launch fails; OpenCode has
// no variant of that name, so the model runs with its default instead of the effort asked. A model the listing
// doesn't name at all is the retired-model check's, not this one's; a model with no variants (OpenCode) takes
// no effort and is left alone.
func (d *Daemon) effortProblems(r store.Reader, offered map[string]map[string][]string) map[string][]string {
	out := map[string][]string{}
	for kind, byModel := range offered {
		var problems []string
		for _, key := range d.schema.Keys("tiers." + kind + ".") {
			base, ok := strings.CutSuffix(key, ".model")
			if !ok {
				continue
			}
			mv, err := d.Setting(r, key)
			if err != nil {
				continue
			}
			var models []string
			list, _ := mv.([]any)
			for _, m := range list {
				if name, ok := m.(string); ok {
					models = append(models, name)
				}
			}
			for _, leaf := range []string{"effort", "exec_effort", "review_effort"} {
				ev, err := d.Setting(r, base+"."+leaf)
				if err != nil {
					continue
				}
				effort, _ := ev.(string)
				var lacking []string
				for _, m := range models {
					id := m
					if kind == agy.Kind {
						if fam, ok := agy.ModelFamily(m); ok {
							id = fam
						}
					}
					have, listed := byModel[id]
					if !listed || len(have) == 0 || slices.Contains(have, effort) {
						continue
					}
					lacking = append(lacking, fmt.Sprintf("%s (has %s)", m, strings.Join(have, ", ")))
				}
				if len(lacking) > 0 {
					problems = append(problems, fmt.Sprintf("%s.%s = %s: %s", base, leaf, effort, strings.Join(lacking, ", ")))
				}
			}
		}
		sort.Strings(problems)
		if len(problems) > 0 {
			out[kind] = slices.Compact(problems)
		}
	}
	return out
}
