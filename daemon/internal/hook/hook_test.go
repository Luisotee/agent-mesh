package hook

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func setup(t *testing.T) (statedir.Paths, Options) {
	t.Helper()
	p := statedir.Paths{Dir: t.TempDir()}
	os.MkdirAll(p.Agent("a1"), 0o700)
	if err := fsutil.WriteAtomic(p.AgentToken("a1"), []byte("amt_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, Options{StateDir: p.Dir, AgentID: "a1", Kind: "claude", DialTimeout: 100 * time.Millisecond,
		CallTimeout: 200 * time.Millisecond}
}

func TestADownDaemonSpoolsInOrder(t *testing.T) {
	p, o := setup(t)
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for i, ev := range []string{"UserPromptSubmit", "Stop"} {
		o.Event, o.Stdin = ev, strings.NewReader(`{"session_id":"s"}`)
		o.Now = func() time.Time { return clock.Add(time.Duration(i) * time.Second) }
		start := time.Now()
		if err := Run(o); err == nil {
			t.Fatal("no error for a missing daemon")
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("a down daemon held the hook %s", time.Since(start))
		}
	}
	var got []string
	unlock, _ := LockSpool(p, "a1")
	err := Drain(p, "a1", func(r *v1.ReportHookRequest) error {
		got = append(got, r.EventName)
		if string(r.Payload) != `{"session_id":"s"}` || r.Kind != "claude" || !strings.HasPrefix(r.Meta.MutationId, "hook-") {
			t.Errorf("spooled %v", r)
		}
		return nil
	})
	unlock()
	if err != nil || strings.Join(got, ",") != "UserPromptSubmit,Stop" {
		t.Errorf("drained %v, %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 0 {
		t.Errorf("left in the spool: %v", left)
	}
}

func TestDrainStopsAtARetryableFailureAndDropsRefusals(t *testing.T) {
	p, _ := setup(t)
	for i, ev := range []string{"A", "B", "C"} {
		spool(p, "a1", &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: ev}, EventName: ev},
			time.Unix(int64(i), 0))
	}
	var sent []string
	err := Drain(p, "a1", func(r *v1.ReportHookRequest) error {
		sent = append(sent, r.EventName)
		switch r.EventName {
		case "A":
			return connect.NewError(connect.CodePermissionDenied, errors.New("revoked"))
		case "B":
			return connect.NewError(connect.CodeUnavailable, errors.New("down"))
		}
		return nil
	})
	if err == nil || strings.Join(sent, "") != "AB" {
		t.Fatalf("sent %v, err %v", sent, err)
	}
	left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json"))
	if len(left) != 2 {
		t.Errorf("want B and C kept, have %v", left)
	}
}

func TestNoTokenIsAnErrorNotACrash(t *testing.T) {
	_, o := setup(t)
	o.AgentID = "nobody"
	o.Event, o.Stdin = "Stop", strings.NewReader("{}")
	if err := Run(o); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("err = %v", err)
	}
}

func TestAHookThatFailsEveryTimeIsDroppedOnlyAfterMaxAttemptsAndMaxAge(t *testing.T) {
	p, _ := setup(t)
	clock := time.Unix(1_000_000, 0)
	now = func() time.Time { return clock }
	defer func() { now = time.Now }()
	for i, ev := range []string{"A", "B"} {
		spool(p, "a1", &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: ev}, EventName: ev}, time.Unix(int64(i), 0))
	}
	var sent, dropped []string
	send := func(r *v1.ReportHookRequest) error {
		sent = append(sent, r.EventName)
		if r.EventName == "A" {
			return errors.New("a store failure") // reaches the client as CodeUnknown
		}
		return nil
	}
	drain := func() error {
		return DrainReporting(p, "a1", send, func(r *v1.ReportHookRequest, _ error) { dropped = append(dropped, r.EventName) })
	}
	// Many quick failures: kept, and B waits behind it.
	for i := 0; i < 3*MaxAttempts; i++ {
		clock = clock.Add(2 * time.Second)
		if err := drain(); err == nil {
			t.Fatalf("attempt %d: no error", i+1)
		}
	}
	if len(dropped) != 0 || strings.Contains(strings.Join(sent, ""), "B") {
		t.Fatalf("dropped on seconds of trouble: %v, sent %v", dropped, sent)
	}
	// Past MaxAge it goes, and the next one is sent after it.
	clock = clock.Add(MaxAge)
	if err := drain(); err != nil {
		t.Fatalf("after MaxAge: %v", err)
	}
	if strings.Join(dropped, "") != "A" || sent[len(sent)-1] != "B" {
		t.Errorf("dropped %v, sent %v: want A dropped and B sent after it", dropped, sent)
	}
	if bad, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.bad")); len(bad) != 1 {
		t.Errorf("the dropped hook is kept as .bad: %v", bad)
	}
	// While the daemon is down nothing is counted: a hook waits as long as it must.
	spool(p, "a1", &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: "C"}, EventName: "C"}, time.Unix(9, 0))
	for i := 0; i < 3*MaxAttempts; i++ {
		clock = clock.Add(time.Hour)
		DrainReporting(p, "a1", func(*v1.ReportHookRequest) error {
			return connect.NewError(connect.CodeUnavailable, errors.New("down"))
		}, func(*v1.ReportHookRequest, error) { t.Fatal("dropped while the daemon was down") })
	}
}

// The hook process's own drain can't report a drop, so it never makes one.
func TestTheHookProcessesDrainNeverDrops(t *testing.T) {
	p, _ := setup(t)
	clock := time.Unix(1_000_000, 0)
	now = func() time.Time { return clock }
	defer func() { now = time.Now }()
	spool(p, "a1", &v1.ReportHookRequest{Meta: &v1.RequestMeta{MutationId: "A"}, EventName: "A"}, time.Unix(1, 0))
	for i := 0; i < 3*MaxAttempts; i++ {
		clock = clock.Add(time.Hour)
		if err := Drain(p, "a1", func(*v1.ReportHookRequest) error { return errors.New("store") }); err == nil {
			t.Fatal("no error")
		}
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 1 {
		t.Errorf("the hook was dropped by Drain: %v", left)
	}
	if bad, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.bad")); len(bad) != 0 {
		t.Errorf("a .bad file: %v", bad)
	}
}

// With a capture dir the raw payload is saved, byte for byte, in the order the hooks fired, and the hook still
// reports (here: spools, the daemon being down).
func TestACaptureDirKeepsEachRawPayloadInOrder(t *testing.T) {
	p, o := setup(t)
	o.CaptureDir = filepath.Join(t.TempDir(), "cap")
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	payloads := map[string]string{"SessionStart": `{"session_id":"s","source":"startup","x":[1, 2]}`, "Stop": `{"session_id":"s"}`}
	for i, ev := range []string{"SessionStart", "Stop"} {
		o.Event, o.Stdin = ev, strings.NewReader(payloads[ev])
		o.Now = func() time.Time { return clock.Add(time.Duration(i) * time.Second) }
		Run(o)
	}
	files, _ := filepath.Glob(filepath.Join(o.CaptureDir, "*.json"))
	if len(files) != 2 || !strings.HasSuffix(files[0], "-SessionStart.json") || !strings.HasSuffix(files[1], "-Stop.json") {
		t.Fatalf("captured %v", files)
	}
	if b, _ := os.ReadFile(files[0]); string(b) != payloads["SessionStart"] {
		t.Errorf("payload %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 2 {
		t.Errorf("the hook stopped reporting: %v", left)
	}
}

// A capture that fails (an unwritable folder) is only a side channel: the hook still reports.
func TestACaptureThatFailsStillReportsTheHook(t *testing.T) {
	p, o := setup(t)
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	o.CaptureDir = filepath.Join(blocker, "cap") // under a file: can't be made
	o.Event, o.Stdin = "Stop", strings.NewReader(`{"session_id":"s"}`)
	err := Run(o)
	if err == nil || !strings.Contains(err.Error(), "capturing the payload") {
		t.Errorf("the capture's failure isn't reported: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 1 {
		t.Errorf("the hook stopped reporting: %v", left)
	}
}

func TestAgyPreToolUseDenyEvaluatesLocallyEvenWhenDaemonIsDown(t *testing.T) {
	p, o := setup(t)
	wt := t.TempDir()
	var out bytes.Buffer
	o.Kind, o.Event, o.Stdout, o.Cwd = "agy", "PreToolUse", &out, wt
	o.DenyPaths, o.DenyPublish = []string{wt}, true
	o.Stdin = strings.NewReader(`{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"foo.txt"}}}`)
	// Daemon is down: Run returns spool/dial error (mesh still exits 0), and stdout got the deny JSON.
	_ = Run(o)
	if !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatalf("expected deny decision on stdout, got %q", out.String())
	}
	// Allowed path outside denyPaths prints nothing at all.
	out.Reset()
	o.Stdin = strings.NewReader(`{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"` + p.Dir + `/plans/a1.md"}}}`)
	_ = Run(o)
	if out.Len() != 0 {
		t.Fatalf("allowed path should print nothing, got %q", out.String())
	}
}

func TestAgyPreToolUseFailsClosedOnAPayloadTooLargeToCheck(t *testing.T) {
	_, o := setup(t)
	wt := t.TempDir()
	var out bytes.Buffer
	o.Kind, o.Event, o.Stdout, o.Cwd = "agy", "PreToolUse", &out, wt
	o.DenyPaths, o.DenyPublish = []string{wt}, true
	big := strings.Repeat("x", MaxPayload)
	o.Stdin = strings.NewReader(`{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"/elsewhere/a","Content":"` + big + `"}}}`)
	_ = Run(o)
	if !strings.Contains(out.String(), `"decision":"deny"`) || !strings.Contains(out.String(), "too large") {
		t.Fatalf("an oversized payload was not denied: %.200q", out.String())
	}
}

func TestAgyCheckOnlyDeniesAndReportsNothing(t *testing.T) {
	p, o := setup(t)
	wt := t.TempDir()
	var out bytes.Buffer
	o.Kind, o.Event, o.Stdout, o.Cwd, o.CheckOnly = "agy", "PreToolUse", &out, wt, true
	o.DenyPaths, o.DenyPublish = []string{wt}, true
	o.Stdin = strings.NewReader(`{"cwd":"` + wt + `","toolCall":{"name":"run_command","args":{"CommandLine":"git push"}}}`)
	if err := Run(o); err != nil {
		t.Fatalf("a check-only hook tried to report: %v", err)
	}
	if !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatalf("check-only did not deny: %q", out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 0 {
		t.Errorf("a check-only hook spooled: %v", left)
	}
}

func TestAgyPreToolUseNeverWaitsOnTheSpoolLock(t *testing.T) {
	p, o := setup(t)
	unlock, err := LockSpool(p, "a1") // another hook (or the daemon's drain) is draining
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	o.Kind, o.Event, o.Stdin = "agy", "PreToolUse", strings.NewReader(`{"toolCall":{"name":"read_file"}}`)
	done := make(chan error, 1)
	go func() { done <- Run(o) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("quick report: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an agy PreToolUse hook waited on the spool lock: agy denies a tool whose hook outlives its timeout")
	}
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 1 {
		t.Errorf("the hook was not spooled behind the lock holder: %v", left)
	}
}

func TestAgyPreToolUseQueuesBehindSpooledHooks(t *testing.T) {
	p, o := setup(t)
	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	o.Kind = "agy"
	for i, ev := range []string{"Stop", "PreToolUse"} { // the daemon is down: Stop spools, PreToolUse goes after it
		o.Event, o.Stdin = ev, strings.NewReader(`{}`)
		o.Now = func() time.Time { return clock.Add(time.Duration(i) * time.Second) }
		_ = Run(o)
	}
	var got []string
	unlock, _ := LockSpool(p, "a1")
	_ = Drain(p, "a1", func(r *v1.ReportHookRequest) error { got = append(got, r.EventName); return nil })
	unlock()
	if strings.Join(got, ",") != "Stop,PreToolUse" {
		t.Errorf("drained %v", got)
	}
}

// A quick report is bounded by QuickTimeout even when the caller set no timeouts: zero is not "no limit".
func TestQuickTimeoutsDefaultWhenUnset(t *testing.T) {
	for _, c := range []struct {
		o          Options
		call, dial time.Duration
	}{
		{Options{}, QuickTimeout, QuickTimeout},
		{Options{CallTimeout: 3 * time.Second, DialTimeout: 2 * time.Second}, QuickTimeout, QuickTimeout},
		{Options{CallTimeout: 200 * time.Millisecond}, 200 * time.Millisecond, 200 * time.Millisecond},
		{Options{DialTimeout: 100 * time.Millisecond}, QuickTimeout, 100 * time.Millisecond},
	} {
		if call, dial := quickTimeouts(c.o); call != c.call || dial != c.dial {
			t.Errorf("quickTimeouts(%+v) = %v, %v; want %v, %v", c.o, call, dial, c.call, c.dial)
		}
	}
}

// Claude's PreToolUse deny is checked here too, with the daemon down, and printed as Claude's permission decision;
// a Claude agent without the rule, and every other event, print nothing.
func TestClaudePreToolUsePublishDenyEvaluatesLocallyEvenWhenDaemonIsDown(t *testing.T) {
	p, o := setup(t)
	var out bytes.Buffer
	o.Kind, o.Event, o.Stdout, o.DenyPublish = "claude", "PreToolUse", &out, true
	o.Stdin = strings.NewReader(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"git -C . push"}}`)
	_ = Run(o)
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) || !strings.Contains(out.String(), `"hookEventName":"PreToolUse"`) ||
		!strings.Contains(out.String(), `may not run \"git push\"`) {
		t.Fatalf("expected a Claude deny on stdout, got %q", out.String())
	}
	// The hook still reports (spooled while the daemon is down).
	if left, _ := filepath.Glob(filepath.Join(p.Spool("a1"), "*.json")); len(left) != 1 {
		t.Errorf("the denied call's hook was not spooled: %v", left)
	}
	for name, mk := range map[string]func(){
		"a read": func() { o.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"gh pr view 5"}}`) },
		"no rule": func() {
			o.DenyPublish = false
			o.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git push"}}`)
		},
		"another event": func() {
			o.DenyPublish, o.Event = true, "PostToolUse"
			o.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git push"}}`)
		},
	} {
		out.Reset()
		o.DenyPublish, o.Event = true, "PreToolUse"
		mk()
		_ = Run(o)
		if out.Len() != 0 {
			t.Errorf("%s printed %q", name, out.String())
		}
	}
	// Fail closed: a payload that can't be read, or is too large to check, is denied.
	for name, in := range map[string]string{"unreadable": `{not json`,
		"oversize": `{"tool_name":"Bash","tool_input":{"command":"ls","x":"` + strings.Repeat("x", MaxPayload) + `"}}`} {
		out.Reset()
		o.DenyPublish, o.Event, o.Stdin = true, "PreToolUse", strings.NewReader(in)
		_ = Run(o)
		if !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
			t.Errorf("%s was not denied: %.200q", name, out.String())
		}
	}
}

func TestASessionStartThatCantReachTheDaemonPrintsAShortReminder(t *testing.T) {
	_, o := setup(t)
	for source, want := range map[string]bool{"compact": true, "clear": true, "resume": true, "startup": false} {
		var out bytes.Buffer
		o.Event, o.Stdout = "SessionStart", &out
		o.Stdin = strings.NewReader(`{"session_id":"s","source":"` + source + `"}`)
		Run(o)
		if got := strings.Contains(out.String(), "agent-mesh reminder ("+source+")"); got != want {
			t.Errorf("source %s: printed %q", source, out.String())
		}
	}
	// Another event, or another CLI, prints nothing.
	var out bytes.Buffer
	o.Event, o.Stdout, o.Stdin = "Stop", &out, strings.NewReader(`{"session_id":"s"}`)
	Run(o)
	if out.Len() != 0 {
		t.Errorf("a Stop printed %q", out.String())
	}
}
