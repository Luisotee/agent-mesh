package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestRemovingAPeerDeletesItsCloneOnlyWhenAskedAndWhenTheDaemonMadeIt(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	cloned := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	if !cloned.Cloned {
		t.Fatalf("a URL's clone is the daemon's: %v", cloned)
	}
	own := filepath.Join(t.TempDir(), "beta")
	if out, err := exec.Command("git", "clone", "-q", up.dir, own).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	byPath := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: own, Name: "beta-research"})
	if byPath.Cloned {
		t.Fatalf("a clone the user made is theirs: %v", byPath)
	}
	// Theirs is never deleted, and the peer stays.
	if _, err := w.asRoot.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: byPath.Name, DeleteClone: true})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("delete_clone of the user's clone: %v", err)
	}
	w.peer(t, byPath.Name)
	// Without the flag the clone stays; with it, the daemon's goes.
	res, err := w.asRoot.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: byPath.Name}))
	if err != nil || res.Msg.DeletedClone != "" {
		t.Fatalf("remove: %v %v", res, err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the user's clone is gone: %v", err)
	}
	res, err = w.asRoot.RemovePeer(ctx, connect.NewRequest(&v1.RemovePeerRequest{Meta: meta(""), Name: cloned.Name, DeleteClone: true}))
	if err != nil || res.Msg.DeletedClone != cloned.Repo {
		t.Fatalf("remove with delete_clone: %v %v", res, err)
	}
	if _, err := os.Stat(cloned.Repo); !os.IsNotExist(err) {
		t.Errorf("the clone stayed: %v", err)
	}
}

// A peer's conversation has one process: while run 2's agent continues it, run 1's retired agent for it can't be
// brought back by a message, and a third ask from run 1 is refused the same way.
func TestARetiredPeerAgentIsNotResumedWhileAnotherRunsHoldsItsConversation(t *testing.T) {
	w := startPeers(t)
	ctx := context.Background()
	up := newUpstream(t, "alpha")
	p := w.add(t, &v1.AddPeerRequest{Meta: meta(""), Source: up.url(), Group: "g"})
	res := w.ask(t, &v1.AskRequest{Peers: p.Name, Tier: "mid", Body: "first?", Timeout: 60})
	a1 := res.Agents[0]
	w.quiet(t, a1.Id)
	w.say(t, a1.Id, v1.MessageType_MESSAGE_TYPE_DONE, "first answer")
	waitFor(t, "run 1's message", func() bool { return len(w.answers(t)) == 1 })
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a1.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(a1.Id, v1.AgentState_AGENT_STATE_RETIRED)

	run2 := startRun(t, w.device.Mesh, "")
	_, as2 := w.root(t, run2.Id)
	res2, err := as2.Ask(ctx, connect.NewRequest(&v1.AskRequest{Continue: true, Peers: p.Name, Tier: "mid", Body: "second?", Timeout: 60}))
	if err != nil || len(res2.Msg.Agents) != 1 {
		t.Fatalf("run 2's ask: %v %v", res2, err)
	}
	// Run 1's retired agent stays retired, the task stored and refused with the reason.
	sent, err := w.asRoot.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a1.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "third?"}))
	if err != nil {
		t.Fatal(err)
	}
	if sent.Msg.Resumed != nil || !strings.Contains(sent.Msg.ResumeError, "is already running in") {
		t.Errorf("resumed %v (%q)", sent.Msg.Resumed, sent.Msg.ResumeError)
	}
	// An ask from run 1 is refused for the same reason, and no task is left in the retired agent's inbox.
	if _, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Continue: true, Peers: p.Name, Tier: "mid", Body: "fourth?", Timeout: 60})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "one process per conversation") {
		t.Errorf("run 1's ask: %v", err)
	}
	unread, _ := w.d.Store().Read(ctx).Inbox(a1.Id, true)
	for _, m := range unread {
		if strings.Contains(m.Body, "fourth?") {
			t.Errorf("a refused ask left its task unread: %v", m)
		}
	}
}

func TestImportingAPeerWithAnOddGroupOrAnUpstreamThatReadsAsAnOptionLeavesWhatIsUnsafe(t *testing.T) {
	w := startPeers(t)
	up := newUpstream(t, "alpha")
	clone := filepath.Join(t.TempDir(), "alpha")
	if out, err := exec.Command("git", "clone", "-q", up.dir, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	long := strings.Repeat("é", 500)
	file := filepath.Join(t.TempDir(), "peers.json")
	data, _ := json.Marshal(map[string]any{
		"peers": map[string]any{
			"bad-group-research": map[string]any{"repo": clone, "group": "no good!", "about": "x"},
			"opt-research":       map[string]any{"repo": clone, "group": "g", "upstream": map[string]any{"remote": "--upload-pack=evil", "branch": "main"}},
		},
		"groups": map[string]any{"g": map[string]any{"about": long}},
	})
	os.WriteFile(file, data, 0o600)
	res, err := w.asRoot.ImportPeers(context.Background(), connect.NewRequest(&v1.ImportPeersRequest{Meta: meta(""), From: file}))
	if err != nil {
		t.Fatal(err)
	}
	skipped := strings.Join(res.Msg.Skipped, "|")
	if !strings.Contains(skipped, "bad-group-research: its group") || !strings.Contains(skipped, "opt-research: imported without its upstream") {
		t.Errorf("skipped %v, imported %v", res.Msg.Skipped, res.Msg.Imported)
	}
	if got := w.peer(t, "opt-research"); got.Upstream.GetRemote() != "" {
		t.Errorf("the option-like remote was kept: %v", got.Upstream)
	}
	groups, _ := w.asRoot.ListPeers(context.Background(), connect.NewRequest(&v1.ListPeersRequest{}))
	if len(groups.Msg.Groups) != 1 || len([]rune(groups.Msg.Groups[0].About)) != 400 {
		t.Errorf("groups %v", groups.Msg.Groups)
	}
}
