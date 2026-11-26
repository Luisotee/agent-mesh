// Package shellmatch inspects shell command lines for publishing commands (`git push`, `gh` writes) so
// read-only and planning agents can deny them in `PreToolUse` hooks.
package shellmatch

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ghReads are the gh commands a read-only or planning agent may run, by group and verb: reads of the remote
// and local inspection. `*` is any verb of the group, and a group with only `*` and no verb is the command
// itself (`gh status`). Everything else is a publish (a create, comment, review, merge, close, edit, delete, a
// workflow run, a secret set, an extension, an alias), so a gh command added later is denied until listed here.
// `git push` and gh api writes are matched apart (matchPublishTokens, matchGh).
var ghReads = map[string]map[string]bool{
	"pr":          {"list": true, "view": true, "diff": true, "checks": true, "status": true},
	"issue":       {"list": true, "view": true, "status": true},
	"release":     {"list": true, "view": true, "download": true},
	"repo":        {"view": true, "list": true, "clone": true, "gitignore": true, "license": true},
	"run":         {"list": true, "view": true, "watch": true, "download": true},
	"workflow":    {"list": true, "view": true},
	"gist":        {"list": true, "view": true, "clone": true},
	"label":       {"list": true},
	"cache":       {"list": true},
	"secret":      {"list": true},
	"variable":    {"list": true, "get": true},
	"ruleset":     {"list": true, "view": true, "check": true},
	"project":     {"list": true, "view": true, "field-list": true, "item-list": true},
	"org":         {"list": true},
	"codespace":   {"list": true, "view": true},
	"ssh-key":     {"list": true},
	"gpg-key":     {"list": true},
	"auth":        {"status": true},
	"config":      {"get": true, "list": true},
	"attestation": {"verify": true, "download": true, "inspect": true},
	"search":      {"*": true},
	"status":      {"*": true},
	"browse":      {"*": true},
	"help":        {"*": true},
	"version":     {"*": true},
	"completion":  {"*": true},
}

var segmentSplitRE = regexp.MustCompile(`(?:\|\||&&|[;\n|&])+`)

// maxShellDepth bounds how deep `sh -c`, `eval` and `env -S` strings are looked into.
const maxShellDepth = 4

// MatchPublishCommand names the publishing command a shell command line runs (`git push`, a gh write), or "".
// It reads the line the way a careful person would, not as a shell does: each segment between `;`, `&&`, `|`
// and `$(…)` is unwrapped (variable assignments, `env`, `sudo`, `nohup`, `timeout`, `xargs` and the like, `sh -c`
// and `eval` strings) and its command checked. It guards against an agent's honest mistake, not a determined one
// (a script it writes, a git alias): the role's prompt says what it may do.
func MatchPublishCommand(cmdline string) string { return matchPublishDepth(cmdline, 0) }

func matchPublishDepth(cmdline string, depth int) string {
	if depth > maxShellDepth {
		return ""
	}
	// Also inspect subshells $(...) and `...` (which run inside double quotes too) by turning them into segment
	// boundaries, and the (…) and { …; } groups outside quotes, so that `grep "(git push)"` stays a read.
	expanded := strings.NewReplacer("$(", ";", ")", ";", "`", ";").Replace(splitGroups(cmdline))
	for _, seg := range segmentSplitRE.Split(expanded, -1) {
		fields := shellWords(seg)
		if len(fields) == 0 {
			continue
		}
		if matched := matchPublishTokens(fields, depth); matched != "" {
			return matched
		}
	}
	return ""
}

// splitGroups turns the (, { and } that stand outside quotes into `;`.
func splitGroups(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' {
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '{' || c == '}':
			b[i] = ';'
		}
	}
	return string(b)
}

var assignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// wrapperValueFlags are, per command wrapper, the flags that take the next word as their value.
var wrapperValueFlags = map[string]map[string]bool{
	"command": {}, "builtin": {}, "nohup": {}, "time": {}, "stdbuf": {}, "setsid": {},
	"exec":    {"-a": true},
	"sudo":    {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true},
	"doas":    {"-u": true, "-C": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"xargs": {"-I": true, "-n": true, "-L": true, "-P": true, "-s": true, "-d": true, "-E": true, "-a": true,
		"--max-args": true, "--max-procs": true, "--delimiter": true, "--arg-file": true},
	"env": {"-u": true, "-C": true, "--unset": true, "--chdir": true},
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "busybox": true}

func matchPublishTokens(tokens []string, depth int) string {
	for len(tokens) > 0 {
		if assignmentRE.MatchString(tokens[0]) {
			tokens = tokens[1:]
			continue
		}
		name := filepath.Base(tokens[0])
		if name == "eval" {
			return matchPublishDepth(strings.Join(tokens[1:], " "), depth+1)
		}
		if shells[name] {
			args := tokens[1:]
			if name == "busybox" && len(args) > 0 && shells[args[0]] {
				args = args[1:]
			}
			for i := 0; i < len(args); i++ {
				t := args[i]
				switch {
				case t == "-o" || t == "+o" || t == "-O" || t == "+O" || t == "--rcfile" || t == "--init-file":
					i++
				case strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'c'):
					if i+1 < len(args) {
						return matchPublishDepth(args[i+1], depth+1)
					}
					return ""
				case strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+"):
				default:
					return "" // a script file: not looked into
				}
			}
			return ""
		}
		values, wrapper := wrapperValueFlags[name]
		if !wrapper {
			break
		}
		tokens = tokens[1:]
		for len(tokens) > 0 {
			t := tokens[0]
			if name == "env" && (t == "-S" || t == "--split-string") && len(tokens) > 1 {
				return matchPublishDepth(strings.Join(tokens[1:], " "), depth+1)
			}
			if name == "env" && assignmentRE.MatchString(t) {
				tokens = tokens[1:]
				continue
			}
			if t == "--" {
				tokens = tokens[1:]
				break
			}
			if !strings.HasPrefix(t, "-") || t == "-" && name != "env" {
				break
			}
			tokens = tokens[1:]
			if values[t] && len(tokens) > 0 {
				tokens = tokens[1:]
			}
		}
		if name == "timeout" && len(tokens) > 0 {
			tokens = tokens[1:] // its duration
		}
	}
	if len(tokens) < 2 {
		return ""
	}
	cmd := filepath.Base(tokens[0])
	args := tokens[1:]
	switch cmd {
	case "git":
		// Skip global git flags such as -C <dir>, -c <k=v>, --git-dir <d>, --git-dir=<d>, --no-pager.
		for i := 0; i < len(args); i++ {
			t := args[i]
			if !strings.HasPrefix(t, "-") {
				if t == "push" {
					return "git push"
				}
				return ""
			}
			if t == "-C" || t == "-c" || t == "--git-dir" || t == "--work-tree" || t == "--namespace" ||
				t == "--exec-path" || t == "--super-prefix" || t == "--config-env" {
				i++
			}
		}
	case "gh":
		return matchGh(args)
	}
	return ""
}

// ghValueFlags are gh flags that take the next word as their value, wherever they stand: cobra reads a
// subcommand's flags before its name too (`gh pr -R o/r merge 5`).
var ghValueFlags = map[string]bool{
	"-R": true, "--repo": true, "-b": true, "--body": true, "-F": true, "--body-file": true, "-t": true, "--title": true,
	"-B": true, "--base": true, "-H": true, "--head": true, "-l": true, "--label": true, "-a": true, "--assignee": true,
	"-m": true, "--milestone": true, "-r": true, "--reviewer": true, "-p": true, "--project": true, "-S": true,
	"--search": true, "-s": true, "--state": true, "-L": true, "--limit": true, "-A": true, "--author": true,
	"-q": true, "--jq": true, "-T": true, "--template": true, "--json": true, "-X": true, "--method": true,
	"-f": true, "--field": true, "--raw-field": true, "--input": true, "--hostname": true,
	"--header": true, "--cache": true, "--preview": true,
	"--workflow": true, "-w": true, "--job": true, "-j": true, "--branch": true, "--name": true, "--event": true, "--status": true,
	"--user": true, "--org": true, "--env": true,
}

func matchGh(args []string) string {
	var pos []string
	method, fields, mutation := "", false, false
	for i := 0; i < len(args); i++ {
		t := args[i]
		if !strings.HasPrefix(t, "-") || t == "-" {
			pos = append(pos, t)
			continue
		}
		var flag, value string
		if len(t) > 2 && !strings.HasPrefix(t, "--") && ghValueFlags[t[:2]] {
			flag, value = t[:2], t[2:] // an attached short flag: -XPOST, -fbody=x, -Fevent=APPROVE
		} else {
			var inline bool
			flag, value, inline = strings.Cut(t, "=")
			if !inline && ghValueFlags[flag] && i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		switch flag {
		case "-X", "--method":
			method = strings.ToUpper(value)
		case "-f", "-F", "--field", "--raw-field", "--input":
			fields = true
		}
		if strings.Contains(value, "mutation") {
			mutation = true
		}
	}
	if len(pos) == 0 {
		return ""
	}
	if pos[0] == "api" {
		graphql := len(pos) > 1 && pos[1] == "graphql"
		switch {
		case method != "" && method != "GET" && method != "HEAD":
			return "gh api " + method
		case graphql && mutation:
			return "gh api graphql mutation"
		case !graphql && method == "" && fields:
			return "gh api POST" // fields make gh api a POST
		}
		return ""
	}
	reads, known := ghReads[pos[0]]
	switch {
	case !known:
		return "gh " + pos[0]
	case len(pos) == 1 || reads["*"] || reads[pos[1]]:
		return "" // a read, or the group's own help
	}
	return "gh " + pos[0] + " " + pos[1]
}

func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			inWord = true
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				inWord = true
			}
		case ' ', '\t', '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}
