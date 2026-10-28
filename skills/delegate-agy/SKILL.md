---
name: delegate-agy
description: >-
  agent-mesh, mode `agy`: hands each repo's part of a change, a task list, or a question about another repo to agy
  agents working inside those repos, in visible herdr panes, and supervises them; large tasks get their plan reviewed
  first, and each PR gets one review (agy while it has quota, else Copilot, else Claude), or two for a large PR (Claude plus agy or Copilot), in one round (one root wake), except a
  trivial or small task's or group's PR of up to 150 lines touching nothing risky, which the root reviews itself. Also
  asks the registered peer projects a question. Use it, typed or not, when the user wants work or a question handed to
  agents in other repos ("have agy do it", "delegate to Gemini", "ask the shop-api agent") and names no other CLI
  (Claude, Opus, Sonnet, Fable); with none named, use it when you are agy. Not for a PR review the user asks for
  (/delegate-review), a research round by the peers (/harvest-peers), work in your own repo alone, or a mesh worker
  (your task came by a `📬 [mesh]` doorbell).
argument-hint: <what to build, fix or ask, in which repos, or a task list>
---

# /delegate-agy

Run an **agent-mesh** in mode **`agy`** for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Follow the "Root" section with `mesh init --mode agy`.
   - **Propose before you start** ("Root" step 3): what you'll do yourself and each worker's repo, size, model and view, then wait for the user's go, unless they said to run autonomously. A question, a review or a peer ask they asked for needs none.
   - **Size every task** ("Sizes") and pass `--size` to every `mesh spawn` and `mesh ask`. `--kind agy` is the default CLI; propose Claude only as "Routing" describes.
   - Pass `--model` / `--effort` only if the request names them. A large task plans on the best model with quota and moves to Flash when you `mesh approve` its plan. On a quota alert, `mesh switch` the worker.
   - **Your own repo:** keep doing its part yourself; spawn workers only for the other repos (and for yours only if the user asks).
   - **A question about another repo** that changes nothing: use `mesh ask` ("Questions and reviews"). Run it with `run_in_background` if you're a Claude root. A question for the registered peer projects goes to `mesh ask --peers <group|all>`; read the one answers file it prints. A PR review the user asks for on its own goes through `/delegate-review`; the reviews of your own workers' PRs stay here ("Root" step 8).
   - **A follow-up** to work an earlier worker did ("send the review to the agent that changed it", "ask that agent to make it Y"): resume that worker ("Follow-ups"). It keeps its own CLI and model (a change it must plan first plans on the best model with quota), and it's compacted first when its conversation is large.
   - **Decide small things yourself** and ask the user only where their input adds value; fix review findings and the follow-ups they turn up in the PR they came from, on its branch, by the worker that made it, never in a new PR ("Rules for everyone"); give new small work to the worker that already knows that code, not a fresh agent; put other follow-ups on the repo's list of open work.
   - **A list of tasks:** follow "Task lists". Triage it into a few themed groups (no size cap: a worker can take a whole batch), each one worker with one PR, sized by its combined work (independent small tasks: `small`, no plan; one plan for a `large` group); get the user's one go on the board, then supervise without writing code yourself.
   - If you're a Claude root and `mesh` prints a `send_message`, pass it on with the `SendMessage` tool, exactly as printed.
3. Once the user says go (at once, when they said to run autonomously), start the work and tell the user in one line what you started: run id, and each worker's repo, size and model. Then supervise until every worker reports back. For a question, give the user the answer and the name of the worker that gave it, so they can ask it again.
