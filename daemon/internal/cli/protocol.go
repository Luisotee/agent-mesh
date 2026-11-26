package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
)

// `mesh protocol [name]` prints one part of the protocol an agent reads (ORCHESTRATE.md or PROTOCOL.md), so an
// agent loads only the section a step needs (docs/design.md §15): every heading of those files by its slug, up to
// the next heading, `protocol` (all of PROTOCOL.md), `root-card` and `root.<n>` (the Root section's numbered
// step n). The files are in $AGENT_MESH_SHARE/protocol, else in `share/protocol` beside the binary.

var (
	nonSlug    = regexp.MustCompile(`[^a-z0-9]+`)
	headingRE  = regexp.MustCompile(`^#{2,3} `)
	stepStart  = regexp.MustCompile(`(?m)^(?:\d+\. )`)
	stepNumber = regexp.MustCompile(`^(\d+)\. `)
)

func slug(heading string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(heading), "-"), "-")
}

// protocolDir is where the protocol files are: $AGENT_MESH_SHARE/protocol, else share/protocol beside the
// binary or one level up (an installed or a built tree).
func protocolDir(env Env) string {
	if s := env.Getenv(client.ShareEnv); s != "" {
		return filepath.Join(s, "protocol")
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, rel := range []string{"share/protocol", "../share/protocol", "../../share/protocol"} {
			if st, err := os.Stat(filepath.Join(dir, rel, "ORCHESTRATE.md")); err == nil && !st.IsDir() {
				return filepath.Join(dir, rel)
			}
		}
	}
	return ""
}

// Sections splits the protocol into its readable parts, in order of appearance.
func Sections(dir string) (names []string, parts map[string]string) {
	parts = map[string]string{}
	add := func(name, text string) {
		if _, dup := parts[name]; !dup {
			names = append(names, name)
		}
		parts[name] = text
	}
	for _, file := range []string{"ORCHESTRATE.md", "PROTOCOL.md"} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		fence, start, name := false, 0, ""
		for i, line := range append(append([]string{}, lines...), "## ") {
			if strings.HasPrefix(line, "```") {
				fence = !fence
			}
			if !fence && headingRE.MatchString(line) {
				if name != "" {
					if _, dup := parts[name]; !dup {
						add(name, strings.TrimRight(strings.Join(lines[start:i], "\n"), " \t\r\n"))
					}
				}
				start, name = i, slug(strings.TrimLeft(line, "# "))
			}
		}
	}
	// The Root section's numbered steps.
	if root, ok := parts["root"]; ok {
		body := root
		if i := strings.Index(body, "\n"); i >= 0 {
			body = body[i+1:]
		}
		var idx [][]int
		for _, loc := range stepStart.FindAllStringIndex(body, -1) {
			// Steps run 1, 2, 3, …: a numbered line inside a step (a nested list) is not the next one.
			if m := stepNumber.FindStringSubmatch(body[loc[0]:]); m != nil && m[1] == fmt.Sprint(len(idx)+1) {
				idx = append(idx, loc)
			}
		}
		for k, loc := range idx {
			end := len(body)
			if k+1 < len(idx) {
				end = idx[k+1][0]
			}
			step := body[loc[0]:end]
			if m := stepNumber.FindStringSubmatch(step); m != nil {
				add("root."+m[1], fmt.Sprintf("## Root, step %s\n\n%s", m[1], strings.TrimRight(step, " \t\r\n")))
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ROOT_CARD.md")); err == nil {
		add("root-card", strings.TrimRight(string(b), " \t\r\n"))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "PROTOCOL.md")); err == nil {
		add("protocol", strings.TrimRight(string(b), " \t\r\n")) // reserved: all of PROTOCOL.md
	}
	return names, parts
}

// Protocol is `mesh protocol [name]`.
func Protocol(env Env, args []string) int {
	fs := flag.NewFlagSet("mesh protocol", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: mesh protocol [name]\nprints one part of ORCHESTRATE.md or PROTOCOL.md; without a name, lists the parts")
	}
	if code, done := parseHand(fs, args, env); done {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(env.Stderr, "mesh protocol: takes one name")
		return ExitUsage
	}
	dir := protocolDir(env)
	if dir == "" {
		fmt.Fprintln(env.Stderr, "mesh protocol: can't find the protocol files: set $"+client.ShareEnv)
		return ExitError
	}
	names, parts := Sections(dir)
	if fs.NArg() == 0 {
		fmt.Fprintln(env.Stdout, "mesh protocol <name> prints one part of ORCHESTRATE.md or PROTOCOL.md (about 4 characters a token):")
		for _, n := range names {
			fmt.Fprintf(env.Stdout, "  %-26s %6d chars\n", n, len(parts[n]))
		}
		return ExitOK
	}
	name := strings.ToLower(fs.Arg(0))
	name = strings.TrimPrefix(strings.TrimPrefix(name, "orchestrate."), "protocol.")
	text, ok := parts[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "mesh protocol: no protocol part named %s; the names are: %s\n", fs.Arg(0), strings.Join(names, ", "))
		return ExitError
	}
	fmt.Fprintln(env.Stdout, text)
	return ExitOK
}
