package shellmatch

import (
	"path/filepath"
	"regexp"
	"strings"
)

// patternKillers are the commands that signal every process whose name or command line matches: on a host
// several agents share, that is other sessions' processes too.
var patternKillers = map[string]bool{"pkill": true, "killall": true, "killall5": true}

// MatchPatternKill names the kill-by-pattern a shell command line runs, or "": `pkill`, `killall`, and a `kill`
// fed by a lookup of the same kind: `kill $(pgrep -f x)`, `pid=$(pgrep x); kill $pid`, `for p in $(pgrep x); do kill
// $p; done`, `pgrep x | xargs kill`, `pgrep x | while read p; do kill $p; done`, `ps … | grep … | xargs kill`.
// A kill of a PID the agent holds (`kill $!`, `kill "$(cat pidfile)"`, `kill -0 …`) and a lookup by parentage,
// session or group (`pgrep -P <pid>`, which walks one's own children) pass. It reads the line as
// MatchPublishCommand does, and guards against a mistake, not a determined agent.
func MatchPatternKill(cmdline string) string {
	switch got := matchLine(cmdline, 0, killFinal); got {
	case "kill":
		if capturedLookup(cmdline) || pipedKill(cmdline) {
			return "kill of a pgrep/pidof/ps result"
		}
	default:
		return got
	}
	return ""
}

func killFinal(tokens []string, piped bool) string {
	cmd := filepath.Base(tokens[0])
	switch {
	case patternKillers[cmd]:
		return cmd
	case cmd == "kill" && len(tokens) > 1 && (tokens[1] == "-0" || tokens[1] == "-l" || tokens[1] == "-L"):
		return "" // sends no signal
	case cmd == "kill":
		return "kill"
	}
	return ""
}

var (
	// a lookup by name or pattern whose output is captured, in a substitution (`$(…)`, backticks): its PIDs are
	// then in a variable, a loop or the kill's own arguments. The lookup's own arguments are group 2.
	capturedRE = regexp.MustCompile("(?:\\$\\(|`)[^)`]*?\\b(pgrep|pidof)\\b([^)`]*)")
	// a ps with a filter, in a substitution
	capturedPsRE = regexp.MustCompile("(?:\\$\\(|`)[^)`]*?\\bps\\b[^)`]*\\|\\s*(?:grep|awk)\\b")
	// a lookup piped on, to `xargs … kill` or a `while read` loop (the loop's kill is in the same line); group 2 is
	// the lookup's own arguments, up to its pipe
	pipedLookupRE = regexp.MustCompile("\\b(pgrep|pidof)\\b([^;&|)`\\n]*)\\|[^;&\\n]*?(?:\\bxargs\\b[^;&\\n]*\\bkill\\b|\\bwhile\\b)")
	pipedPsRE     = regexp.MustCompile("\\bps\\b[^;&|)`\\n]*\\|\\s*(?:grep|awk)\\b[^;&\\n]*\\|[^;&\\n]*\\bxargs\\b[^;&\\n]*\\bkill\\b")
	// pgrep's own flags that bound a lookup to what the caller holds: parent, session, process group (not -G, a
	// group id, which other agents share); a value may be attached (`-P1234`, `--parent=1234`)
	scopeFlagRE = regexp.MustCompile(`(?:^|\s)(?:-[a-zA-Z]*[Psg](?:\s|\d|$)|--(?:parent|session|pgroup)(?:\s|=|$))`)
)

func unscoped(name, args string) bool {
	return name == "pidof" || !scopeFlagRE.MatchString(" "+strings.TrimSpace(args))
}

// capturedLookup says whether the line captures PIDs looked up by name or pattern.
func capturedLookup(line string) bool {
	if capturedPsRE.MatchString(line) {
		return true
	}
	for _, m := range capturedRE.FindAllStringSubmatch(line, -1) {
		if unscoped(m[1], m[2]) {
			return true
		}
	}
	return false
}

// pipedKill says whether a lookup by name or pattern is piped into `xargs kill` or a loop that kills.
func pipedKill(line string) bool {
	if pipedPsRE.MatchString(line) {
		return true
	}
	for _, m := range pipedLookupRE.FindAllStringSubmatch(line, -1) {
		if unscoped(m[1], m[2]) {
			return true
		}
	}
	return false
}

// KillAdvice is what a refused pattern kill tells the agent.
func KillAdvice(what string) string {
	return "refused (" + what + "): it signals every process on this machine that matches, other agents' sessions " +
		"included. Kill only a process you started and recorded: keep its PID when you start it ($!, a pidfile, or " +
		"setsid and kill its process group), then `kill <pid>`; to find your own children use `pgrep -P <pid>`."
}
