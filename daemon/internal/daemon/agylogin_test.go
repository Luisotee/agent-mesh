package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func probeCount(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "login")
}

// A logged-out agy opens its login in a browser when meshd probes `agy -p /usage`. The probe can't open it: the
// attempt is caught, `mesh usage` says agy needs `/login`, the root of a run with an agy worker is told once, and
// agy is left alone until its token file changes (before, it was probed again every two minutes, opening the
// login each time).
func TestAnAgyThatWantsALoginIsCaughtReportedAndLeftAlone(t *testing.T) {
	probeLog := filepath.Join(t.TempDir(), "probes")
	home := t.TempDir()
	w := startReviews(t, func(c *daemon.AgentConfig) {
		c.Env["FAKEAGY_LOGIN"] = "1"
		c.Env["FAKEAGY_PROBE_LOG"] = probeLog
		c.Env["HOME"] = home
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	ctx := context.Background()
	usage := func() *v1.GetUsageResponse {
		res, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}

	start := time.Now()
	u := usage()
	if len(u.Notes) != 1 || !strings.Contains(u.Notes[0], "agy needs `/login`") || len(u.AgyRows) != 0 {
		t.Fatalf("usage after a probe that wanted a login: notes %v, rows %v", u.Notes, u.AgyRows)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("the probe was left running for %s", time.Since(start))
	}
	if n := probeCount(t, probeLog); n != 1 {
		t.Fatalf("agy was probed %d times, want 1", n)
	}

	// Asked again (even with a refresh), agy is not probed: it would only try to open the login again.
	if u := usage(); len(u.Notes) != 1 {
		t.Errorf("the note went away: %v", u.Notes)
	}
	if n := probeCount(t, probeLog); n != 1 {
		t.Errorf("agy was probed again within the wait: %d probes", n)
	}

	// The root of a run that uses agy is told once.
	if _, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Repo: gitRepo(t), Slug: "agy-w", Kind: "agy", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the root told agy needs a login", func() bool { return w.infoTo(t, w.lead, "[mesh alert login agy]") != nil })
	time.Sleep(time.Second)
	msgs, _ := w.d.Store().Read(ctx).Inbox(w.lead.Id, true)
	told := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert login agy]") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("the root was told %d times, want once", told)
	}

	// The user logs in: the token file appears, and agy is probed again.
	tokenDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "antigravity-oauth-token"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	usage()
	if n := probeCount(t, probeLog); n != 2 {
		t.Errorf("after the token file changed agy was probed %d times in all, want 2", n)
	}
}
