package hook

import (
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
