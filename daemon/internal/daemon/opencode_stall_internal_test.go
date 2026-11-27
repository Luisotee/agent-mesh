package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/events"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

// stallRig is a daemon over a store and an opencode.db by hand, with a WORKING OpenCode worker whose reply is open.
type stallRig struct {
	t      *testing.T
	d      *Daemon
	st     *store.Store
	clock  time.Time
	term   *stubListTerm
	dbPath string
}

func newStallRig(t *testing.T, tweak func(*Daemon)) *stallRig {
	t.Helper()
	dir := t.TempDir()
	rig := &stallRig{t: t, clock: time.Now().Truncate(time.Millisecond)}
	// The store stamps its messages with the rig's clock too: an alert's dedupe compares them with the agent's activity.
	st, err := store.OpenAt(filepath.Join(dir, "state.db"), func() time.Time { return rig.clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rig.st = st
	dbPath := filepath.Join(dir, "opencode.db")
	rt, err := newRuntime(AgentConfig{Env: map[string]string{"AGENT_MESH_OPENCODE_DB": dbPath},
		LookPath: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	rig.term = &stubListTerm{terminals: []*termv1.Terminal{{Id: "t1", StartedAt: timestamppb.New(rig.clock.Add(-time.Hour)),
		LastOutput: timestamppb.New(rig.clock)}}}
	rt.term = &termclient.Client{API: rig.term}
	d := &Daemon{store: st, paths: Paths{Dir: dir}, rt: rt, schema: settings.Default(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return rig.clock }}
	d.bus = events.New(st)
	if tweak != nil {
		tweak(d)
	}
	rig.d = d
	t.Cleanup(d.bgWG.Wait)

	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.New(rig.clock)}); err != nil {
			return err
		}
		if err := tx.PutAgent(&v1.Agent{Id: "root", RunId: "r1", Name: "lead", Kind: "claude", Role: v1.AgentRole_AGENT_ROLE_ROOT,
			State: v1.AgentState_AGENT_STATE_IDLE, CreatedAt: timestamppb.New(rig.clock)}); err != nil {
			return err
		}
		return tx.PutAgent(&v1.Agent{Id: "a1", RunId: "r1", ParentId: "root", Name: "repo-oc", Kind: opencode.Kind,
			Role: v1.AgentRole_AGENT_ROLE_WORKER, Model: "opencode/big-pickle", Worktree: wt, TerminalId: "t1", SessionId: "ses_1",
			State: v1.AgentState_AGENT_STATE_WORKING, CreatedAt: timestamppb.New(rig.clock.Add(-time.Hour)),
			LastActivity: timestamppb.New(rig.clock)})
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`); err != nil {
		t.Fatal(err)
	}
	at := rig.clock.UnixMilli()
	raw, _ := json.Marshal(map[string]any{"role": "assistant", "providerID": "opencode", "modelID": "big-pickle",
		"time": map[string]any{"created": at}})
	if _, err := db.Exec(`INSERT INTO session VALUES ('ses_1', ?, NULL, ?, ?)`, wt, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('m1', 'ses_1', ?, ?, ?)`, at, at, string(raw)); err != nil {
		t.Fatal(err)
	}
	rig.dbPath = dbPath
	return rig
}

// part adds a part to the open reply, as the model streams one, at the given time.
func (g *stallRig) part(at time.Time) {
	g.t.Helper()
	db, err := sql.Open("sqlite", "file:"+g.dbPath)
	if err != nil {
		g.t.Fatal(err)
	}
	defer db.Close()
	ms := at.UnixMilli()
	if _, err := db.Exec(`INSERT INTO part VALUES (?, 'm1', 'ses_1', ?, ?, '{}')`, "p"+at.Format("150405.000"), ms, ms); err != nil {
		g.t.Fatal(err)
	}
}

// outputAt is the terminal's last output.
func (g *stallRig) outputAt(at time.Time) {
	g.term.terminals[0].LastOutput = timestamppb.New(at)
	g.d.rt.mu.Lock()
	g.d.rt.lastOutput["t1"] = at // what the daemon's terminal watch records: a drawing screen is not UNKNOWN
	g.d.rt.mu.Unlock()
}

func (g *stallRig) alerts() int {
	g.t.Helper()
	msgs, err := g.st.Read(context.Background()).Inbox("root", true)
	if err != nil {
		g.t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, "[mesh alert stalled a1]") {
			n++
		}
	}
	return n
}

// An OpenCode reply open for opencode.stall_after (15 min) with no new part or token is told to the parent once; a
// part that arrives, or a shorter time, is not a stall.
func TestAnOpenOpenCodeReplyWithNothingNewForStallAfterIsToldOnce(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	g.clock = g.clock.Add(14 * time.Minute)
	g.outputAt(g.clock) // a spinner keeps the screen moving
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 {
		t.Fatalf("a stall alert after 14 minutes: %d", n)
	}
	// A part streams in: the reply is alive again.
	g.part(g.clock)
	g.clock = g.clock.Add(14 * time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 {
		t.Fatalf("a stall alert 14 minutes after a part: %d", n)
	}
	g.clock = g.clock.Add(2 * time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 1 {
		t.Fatalf("stall alerts after 16 minutes with no new part: %d, want 1", n)
	}
	g.clock = g.clock.Add(time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 1 {
		t.Fatalf("a second alert for the same stretch: %d", n)
	}
	msgs, _ := g.st.Read(ctx).Inbox("root", true)
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, "[mesh alert stalled a1]") && !strings.Contains(m.Body, "no new part or token") {
			t.Errorf("alert text: %s", m.Body)
		}
	}
}

// The same reply that stalls, recovers and stalls again is told twice: no event moves the agent's activity, so the
// reply's own update time keys the dedupe. A stretch already told is not read again by the next poll.
func TestAReplyThatStallsRecoversAndStallsAgainIsToldAgain(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	g.clock = g.clock.Add(16 * time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 1 {
		t.Fatalf("first stall: %d alerts, want 1", n)
	}
	g.d.rt.openCode.mu.Lock()
	told := g.d.rt.openCode.polled["a1"].stalledFor
	g.d.rt.openCode.mu.Unlock()
	if told.IsZero() {
		t.Fatal("the told stretch is not remembered")
	}
	// Recovers (a part lands in the same message), then stalls again.
	g.clock = g.clock.Add(5 * time.Minute)
	g.part(g.clock)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	g.clock = g.clock.Add(16 * time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 2 {
		t.Fatalf("after the second stall: %d alerts, want 2", n)
	}
}

// A terminal with nothing new drawn is looked at too, once it has been quiet as long as a stall takes (here a
// stall_after under the 10 minutes after which the agent shows as UNKNOWN).
func TestAQuietTerminalsOpenReplyIsCheckedForAStallToo(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	if err := g.st.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutSetting("opencode.stall_after", "300") }); err != nil {
		t.Fatal(err)
	}
	g.d.pollOpenCode(ctx) // takes the screen's output in: the reply is working
	g.clock = g.clock.Add(6 * time.Minute)
	g.d.pollOpenCode(ctx) // no new output at all
	if n := g.alerts(); n != 1 {
		t.Fatalf("stall alerts on a silent terminal: %d, want 1", n)
	}
}

// One hang is one alert: an agent the view shows as UNKNOWN (nothing heard for UnknownAfter) is told by that alert,
// and the stall alert stays quiet.
func TestAHangTheViewCallsUnknownGetsNoStallAlert(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	g.d.pollOpenCode(ctx)
	g.clock = g.clock.Add(16 * time.Minute) // past stall_after and past UnknownAfter, no output at all
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 {
		t.Fatalf("a stall alert for an UNKNOWN agent: %d", n)
	}
}

// A short stall_after is not made to wait a whole minute on a quiet terminal.
func TestAQuietTerminalIsCheckedAtStallAfterWhenThatIsUnderAMinute(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	if err := g.st.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutSetting("opencode.stall_after", "20") }); err != nil {
		t.Fatal(err)
	}
	g.d.pollOpenCode(ctx)
	g.clock = g.clock.Add(25 * time.Second)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 1 {
		t.Fatalf("stall alerts 25 s into a 20 s stall_after on a silent terminal: %d, want 1", n)
	}
}

// A row whose time_updated says 0 says nothing: it is not a stall of fifty years.
func TestAReplyRowWithZeroUpdatedIsNotAStall(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+g.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE message SET time_updated = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	g.clock = g.clock.Add(time.Minute)
	g.outputAt(g.clock)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 {
		t.Fatalf("a stall alert from time_updated 0: %d", n)
	}
}

// A root has no parent: its stall (or any alert) builds no text, reads no screen and opens no write.
func TestAnAlertForARootBuildsNothing(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	root, err := g.st.Read(ctx).GetAgent("root")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := g.d.alertOnce(ctx, g.st.Read(ctx), root, "stalled", func(context.Context) string { calls++; return "x" }); err != nil || calls != 0 {
		t.Fatalf("alertOnce for a root: %d text builds, %v", calls, err)
	}
	// And the stall path itself: an OpenCode root whose reply is old gets nothing, however often it is polled.
	if err := g.st.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent("a1")
		if err != nil {
			return err
		}
		a.Role, a.ParentId = v1.AgentRole_AGENT_ROLE_ROOT, ""
		return tx.PutAgent(a)
	}); err != nil {
		t.Fatal(err)
	}
	g.clock = g.clock.Add(time.Hour)
	g.outputAt(g.clock)
	seq := g.st.Generation()
	g.d.pollOpenCode(ctx)
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 || g.st.Generation() != seq {
		t.Fatalf("a root's stall: %d alerts", n)
	}
}

// The stall alert's text is built before the write, and that takes seconds (git, the screen): the agent can cross
// UnknownAfter meanwhile, and the write then leaves no alert. That must not count as told: when the terminal draws
// again the stall is told.
func TestAStallAlertTheWriteLeftOutIsNotRememberedAsTold(t *testing.T) {
	g := newStallRig(t, nil)
	ctx := context.Background()
	g.clock = g.clock.Add(16 * time.Minute)
	g.outputAt(g.clock) // drawing: the view says WORKING when the poll looks
	// The poll reads the screen for a limit first, then the text reads it again: that second read takes 11 minutes,
	// as a hung host would, and the agent then reads as UNKNOWN when the alert is written.
	reads := 0
	g.d.rt.term.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if reads++; reads == 2 {
			g.clock = g.clock.Add(11 * time.Minute)
		}
		return nil, errors.New("no screen")
	})}
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 0 {
		t.Fatalf("a stall alert written for an agent that became UNKNOWN meanwhile: %d", n)
	}
	g.d.rt.openCode.mu.Lock()
	told := g.d.rt.openCode.polled["a1"].stalledFor
	g.d.rt.openCode.mu.Unlock()
	if !told.IsZero() {
		t.Fatalf("the stretch is remembered as told though nothing was written: %v", told)
	}
	if reads < 2 {
		t.Fatalf("the alert's text read the screen %d times: the clock never moved in the write", reads)
	}
	g.d.rt.term.HTTP = nil
	g.outputAt(g.clock) // it draws again
	g.d.pollOpenCode(ctx)
	if n := g.alerts(); n != 1 {
		t.Fatalf("after it drew again: %d stall alerts, want 1", n)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stall_after 0 turns the alert off, however long the reply sits.
func TestStallAfterZeroNeverTells(t *testing.T) {
	g := newStallRig(t, nil)
	if err := g.d.store.Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutSetting("opencode.stall_after", "0")
	}); err != nil {
		t.Fatal(err)
	}
	g.d.pollOpenCode(context.Background())
	g.clock = g.clock.Add(3 * time.Hour)
	g.outputAt(g.clock)
	g.d.pollOpenCode(context.Background())
	if n := g.alerts(); n != 0 {
		t.Fatalf("stall alerts with stall_after 0: %d", n)
	}
}
