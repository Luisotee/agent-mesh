// A guard the daemon loads into the OpenCode of every agent (`OPENCODE_CONFIG_CONTENT`'s `plugin`; adapter/opencode
// `PlanLaunch`): no kill by pattern. `pkill`, `killall` and a `kill` fed by `pgrep`/`pidof` signal every process on the
// host that matches, other agents' sessions included, so a bash command that runs one is refused with what to do
// instead. The other adapters check the same in Go (`internal/shellmatch`, `MatchPatternKill`); this is that check
// by regular expression, which reads a command line less exactly. It guards against a mistake, not a determined agent.

// what may stand between a command's start and the command: a shell keyword, an assignment, or a wrapper with its
// options (`sudo -u x`, `timeout 5`, `xargs -n1`)
const lead =
  "(?:(?:if|then|else|elif|do|while|until|!)\\s+|[A-Za-z_]\\w*=\\S*\\s+|" +
  "(?:sudo|doas|env|nohup|setsid|timeout|time|exec|command|nice|ionice|stdbuf|xargs)\\s+(?:-\\S+\\s+|\\d+[smhd]?\\s+|[A-Za-z_]\\w*=\\S*\\s+|\\{\\}\\s+)*)*"
const start = "(?:^|[;&|(`{\\n]|\\$\\(|\\s-c\\s+[\"'])\\s*"
const killers = new RegExp(start + lead + "(?:\\S*/)?(?:pkill|killall5?)(?=\\s|$|[;&|)`])")
const hasKill = new RegExp("(?:" + start + ")" + lead + "(?:\\S*/)?kill(?!\\s+-[0lL]\\b)(?=\\s|$|[;&|)`])")

// pgrep's own flags that keep a lookup to what the caller holds: parent, session, process group (not -G, a group id);
// a value may be attached (`-P1234`, `--parent=1234`)
const scoped = /(?:^|\s)(?:-[a-zA-Z]*[Psg](?:\s|\d|$)|--(?:parent|session|pgroup)(?:\s|=|$))/
const unscopedIn = (re, line) => {
  for (const m of line.matchAll(re)) if (m[1] === "pidof" || !scoped.test(" " + m[2].trim())) return true
  return false
}
const captured = /(?:\$\(|`)[^)`]*?\b(pgrep|pidof)\b([^)`]*)/g
const capturedPs = /(?:\$\(|`)[^)`]*?\bps\b[^)`]*\|\s*(?:grep|awk)\b/
const pipedLookup = /\b(pgrep|pidof)\b([^;&|)`\n]*)\|[^;&\n]*?(?:\bxargs\b[^;&\n]*\bkill\b|\bwhile\b)/g
const pipedPs = /\bps\b[^;&|)`\n]*\|\s*(?:grep|awk)\b[^;&\n]*\|[^;&\n]*\bxargs\b[^;&\n]*\bkill\b/

const killsByPattern = (command) =>
  killers.test(command) ||
  (hasKill.test(command) &&
    (capturedPs.test(command) || pipedPs.test(command) || unscopedIn(captured, command) || unscopedIn(pipedLookup, command)))

export const KillGuard = async () => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash") return
    const command = String(output.args?.command ?? "")
    if (killsByPattern(command)) {
      throw new Error(
        "agent-mesh: refused, a kill by pattern (pkill, killall, kill of a pgrep/pidof result) signals every process on " +
          "this machine that matches, other agents' sessions included. Kill only a process you started and recorded: keep " +
          "its PID when you start it ($!, a pidfile, or setsid and kill its process group), then `kill <pid>`; to find " +
          "your own children use `pgrep -P <pid>`.",
      )
    }
  },
})
