package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// agyOnPath makes lookAgy find an agy on PATH (or none) for a test.
func agyOnPath(t *testing.T, found bool) {
	t.Helper()
	old := lookAgy
	t.Cleanup(func() { lookAgy = old })
	lookAgy = func() (string, error) {
		if found {
			return "/usr/bin/agy", nil
		}
		return "", os.ErrNotExist
	}
}

// An agy found on PATH is not run while its token file is missing: a logged-out agy opens the user's login in a
// browser, and a meshd with a temp HOME (a dev or test one) that reaches the real agy this way must not do it.
func TestAnAgyFoundOnPathIsNotRunWithoutItsTokenFile(t *testing.T) {
	home := t.TempDir()
	agyOnPath(t, true)
	d := &Daemon{rt: &runtime{cfg: AgentConfig{Env: map[string]string{"HOME": home}}}}
	if !d.agyProbeBlocked() {
		t.Fatal("agy on PATH with no token file would be run")
	}
	d.rt.cfg.Binaries = map[string]string{"agy": "/fake/agy"}
	if d.agyProbeBlocked() {
		t.Error("an agy the config names (a fake in tests) is blocked")
	}
	d.rt.cfg.Binaries = nil
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d.agyProbeBlocked() {
		t.Error("agy with its token file is blocked")
	}
}

func loginDaemon(t *testing.T, now func() time.Time) *Daemon {
	t.Helper()
	return &Daemon{paths: Paths{Dir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: now,
		rt: &runtime{cfg: AgentConfig{Env: map[string]string{"HOME": t.TempDir()}}}}
}

// A second probe that wants a login while the first record holds leaves the record alone: its time is the hold
// and the alert's key, and rewriting it would push both back every time.
func TestARecordedLoginIsNotRewrittenWhileItHolds(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d := loginDaemon(t, func() time.Time { return now })
	d.noteAgyLogin()
	first, err := os.ReadFile(d.paths.AgyLoginNeeded())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	d.noteAgyLogin()
	if second, _ := os.ReadFile(d.paths.AgyLoginNeeded()); string(second) != string(first) {
		t.Errorf("the record was rewritten within the hold: %q -> %q", first, second)
	}
	now = now.Add(time.Hour) // the hold is over: the next one is recorded again
	d.noteAgyLogin()
	if third, _ := os.ReadFile(d.paths.AgyLoginNeeded()); string(third) == string(first) {
		t.Errorf("the record was not renewed after the hold")
	}
}

// With nothing recorded the alert pass reads no clock and does nothing (it runs every pass, in every test and
// every meshd).
func TestTheLoginAlertPassWithNothingRecordedReadsNoClock(t *testing.T) {
	d := loginDaemon(t, func() time.Time { t.Error("the clock was read"); return time.Time{} })
	d.agyLoginAlertPass(context.Background())
}

// With no agy installed (a Claude-only user) nothing is blocked and no login is wanted: agy is "not found", and
// `mesh usage` does not say it needs a login.
func TestNoAgyInstalledIsNotALoginNeeded(t *testing.T) {
	agyOnPath(t, false)
	d := loginDaemon(t, time.Now)
	if d.agyProbeBlocked() {
		t.Error("a host with no agy has agy blocked")
	}
	if d.agyHeld() {
		t.Error("a host with no agy has agy held")
	}
}

// agy is held while a probe's login record holds (the 30 minutes), as well as while its token file is missing:
// `agy models` honours both.
func TestAgyIsHeldWhileALoginIsRecorded(t *testing.T) {
	agyOnPath(t, false)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d := loginDaemon(t, func() time.Time { return now })
	if d.agyHeld() {
		t.Fatal("held with nothing recorded")
	}
	d.noteAgyLogin()
	if !d.agyHeld() {
		t.Error("not held right after a probe wanted a login")
	}
	now = now.Add(agyLoginRetry + time.Minute)
	if d.agyHeld() {
		t.Error("still held after the wait")
	}
}

// loginWorld is a daemon with a store holding an open run: a root and an agy worker.
func loginWorld(t *testing.T, now func() time.Time) (*Daemon, *v1.Agent) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rt, err := newRuntime(AgentConfig{Env: map[string]string{"HOME": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: st, paths: Paths{Dir: dir}, rt: rt, now: now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lead := &v1.Agent{Id: "lead", RunId: "r1", Name: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude",
		State: v1.AgentState_AGENT_STATE_IDLE, CreatedAt: timestamppb.New(now())}
	if err := st.Write(context.Background(), nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.New(now())}); err != nil {
			return err
		}
		if err := tx.PutAgent(lead); err != nil {
			return err
		}
		return tx.PutAgent(&v1.Agent{Id: "w", RunId: "r1", Name: "w", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: "agy",
			ParentId: "lead", State: v1.AgentState_AGENT_STATE_IDLE, CreatedAt: timestamppb.New(now())})
	}); err != nil {
		t.Fatal(err)
	}
	return d, lead
}

func loginAlerts(t *testing.T, d *Daemon, lead *v1.Agent) int {
	t.Helper()
	msgs, err := d.store.Read(context.Background()).Inbox(lead.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert login agy]") {
			n++
		}
	}
	return n
}

// agy stays logged out for hours: the record is renewed every half hour (each probe that wants a login after the
// hold), and the root is still told once, not once per renewal. Logging in and then out again tells it anew.
func TestTheRootIsToldOnceWhileAgyStaysLoggedOut(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d, lead := loginWorld(t, func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < 6; i++ { // three hours, a probe and a pass every 31 minutes
		d.noteAgyLogin()
		d.agyLoginAlertPass(ctx)
		now = now.Add(agyLoginRetry + time.Minute)
	}
	if n := loginAlerts(t, d, lead); n != 1 {
		t.Fatalf("the root was told %d times in three hours, want 1", n)
	}
	now = now.Add(24 * time.Hour) // a day on, it still is: told again
	d.noteAgyLogin()
	d.agyLoginAlertPass(ctx)
	if n := loginAlerts(t, d, lead); n != 2 {
		t.Errorf("a day later the root was told %d times in all, want 2", n)
	}
	d.clearAgyLogin() // the user logged in ...
	d.noteAgyLogin()  // ... and agy is logged out again
	d.agyLoginAlertPass(ctx)
	if n := loginAlerts(t, d, lead); n != 3 {
		t.Errorf("after a login and a new logout the root was told %d times in all, want 3", n)
	}
}

// Two passes at once (the background pass and the one a usage call runs) tell the root once.
func TestTwoLoginAlertPassesAtOnceTellTheRootOnce(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d, lead := loginWorld(t, func() time.Time { return now })
	d.noteAgyLogin()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.agyLoginAlertPass(context.Background()) }()
	}
	wg.Wait()
	if n := loginAlerts(t, d, lead); n != 1 {
		t.Errorf("8 concurrent passes told the root %d times, want 1", n)
	}
}

// A pass whose message was not stored (the root stopped meanwhile) gives its claim back, so the next pass tells
// the root.
func TestAFailedLoginAlertIsSentAtTheNextPass(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d, lead := loginWorld(t, func() time.Time { return now })
	d.noteAgyLogin()
	setState := func(s v1.AgentState) {
		if err := d.store.Write(context.Background(), nil, "", func(tx *store.Tx) error {
			a, err := tx.GetAgent(lead.Id)
			if err != nil {
				return err
			}
			a.State = s
			return tx.PutAgent(a)
		}); err != nil {
			t.Fatal(err)
		}
	}
	agyAlertPlannedHook = func() { setState(v1.AgentState_AGENT_STATE_EXITED) }
	t.Cleanup(func() { agyAlertPlannedHook = nil })
	d.agyLoginAlertPass(context.Background()) // the root is gone by the write: nothing stored
	agyAlertPlannedHook = nil
	setState(v1.AgentState_AGENT_STATE_IDLE)
	d.agyLoginAlertPass(context.Background())
	if n := loginAlerts(t, d, lead); n != 1 {
		t.Errorf("the root was told %d times after a failed pass and a good one, want 1", n)
	}
}

// The agy adapter's `agy models` is held by the daemon's agyHeld: while a probe's login record holds, as well as
// while an agy on PATH has no token file.
func TestTheAgyAdaptersModelsAreHeldWhileALoginIsRecorded(t *testing.T) {
	agyOnPath(t, false)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d := loginDaemon(t, func() time.Time { return now })
	rt, err := newRuntime(AgentConfig{Env: map[string]string{"HOME": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	d.rt = rt
	d.wireProbes()
	a, err := d.rt.adapters.Get(agy.Kind)
	if err != nil {
		t.Fatal(err)
	}
	ag := a.(*agy.Adapter)
	if ag.Hold() {
		t.Fatal("held with nothing recorded")
	}
	d.noteAgyLogin()
	if !ag.Hold() {
		t.Error("`agy models` is not held while a login is recorded")
	}
	ag.LoginNeeded()
	if ag.ProbeDir != d.paths.Probe() {
		t.Errorf("ProbeDir = %q, want %q", ag.ProbeDir, d.paths.Probe())
	}
}
