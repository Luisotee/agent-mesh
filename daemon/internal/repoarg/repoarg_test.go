package repoarg

import "testing"

func TestIsBareName(t *testing.T) {
	for arg, want := range map[string]bool{
		"agent-mesh": true, "api": true, "a.b": true,
		"": false, ".": false, "..": false, "./x": false, "a/b": false, "/abs": false, "~": false, "~/x": false,
		"-foo": false, "git@host:org/r": false, "https://h/r": false, "a:b": false, "u@h": false,
	} {
		if got := IsBareName(arg); got != want {
			t.Errorf("IsBareName(%q) = %v, want %v", arg, got, want)
		}
	}
}
