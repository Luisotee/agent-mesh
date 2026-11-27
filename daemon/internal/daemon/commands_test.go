package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestListAgentCommandsAnswersWithTheCLIsOwnListAndItsTerminalOnes(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_ARGV_LOG": argvLog})
	res, err := h.device.Mesh.ListAgentCommands(context.Background(), connect.NewRequest(&v1.ListAgentCommandsRequest{Agent: a.Name}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.Supported {
		t.Fatal("a Claude agent's commands are not supported")
	}
	got := map[string]*v1.AgentCommand{}
	for _, c := range res.Msg.Commands {
		got[c.Name] = c
	}
	// From the CLI's own answer (the fake's), a dialog the table knows, and one only the terminal has.
	if c := got["deploy"]; c == nil || c.Source != "project" || c.ArgumentHint != "[env]" || c.Description != "Ship it" {
		t.Errorf("/deploy: %v", c)
	}
	if c := got["model"]; c == nil || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_BARE {
		t.Errorf("/model: %v", c)
	}
	if c := got["resume"]; c == nil || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_ALWAYS {
		t.Errorf("/resume: %v", c)
	}
	// The probe ran in the agent's worktree, as nobody's agent, and left no trace.
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	var probe string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "stream-json") {
			probe = l
		}
	}
	for _, want := range []string{"--no-session-persistence", "disableAllHooks", "--strict-mcp-config"} {
		if !strings.Contains(probe, want) {
			t.Errorf("the probe's argv lacks %s: %s", want, probe)
		}
	}
	if probe == "" {
		t.Fatal("no probe was started")
	}
}

func TestListAgentCommandsOfAnUnknownAgentIsAnError(t *testing.T) {
	h, _ := chatDaemon(t, nil)
	_, err := h.device.Mesh.ListAgentCommands(context.Background(), connect.NewRequest(&v1.ListAgentCommandsRequest{Agent: "nobody"}))
	if connect.CodeOf(err) != connect.CodeNotFound && connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an unknown agent: %v", err)
	}
}

func TestACommandTheUserTypedShowsItsOutputInTheChat(t *testing.T) {
	h, a := chatDaemon(t, nil)
	if err := h.promptWhenReady(a.Id, "/context"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command and its output in the transcript", func() bool {
		return kindsOf(h.transcript(a.Id, -1, true).Entries) == "USER COMMAND_OUTPUT"
	})
	es := h.transcript(a.Id, -1, true).Entries
	if es[0].Text != "/context" || es[1].Text != "Context 12k/200k tokens" {
		t.Errorf("the command and its output: %q, %q", es[0].Text, es[1].Text)
	}
}
