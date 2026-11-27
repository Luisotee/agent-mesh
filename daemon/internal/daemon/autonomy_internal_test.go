package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInRepoListTakesAPathAHomePathAndABareName(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	repo := filepath.Join(home, "projects", "api")
	for _, c := range []struct {
		name string
		list []string
		want bool
	}{
		{"the absolute path", []string{repo}, true},
		{"a trailing slash", []string{repo + "/"}, true},
		{"a path with ~", []string{"~/projects/api"}, true},
		{"the folder's bare name", []string{"api"}, true},
		{"another repo", []string{"~/projects/web", "web"}, false},
		{"a blank entry", []string{" ", ""}, false},
		{"no list", nil, false},
	} {
		if got := inRepoList(c.list, repo); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if inRepoList([]string{"api"}, "") {
		t.Error("an empty repo is in no list")
	}
}
