---
name: delegate-agy
description: >-
  agent-mesh, mode `agy`: agy with Gemini Argon (high effort) plans inside each repo; agy with Gemini Flash executes, all in visible herdr panes with two-way
  messaging. Use ONLY when the user explicitly types /delegate-agy; never on your own initiative.
disable-model-invocation: true
argument-hint: <what to build or fix, in which repos>
---

# /delegate-agy

Run an **agent-mesh** in mode **`agy`** for the user's request: the text after the command, which is also `$ARGUMENTS`.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Follow the "Root" section with `mesh init --mode agy`, and use the `agy` row of the mode table for every planner and executor you start, unless the request names other models.
3. Tell the user in one line what you started: run id, repos, who plans and who executes. Then supervise until every repo reports back.
