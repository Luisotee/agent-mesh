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
		// Hand-written, with no flags: its one argument names a part.
		"protocol": flag.NewFlagSet("protocol", flag.ContinueOnError),
	}
	flags["help"] = flag.NewFlagSet("help", flag.ContinueOnError)
	for _, name := range []string{"dir", "whoami", "hook", "version"} {
		flags[name].String("state", "", "")
	}
	flags["dir"].String("run", "", "")
	positional := map[string]bool{}     // commands whose one bare argument is a field (`to` or `agent`)
	positionalList := map[string]bool{} // commands whose bare arguments fill a repeated list (`names`)
	for _, a := range reg.Actions() {
		if a.CLI != "" {
			flags[a.CLI] = newCommand(a, Env{}, "").flags
			pf := positionalField(a.Method.Input())
			positional[a.CLI] = pf != nil
			positionalList[a.CLI] = pf != nil && pf.IsList()
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
	subRE := regexp.MustCompile(`\$\(([^()]*)\)`)
	flagRE := regexp.MustCompile(`(?:^|\s)--([a-z][a-z-]*)`)
	placeholderRE := regexp.MustCompile(`<[^<>]*>`) // <root name> is one argument
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
		// A `$(mesh …)` inside a command is checked as a segment of its own and cut from the outer one.
		for _, seg := range segments {
			for _, m := range subRE.FindAllStringSubmatch(seg.text, -1) {
				segments = append(segments, struct{ text, line string }{m[1], seg.line})
			}
		}
		for _, seg := range segments {
			seg.text = subRE.ReplaceAllString(seg.text, "")
			if strings.HasPrefix(seg.text, "[mesh ") { // `[mesh alert …]` is a message's tag
				continue
			}
			// Every `mesh <cmd>` of the segment is checked; a command's flags run to the next one.
			locs := cmdRE.FindAllStringSubmatchIndex(seg.text, -1)
			for i, loc := range locs {
				name := seg.text[loc[2]:loc[3]]
				argStart := loc[1]
				if fields := strings.Fields(seg.text[argStart:]); len(fields) > 0 && !strings.HasPrefix(fields[0], "-") {
					sub := name + "-" + fields[0]
					if _, ok := flags[sub]; ok {
						name = sub
						argStart += strings.Index(seg.text[argStart:], fields[0]) + len(fields[0])
					}
				}
				fs, ok := flags[name]
				if !ok {
					if strings.Contains(seg.line, " yet") || strings.Contains(seg.line, "not on the daemon") {
						continue
					}
					t.Errorf("%s: `%s` is not a command of this CLI", file, seg.text)
					continue
				}
				checked++
				end := len(seg.text)
				if i+1 < len(locs) {
					end = locs[i+1][0]
				}
				// The words written before the command's first flag are its bare arguments: only a command with an
				// `agent` or `to` field takes one (and repeated `names` takes several).
				if _, generated := positional[name]; generated && argStart <= end {
					rest := placeholderRE.ReplaceAllString(seg.text[argStart:end], "ARG")
					rest = strings.NewReplacer("…", " ", "...", " ").Replace(rest)
					if f := strings.Index(rest, "--"); f >= 0 {
						rest = rest[:f]
					}
					if f := strings.IndexAny(rest, "|>;&#[("); f >= 0 {
						rest = rest[:f]
					}
					words := strings.Fields(rest)
					if (len(words) > 0 && !positional[name]) || (len(words) > 1 && !positionalList[name]) {
						t.Errorf("%s: `%s` gives %q as arguments, but mesh %s takes %s", file, seg.text, words, name,
							map[bool]string{true: "one (its agent)", false: "none (every field is a flag)"}[positional[name]])
					}
				}
				if argStart <= end {
					for _, m := range flagRE.FindAllStringSubmatch(seg.text[argStart:end], -1) {
						if fs.Lookup(m[1]) == nil && m[1] != "help" {
							t.Errorf("%s: `%s` has no --%s", file, seg.text, m[1])
						}
					}
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
