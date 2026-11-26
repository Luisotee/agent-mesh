package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// flat drops escapes and spaces: the screen wraps long lines and moves the cursor between the pieces.
func flat(s string) string { return strings.Join(strings.Fields(ansi.ReplaceAllString(s, "")), "") }

// fakeShare is a share folder with a protocol and a Claude plugin manifest, under a short path of its own.
func fakeShare(t *testing.T, plugin bool) string {
	t.Helper()
	share, err := os.MkdirTemp("", "share-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(share) })
	if err := os.MkdirAll(filepath.Join(share, "protocol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if plugin {
		os.MkdirAll(filepath.Join(share, ".claude-plugin"), 0o755)
		os.WriteFile(filepath.Join(share, ".claude-plugin", "plugin.json"), []byte(`{"name":"agent-mesh-daemon"}`), 0o644)
	}
	return share
}

// The share folder reaches an agent three ways: $AGENT_MESH_SHARE, a Claude launch's --plugin-dir (the skills,
// for this session only), and the first doorbell of each process, which names the protocol file to read.
func TestAgentsGetTheShareFolderTheSkillsAndTheProtocolInTheirFirstDoorbell(t *testing.T) {
	share := fakeShare(t, true)
	envLog := filepath.Join(t.TempDir(), "env.jsonl")
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Share = share
		c.Env["FAKECLAUDE_ENV_LOG"] = envLog
	})
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "newcomer", &v1.SpawnAgentRequest{Task: "do the thing"})
	waitFor(t, "the task rung", func() bool { return w.delivered(t, worker.Id, taskIDTo(t, w, worker.Id)) })

	argv := w.launches(t, worker.SessionId)
	if len(argv) != 1 || !strings.Contains(strings.Join(argv[0], " "), "--plugin-dir "+share) {
		t.Fatalf("the launch: %v", argv)
	}
	raw, err := os.ReadFile(envLog)
	if err != nil || !strings.Contains(string(raw), "AGENT_MESH_SHARE="+share) {
		t.Fatalf("the agent's environment lacks AGENT_MESH_SHARE: %v %.200s", err, raw)
	}
	pointer := "firstonehere:read" + flat(filepath.Join(share, "protocol", "PROTOCOL.md"))
	waitFor(t, "the first doorbell naming the protocol", func() bool { return strings.Contains(flat(w.screen(t, worker)), pointer) })
	// A second doorbell of the same process doesn't repeat it.
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	readInbox(t, w.as(w.token(t, worker.Id)))
	res, err := asLead.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""),
		To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "fyi"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second doorbell", func() bool { return !w.deliveredAt(t, worker.Id, res.Msg.Message.Id).IsZero() })
	time.Sleep(300 * time.Millisecond)
	second := "infofrom" + flat(lead.Name) + ":run`meshinbox`" // the screen keeps the first doorbell above it
	if screen := flat(w.screen(t, worker)); !strings.Contains(screen, second) {
		t.Fatalf("the second doorbell isn't on the screen:\n%s", screen)
	} else if strings.Contains(screen, second+"(first") {
		t.Errorf("the second doorbell names the protocol again:\n%s", screen)
	}
	// A root is pointed at its card, and at the protocol.
	m := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "the root's first doorbell", func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
	rootScreen := flat(w.screen(t, lead))
	if !strings.Contains(rootScreen, "firstonehere:read"+flat(filepath.Join(share, "protocol", "ROOT_CARD.md"))+"andfollowit") {
		t.Errorf("the root's screen:\n%s", w.screen(t, lead))
	}
}

// No share folder, or no plugin manifest in it: no --plugin-dir, and the doorbell is the plain one.
func TestWithoutAShareFolderOrItsManifestNothingIsAdded(t *testing.T) {
	for name, share := range map[string]string{"none": "", "no plugin": fakeShare(t, false)} {
		w := startReopen(t, func(c *daemon.AgentConfig) { c.Share = share })
		run := startRun(t, w.device.Mesh, "")
		_, asLead := w.root(t, run.Id)
		worker := w.spawnUnder(t, asLead, "plain", &v1.SpawnAgentRequest{Task: "do the thing"})
		waitFor(t, name+": the task rung", func() bool { return w.delivered(t, worker.Id, taskIDTo(t, w, worker.Id)) })
		argv := w.launches(t, worker.SessionId)
		if len(argv) != 1 || strings.Contains(strings.Join(argv[0], " "), "--plugin-dir") {
			t.Errorf("%s: the launch: %v", name, argv)
		}
		if share == "" && strings.Contains(flat(w.screen(t, worker)), "firstonehere") {
			t.Errorf("%s: a pointer without a share folder", name)
		}
	}
}
