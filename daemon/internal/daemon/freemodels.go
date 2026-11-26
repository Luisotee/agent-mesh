package daemon

import (
	"fmt"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Some of OpenCode's free models are served by providers that keep prompts or train on them (OpenCode's Zen docs),
// so a repo can be set to keep its code from them: the `repos.no_free_models` setting lists such repos' top
// folders. Every repo may use them until it is listed (the user's choice, 2026-11-25).
//
// Only the zero-cost models are held back: `opencode.FreeFamily` and the models `opencode models` lists at zero
// cost (the daemon's cache, warmOpenCodeModels). A paid OpenCode model is another provider policy and stays
// allowed. When the zero-cost list is not known, a named model can't be told from a free one and is refused,
// as is a tier chain with models in it, rather than guessed at.

// freeModelsOff returns the listed folder a repo is in (the folder as the setting names it), or "" when the repo
// may use free models. A repo is listed by its top folder, compared as the same directory (symlinks resolved).
func (d *Daemon) freeModelsOff(r store.Reader, top string) string {
	if top == "" {
		return ""
	}
	v, err := d.Setting(r, "repos.no_free_models")
	if err != nil {
		return ""
	}
	list, _ := v.([]any)
	for _, e := range list {
		s, _ := e.(string)
		if s == "" {
			continue
		}
		if sameDir(top, expandHome(s)) {
			return s
		}
	}
	return ""
}

// freeOffFor is freeModelsOff for an agent about to run on a model: only OpenCode has free models.
func (d *Daemon) freeOffFor(r store.Reader, kind, repo string) string {
	if kind != opencode.Kind {
		return ""
	}
	return d.freeModelsOff(r, repo)
}

// openCodeFreeSet is the zero-cost models of the daemon's cache, and whether the cache has them at all.
func (d *Daemon) openCodeFreeSet() (map[string]bool, bool) {
	d.rt.openCode.mu.Lock()
	defer d.rt.openCode.mu.Unlock()
	set := make(map[string]bool, len(d.rt.openCode.freeModels))
	for _, m := range d.rt.openCode.freeModels {
		set[m] = true
	}
	return set, len(set) > 0
}

// freeRefusal is the error for a model a repo set against free models would have run on.
func freeRefusal(repo, listed string) error {
	return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s): pick a paid OpenCode model, "+
		"another kind, or take the repo off the list", repo, listed)
}

// freeUnknown is the error when a repo set against free models names a model the daemon can't tell is paid.
func (d *Daemon) freeUnknown(repo, listed string) error {
	d.rt.openCode.mu.Lock()
	why := d.rt.openCode.lastWarmErr
	d.rt.openCode.mu.Unlock()
	if why == "" {
		why = "`opencode models` gave no list"
	}
	return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s), and the daemon can't tell which "+
		"opencode models are free (%s): try again once `opencode models` answers", repo, listed, why)
}

// checkNotFree refuses `model` (named, or `free` for the family) for a repo set against free models.
func (d *Daemon) checkNotFree(repo, listed, model string) error {
	if model == opencode.FreeFamily {
		return freeRefusal(repo, listed)
	}
	set, known := d.openCodeFreeSet()
	if !known {
		return d.freeUnknown(repo, listed)
	}
	if set[model] {
		return freeRefusal(repo, listed)
	}
	return nil
}

// dropFree removes the free models from an expanded OpenCode chain for a repo set against them. It errors when
// nothing paid is left, or the free models are not known (so a chain of named models can't be sorted).
func (d *Daemon) dropFree(repo, listed string, chain []string) ([]string, error) {
	set, known := d.openCodeFreeSet()
	if !known {
		if len(chain) == 0 {
			return nil, freeRefusal(repo, listed)
		}
		return nil, d.freeUnknown(repo, listed)
	}
	var kept []string
	for _, m := range chain {
		if !set[m] && m != opencode.FreeFamily {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return nil, freeRefusal(repo, listed)
	}
	return kept, nil
}

// pickFor is pick for an agent already in a repo (a reopen, a switch, an approve): the repo's setting applies.
func (d *Daemon) pickFor(r store.Reader, a *v1.Agent, tier, model, effort, what string) (string, string, error) {
	off := d.freeOffFor(r, a.Kind, a.Repo)
	return d.pickIn(r, a.Repo, off, a.Kind, tier, model, effort, what)
}

// freeModelsNote words why OpenCode is out of a reviewer round for a repo.
func freeModelsNote(repo string) string {
	return fmt.Sprintf("opencode is out: %s is set to keep its code from free models (repos.no_free_models)", repo)
}
