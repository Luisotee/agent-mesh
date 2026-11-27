package opencode

import (
	"strings"
	"testing"
	"unicode/utf8"
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
