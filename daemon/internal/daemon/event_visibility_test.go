package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// An asker or a reviewer reads, in the event stream and the log, only the messages sent to or by it; a worker, a root and a
// device read the run's.
func TestAnAskerOrReviewerSeesOnlyItsOwnMessagesInEventsAndLog(t *testing.T) {
	h := start(t, "")
	run := startRun(t, h.device.Mesh, "")
	rootTok := h.seedAgent(run.Id, "a-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	workTok := h.seedAgent(run.Id, "a-w1", "worker", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	revTok := h.seedAgent(run.Id, "a-rv", "reviewer", "a-root", v1.AgentRole_AGENT_ROLE_REVIEWER)
	askTok := h.seedAgent(run.Id, "a-as", "asker", "a-root", v1.AgentRole_AGENT_ROLE_ASKER)
	ctx := context.Background()
	send := func(from, to, body string) {
		t.Helper()
		if _, err := h.as(from).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: to,
			Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send(rootTok, "a-w1", "PRIVATE root to worker")
	send(rootTok, "a-rv", "for the reviewer only")
	send(workTok, "a-root", "PRIVATE worker to root")
	send(rootTok, "a-as", "for the asker only")
	send(rootTok, "a-w1", "END")

	// The events: a resumed stream from the start replays the log; read until the last message.
	read := func(tok string) []string {
		t.Helper()
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		first, err := h.as(tok).SubscribeEvents(sctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id}))
		if err != nil {
			t.Fatal(err)
		}
		if !first.Receive() || first.Msg().GetSnapshot() == nil {
			t.Fatalf("no snapshot: %v", first.Err())
		}
		gen := first.Msg().GetSnapshot().Cursor.Generation
		first.Close()
		s, err := h.as(tok).SubscribeEvents(sctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run.Id,
			Cursor: &v1.Cursor{Generation: gen, Seq: 0}}))
		if err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for s.Receive() {
			if m := s.Msg().GetEvent().GetMessageSent().GetMessage(); m != nil {
				bodies = append(bodies, m.Body)
				if m.Body == "END" {
					break
				}
			}
		}
		return bodies
	}
	// END goes to the worker: the reviewer and the asker never see it, so they read until the stream's timeout: give
	// them their own last message instead.
	send(rootTok, "a-rv", "END")
	send(rootTok, "a-as", "END")
	all := strings.Join(read(rootTok), "|")
	for _, want := range []string{"PRIVATE root to worker", "for the reviewer only", "PRIVATE worker to root", "for the asker only"} {
		if !strings.Contains(all, want) {
			t.Errorf("the root's events lack %q: %s", want, all)
		}
	}
	if w := strings.Join(read(workTok), "|"); !strings.Contains(w, "for the asker only") || !strings.Contains(w, "PRIVATE root to worker") {
		t.Errorf("a worker sees the run's messages: %s", w)
	}
	rev := strings.Join(read(revTok), "|")
	if !strings.Contains(rev, "for the reviewer only") || strings.Contains(rev, "PRIVATE") || strings.Contains(rev, "for the asker only") {
		t.Errorf("the reviewer's events: %s", rev)
	}
	ask := strings.Join(read(askTok), "|")
	if !strings.Contains(ask, "for the asker only") || strings.Contains(ask, "PRIVATE") || strings.Contains(ask, "reviewer") {
		t.Errorf("the asker's events: %s", ask)
	}

	// The log: the same rule.
	log := func(tok string) string {
		t.Helper()
		s, err := h.as(tok).StreamLog(ctx, connect.NewRequest(&v1.StreamLogRequest{RunId: run.Id}))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for s.Receive() {
			e := s.Msg().Entry
			out = append(out, e.Kind+" "+e.Who+" "+e.Detail)
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "|")
	}
	if l := log(revTok); strings.Contains(l, "PRIVATE") || strings.Contains(l, "for the asker only") || !strings.Contains(l, "for the reviewer only") {
		t.Errorf("the reviewer's log: %s", l)
	}
	if l := log(rootTok); !strings.Contains(l, "PRIVATE root to worker") {
		t.Errorf("the root's log: %s", l)
	}
}
