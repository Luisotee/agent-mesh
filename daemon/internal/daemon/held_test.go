package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// heldWorld is a lead with two idle workers, a and b.
type heldWorld struct {
	*reopenWorld
	run      *v1.Run
	lead     *v1.Agent
	asLead   agentmeshv1connect.MeshServiceClient
	a, b     *v1.Agent
	asA, asB agentmeshv1connect.MeshServiceClient
}

func startHeld(t *testing.T) *heldWorld {
	t.Helper()
	w := startReopen(t)
	h := &heldWorld{reopenWorld: w}
	h.run = startRun(t, w.device.Mesh, "")
	h.lead, h.asLead = w.root(t, h.run.Id)
	h.a = w.spawnUnder(t, h.asLead, "wa", &v1.SpawnAgentRequest{About: "test worker"})
	h.b = w.spawnUnder(t, h.asLead, "wb", &v1.SpawnAgentRequest{About: "test worker"})
	h.asA, h.asB = w.as(w.token(t, h.a.Id)), w.as(w.token(t, h.b.Id))
	return h
}

// inbox is every message an agent has, read or not, without marking any read.
func inbox(t *testing.T, c agentmeshv1connect.MeshServiceClient) []*v1.Message {
	t.Helper()
	res, err := c.ReadInbox(context.Background(), connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true, All: true}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Messages
}

func hasBody(ms []*v1.Message, sub string) bool {
	for _, m := range ms {
		if strings.Contains(m.Body, sub) {
			return true
		}
	}
	return false
}

// task sends a task from the lead and returns it; to reads it so a report is not refused for unread instructions.
func (h *heldWorld) task(t *testing.T, to *v1.Agent, as agentmeshv1connect.MeshServiceClient, body string) *v1.Message {
	t.Helper()
	res, err := h.asLead.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: to.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan)\n\n" + body}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.ReadInbox(context.Background(), connect.NewRequest(&v1.ReadInboxRequest{Meta: meta("")})); err != nil {
		t.Fatal(err)
	}
	return res.Msg.Message
}

func (h *heldWorld) hold(t *testing.T, to *v1.Agent, after, body string) (*v1.SendMessageResponse, error) {
	t.Helper()
	res, err := h.asLead.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: to.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan)\n\n" + body, After: after}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (h *heldWorld) held(t *testing.T) []*v1.HeldTask {
	t.Helper()
	res, err := h.asLead.ListHeldTasks(context.Background(), connect.NewRequest(&v1.ListHeldTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Held
}

func TestATaskHeldForAnotherIsSentWhenThatTaskIsReportedDone(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")

	res, err := h.hold(t, h.b, t1.Id, "second")
	if err != nil {
		t.Fatal(err)
	}
	if res.Held == "" || res.Message != nil {
		t.Fatalf("a held task answers with its id and no message: %v", res)
	}
	if hasBody(inbox(t, h.asB), "second") {
		t.Fatal("a held task is in the inbox before its task is done")
	}
	held := h.held(t)
	if len(held) != 1 || held[0].Id != res.Held || held[0].After != t1.Id || held[0].To != h.b.Id {
		t.Fatalf("held %v", held)
	}
	if b, err := h.asLead.GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{})); err != nil ||
		!strings.Contains(b.Msg.Text, "held: "+res.Held+" for "+h.b.Name+", until task "+t1.Id+" is done") {
		t.Fatalf("the lead's brief names the held task: %v %v", b, err)
	}
	// A device sees it too, and a read-only listing needs no write.
	if l, err := h.device.Mesh.ListHeldTasks(context.Background(), connect.NewRequest(&v1.ListHeldTasksRequest{})); err != nil || len(l.Msg.Held) != 1 {
		t.Fatalf("a device lists: %v %v", l, err)
	}

	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "the held task sent", func() bool { return hasBody(inbox(t, h.asB), "second") })
	waitFor(t, "the held row gone", func() bool { return len(h.held(t)) == 0 })

	// Its task is done now: a new one sent after it goes at once.
	res, err = h.hold(t, h.b, t1.Id, "third")
	if err != nil {
		t.Fatal(err)
	}
	if res.Held != "" || res.Message == nil {
		t.Fatalf("a task whose dependency is done is sent at once: %v", res)
	}
}

func TestAHeldTaskIsDroppedAndTheLeadToldWhenItsTaskFails(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	if _, err := h.hold(t, h.b, t1.Id, "second"); err != nil {
		t.Fatal(err)
	}
	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_FAILED)
	waitFor(t, "the lead told", func() bool { return hasBody(inbox(t, h.asLead), "[mesh held ") })
	if hasBody(inbox(t, h.asB), "second") {
		t.Fatal("the task of a failed dependency was sent")
	}
	if n := len(h.held(t)); n != 0 {
		t.Fatalf("still held: %d", n)
	}
}

func TestACancelledHeldTaskIsNeverSent(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	res, err := h.hold(t, h.b, t1.Id, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.asB.CancelHeldTask(context.Background(), connect.NewRequest(&v1.CancelHeldTaskRequest{Meta: meta(""), Id: res.Held})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker cancelling: %v", err)
	}
	if _, err := h.asLead.CancelHeldTask(context.Background(), connect.NewRequest(&v1.CancelHeldTaskRequest{Meta: meta(""), Id: res.Held})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.asLead.CancelHeldTask(context.Background(), connect.NewRequest(&v1.CancelHeldTaskRequest{Meta: meta(""), Id: res.Held})); code(err) != connect.CodeNotFound {
		t.Errorf("cancelling twice: %v", err)
	}
	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_DONE)
	// The release runs in the background after the done: give it its turn, then look.
	waitFor(t, "the done stored", func() bool { return hasBody(inbox(t, h.asLead), "reported") })
	if hasBody(inbox(t, h.asB), "second") {
		t.Fatal("a cancelled held task was sent")
	}
}

func TestAHeldTaskIsRefusedForWhatIsNoTaskOfThisRunOrNoLeadSend(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	ctx := context.Background()
	if _, err := h.hold(t, h.b, "m-nope", "x"); code(err) != connect.CodeNotFound {
		t.Errorf("an unknown task: %v", err)
	}
	if _, err := h.asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.b.Id,
		Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "x", After: t1.Id})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("--after on an info: %v", err)
	}
	// The task named is the lead's own message of another kind: not a task.
	info, err := h.asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.b.Id,
		Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "fyi"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hold(t, h.b, info.Msg.Message.Id, "x"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("after a message that is no task: %v", err)
	}
	// A worker holds no tasks.
	if _, err := h.asA.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.b.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "x", After: t1.Id})); code(err) != connect.CodePermissionDenied && code(err) != connect.CodeInvalidArgument {
		t.Errorf("a worker holding a task: %v", err)
	}
	// Another run's task.
	other := startRun(t, h.device.Mesh, "")
	_, asOther := h.root(t, other.Id)
	if _, err := asOther.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.b.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "x", After: t1.Id})); code(err) != connect.CodeNotFound {
		t.Errorf("a task of another run: %v", err)
	}
}

func TestAReplayedHeldRequestHoldsOnce(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	req := &v1.SendMessageRequest{Meta: meta("hold-1"), To: h.b.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Direct task (no plan)\n\nsecond", After: t1.Id}
	first, err := h.asLead.SendMessage(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.asLead.SendMessage(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if first.Msg.Held == "" || second.Msg.Held != first.Msg.Held || len(h.held(t)) != 1 {
		t.Fatalf("held %q then %q, %d rows", first.Msg.Held, second.Msg.Held, len(h.held(t)))
	}
}

func TestAHeldTaskWhoseTaskWasDoneWhileTheDaemonWasDownIsSentAtStart(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	if _, err := h.hold(t, h.b, t1.Id, "second"); err != nil {
		t.Fatal(err)
	}
	// The done is stored with no release after it, as if the daemon stopped between its commit and the release.
	done := &v1.Message{Id: "m-done-1", RunId: h.run.Id, From: &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_WORKER, Id: h.a.Id, Name: h.a.Name, RunId: h.run.Id},
		ToId: h.lead.Id, ToName: h.lead.Name, Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "finished", Re: t1.Id, CreatedAt: timestamppb.Now()}
	if err := h.d.Store().Write(context.Background(), done.From, "", func(tx *store.Tx) error { return tx.PutMessage(done) }); err != nil {
		t.Fatal(err)
	}
	h.stop()
	h.agentsHarness = h.restart(t)
	b := h.as(h.token(t, h.b.Id))
	waitFor(t, "the held task sent at start", func() bool { return hasBody(inbox(t, b), "second") })
}

func TestAHeldTaskOfAClosedRunIsDroppedAtStart(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	if _, err := h.hold(t, h.b, t1.Id, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.device.Mesh.StopRun(context.Background(), connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: h.run.Id})); err != nil {
		t.Fatal(err)
	}
	h.stop()
	h.agentsHarness = h.restart(t)
	waitFor(t, "the held task of the closed run dropped", func() bool {
		l, err := h.device.Mesh.ListHeldTasks(context.Background(), connect.NewRequest(&v1.ListHeldTasksRequest{}))
		return err == nil && len(l.Msg.Held) == 0
	})
}

// A follow-up sent to the worker before it reported makes its done name the follow-up, and that done answers the task
// before it too.
func TestAFollowUpToTheWorkerDoesNotStrandATaskHeldForItsFirstTask(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	if _, err := h.hold(t, h.b, t1.Id, "second"); err != nil {
		t.Fatal(err)
	}
	h.task(t, h.a, h.asA, "first, and one more thing") // a's done now names this one
	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "the held task sent", func() bool { return hasBody(inbox(t, h.asB), "second") })
}

func TestAHeldTaskIsRefusedForAFailedDependencyOrAHeldIdOrTheRoot(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	held, err := h.hold(t, h.b, t1.Id, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hold(t, h.b, held.Held, "third"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("after a held id: %v", err)
	}
	// The root takes no `--after` on a task for it: it reports to nobody.
	toRoot, err := h.device.Mesh.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""),
		To: h.lead.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "for the root"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.hold(t, h.b, toRoot.Msg.Message.Id, "x"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("after a task for the root: %v", err)
	}
	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_FAILED)
	waitFor(t, "the first held task dropped", func() bool { return len(h.held(t)) == 0 })
	if _, err := h.hold(t, h.b, t1.Id, "fourth"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("after a failed task: %v", err)
	}
}

// What a device queued is dropped with its token, not sent in its name afterwards.
func TestARevokedDevicesHeldTasksAreDropped(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	ph := pairPhone(t, h.harness, "phone", false)
	if _, err := ph.c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.b.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "from the phone", After: t1.Id})); err != nil {
		t.Fatal(err)
	}
	if n := len(h.held(t)); n != 1 {
		t.Fatalf("held %d", n)
	}
	if _, err := h.device.Mesh.RevokeDevice(context.Background(), connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta(""), DeviceId: ph.id})); err != nil {
		t.Fatal(err)
	}
	if n := len(h.held(t)); n != 0 {
		t.Fatalf("a revoked device's task is still held: %d", n)
	}
}

// A later task from someone else (the daemon's PR feedback, a device) is other work: its done answers nothing held for the
// first task.
func TestADoneForAnotherSendersTaskDoesNotReleaseATaskHeldForTheFirst(t *testing.T) {
	h := startHeld(t)
	t1 := h.task(t, h.a, h.asA, "first")
	if _, err := h.hold(t, h.b, t1.Id, "second"); err != nil {
		t.Fatal(err)
	}
	// A device sends the worker something else; the worker does it and reports it.
	if _, err := h.device.Mesh.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: h.a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan)\n\nsomething else"})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.asA.ReadInbox(context.Background(), connect.NewRequest(&v1.ReadInboxRequest{Meta: meta("")})); err != nil {
		t.Fatal(err)
	}
	h.report(t, h.a, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "the done stored", func() bool { return hasBody(inbox(t, h.asLead), "reported") })
	// The release runs in the background after the done: wait for the task to be settled either way, then look.
	time.Sleep(500 * time.Millisecond)
	if hasBody(inbox(t, h.asB), "second") || len(h.held(t)) != 1 {
		t.Fatalf("a done for another sender's task released it (held %d)", len(h.held(t)))
	}
}
