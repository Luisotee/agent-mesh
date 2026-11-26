package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
)

// The daemon's protocol and skills (daemon/share) tell agents to run `mesh` commands: every command they name,
// with every flag written after it, must exist in this CLI. What the CLI lacks is allowed only in a sentence
// that says it isn't there yet.
func TestTheSharedProtocolNamesOnlyCommandsAndFlagsTheCLIHas(t *testing.T) {
	reg, err := registry.Mesh()
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]*flag.FlagSet{
		"dir":     flag.NewFlagSet("dir", flag.ContinueOnError),
		"whoami":  flag.NewFlagSet("whoami", flag.ContinueOnError),
		"hook":    flag.NewFlagSet("hook", flag.ContinueOnError),
		"version": flag.NewFlagSet("version", flag.ContinueOnError),
	}
	flags["help"] = flag.NewFlagSet("help", flag.ContinueOnError)
	for _, name := range []string{"dir", "whoami", "hook", "version"} {
		flags[name].String("state", "", "")
	}
	flags["dir"].String("run", "", "")
	for _, a := range reg.Actions() {
		if a.CLI != "" {
			flags[a.CLI] = newCommand(a, Env{}, "").flags
		}
	}

	files, _ := filepath.Glob("../../share/protocol/*.md")
	skills, _ := filepath.Glob("../../share/skills/*/SKILL.md")
	files = append(files, skills...)
	if len(files) < 6 {
		t.Fatalf("found only %v", files)
	}
	span := regexp.MustCompile("`([^`\n]+)`")
	cmdRE := regexp.MustCompile(`\bmesh ([a-z][a-z-]*)\b`)
	flagRE := regexp.MustCompile(`(?:^|\s)--([a-z][a-z-]*)`)
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		var segments []struct{ text, line string }
		fenced := false
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenced = !fenced
				continue
			}
			if fenced {
				joined := strings.TrimSpace(line)
				for strings.HasSuffix(joined, `\`) && i+1 < len(lines) {
					i++
					joined = strings.TrimSuffix(joined, `\`) + " " + strings.TrimSpace(lines[i])
				}
				segments = append(segments, struct{ text, line string }{joined, joined})
				continue
			}
			for _, m := range span.FindAllStringSubmatch(line, -1) {
				segments = append(segments, struct{ text, line string }{m[1], line})
			}
		}
		for _, seg := range segments {
			loc := cmdRE.FindStringSubmatchIndex(seg.text)
			if loc == nil || strings.HasPrefix(seg.text, "[mesh ") { // `[mesh alert …]` is a message's tag
				continue
			}
			name := seg.text[loc[2]:loc[3]]
			fs, ok := flags[name]
			if !ok {
				if strings.Contains(seg.line, " yet") || strings.Contains(seg.line, "not on the daemon") {
					continue
				}
				t.Errorf("%s: `%s` is not a command of this CLI", file, seg.text)
				continue
			}
			checked++
			for _, m := range flagRE.FindAllStringSubmatch(seg.text[loc[1]:], -1) {
				if fs.Lookup(m[1]) == nil && m[1] != "help" {
					t.Errorf("%s: `%s` has no --%s", file, seg.text, m[1])
				}
			}
		}
	}
	if checked < 40 {
		t.Errorf("only %d commands checked: the extraction is broken", checked)
	}
}

// The share folder is a Claude plugin of its own: a manifest, and a skill per folder whose front matter names
// it and says when to use it.
func TestTheSharedSkillsAreAPlugin(t *testing.T) {
	raw, err := os.ReadFile("../../share/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Name, Description string }
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Name != "agent-mesh-daemon" || manifest.Description == "" {
		t.Errorf("plugin.json: %+v, %v", manifest, err)
	}
	skills, _ := filepath.Glob("../../share/skills/*/SKILL.md")
	if len(skills) < 4 {
		t.Fatalf("skills: %v", skills)
	}
	for _, file := range skills {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(filepath.Dir(file))
		head := string(text)
		if !strings.HasPrefix(head, "---\nname: "+name+"\ndescription:") || strings.Count(head, "\n---\n") < 1 {
			t.Errorf("%s: front matter doesn't name %q and describe it", file, name)
		}
	}
}
