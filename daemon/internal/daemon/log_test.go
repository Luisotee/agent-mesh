package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestStreamLogFiltersFormatsAndFollowsUntilStop(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "log-demo")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "alpha", &v1.SpawnAgentRequest{})
	other := w.spawnUnder(t, asLead, "beta", &v1.SpawnAgentRequest{})

	// Send a task to alpha and an alert-tagged info to lead mentioning alpha.
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "fix the bug\nin detail",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{
		Agent: worker.Id, Effort: "low", Why: "test-swap",
	})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, worker.Id, worker.TerminalId)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: other.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "other task",
	})); err != nil {
		t.Fatal(err)
	}

	// Stream backlog filtered to alpha: sees alpha's spawn, send, swap, and not beta's spawn/task.
	stream, err := w.device.Mesh.StreamLog(ctx, connect.NewRequest(&v1.StreamLogRequest{
		RunId: run.Id, Agent: worker.Name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var entries []*v1.LogEntry
	for stream.Receive() {
		entries = append(entries, stream.Msg().Entry)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected log entries for alpha")
	}
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
		if strings.Contains(e.Who, other.Name) || strings.Contains(e.Detail, "other task") {
			t.Errorf("filter leaked beta entry: %+v", e)
		}
	}
	hasKind := func(k string) bool {
		for _, x := range kinds {
			if x == k {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"spawn", "send", "swap_requested", "swap"} {
		if !hasKind(want) {
			t.Errorf("missing %q in kinds %v", want, kinds)
		}
	}

	// --lines N returns only the last N entries.
	tailStream, err := w.device.Mesh.StreamLog(ctx, connect.NewRequest(&v1.StreamLogRequest{
		RunId: run.Id, Lines: 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var tail []*v1.LogEntry
	for tailStream.Receive() {
		tail = append(tail, tailStream.Msg().Entry)
	}
	if err := tailStream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("--lines 2 returned %d entries", len(tail))
	}

	// Worker in another run is denied; read-only device is allowed.
	run2 := startRun(t, w.device.Mesh, "other-run")
	if _, err := w.as(w.token(t, worker.Id)).StreamLog(ctx, connect.NewRequest(&v1.StreamLogRequest{RunId: run2.Id})); err == nil {
		// Check on Receive since server-stream handlers return errors via Receive/Err.
	}
	sDenied, err := w.as(w.token(t, worker.Id)).StreamLog(ctx, connect.NewRequest(&v1.StreamLogRequest{RunId: run2.Id}))
	if err == nil {
		for sDenied.Receive() {
		}
		err = sDenied.Err()
	}
	if code(err) != connect.CodePermissionDenied {
		t.Errorf("cross-run worker StreamLog = %v, want PermissionDenied", err)
	}

	// Follow streams live entries until StopRun ends it.
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	defer fcancel()
	follow, err := w.as(w.token(t, lead.Id)).StreamLog(fctx, connect.NewRequest(&v1.StreamLogRequest{
		Follow: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	gotStop := make(chan bool, 1)
	go func() {
		saw := false
		for follow.Receive() {
			if follow.Msg().Entry.Kind == "stop" {
				saw = true
			}
		}
		gotStop <- (saw && follow.Err() == nil)
	}()
	// Trigger a live event then stop the run.
	if _, err := w.device.Mesh.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-live"), RunId: run.Id, Repo: worker.Repo, Slug: "live-log",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta("stop-log"), RunId: run.Id,
	})); err != nil {
		t.Fatal(err)
	}
	if ok := <-gotStop; !ok {
		t.Fatalf("follow stream did not receive stop cleanly (err=%v)", follow.Err())
	}
}
