package daemon

import (
	"reflect"
	"testing"
)

func TestAuthorLast(t *testing.T) {
	small := []string{"opencode", "agy", "claude"}
	large := []string{"agy", "claude", "opencode"}
	cases := []struct {
		name   string
		chain  []string
		author string
		want   []string
	}{
		{"small, claude author", small, "claude", []string{"opencode", "agy", "claude"}},
		{"small, agy author", small, "agy", []string{"opencode", "claude", "agy"}},
		{"small, opencode author goes after the others", small, "opencode", []string{"agy", "claude", "opencode"}},
		{"small, author not in the slot", small, "copilot", small},
		{"large, claude author", large, "claude", []string{"agy", "claude", "opencode"}},
		{"large, agy author", large, "agy", []string{"claude", "agy", "opencode"}},
		{"large, opencode author", large, "opencode", []string{"agy", "claude", "opencode"}},
		{"large, author not in the slot", large, "copilot", large},
		{"no author known", large, "", large},
		{"only OpenCode besides the author", []string{"opencode", "agy"}, "agy", []string{"opencode", "agy"}},
		{"no opencode in the slot", []string{"claude", "agy"}, "claude", []string{"agy", "claude"}},
	}
	for _, c := range cases {
		in := append([]string(nil), c.chain...)
		if got := authorLast(in, c.author); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if !reflect.DeepEqual(in, c.chain) {
			t.Errorf("%s: the input chain was changed to %v", c.name, in)
		}
	}
}
