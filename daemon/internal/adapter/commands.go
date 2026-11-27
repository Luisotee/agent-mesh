package adapter

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Cmd describes one command of a CLI for the composer's `/` completion (`ListAgentCommands`): what the adapters'
// built-in tables are made of.
type Cmd struct {
	Name    string
	Aliases []string
	Desc    string
	Hint    string
	// Source is `builtin` when empty.
	Source string
	Dialog v1.CommandDialog
	// Danger: typing it ends the CLI's process or signs it out.
	Danger bool
}

// Dialog kinds, short for the tables.
const (
	DialogNo     = v1.CommandDialog_COMMAND_DIALOG_UNSPECIFIED
	DialogBare   = v1.CommandDialog_COMMAND_DIALOG_BARE
	DialogAlways = v1.CommandDialog_COMMAND_DIALOG_ALWAYS
)

// Commands turns a table into the API's rows.
func Commands(cs []Cmd) []*v1.AgentCommand {
	out := make([]*v1.AgentCommand, 0, len(cs))
	for _, c := range cs {
		src := c.Source
		if src == "" {
			src = "builtin"
		}
		out = append(out, &v1.AgentCommand{Name: c.Name, Aliases: c.Aliases, Description: c.Desc, ArgumentHint: c.Hint,
			Source: src, Dialog: c.Dialog, Danger: c.Danger})
	}
	return out
}

// MergeCommands joins command lists, the first list's entry winning for a name (or alias) the others repeat, and
// sorts them by name.
func MergeCommands(lists ...[]*v1.AgentCommand) []*v1.AgentCommand {
	seen := map[string]bool{}
	var out []*v1.AgentCommand
	for _, l := range lists {
		for _, c := range l {
			if c.Name == "" || seen[c.Name] {
				continue
			}
			seen[c.Name] = true
			for _, al := range c.Aliases {
				seen[al] = true // a later skill named like this one's alias doesn't shadow it
			}
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SkillCmds reads the skills under dir (`<dir>/<skill>/SKILL.md`, a link to a folder included) as commands:
// the front matter's `name` (else the folder's) and `description`. prefix, when set, goes before the name as
// `prefix:name` (a plugin's skills). A folder with no readable SKILL.md is skipped.
func SkillCmds(dir, prefix, source string) []Cmd {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Cmd
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		name, desc := SkillFrontMatter(string(b))
		if name == "" {
			name = e.Name()
		}
		if prefix != "" {
			name = prefix + ":" + name
		}
		out = append(out, Cmd{Name: name, Desc: desc, Source: source})
	}
	return out
}

// SkillFrontMatter reads `name` and `description` from a SKILL.md's front matter: a value on its line or folded
// or literal under `>`/`|` (indented lines joined by spaces).
func SkillFrontMatter(text string) (name, desc string) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "---" {
			break
		}
		key, val, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, " ") {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "name":
			name = strings.Trim(val, `"'`)
		case "description":
			if strings.HasPrefix(val, ">") || strings.HasPrefix(val, "|") {
				var parts []string
				for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.TrimSpace(lines[i+1]) == "") && strings.TrimSpace(lines[i+1]) != "---" {
					i++
					if t := strings.TrimSpace(lines[i]); t != "" {
						parts = append(parts, t)
					}
				}
				val = strings.Join(parts, " ")
			}
			desc = strings.Trim(val, `"'`)
		}
	}
	return name, desc
}
