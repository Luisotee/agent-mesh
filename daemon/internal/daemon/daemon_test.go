package daemon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

type harness struct {
	t      *testing.T
	d      *daemon.Daemon
	state  string
	device *client.Conn
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, state string) *harness { return startWith(t, state, nil) }

// startWith is start with a say in the config.
func startWith(t *testing.T, state string, tweak func(*daemon.Config)) *harness {
	t.Helper()
	if state == "" {
		state = filepath.Join(t.TempDir(), "state")
	}
	cfg := daemon.Config{StateDir: state, TCP: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if tweak != nil {
		tweak(&cfg)
	}
	d, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, d: d, state: state, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- d.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := daemon.ReadInfo(state); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never wrote meshd.json")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tok, err := client.Token(state)
	if err != nil {
		t.Fatal(err)
	}
	h.device, _ = client.Dial(state, tok, client.Options{})
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("daemon did not stop")
	}
	h.cancel = func() {}
	h.done = closed()
}

func closed() chan error {
	c := make(chan error, 1)
	c <- nil
	return c
}

func (h *harness) as(token string) agentmeshv1connect.MeshServiceClient {
	c, _ := client.Dial(h.state, token, client.Options{})
	return c.Mesh
}

// seedAgent stores an agent with a token of its role's class, as SpawnAgent will.
func (h *harness) seedAgent(run, id, name, parent string, role v1.AgentRole) string {
	h.t.Helper()
	tok := auth.NewToken()
	err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		if err := tx.PutAgent(&v1.Agent{Id: id, RunId: run, Name: name, ParentId: parent, Role: role,
			CreatedAt: timestamppb.Now()}); err != nil {
			return err
		}
		return tx.PutToken(auth.Hash(tok), store.TokenRecord{Class: daemon.ClassOf(role), SubjectID: id, Name: name, RunID: run})
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func code(err error) connect.Code { return connect.CodeOf(err) }

func meta(id string) *v1.RequestMeta { return &v1.RequestMeta{MutationId: id} }

func startRun(t *testing.T, c agentmeshv1connect.MeshServiceClient, mid string) *v1.Run {
	t.Helper()
	res, err := c.StartRun(context.Background(), connect.NewRequest(&v1.StartRunRequest{Meta: meta(mid), Title: "demo"}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Run
}

func TestHelloTakesTheActorFromTheToken(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	res, err := h.device.Mesh.Hello(ctx, connect.NewRequest(&v1.HelloRequest{ProtocolVersion: daemon.ProtocolVersion}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Actor.GetClass() != v1.ActorClass_ACTOR_CLASS_DEVICE || res.Msg.DaemonId == "" || res.Msg.InstanceId == "" {
		t.Errorf("hello %v", res.Msg)
	}
	if d := time.Since(res.Msg.ServerTime.AsTime()); d < -time.Minute || d > time.Minute || res.Msg.ServerTime == nil {
		t.Errorf("hello's server time %v is not the host's clock now", res.Msg.ServerTime)
	}
	run := startRun(t, h.device.Mesh, "")
	worker := h.seedAgent(run.Id, "a1", "repo-fix", "", v1.AgentRole_AGENT_ROLE_WORKER)
	res, err = h.as(worker).Hello(ctx, connect.NewRequest(&v1.HelloRequest{}))
	if err != nil || res.Msg.Actor.GetId() != "a1" || res.Msg.Actor.GetClass() != v1.ActorClass_ACTOR_CLASS_WORKER {
		t.Errorf("worker hello %v, %v", res, err)
	}
	if _, err := h.device.Mesh.Hello(ctx, connect.NewRequest(&v1.HelloRequest{ProtocolVersion: 99})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "upgrade the daemon") {
		t.Errorf("newer protocol: %v", err)
	}
}

func TestUnauthenticatedAndForbiddenCalls(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	if _, err := h.as("").Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}
	if _, err := h.as("amt_bogus").Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("bad token: %v", err)
	}
	run := startRun(t, h.device.Mesh, "")
	worker := h.seedAgent(run.Id, "a1", "repo-fix", "", v1.AgentRole_AGENT_ROLE_WORKER)
	if _, err := h.as(worker).StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{})); code(err) != connect.CodePermissionDenied {
		t.Errorf("worker starting a run: %v", err)
	}
	// A revoked token stops working at once.
	h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error { return tx.RevokeTokens("a1") })
	if _, err := h.as(worker).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("revoked token: %v", err)
	}
}

func TestBrowserRequestsAreRefusedOnEveryListener(t *testing.T) {
	h := start(t, "")
	info, _ := daemon.ReadInfo(h.state)
	tok, _ := client.Token(h.state)
	for name, do := range map[string]func(*http.Request) (*http.Response, error){
		"unix": client.UnixHTTP(info.Socket, time.Second).Do,
		"tcp":  http.DefaultClient.Do,
	} {
		for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
			url := "http://" + info.TCP + agentmeshv1connect.MeshServiceHelloProcedure
			req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set(header, "https://evil.example")
			res, err := do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s with %s: status %d, want 403", name, header, res.StatusCode)
			}
		}
	}
	// Without a token, plain HTTP gets 401.
	req, _ := http.NewRequest("POST", "http://"+info.TCP+agentmeshv1connect.MeshServiceHelloProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", res.StatusCode)
	}
	st, _ := os.Stat(info.Socket)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", st.Mode().Perm())
	}
}

func TestNonLoopbackTCPIsRefused(t *testing.T) {
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), TCP: "0.0.0.0:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("Serve = %v", err)
	}
}

func TestOneDaemonPerStateDir(t *testing.T) {
	h := start(t, "")
	if _, err := daemon.New(daemon.Config{StateDir: h.state}); err == nil || !strings.Contains(err.Error(), "another meshd") {
		t.Errorf("second daemon: %v", err)
	}
}

func TestWritesAreIdempotentAndExpire(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	a := startRun(t, h.device.Mesh, "m-1")
	b := startRun(t, h.device.Mesh, "m-1")
	if !proto.Equal(a, b) {
		t.Errorf("a retried write ran again: %v then %v", a, b)
	}
	runs, _ := h.device.Mesh.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{}))
	if len(runs.Msg.Runs) != 1 {
		t.Errorf("%d runs after a retry", len(runs.Msg.Runs))
	}
	evs, _ := h.d.Store().Read(ctx).EventsAfter(0, "", 100)
	if len(evs) != 1 || evs[0].MutationId != "m-1" {
		t.Errorf("events %v", evs)
	}
	_, err := h.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta("m-1"), To: "x",
		Type: v1.MessageType_MESSAGE_TYPE_INFO}))
	if code(err) != connect.CodeInvalidArgument {
		t.Errorf("mutation id reused for another action: %v", err)
	}
	past := &v1.RequestMeta{MutationId: "m-2", ExpiresAt: timestamppb.New(time.Now().Add(-time.Second))}
	if _, err := h.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: past})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("expired write: %v", err)
	}
}

func TestMessagesGoOnlyToParentOrChildren(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	root := h.seedAgent(run.Id, "a-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w1 := h.seedAgent(run.Id, "a-w1", "repo-one", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	h.seedAgent(run.Id, "a-w2", "repo-two", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	send := func(tok, to string) error {
		_, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: to,
			Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "hi from " + to}))
		return err
	}
	if err := send(w1, "parent"); err != nil {
		t.Errorf("worker to parent: %v", err)
	}
	if err := send(w1, "repo-two"); code(err) != connect.CodePermissionDenied {
		t.Errorf("worker to sibling: %v", err)
	}
	if err := send(root, "repo-one"); err != nil {
		t.Errorf("root to child: %v", err)
	}
	if err := send(root, "parent"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("root has no parent: %v", err)
	}
	devTok, _ := client.Token(h.state)
	if err := send(devTok, "a-w2"); err != nil {
		t.Errorf("device to any agent: %v", err)
	}
	if err := send(devTok, "nobody"); code(err) != connect.CodeNotFound {
		t.Errorf("unknown agent: %v", err)
	}

	peek, err := h.as(root).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Peek: true}))
	if err != nil || len(peek.Msg.Messages) != 1 || peek.Msg.Messages[0].From.GetId() != "a-w1" {
		t.Fatalf("root peek %v, %v", peek, err)
	}
	read, _ := h.as(root).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{}))
	if len(read.Msg.Messages) != 1 || read.Msg.Messages[0].ReadAt == nil {
		t.Errorf("root read %v", read.Msg)
	}
	again, _ := h.as(root).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{}))
	if len(again.Msg.Messages) != 0 {
		t.Errorf("read messages came back: %v", again.Msg)
	}
	all, _ := h.as(root).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{All: true}))
	if len(all.Msg.Messages) != 1 {
		t.Errorf("all %v", all.Msg)
	}
	if _, err := h.device.Mesh.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{})); code(err) != connect.CodePermissionDenied {
		t.Errorf("device inbox: %v", err)
	}
}

func recv(t *testing.T, s *connect.ServerStreamForClient[v1.SubscribeEventsResponse]) *v1.SubscribeEventsResponse {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- s.Receive() }()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("stream ended: %v", s.Err())
		}
		return s.Msg()
	case <-time.After(15 * time.Second):
		t.Fatal("no stream message")
	}
	return nil
}

func TestEventStreamSnapshotsResumesAndEnds(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h := start(t, state)
	ctx := context.Background()
	first := startRun(t, h.device.Mesh, "")

	s, err := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	snap := recv(t, s).GetSnapshot()
	if snap == nil || len(snap.Runs) != 1 || snap.Runs[0].Id != first.Id || snap.Cursor.Seq != 1 {
		t.Fatalf("first frame %v", snap)
	}
	second := startRun(t, h.device.Mesh, "")
	ev := recv(t, s).GetEvent()
	if ev.GetRunStarted().GetRun().GetId() != second.Id || ev.Seq != 2 {
		t.Fatalf("live event %v", ev)
	}
	cursor := &v1.Cursor{Generation: ev.Generation, Seq: ev.Seq}

	// The daemon stops: the stream gets an explicit End, so the client knows to resume, not give up.
	h.stop()
	if end := recv(t, s).GetEnd(); end == nil {
		t.Fatal("no End before the stream closed")
	}
	s.Close()

	// Restarted on the same state, the log resumes from the cursor.
	h2 := start(t, state)
	third := startRun(t, h2.device.Mesh, "")
	s2, err := h2.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Cursor: cursor}))
	if err != nil {
		t.Fatal(err)
	}
	if r := recv(t, s2).GetResumed(); r == nil || r.From.Seq != 2 {
		t.Fatalf("expected Resumed from 2, got %v", r)
	}
	if ev := recv(t, s2).GetEvent(); ev.GetRunStarted().GetRun().GetId() != third.Id || ev.Seq != 3 {
		t.Fatalf("backlog event %v", ev)
	}
	s2.Close()

	// A cursor from another generation (a recreated log) gets a snapshot, never a wrong resume.
	s3, _ := h2.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{
		Cursor: &v1.Cursor{Generation: "gOther", Seq: 1}}))
	if snap := recv(t, s3).GetSnapshot(); snap == nil || len(snap.Runs) != 3 {
		t.Fatalf("other generation: %v", snap)
	}
	s3.Close()
}

// TestStreamAndReplayAgree is the parity check: following the live stream from a snapshot ends in the same
// state as a fresh snapshot.
func TestStreamAndReplayAgree(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	s, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	runs := map[string]*v1.Run{}
	for _, r := range recv(t, s).GetSnapshot().Runs {
		runs[r.Id] = r
	}
	for i := 0; i < 5; i++ {
		startRun(t, h.device.Mesh, "")
		r := recv(t, s).GetEvent().GetRunStarted().GetRun()
		runs[r.Id] = r
	}
	s.Close()
	s2, _ := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	fresh := recv(t, s2).GetSnapshot()
	s2.Close()
	if len(fresh.Runs) != len(runs) {
		t.Fatalf("%d runs by stream, %d by snapshot", len(runs), len(fresh.Runs))
	}
	for _, r := range fresh.Runs {
		if !proto.Equal(r, runs[r.Id]) {
			t.Errorf("run %s differs: %v vs %v", r.Id, r, runs[r.Id])
		}
	}
}

func TestUnknownRunIsNotFound(t *testing.T) {
	h := start(t, "")
	s, _ := h.device.Mesh.SubscribeEvents(context.Background(), connect.NewRequest(&v1.SubscribeEventsRequest{RunId: "rnope"}))
	if s.Receive() || code(s.Err()) != connect.CodeNotFound {
		t.Errorf("err %v", s.Err())
	}
	var ce *connect.Error
	if !errors.As(s.Err(), &ce) {
		t.Errorf("not a connect error: %v", s.Err())
	}
}

func TestARetryOfAnAppliedWriteGetsItsResultEvenPastItsExpiry(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	m := &v1.RequestMeta{MutationId: "m-late", ExpiresAt: timestamppb.New(time.Now().Add(300 * time.Millisecond))}
	first, err := h.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: m}))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	again, err := h.device.Mesh.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: m}))
	if err != nil || again.Msg.Run.Id != first.Msg.Run.Id {
		t.Errorf("late retry: %v, %v", again, err)
	}
}

func TestAnAgentReadsOnlyItsOwnRun(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	r1, r2 := startRun(t, h.device.Mesh, ""), startRun(t, h.device.Mesh, "")
	asker := h.seedAgent(r1.Id, "a1", "repo-ask", "", v1.AgentRole_AGENT_ROLE_ASKER)
	h.seedAgent(r2.Id, "a2", "repo-other", "", v1.AgentRole_AGENT_ROLE_WORKER)
	c := h.as(asker)
	runs, err := c.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{All: true}))
	if err != nil || len(runs.Msg.Runs) != 1 || runs.Msg.Runs[0].Id != r1.Id {
		t.Errorf("runs %v, %v", runs, err)
	}
	agents, err := c.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil || len(agents.Msg.Agents) != 1 {
		t.Errorf("agents %v, %v", agents, err)
	}
	if _, err := c.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: r2.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("another run's agents: %v", err)
	}
	if _, err := c.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: "a2"})); code(err) != connect.CodeNotFound {
		t.Errorf("another run's agent by id: %v", err)
	}
	s, _ := c.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	defer s.Close()
	snap := recv(t, s).GetSnapshot()
	if len(snap.Runs) != 1 || snap.Runs[0].Id != r1.Id || len(snap.Agents) != 1 {
		t.Errorf("snapshot %v", snap)
	}
}

func TestARevokedDeviceTokenGetsANewOneOnTheNextStart(t *testing.T) {
	h := start(t, "")
	state := h.state
	old, _ := client.Token(state)
	if err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.RevokeTokens("device-local")
	}); err != nil {
		t.Fatal(err)
	}
	h.stop()
	h2 := start(t, state)
	fresh, err := client.Token(state)
	if err != nil || fresh == old {
		t.Fatalf("token after the revoked one: %q, %v", fresh, err)
	}
	if _, err := h2.device.Mesh.Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Errorf("the new token: %v", err)
	}
	if _, err := h2.as(old).Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("the revoked token: %v", err)
	}
}

func TestAnAgentTokenWithoutARunIsRefusedAndReadsNothing(t *testing.T) {
	h := start(t, "")
	err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutToken("x", store.TokenRecord{Class: v1.ActorClass_ACTOR_CLASS_WORKER, SubjectID: "a1"})
	})
	if err == nil {
		t.Error("a worker token with no run was stored")
	}
	// Were one stored anyway (an actor with no run), it reads no run rather than every run.
	startRun(t, h.device.Mesh, "")
	actor := &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_WORKER, Id: "a1"}
	ctx := auth.WithActor(context.Background(), actor)
	res, err := h.d.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{All: true}))
	if code(err) != connect.CodeNotFound || res != nil {
		t.Errorf("runs of an agent with no run: %v, %v", res, err)
	}
}

func TestAReplayedMessageRingsNoSecondDoorbell(t *testing.T) {
	h := start(t, "")
	var rung []string
	h.d.SetNotify(func(id string) { rung = append(rung, id) })
	run := startRun(t, h.device.Mesh, "")
	h.seedAgent(run.Id, "a1", "repo-fix", "", v1.AgentRole_AGENT_ROLE_WORKER)
	req := &v1.SendMessageRequest{Meta: meta("m-1"), To: "a1", Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "go"}
	for i := 0; i < 2; i++ {
		if _, err := h.device.Mesh.SendMessage(context.Background(), connect.NewRequest(proto.Clone(req).(*v1.SendMessageRequest))); err != nil {
			t.Fatal(err)
		}
	}
	if len(rung) != 1 {
		t.Errorf("doorbells rung for one message sent twice: %v", rung)
	}
}

func TestATooLongStateDirIsAClearError(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	d, err := daemon.New(daemon.Config{StateDir: long, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "shorter --state") {
		t.Errorf("serve in a long state dir: %v", err)
	}
}

func TestListMessagesPagesAThrough(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	other := startRun(t, h.device.Mesh, "other")
	root := h.seedAgent(run.Id, "a-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	h.seedAgent(other.Id, "b-root", "app-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w1 := h.seedAgent(run.Id, "a-w1", "repo-one", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	send := func(tok, to string, typ v1.MessageType, body string) {
		t.Helper()
		readInbox(t, h.as(tok))
		if _, err := h.as(tok).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: to, Type: typ, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send(w1, "parent", v1.MessageType_MESSAGE_TYPE_PLAN, "plan")
	send(root, "repo-one", v1.MessageType_MESSAGE_TYPE_ANSWER, "approved")
	send(w1, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done")
	devTok, _ := client.Token(h.state)
	send(devTok, "app-lead", v1.MessageType_MESSAGE_TYPE_INFO, "another run's")

	list := func(req *v1.ListMessagesRequest) (*v1.ListMessagesResponse, error) {
		res, err := h.device.Mesh.ListMessages(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	all, err := list(&v1.ListMessagesRequest{RunId: run.Id})
	if err != nil || all.More || len(all.Messages) != 3 {
		t.Fatalf("all: %v, %v", all, err)
	}
	var bodies []string
	for _, m := range all.Messages {
		bodies = append(bodies, m.Body)
	}
	if got := strings.Join(bodies, ","); got != "plan,approved,done" {
		t.Errorf("order %s", got)
	}

	page, _ := list(&v1.ListMessagesRequest{RunId: run.Id, Limit: 2})
	if !page.More || len(page.Messages) != 2 {
		t.Fatalf("first page %v", page)
	}
	rest, _ := list(&v1.ListMessagesRequest{RunId: run.Id, Limit: 2, AfterId: page.Messages[1].Id})
	if rest.More || len(rest.Messages) != 1 || rest.Messages[0].Body != "done" {
		t.Errorf("second page %v", rest)
	}
	none, _ := list(&v1.ListMessagesRequest{RunId: run.Id, AfterId: all.Messages[2].Id})
	if none.More || len(none.Messages) != 0 {
		t.Errorf("after the last %v", none)
	}

	if _, err := list(&v1.ListMessagesRequest{}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("no run: %v", err)
	}
	if _, err := list(&v1.ListMessagesRequest{RunId: "nope"}); code(err) != connect.CodeNotFound {
		t.Errorf("unknown run: %v", err)
	}
	if _, err := list(&v1.ListMessagesRequest{RunId: run.Id, AfterId: "nope"}); code(err) != connect.CodeNotFound {
		t.Errorf("unknown cursor: %v", err)
	}
	// A cursor from another run is not a place in this one.
	foreign, _ := list(&v1.ListMessagesRequest{RunId: other.Id})
	if _, err := list(&v1.ListMessagesRequest{RunId: run.Id, AfterId: foreign.Messages[0].Id}); code(err) != connect.CodeNotFound {
		t.Errorf("cursor of another run: %v", err)
	}
	if _, err := list(&v1.ListMessagesRequest{RunId: run.Id, Limit: -1}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("negative limit: %v", err)
	}
	// A root reads messages of any run (for cross-run follow-ups), while a worker cannot read run messages.
	if res, err := h.as(root).ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{RunId: run.Id})); err != nil || len(res.Msg.Messages) != 3 {
		t.Errorf("root listing own run messages: %v, %v", res, err)
	}
	if res, err := h.as(root).ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{RunId: other.Id})); err != nil || len(res.Msg.Messages) != 1 {
		t.Errorf("root listing earlier run messages: %v, %v", res, err)
	}
	if _, err := h.as(w1).ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{RunId: run.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker listing messages: %v", err)
	}
}

// A run gets its folders when it starts: where its agents write plans, reports and tasks (`mesh dir`).
func TestStartRunMakesTheRunsFolders(t *testing.T) {
	h := start(t, "")
	res, err := h.device.Mesh.StartRun(context.Background(), connect.NewRequest(&v1.StartRunRequest{Meta: meta("run-dirs"), Title: "demo"}))
	if err != nil {
		t.Fatal(err)
	}
	dir := statedir.Paths{Dir: h.state}.RunDir(res.Msg.Run.Id)
	for _, f := range statedir.RunFolders {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || !st.IsDir() {
			t.Errorf("%s/%s: %v", dir, f, err)
		}
	}
}
