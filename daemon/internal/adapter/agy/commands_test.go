package agy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestListCommandsHasTheBuiltInsAndTheSkillsAgyReadsFromDisk(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, ".gemini", "config", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/delegate/SKILL.md", "---\nname: delegate\ndescription: hands work to agents\n---\n")
	write("plugins/gdocs/skills/gdocs/SKILL.md", "---\nname: gdocs\ndescription: Google Docs\n---\n")
	res, err := New().ListCommands(context.Background(), connect.NewRequest(&adapterv1.ListCommandsRequest{Env: map[string]string{"HOME": home}}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*v1.AgentCommand{}
	for _, c := range res.Msg.Commands {
		if got[c.Name] != nil {
			t.Errorf("%s listed twice", c.Name)
		}
		got[c.Name] = c
	}
	if c := got["delegate"]; c == nil || c.Source != "skill" || c.Description != "hands work to agents" {
		t.Errorf("/delegate: %v", c)
	}
	if c := got["gdocs:gdocs"]; c == nil || c.Source != "plugin" {
		t.Errorf("/gdocs:gdocs: %v", c)
	}
	if c := got["exit"]; c == nil || !c.Danger || c.Aliases[0] != "quit" {
		t.Errorf("/exit: %v", c)
	}
	if c := got["resume"]; c == nil || c.Dialog != v1.CommandDialog_COMMAND_DIALOG_ALWAYS {
		t.Errorf("/resume: %v", c)
	}
	// No HOME: the built-ins only.
	res, _ = New().ListCommands(context.Background(), connect.NewRequest(&adapterv1.ListCommandsRequest{}))
	if len(res.Msg.Commands) != len(builtinCommands) {
		t.Errorf("without a home: %d commands, want %d", len(res.Msg.Commands), len(builtinCommands))
	}
}

func TestTheBuiltInTableHasNoRepeatedNameOrAlias(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range builtinCommands {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if n == "" || seen[n] {
				t.Errorf("empty or repeated %q", n)
			}
			seen[n] = true
		}
		if c.Desc == "" {
			t.Errorf("%s has no description", c.Name)
		}
	}
}
