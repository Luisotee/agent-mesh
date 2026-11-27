package notice

import (
	"strings"
	"testing"
	"time"
)

func TestBriefIsPlainTextCutToANotificationsSize(t *testing.T) {
	for in, want := range map[string]string{
		"## a\n\n> b\n1. c":                "a b c",
		"Change **the typo** in `main.go`": "Change the typo in main.go",
		"does 2*3 equal 6?":                "does 2*3 equal 6?",
		"2*3 and 4*5":                      "2*3 and 4*5",
		"a *star and a ` tick":             "a *star and a ` tick",
		"```sh\nrun it\n```":               "run it",
		"*one* and ***three***":            "one and three",
	} {
		if got := Brief(in); got != want {
			t.Errorf("Brief(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(Brief(strings.Repeat("x", 500))); len(got) != 200 {
		t.Fatalf("len %d", len(got))
	}
	t0 := time.Now()
	if got := []rune(Brief(strings.Repeat("a *b* ", 500_000))); len(got) != 200 {
		t.Fatalf("len %d", len(got))
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}
