package daemon

import "github.com/Luisotee/agent-mesh/daemon/internal/store"

// reviewEffort is the effort a fresh reviewer runs at when its caller names none: a relay step's own
// (`review.effort.relay`, the same for every CLI: a risky PR's reviewers go deep), else its CLI's and tier's
// (`tiers.<kind>.<tier>.review_effort`: Opus at medium, Sonnet at high, and so on). A tier the CLI doesn't have
// gives "", which the spawn then refuses with the tier error.
func (d *Daemon) reviewEffort(r store.Reader, kind, tier string, relay bool) (string, error) {
	if relay {
		return d.settingString(r, "review.effort.relay")
	}
	key := "tiers." + kind + "." + tier + ".review_effort"
	if _, ok := d.schema.Lookup(key); !ok {
		return "", nil
	}
	return d.settingString(r, key)
}

// execEffort is the effort a planned task executes at when `mesh approve` picks the tier without one
// (`tiers.<kind>.<tier>.exec_effort`): "" when the tier has none, which leaves the tier's own effort.
func (d *Daemon) execEffort(r store.Reader, kind, tier string) (string, error) {
	key := "tiers." + kind + "." + tier + ".exec_effort"
	if _, ok := d.schema.Lookup(key); !ok {
		return "", nil
	}
	return d.settingString(r, key)
}
