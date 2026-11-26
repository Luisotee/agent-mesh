package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// setState puts an agent in a state, as its hooks would.
func (h *harness) setState(id string, s v1.AgentState, detail string) {
	h.t.Helper()
	err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := h.d.Store().Read(context.Background()).GetAgent(id)
		if err != nil {
			return err
		}
		a.State, a.StateDetail, a.LastActivity = s, detail, timestamppb.Now()
		return tx.PutAgent(a)
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func TestListWaitingIsWhatNobodyHasAnsweredYet(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	other := startRun(t, h.device.Mesh, "other")
	root := h.seedAgent(run.Id, "a-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	otherRoot := h.seedAgent(other.Id, "b-root", "app-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w1 := h.seedAgent(run.Id, "a-w1", "repo-one", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	w2 := h.seedAgent(run.Id, "a-w2", "repo-two", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	w3 := h.seedAgent(run.Id, "a-w3", "repo-three", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	w4 := h.seedAgent(run.Id, "a-w4", "repo-four", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	send := func(tok, to string, typ v1.MessageType, body string) {
		t.Helper()
		readInbox(t, h.as(tok))
		if _, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: to, Type: typ, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(c agentmeshv1connect.MeshServiceClient, runID string) string {
		t.Helper()
		res, err := c.ListWaiting(ctx, connect.NewRequest(&v1.ListWaitingRequest{RunId: runID}))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range res.Msg.Waiting {
			out = append(out, fmt.Sprintf("%s:%s:%s", strings.TrimPrefix(w.Kind.String(), "WAITING_KIND_"), w.AgentId, w.Message.GetBody()))
		}
		return strings.Join(out, " ")
	}
	if got := rows(h.device.Mesh, ""); got != "" {
		t.Fatalf("nothing waits yet, got %q", got)
	}

	send(w1, "parent", v1.MessageType_MESSAGE_TYPE_PLAN, "plan one")
	send(w2, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION, "which db?")
	send(w3, "parent", v1.MessageType_MESSAGE_TYPE_PLAN, "plan three")
	send(w4, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION, "ignored: its author exits")
	// The lead answers the third plan; a task, a report and an info to the lead are not waiting.
	send(root, "repo-three", v1.MessageType_MESSAGE_TYPE_ANSWER, "approved")
	send(w1, "parent", v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	h.setState("a-w4", v1.AgentState_AGENT_STATE_EXITED, "")
	if got, want := rows(h.device.Mesh, ""), "PLAN:a-w1:plan one QUESTION:a-w2:which db?"; got != want {
		t.Errorf("waiting\n got %s\nwant %s", got, want)
	}

	// An answer after the plan closes it; a plan sent after the answer waits again; the user may answer too.
	send(root, "repo-one", v1.MessageType_MESSAGE_TYPE_ANSWER, "change X")
	send(w1, "parent", v1.MessageType_MESSAGE_TYPE_PLAN, "plan one, again")
	devTok, _ := client.Token(h.state)
	send(devTok, "repo-two", v1.MessageType_MESSAGE_TYPE_ANSWER, "postgres")
	if got, want := rows(h.device.Mesh, ""), "PLAN:a-w1:plan one, again"; got != want {
		t.Errorf("after answers\n got %s\nwant %s", got, want)
	}

	// An agent stopped at a prompt waits, after the messages.
	h.setState("a-w3", v1.AgentState_AGENT_STATE_NEEDS_INPUT, "Bash: rm -r build")
	if got, want := rows(h.device.Mesh, ""), "PLAN:a-w1:plan one, again INPUT:a-w3:"; got != want {
		t.Errorf("with a prompt\n got %s\nwant %s", got, want)
	}

	// Another run's lead sees nothing of this run; this run's lead sees its own and only its own; a worker is refused.
	if got := rows(h.as(otherRoot), ""); got != "" {
		t.Errorf("another run's agent saw %q", got)
	}
	if got := rows(h.as(root), ""); got != "PLAN:a-w1:plan one, again INPUT:a-w3:" {
		t.Errorf("the lead's view %q", got)
	}
	if _, err := h.as(root).ListWaiting(ctx, connect.NewRequest(&v1.ListWaitingRequest{RunId: other.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a lead reading another run: %v", err)
	}
	if _, err := h.as(w1).ListWaiting(ctx, connect.NewRequest(&v1.ListWaitingRequest{})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker may not list what waits on the user: %v", err)
	}
	if got := rows(h.device.Mesh, other.Id); got != "" {
		t.Errorf("the other run %q", got)
	}
	if _, err := h.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	if got := rows(h.device.Mesh, ""); got != "" {
		t.Errorf("a stopped run still waits: %q", got)
	}
}
