package daemon

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Some of OpenCode's free models are served by providers that keep prompts or train on them (OpenCode's Zen docs),
// so a repo can be set to keep its code from them: the `repos.no_free_models` setting lists such repos' top
// folders. Every repo may use them until it is listed (the user's choice, 2026-11-25).
//
// Only the zero-cost models are held back, and "zero-cost" is what OpenCode itself reports: the `cost` fields of
// `opencode models --verbose` (the daemon's cache, warmOpenCodeModels). A model counts when both its input and output
// price are listed and zero, whatever its provider, status or tool support (this is wider than the `free` family,
// which is only the active, tool-calling `opencode/*` ones); the family itself is held back too. A paid OpenCode model
// is another provider policy and stays allowed. A model whose cost is not listed (or a listing the daemon could not
// read) can't be told from a free one, so it is refused, as is a tier chain with only such models, rather than guessed at.

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

// openCodeCosts is each listed model's cost class (opencode.CostClasses), and whether a listing was read at all.
func (d *Daemon) openCodeCosts() (map[string]string, bool) {
	d.rt.openCode.mu.Lock()
	defer d.rt.openCode.mu.Unlock()
	return d.rt.openCode.costs, d.rt.openCode.costs != nil
}

// freeRefusal is the error for a model a repo set against free models would have run on.
func freeRefusal(repo, listed string) error {
	return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s): pick a paid OpenCode model, "+
		"another kind, or take the repo off the list", repo, listed)
}

// freeRootRefusal is the error for an OpenCode root in a repo set against free models that names no tier or model: it
// would run on the CLI's default model, which may be a free one.
func freeRootRefusal(repo, listed string) error {
	return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s), and a root that names no tier or "+
		"model runs on OpenCode's default model, which may be a free one: name a paid model, or take the repo off the list", repo, listed)
}

// freeUnknown is the error when a repo set against free models names a model the daemon can't tell is paid: no
// listing was read, or the listing gives no cost for it (model is "" for the first).
func (d *Daemon) freeUnknown(repo, listed, model string) error {
	if model != "" {
		return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s), and `opencode models` gives no cost "+
			"for %s, so the daemon can't tell it is paid: pick a model with a listed price", repo, listed, model)
	}
	d.rt.openCode.mu.Lock()
	why := d.rt.openCode.lastWarmErr
	d.rt.openCode.mu.Unlock()
	if why == "" {
		why = "`opencode models` gave no list"
	}
	return invalid("%s is set to keep its code from free models (`repos.no_free_models` lists %s), and the daemon can't tell which "+
		"opencode models are free (%s): try again once `opencode models` answers", repo, listed, why)
}

// checkNotFree refuses `model` (named, or `free` for the family) for a repo set against free models: only a model
// OpenCode lists with a non-zero price passes.
func (d *Daemon) checkNotFree(repo, listed, model string) error {
	if model == opencode.FreeFamily {
		return freeRefusal(repo, listed)
	}
	costs, known := d.openCodeCosts()
	if !known {
		return d.freeUnknown(repo, listed, "")
	}
	switch costs[model] {
	case opencode.CostPaid:
		return nil
	case opencode.CostZero:
		return freeRefusal(repo, listed)
	}
	return d.freeUnknown(repo, listed, model)
}

// dropFree removes the models that are not listed as paid from an expanded OpenCode chain for a repo set against
// free models. It errors when nothing is left, or no listing was read (so a chain of named models can't be sorted).
func (d *Daemon) dropFree(repo, listed string, chain []string) ([]string, error) {
	costs, known := d.openCodeCosts()
	if !known {
		if len(chain) == 0 {
			return nil, freeRefusal(repo, listed)
		}
		return nil, d.freeUnknown(repo, listed, "")
	}
	var kept []string
	unknown := ""
	for _, m := range chain {
		switch {
		case costs[m] == opencode.CostPaid:
			kept = append(kept, m)
		case costs[m] == "" && m != opencode.FreeFamily && unknown == "":
			unknown = m
		}
	}
	if len(kept) == 0 {
		if unknown != "" {
			return nil, d.freeUnknown(repo, listed, unknown)
		}
		return nil, freeRefusal(repo, listed)
	}
	return kept, nil
}

// checkKept refuses an OpenCode agent in a repo set against free models going on to run on `model` without a new
// pick: a resume, a task sent to a stopped agent, an approve that keeps the model, a reopen after a limit's reset.
// (An agent that is running keeps its model, as the setting says.) It is a FailedPrecondition, which a send to a
// stopped agent reports as the reason it could not be resumed. An agent on the CLI's default model ("") can't be
// told from one on a free model, so it is refused too.
func (d *Daemon) checkKept(r store.Reader, a *v1.Agent, model string) error {
	off := d.freeOffFor(r, a.Kind, a.Repo)
	if off == "" {
		return nil
	}
	var err error
	if model == "" {
		err = freeRootRefusal(a.Repo, off)
	} else {
		err = d.checkNotFree(a.Repo, off, model)
	}
	if err == nil {
		return nil
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return failedPrecondition("%s runs on %s: %s", a.Name, orDefaultModel(model), ce.Message())
	}
	return err
}

func orDefaultModel(m string) string {
	if m == "" {
		return "the CLI's default model"
	}
	return m
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
