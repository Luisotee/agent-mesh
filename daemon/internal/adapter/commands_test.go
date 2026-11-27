package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillFrontMatterReadsAFoldedAndAPlainDescription(t *testing.T) {
	name, desc := SkillFrontMatter("---\nname: check-plan\ndescription: >-\n  Checks a plan you wrote\n  against the code.\n\nmetadata: x\n---\n# body\ndescription: not this\n")
	if name != "check-plan" || desc != "Checks a plan you wrote against the code." {
		t.Errorf("folded: %q %q", name, desc)
	}
	name, desc = SkillFrontMatter("---\nname: \"gdocs\"\ndescription: Read, create: and edit docs\n---\n")
	if name != "gdocs" || desc != "Read, create: and edit docs" {
		t.Errorf("plain: %q %q", name, desc)
	}
	if n, d := SkillFrontMatter("# no front matter\nname: x\n"); n != "" || d != "" {
		t.Errorf("no front matter: %q %q", n, d)
	}
}

func TestSkillCmdsReadsEachFolderAndPrefixesAPluginsSkills(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"a": "---\nname: alpha\ndescription: first\n---\n", "b": "---\ndescription: second\n---\n"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755) // no SKILL.md: skipped
	cs := SkillCmds(dir, "plug", "plugin")
	if len(cs) != 2 || cs[0].Name != "plug:alpha" || cs[0].Desc != "first" || cs[1].Name != "plug:b" || cs[1].Source != "plugin" {
		t.Errorf("%+v", cs)
	}
	if SkillCmds(filepath.Join(dir, "missing"), "", "skill") != nil {
		t.Error("a missing folder gave skills")
	}
}

func TestMergeCommandsKeepsTheFirstOfAName(t *testing.T) {
	first := Commands([]Cmd{{Name: "b", Desc: "first"}, {Name: "a"}})
	second := Commands([]Cmd{{Name: "b", Desc: "second"}, {Name: "c", Source: "skill"}})
	out := MergeCommands(first, second)
	if len(out) != 3 || out[0].Name != "a" || out[1].Description != "first" || out[2].Source != "skill" || out[0].Source != "builtin" {
		t.Errorf("%v", out)
	}
}

func TestMergeCommandsDoesNotLetALaterNameShadowAnEarlierAlias(t *testing.T) {
	built := Commands([]Cmd{{Name: "exit", Aliases: []string{"quit"}, Danger: true}})
	skills := Commands([]Cmd{{Name: "quit", Source: "skill"}, {Name: "other", Source: "skill"}})
	out := MergeCommands(built, skills)
	if len(out) != 2 || out[0].Name != "exit" || out[1].Name != "other" {
		t.Errorf("%v", out)
	}
}
