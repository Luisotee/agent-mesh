package daemon

import (
	"context"
	"strings"

	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The asker role (`roles.asker.*`): what a fresh asker runs on when its ask names no tier and no model. A plain
// `mesh ask --repo` and each asker of `mesh ask --peers` take it, so a question costs free models first and Claude
// only when they are out. Naming a tier or a model keeps the tier path (`pickIn`).

// askerChoice is the first CLI of `roles.asker.kinds` (or the one named) that is installed, allowed for the repo
// and has a model of its list with quota left, at the role's effort (an effort the ask names wins). A CLI that can't
// take it is skipped and the reason kept, so the error says why none could.
func (d *Daemon) askerChoice(ctx context.Context, r store.Reader, runID, repo, only, effort, what string) (kind, model, eff string, err error) {
	kinds := []string{only}
	if only == "" {
		if kinds, err = d.settingStrings(r, "roles.asker.kinds"); err != nil {
			return "", "", "", err
		}
	}
	if effort != "" && !validEffort(effort) {
		return "", "", "", invalid("effort %q: one of %s", effort, strings.Join(settings.Efforts, ", "))
	}
	var why []string
	for _, k := range kinds {
		if _, aerr := d.rt.adapters.Get(k); aerr != nil || !d.installed(k) {
			why = append(why, k+": not installed")
			continue
		}
		d.warmOpenCodeModels(ctx, k)
		freeOff := d.freeOffFor(r, k, repo)
		if ok, note := d.kindAvailable(r, k, "", repo, freeOff); !ok {
			if note == "" {
				note = "not available"
			}
			why = append(why, k+": "+note)
			continue
		}
		models, serr := d.settingStrings(r, "roles.asker."+k+".model")
		if serr != nil {
			why = append(why, k+": "+serr.Error())
			continue
		}
		models = d.expandChain(k, models)
		if freeOff != "" && k == opencode.Kind {
			if models, serr = d.dropFree(repo, freeOff, models); serr != nil {
				why = append(why, k+": "+lineOf(serr.Error()))
				continue
			}
		}
		e := effort
		if e == "" {
			if e, serr = d.settingString(r, "roles.asker."+k+".effort"); serr != nil {
				why = append(why, k+": "+serr.Error())
				continue
			}
		}
		m := d.firstWithQuota(r, k, e, models)
		if m == "" {
			why = append(why, k+": every model of its asker list is at its limit")
			continue
		}
		if rerr := d.reserveCheck(r, runID, k, m, e, what, false); rerr != nil {
			why = append(why, k+": "+lineOf(rerr.Error()))
			continue
		}
		return k, m, e, nil
	}
	if only != "" {
		return "", "", "", invalid("no %s asker can start now (%s): `mesh settings --prefix roles.asker.%s.` has its models; --tier or --model names others", only, strings.Join(why, "; "), only)
	}
	return "", "", "", invalid("no CLI of `roles.asker.kinds` can take an asker now (%s): name --tier, --kind or --model to choose another", strings.Join(why, "; "))
}

// firstWithQuota is the first model of the chain with no hold on it and, for agy, under its quota reserve: unlike
// firstFree it never returns a model that is held or spent.
func (d *Daemon) firstWithQuota(r store.Reader, kind, effort string, chain []string) string {
	now := d.now()
	lim := d.reserveLimits(r, kind)
	for _, m := range chain {
		if m == "" || (kind == opencode.Kind && m == opencode.FreeFamily) {
			continue
		}
		if until, _, err := r.ModelHold(kind, m, now); err != nil || !until.IsZero() {
			continue
		}
		if kind == "agy" && !lim.off() && d.agyModelAtReserveCached(m, effort, lim, false) != nil {
			continue
		}
		return m
	}
	return ""
}

// askerFor is what a fresh asker of an ask runs on: the asker role when the ask names no tier and no model (its
// kind, when named, narrows the role to that CLI), else the tier path of `pick` on the kind named (Claude by
// default).
func (d *Daemon) askerFor(ctx context.Context, r store.Reader, runID, repo, kind, tier, model, effort, what string) (k, m, e string, err error) {
	if tier == "" && model == "" {
		return d.askerChoice(ctx, r, runID, repo, kind, effort, what)
	}
	k = orClaude(kind)
	d.warmOpenCodeModels(ctx, k)
	m, e, err = d.pickIn(r, repo, d.freeOffFor(r, k, repo), k, tier, model, effort, what, false)
	return k, m, e, err
}
