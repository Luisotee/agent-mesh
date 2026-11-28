package daemon

import (
	"reflect"
	"testing"
)

// A model OpenCode no longer lists is skipped by a tier chain once a listing was read (the free models rotate), and
// `free` still expands to the free ones; before any listing the chain is as written.
func TestAChainSkipsAnOpenCodeModelTheListingNoLongerNames(t *testing.T) {
	d := &Daemon{rt: &runtime{}}
	chain := []string{"opencode/deepseek-v4.1-flash-free", "opencode/tidepool-alpha-free", "opencode/beam-free", "free"}
	if got := d.expandChain("opencode", chain); !reflect.DeepEqual(got, []string{"opencode/deepseek-v4.1-flash-free", "opencode/tidepool-alpha-free", "opencode/beam-free"}) {
		t.Errorf("before any listing: %v", got)
	}
	d.rt.openCode.listed = map[string]bool{"opencode/deepseek-v4.1-flash-free": true, "opencode/beam-free": true, "opencode/hy4-flash-free": true}
	d.rt.openCode.freeModels = []string{"opencode/deepseek-v4.1-flash-free", "opencode/hy4-flash-free", "opencode/beam-free"}
	want := []string{"opencode/deepseek-v4.1-flash-free", "opencode/beam-free", "opencode/hy4-flash-free"}
	if got := d.expandChain("opencode", chain); !reflect.DeepEqual(got, want) {
		t.Errorf("with a listing: %v, want %v (tidepool left; free adds hy4 after the named ones)", got, want)
	}
	d.rt.openCode.listed = map[string]bool{} // a listing that came back empty names nothing: the chain stays as written
	if got := d.expandChain("opencode", []string{"opencode/beam-free"}); !reflect.DeepEqual(got, []string{"opencode/beam-free"}) {
		t.Errorf("after an empty listing: %v", got)
	}
	if got := d.expandChain("claude", []string{"opus", "gone"}); !reflect.DeepEqual(got, []string{"opus", "gone"}) {
		t.Errorf("another CLI's chain is left as written: %v", got)
	}
}
