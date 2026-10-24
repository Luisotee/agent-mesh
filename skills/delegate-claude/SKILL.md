---
name: delegate-claude
description: >-
  agent-mesh, mode `claude`: one Claude agent per repo researches and plans on Opus, sends you its plan for review, then executes it itself on Opus at medium effort
  and opens a PR, all in visible herdr panes with two-way messaging. Use ONLY when the user explicitly types /delegate-claude; never on
  your own initiative.
disable-model-invocation: true
argument-hint: <what to build or fix, in which repos>
---

# /delegate-claude

Run an **agent-mesh** in mode **`claude`** for the user's request: the text after the command, which is also `$ARGUMENTS`.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Keep doing your own repo's part yourself; spawn workers only for the other repos (and for yours only if the user asks). Follow the "Root" section with `mesh init --mode claude`, and spawn every worker with the `claude` row of the mode table (no `--model`: it plans on Opus at high effort and drops to medium when you `mesh approve` its plan), unless the request explicitly names another model or agent. If the request is a follow-up to work an earlier worker did ("send the review to the agent that changed it", "ask that agent to make it Y"), resume that worker instead (the "Follow-ups" section): it keeps its own CLI and model, and the mode only sets defaults for new workers.
3. Tell the user in one line what you started: run id, repos, and the worker models. Then review each worker's plan and supervise until every repo reports back.
