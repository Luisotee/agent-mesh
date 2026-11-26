package daemon

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func TestLimitUntil(t *testing.T) {
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	hold := 30 * time.Minute
	reset := at.Add(5 * time.Hour)
	for _, c := range []struct {
		name string
		rl   *adapterv1.RateLimited
		want time.Time
	}{
		{"a named reset time wins", &adapterv1.RateLimited{ResetAt: timestamppb.New(reset), ResetIn: "1h"}, reset},
		{"a relative reset", &adapterv1.RateLimited{ResetIn: "1h2m3s"}, at.Add(time.Hour + 2*time.Minute + 3*time.Second)},
		{"a relative reset in seconds", &adapterv1.RateLimited{ResetIn: "45s"}, at.Add(45 * time.Second)},
		{"an unreadable reset falls back to the hold", &adapterv1.RateLimited{ResetIn: "soon"}, at.Add(hold)},
		{"a zero reset falls back to the hold", &adapterv1.RateLimited{ResetIn: "0s"}, at.Add(hold)},
		{"no reset", &adapterv1.RateLimited{}, at.Add(hold)},
	} {
		if got := limitUntil(c.rl, at, hold); !got.Equal(c.want) {
			t.Errorf("%s: until %v, want %v", c.name, got, c.want)
		}
	}
}
