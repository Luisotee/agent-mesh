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
// fed by a lookup of the same kind (`kill $(pgrep -f x)`, `pgrep x | xargs kill`, `ps … | grep … | xargs kill`).
// A kill of a PID the agent holds (`kill $!`, `kill "$(cat pidfile)"`) and a lookup by parentage, session or
// group (`pgrep -P <pid>`, which walks one's own children) pass. It reads the line as MatchPublishCommand does,
// and guards against a mistake, not a determined agent.
func MatchPatternKill(cmdline string) string {
	switch got := matchLine(cmdline, 0, killFinal); got {
	case "kill":
		if killSubstRE.MatchString(cmdline) && unscopedLookup(cmdline) {
			return "kill of a pgrep/pidof/ps result"
		}
	case "xargs kill":
		if unscopedLookup(cmdline) {
			return "xargs kill of a pgrep/pidof/ps result"
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
	case cmd == "kill" && piped:
		return "xargs kill"
	case cmd == "kill":
		return "kill"
	}
	return ""
}

var (
	// a kill whose arguments hold a command substitution: `kill -9 $(…`, "kill `…"
	killSubstRE = regexp.MustCompile("\\bkill\\b[^;&|\\n]*?(?:\\$\\(|`)")
	// a lookup by name or pattern, up to the end of its command
	lookupRE = regexp.MustCompile(`\b(pgrep|pidof)\b([^;&|)` + "`" + `\n]*)`)
	psGrepRE = regexp.MustCompile(`\bps\b[^;&\n]*\|\s*grep\b`)
	// pgrep's own flags that bound a lookup to what the caller holds: parent, session, process group
	scopeFlagRE = regexp.MustCompile(`(?:^|\s)(?:-[a-zA-Z]*[PsgG]\b|--(?:parent|session|pgroup)\b)`)
)

// unscopedLookup says whether the line looks a PID up by name or pattern.
func unscopedLookup(line string) bool {
	if psGrepRE.MatchString(line) {
		return true
	}
	for _, m := range lookupRE.FindAllStringSubmatch(line, -1) {
		if m[1] == "pidof" || !scopeFlagRE.MatchString(" "+strings.TrimSpace(m[2])) {
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
