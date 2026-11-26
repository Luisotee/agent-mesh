package daemon

import (
	"testing"
	"time"
)

func TestReserveAlertDedupeSurvivesPastOneHour(t *testing.T) {
	d := &Daemon{rt: &runtime{}}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	key := "claude:seven_day:2026-12-01T00:00:00Z"

	if !d.shouldSendReserveAlert("r1", key, t0) {
		t.Fatal("first alert should send")
	}
	d.markReserveAlertSent("r1", key, t0)

	if d.shouldSendReserveAlert("r1", key, t0.Add(2*time.Hour)) {
		t.Error("same alert key sent again after 2h; want once per reset window")
	}
	if d.shouldSendReserveAlert("r1", key, t0.Add(24*time.Hour)) {
		t.Error("same alert key sent again after 24h; want once per reset window")
	}
	if !d.shouldSendReserveAlert("r1", "claude:seven_day:2026-12-08T00:00:00Z", t0.Add(24*time.Hour)) {
		t.Error("new reset window should alert")
	}
}
