// A guard the daemon loads into the OpenCode of every agent (`OPENCODE_CONFIG_CONTENT`'s `plugin`; adapter/opencode
// `PlanLaunch`): no kill by pattern. `pkill`, `killall` and a `kill` fed by `pgrep`/`pidof` signal every process on the
// host that matches, other agents' sessions included, so a bash command that runs one is refused with what to do
// instead. The other adapters check the same in Go (`internal/shellmatch`, `MatchPatternKill`); this is that check
// by regular expression, which reads a command line less exactly. It guards against a mistake, not a determined agent.
const wrappers = "(?:sudo|doas|env|nohup|setsid|timeout|time|exec|command|nice|ionice|stdbuf)"
const killers = new RegExp(
  "(?:^|[;&|(`{\\n]|\\$\\(|\\s-c\\s+[\"']|\\b" + wrappers + "\\b\\s+(?:\\S+\\s+)*?)\\s*(?:\\S*/)?(?:pkill|killall5?)(?=\\s|$|[;&|)`])",
)
// a lookup by name or pattern: not pgrep's own parent, session and group flags, which keep it to what one holds
const lookup = "(?:pidof\\b|pgrep\\b(?![^;&|)`\\n]*(?:\\s-[a-zA-Z]*[PsgG]\\b|--(?:parent|session|pgroup)\\b)))"
const substituted = new RegExp("\\bkill\\b[^;&|\\n]*?(?:\\$\\(|`)\\s*" + lookup)
const piped = new RegExp(lookup + "[^;&\\n]*\\|\\s*(?:\\S+\\s+)*?xargs\\b[^;&|\\n]*\\bkill\\b")
const psGrep = /\bps\b[^;&\n]*\|\s*grep\b[^;&\n]*\|[^;&\n]*xargs\b[^;&|\n]*\bkill\b/

export const KillGuard = async () => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash") return
    const command = String(output.args?.command ?? "")
    if (killers.test(command) || substituted.test(command) || piped.test(command) || psGrep.test(command)) {
      throw new Error(
        "agent-mesh: refused, a kill by pattern (pkill, killall, kill of a pgrep/pidof result) signals every process on " +
          "this machine that matches, other agents' sessions included. Kill only a process you started and recorded: keep " +
          "its PID when you start it ($!, a pidfile, or setsid and kill its process group), then `kill <pid>`; to find " +
          "your own children use `pgrep -P <pid>`.",
      )
    }
  },
})
