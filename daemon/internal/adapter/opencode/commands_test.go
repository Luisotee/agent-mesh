package opencode

import (
	"strings"
	"testing"
	"unicode/utf8"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func TestCustomCommandsAreReadFromTheConfigAfterLogLines(t *testing.T) {
	out := []byte("INFO  loading config\n{\n \"$schema\": \"x\",\n \"command\": {\n  \"deploy\": {\"template\": \"Ship $ARGUMENTS\\nthen tell me\", \"description\": \"Deploy it\"},\n  \"test\": {\"template\": \"Run the tests and report\\nmore\"}\n }\n}\n")
	got := map[string]string{}
	for _, c := range customCommands(out) {
		if c.Source != "custom" {
			t.Errorf("%s: source %q", c.Name, c.Source)
		}
		got[c.Name] = c.Desc
	}
	if len(got) != 2 || got["deploy"] != "Deploy it" || got["test"] != "Run the tests and report" {
		t.Errorf("%v", got)
	}
	if len(customCommands([]byte("not json"))) != 0 || len(customCommands([]byte(`{"command":{}}`))) != 0 {
		t.Error("commands from nothing")
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

func TestADescriptionCutFromALongTemplateIsCutOnACharacter(t *testing.T) {
	long := strings.Repeat("ção ", 60) // multi-byte text, longer than the cut
	out := []byte(`{"command":{"x":{"template":"` + long + `"}}}`)
	cs := customCommands(out)
	if len(cs) != 1 || !utf8.ValidString(cs[0].Desc) || !strings.HasSuffix(cs[0].Desc, "…") || len([]rune(cs[0].Desc)) != 121 {
		t.Errorf("%+v", cs)
	}
}

func TestTheConfigProbeOfAReadOnlyAgentLoadsNoProjectConfigAndCarriesNoIdentity(t *testing.T) {
	m := &adapterv1.ListCommandsRequest{ReadOnly: true, Env: map[string]string{"HOME": "/h", "AGENT_MESH_TOKEN": "x", "AGENT_MESH_AGENT": "a"}}
	env := commandsEnv(m)
	if env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || env["HOME"] != "/h" || env["AGENT_MESH_TOKEN"] != "" || env["AGENT_MESH_AGENT"] != "" {
		t.Errorf("%v", env)
	}
	m.ReadOnly = false
	if env := commandsEnv(m); env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "" {
		t.Errorf("a worker's own project config is its own: %v", env)
	}
}

func TestOpenCodesExitHasTheAliasesItIsTypedWith(t *testing.T) {
	for _, c := range builtinCommands {
		if c.Name == "exit" {
			if !c.Danger || len(c.Aliases) != 2 || c.Aliases[0] != "quit" || c.Aliases[1] != "q" {
				t.Errorf("%+v", c)
			}
			return
		}
	}
	t.Error("no exit")
}
