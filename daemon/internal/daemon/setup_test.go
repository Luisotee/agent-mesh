package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// shareWithSetup makes a share folder that holds the setup agent's instructions.
func shareWithSetup(t *testing.T) string {
	t.Helper()
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "protocol"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"PROTOCOL.md", "SETUP.md"} {
		if err := os.WriteFile(filepath.Join(share, "protocol", f), []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return share
}

func startSetup(t *testing.T, h *agentsHarness) *v1.StartSetupResponse {
	t.Helper()
	res, err := h.device.Mesh.StartSetup(context.Background(), connect.NewRequest(&v1.StartSetupRequest{Meta: meta("setup-" + t.Name())}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func TestStartSetupStartsOnceAndResumes(t *testing.T) {
	share := shareWithSetup(t)
	h := startWithAgents(t, filepath.Join(t.TempDir(), "state"), t.TempDir(), func(c *daemon.AgentConfig) { c.Share = share })
	ctx := context.Background()

	first := startSetup(t, h)
	if first.AlreadyRunning || !first.Run.Setup || first.Run.Title != "Setup" {
		t.Fatalf("first StartSetup = run %+v, already running %v", first.Run, first.AlreadyRunning)
	}
	a := first.Agent
	if a.Role != v1.AgentRole_AGENT_ROLE_ROOT || a.Name != "setup-agent" || a.Tier != "mid" || a.Kind != "claude" || a.RunId != first.Run.Id {
		t.Fatalf("setup agent = role %v name %q tier %q kind %q run %q", a.Role, a.Name, a.Tier, a.Kind, a.RunId)
	}
	// Its first message is the one-line task; the instructions are SETUP.md, which its first doorbell names.
	msgs, err := h.device.Mesh.ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{RunId: first.Run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs.Msg.Messages) != 1 || msgs.Msg.Messages[0].Type != v1.MessageType_MESSAGE_TYPE_TASK || msgs.Msg.Messages[0].ToId != a.Id {
		t.Fatalf("messages = %+v", msgs.Msg.Messages)
	}

	// Again, from another app: the same agent, no second run.
	again, err := h.device.Mesh.StartSetup(ctx, connect.NewRequest(&v1.StartSetupRequest{Meta: meta("setup-again")}))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Msg.AlreadyRunning || again.Msg.Agent.Id != a.Id || again.Msg.Run.Id != first.Run.Id {
		t.Fatalf("second StartSetup = %+v", again.Msg)
	}
	runs, err := h.device.Mesh.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{}))
	if err != nil || len(runs.Msg.Runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}

	// Retired, it is resumed on the next call, with its conversation.
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := h.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta("retire-setup"), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_RETIRED)
	back, err := h.device.Mesh.StartSetup(ctx, connect.NewRequest(&v1.StartSetupRequest{Meta: meta("setup-resume")}))
	if err != nil {
		t.Fatal(err)
	}
	if back.Msg.AlreadyRunning || back.Msg.Agent.Id != a.Id {
		t.Fatalf("resumed StartSetup = %+v", back.Msg)
	}
	// The reopen finishes in the background: the agent leaves RETIRED shortly after the call returns.
	waitFor(t, "the setup agent to leave retired", func() bool { return h.agent(a.Id).State != v1.AgentState_AGENT_STATE_RETIRED })
}

func TestStartSetupNeedsItsInstructions(t *testing.T) {
	// No share folder at all.
	h := startWithAgents(t, filepath.Join(t.TempDir(), "state"), t.TempDir())
	_, err := h.device.Mesh.StartSetup(context.Background(), connect.NewRequest(&v1.StartSetupRequest{Meta: meta("s1")}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("no share: %v, want failed precondition", err)
	}
	// A share folder without SETUP.md (an old install).
	old := t.TempDir()
	os.MkdirAll(filepath.Join(old, "protocol"), 0o755)
	h2 := startWithAgents(t, filepath.Join(t.TempDir(), "state"), t.TempDir(), func(c *daemon.AgentConfig) { c.Share = old })
	_, err = h2.device.Mesh.StartSetup(context.Background(), connect.NewRequest(&v1.StartSetupRequest{Meta: meta("s2")}))
	if code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("no SETUP.md: %v, want failed precondition", err)
	}
	// And nothing was made.
	runs, _ := h2.device.Mesh.ListRuns(context.Background(), connect.NewRequest(&v1.ListRunsRequest{}))
	if len(runs.Msg.Runs) != 0 {
		t.Fatalf("a refused StartSetup left runs: %+v", runs.Msg.Runs)
	}
}

func TestOnlyDevicesStartSetup(t *testing.T) {
	h := start(t, "")
	run := startRun(t, h.device.Mesh, "r")
	tok := h.seedAgent(run.Id, "a-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	_, err := h.as(tok).StartSetup(context.Background(), connect.NewRequest(&v1.StartSetupRequest{Meta: meta("x")}))
	if code(err) != connect.CodePermissionDenied {
		t.Fatalf("a root starting the setup: %v, want permission denied", err)
	}
}

func TestSetupRootMayPairButOthersMayNot(t *testing.T) {
	share := shareWithSetup(t)
	h := startWithAgents(t, filepath.Join(t.TempDir(), "state"), t.TempDir(), func(c *daemon.AgentConfig) { c.Share = share })
	ctx := context.Background()
	setup := startSetup(t, h)
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: h.state}.AgentToken(setup.Agent.Id))
	if err != nil {
		t.Fatal(err)
	}
	asSetup := h.as(tok)

	// The setup agent pairs and lists devices, and reads the tailnet.
	if _, err := asSetup.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p1"), Name: "phone"})); err != nil {
		t.Fatalf("setup root CreatePairing: %v", err)
	}
	if _, err := asSetup.ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{})); err != nil {
		t.Fatalf("setup root ListDevices: %v", err)
	}
	if _, err := asSetup.TailnetStatus(ctx, connect.NewRequest(&v1.TailnetStatusRequest{})); err != nil {
		t.Fatalf("setup root TailnetStatus: %v", err)
	}
	// But it can't revoke a device or start a run: those stay the host device's.
	if _, err := asSetup.RevokeDevice(ctx, connect.NewRequest(&v1.RevokeDeviceRequest{Meta: meta("r1"), DeviceId: "dev-x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("setup root RevokeDevice: %v, want permission denied", err)
	}
	if _, err := asSetup.StartRun(ctx, connect.NewRequest(&v1.StartRunRequest{Meta: meta("sr"), Title: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("setup root StartRun: %v, want permission denied", err)
	}

	// An ordinary root, and a worker of the setup run's... of any run, may not.
	run := startRun(t, h.device.Mesh, "r")
	rootTok := h.seedAgent(run.Id, "a-root", "lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	for _, c := range []struct {
		who string
		cl  func() error
	}{
		{"root", func() error {
			_, err := h.as(rootTok).CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("p2")}))
			return err
		}},
		{"root devices", func() error {
			_, err := h.as(rootTok).ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{}))
			return err
		}},
		{"root tailnet", func() error {
			_, err := h.as(rootTok).TailnetStatus(ctx, connect.NewRequest(&v1.TailnetStatusRequest{}))
			return err
		}},
	} {
		if err := c.cl(); code(err) != connect.CodePermissionDenied {
			t.Fatalf("an ordinary %s: %v, want permission denied", c.who, err)
		}
	}
}

// The setup agent is a root, but its first doorbell names SETUP.md, not the root card.
func TestSetupAgentFirstDoorbellNamesItsInstructions(t *testing.T) {
	share := shareWithSetup(t)
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Share = share })
	res, err := w.device.Mesh.StartSetup(context.Background(), connect.NewRequest(&v1.StartSetupRequest{Meta: meta("setup-bell")}))
	if err != nil {
		t.Fatal(err)
	}
	a := res.Msg.Agent
	waitFor(t, "the first doorbell rung", func() bool { return w.delivered(t, a.Id, taskIDTo(t, w, a.Id)) })
	want := "firstonehere:read" + flat(filepath.Join(share, "protocol", "PROTOCOL.md")) + "once,then" + flat(filepath.Join(share, "protocol", "SETUP.md")) + ",andfollowit"
	waitFor(t, "the doorbell on the screen", func() bool { return strings.Contains(flat(w.screen(t, a)), want) })
	if strings.Contains(flat(w.screen(t, a)), "ROOT_CARD") {
		t.Errorf("the setup agent was pointed at the root card:\n%s", w.screen(t, a))
	}
}

// A scratch repo left with no commit (a first try cut off after `git init`, or whose commit a hook or a signing setting
// refused) is finished by the next StartSetup, not handed to SpawnAgent as it is.
func TestStartSetupFinishesAHalfMadeScratchRepo(t *testing.T) {
	share := shareWithSetup(t)
	state := filepath.Join(t.TempDir(), "state")
	scratch := filepath.Join(state, "setup")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", scratch, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) { c.Share = share })
	got := startSetup(t, h)
	if got.Agent == nil || got.Agent.Name != "setup-agent" {
		t.Fatalf("StartSetup on a half-made repo = %+v", got)
	}
	if out, err := exec.Command("git", "-C", scratch, "rev-parse", "--verify", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("the scratch repo still has no commit: %v %s", err, out)
	} // Let the agent finish starting, so the daemon isn't still writing into the state dir when the test's cleanup runs.
	h.waitState(got.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}
