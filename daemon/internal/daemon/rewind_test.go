package daemon_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func (h *agentsHarness) rewind(agent, entry, mid string) (*v1.RewindAgentResponse, error) {
	res, err := h.device.Mesh.RewindAgent(context.Background(), connect.NewRequest(&v1.RewindAgentRequest{Meta: meta(mid), Agent: agent, EntryId: entry}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// promptEntry is the transcript entry of a prompt with the text given.
func (h *agentsHarness) promptEntry(agent, text string) *v1.TranscriptEntry {
	h.t.Helper()
	for _, e := range h.transcript(agent, -1, true).Entries {
		if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_USER && e.Text == text {
			return e
		}
	}
	h.t.Fatalf("no prompt %q in the transcript", text)
	return nil
}

func TestRewindReopensTheAgentOnItsConversationCutBeforeThePrompt(t *testing.T) {
	h, a := chatDaemon(t, nil)
	for _, p := range []string{"one", "two"} {
		if err := h.promptWhenReady(a.Id, p); err != nil {
			t.Fatal(err)
		}
		h.waitTranscript(a.Id, p)
		h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	old := h.agent(a.Id).SessionId
	two := h.promptEntry(a.Id, "two")
	// The entry has to be a prompt: a reply is refused, with nothing changed.
	for _, e := range h.transcript(a.Id, -1, true).Entries {
		if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT {
			if _, err := h.rewind(a.Id, e.Id, "rw-reply"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("rewinding to a reply: %v", err)
			}
			break
		}
	}
	res, err := h.rewind(a.Id, two.Id, "rw-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "two" || res.SessionId == "" || res.SessionId == old {
		t.Fatalf("rewind = %+v (old session %s)", res, old)
	}
	// A retry is answered from the record: the same copy, no second cut.
	if again, err := h.rewind(a.Id, two.Id, "rw-1"); err != nil || again.SessionId != res.SessionId {
		t.Fatalf("a retried rewind: %+v, %v", again, err)
	}
	// The agent comes back on the copy, which holds the first prompt and not the second.
	waitFor(t, "the agent on the rewound conversation", func() bool {
		cur := h.agent(a.Id)
		return cur.SessionId == res.SessionId && cur.PendingReopen == nil && cur.State == v1.AgentState_AGENT_STATE_IDLE
	})
	var texts []string
	for _, e := range h.transcript(a.Id, -1, true).Entries {
		if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_USER {
			texts = append(texts, e.Text)
		}
	}
	if strings.Join(texts, ",") != "one" {
		t.Errorf("the rewound conversation's prompts: %v", texts)
	}
	// It takes a prompt again, into the copy.
	if err := h.promptWhenReady(a.Id, "three"); err != nil {
		t.Fatal(err)
	}
	h.waitTranscript(a.Id, "three")
}

func TestRewindRefusesAnAgentAtWork(t *testing.T) {
	h, a := chatDaemon(t, nil)
	if err := h.promptWhenReady(a.Id, "one"); err != nil {
		t.Fatal(err)
	}
	h.waitTranscript(a.Id, "one")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	one := h.promptEntry(a.Id, "one")
	h.typeInto(a, "/hold\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
	_, err := h.rewind(a.Id, one.Id, "rw-busy")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "interrupt it first") {
		t.Fatalf("rewinding a working agent: %v", err)
	}
}

func TestAddAttachmentKeepsAnImageInTheRunFolder(t *testing.T) {
	h, a := chatDaemon(t, nil)
	add := func(name string, data []byte, mid string) (string, error) {
		res, err := h.device.Mesh.AddAttachment(context.Background(), connect.NewRequest(&v1.AddAttachmentRequest{Meta: meta(mid), Agent: a.Id, Name: name, Data: data}))
		if err != nil {
			return "", err
		}
		return res.Msg.Path, nil
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	path, err := add("shot.PNG", png, "att-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, filepath.Join("runs", a.RunId, "attachments", a.Id)) || filepath.Ext(path) != ".png" {
		t.Errorf("path = %s", path)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, png) {
		t.Errorf("stored: %v", err)
	}
	if again, err := add("shot.PNG", png, "att-1"); err != nil || again != path {
		t.Errorf("a retry: %s, %v", again, err)
	}
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"notes.txt", []byte("hello")},
		{"fake.png", []byte("GIF89a not a png at all")},
		{"empty.png", nil},
		{"huge.png", append(png, make([]byte, 10<<20)...)},
	} {
		if _, err := add(c.name, c.data, "att-"+c.name); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid_argument", c.name, err)
		}
	}
}
