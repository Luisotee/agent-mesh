// A guard the daemon loads into the OpenCode of an asker, a reviewer and a planner (`OPENCODE_CONFIG_CONTENT`'s `plugin`;
// adapter/opencode `PlanLaunch`). Their bash is fenced by permission patterns (`readOnlyBash`), but OpenCode 1.19.6 asks
// for nothing when a command line has no command in it, so a bare redirect (`> file`, `>> file`, `2> file`) runs with no
// rule seen and truncates or creates a file. This hook runs before every tool call and refuses a bash command that holds
// a `>`: the patterns already deny a `>` in any command they see, so no read-only command is lost.
export const ReadOnlyGuard = async () => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash") return
    const command = String(output.args?.command ?? "")
    if (command.includes(">")) {
      throw new Error("agent-mesh: a read-only agent's bash takes no redirect (a `>` in the command), so it can't write a file")
    }
  },
})
