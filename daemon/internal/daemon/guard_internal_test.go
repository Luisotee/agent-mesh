package daemon

import (
	"context"
	"slices"
	"testing"
)

// A launch's shared agents are a copy: the round goes on appending to its own list after the context is made.
func TestWithCapSharedKeepsACopy(t *testing.T) {
	ids := make([]string, 1, 4) // room to append in place
	ids[0] = "a1"
	ctx := withCapShared(context.Background(), ids)
	ids = append(ids, "a2")
	ids[0] = "changed"
	if got := capShared(ctx); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("the context's shared agents moved with the caller's list: %v", got)
	}
}
