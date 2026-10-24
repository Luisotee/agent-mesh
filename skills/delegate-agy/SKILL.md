---
name: delegate-agy
description: >-
  agent-mesh, mode `agy`: one agy agent with Gemini Flash (high effort) per repo researches, sends you its plan for review, then executes
  it itself and opens a PR, all in visible herdr panes with two-way messaging. Use ONLY when the user explicitly types /delegate-agy; never
  on your own initiative.
disable-model-invocation: true
argument-hint: <what to build or fix, in which repos>
---

# /delegate-agy

Run an **agent-mesh** in mode **`agy`** for the user's request: the text after the command, which is also `$ARGUMENTS`.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Keep doing your own repo's part yourself; spawn workers only for the other repos (and for yours only if the user asks). Follow the "Root" section with `mesh init --mode agy`, and spawn every worker with the `agy` row of the mode table (Gemini Flash, high effort), unless the request explicitly names another model or agent. If the request is a follow-up to work an earlier worker did ("send the review to the agent that changed it", "ask that agent to make it Y"), resume that worker instead (the "Follow-ups" section): it keeps its own CLI and model, and the mode only sets defaults for new workers.
3. Tell the user in one line what you started: run id, repos, and the worker model. Then review each worker's plan and supervise until every repo reports back.
