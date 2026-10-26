---
name: delegate-agy
description: >-
  agent-mesh, mode `agy`: hands each repo's part of a change, or a whole task list, to an agy agent running inside that repo, in a visible herdr pane, and supervises them.
  Work is sized to save quota: trivial and small tasks run on Flash with no plan, large ones plan on the strongest model with quota left (Argon → Opus → Flash),
  get their plan reviewed, then execute on Flash and open a PR, and each PR gets two or three reviews (Claude and agy, plus Copilot on a large PR's first review), except a small one of up to 150 lines, which the root reviews itself. Also answers questions about another repo, with agents you can ask again,
  resume (compacted when large) or hand the change to; asks the registered peer projects' standing askers ("ask the peer projects / the researched projects"); can route large tasks to Claude.
  Use it, whether the user types /delegate-agy or not, when the user wants another repo's part of a change, a question about another repo,
  or a task list handed to agents that work inside those repos ("have agy do it", "delegate to Gemini", "ask the shop-api agent"), and they don't name the other CLI (Claude, Opus, Sonnet, Fable); with neither named, use this one when you are agy.
  Not for a PR review the user asks for (that's /delegate-review), not for work you can do in your own repo alone, and never when you are a mesh worker (your task came by a `📬 [mesh]` doorbell).
argument-hint: <what to build, fix or ask, in which repos, or a task list>
---

# /delegate-agy

Run an **agent-mesh** in mode **`agy`** for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Follow the "Root" section with `mesh init --mode agy`.
   - **Size every task** ("Sizes") and pass `--size` to every `mesh spawn` and `mesh ask`. `--kind agy` is the default CLI; propose Claude only as "Routing" describes.
   - Pass `--model` / `--effort` only if the request names them. A large task plans on the best model with quota and moves to Flash when you `mesh approve` its plan. On a quota alert, `mesh switch` the worker.
   - **Your own repo:** keep doing its part yourself; spawn workers only for the other repos (and for yours only if the user asks).
   - **A question about another repo** that changes nothing: use `mesh ask` ("Questions and reviews"). Run it with `run_in_background` if you're a Claude root. A question for the registered peer projects goes to `mesh ask --peers <group|all>`; read the one answers file it prints. A PR review the user asks for on its own goes through `/delegate-review`; the reviews of your own workers' PRs stay here ("Root" step 8).
   - **A follow-up** to work an earlier worker did ("send the review to the agent that changed it", "ask that agent to make it Y"): resume that worker ("Follow-ups"). It keeps its own CLI and model (a change it must plan first plans on the best model with quota), and it's compacted first when its conversation is large.
   - **Decide small things yourself** and ask the user only where their input adds value; fix review findings and the follow-ups they turn up in the PR they came from, on its branch, by the worker that made it, never in a new PR ("Rules for everyone"); give new small work to the worker that already knows that code, not a fresh agent; put other follow-ups on the repo's list of open work.
   - **A list of tasks:** follow "Task lists". Triage it into a few themed groups (no size cap: a worker can take a whole batch), each one worker with one plan and one PR; get the user's one confirmation, then supervise without writing code yourself.
   - If you're a Claude root and `mesh` prints a `send_message` (for a Claude worker you routed there), pass it on with the `SendMessage` tool, exactly as printed.
3. Tell the user in one line what you started: run id, and each worker's repo, size and model. Then supervise until every worker reports back. For a question, give the user the answer and the name of the worker that gave it, so they can ask it again.
