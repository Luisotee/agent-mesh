package daemon

import (
	"slices"
	"testing"
)

// A plugin skill is hidden under both its spellings, and adding either brings both back.
func TestAddingEitherSpellingOfAPluginSkillUnhidesBoth(t *testing.T) {
	hidden := []string{"delegate", "agent-mesh-daemon:delegate", "other"}
	for _, added := range []string{"delegate", "agent-mesh-daemon:delegate"} {
		if got := filterAdded(hidden, []string{added}, true); !slices.Equal(got, []string{"other"}) {
			t.Errorf("adding %s leaves %v hidden, want [other]", added, got)
		}
	}
	if got := filterAdded([]string{"Edit", "agent-mesh-daemon:Edit"}, []string{"Edit"}, false); !slices.Equal(got, []string{"agent-mesh-daemon:Edit"}) {
		t.Errorf("a tool's spelling is exact: %v", got)
	}
}
