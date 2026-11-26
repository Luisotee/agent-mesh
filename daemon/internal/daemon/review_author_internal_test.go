package daemon

import (
	"reflect"
	"testing"
)

func TestAuthorLast(t *testing.T) {
	def := []string{"agy", "claude", "opencode"}
	cases := []struct {
		name   string
		chain  []string
		author string
		want   []string
	}{
		{"claude author: agy first, claude before opencode", def, "claude", []string{"agy", "claude", "opencode"}},
		{"agy author: claude first, agy before opencode", def, "agy", []string{"claude", "agy", "opencode"}},
		{"opencode author goes last", def, "opencode", []string{"agy", "claude", "opencode"}},
		{"author not in the slot", def, "copilot", def},
		{"author listed after opencode stays last", []string{"opencode", "agy"}, "agy", []string{"opencode", "agy"}},
		{"author listed first in a longer chain", []string{"claude", "agy", "opencode"}, "claude", []string{"agy", "claude", "opencode"}},
		{"no opencode in the slot", []string{"claude", "agy"}, "claude", []string{"agy", "claude"}},
	}
	for _, c := range cases {
		in := append([]string(nil), c.chain...)
		if got := authorLast(in, c.author); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
