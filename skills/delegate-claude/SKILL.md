---
name: delegate-claude
description: >-
  agent-mesh, mode `claude`: hands each repo's part of a change, or a whole task list, to a Claude agent running inside that repo, and supervises them.
  Work is sized: trivial and small tasks run on Opus in the background with no plan, large ones plan on Opus at high effort in a visible herdr pane,
  get their plan reviewed, execute at medium effort and open a PR, and each PR gets two or three reviews (Claude and agy, plus Copilot on a large PR's first review). Workers report back by SendMessage. Also answers questions about another repo,
  with agents you can ask again, resume (compacted when large) or hand the change to, and asks the registered peer projects' standing askers ("ask the peer projects / the researched projects").
  Use it, whether the user types /delegate-claude or not, when the user wants another repo's part of a change, a question about another repo,
  or a task list handed to agents that work inside those repos ("have Claude do it", "delegate this", "ask the shop-api agent"), and they don't name the other CLI (agy, Gemini, Argon, Flash); with neither named, use this one when you are Claude Code.
  Not for a PR review the user asks for (that's /delegate-review), not for work you can do in your own repo alone, and never when you are a mesh worker (your task came by a `📬 [mesh]` doorbell).
argument-hint: <what to build, fix or ask, in which repos, or a task list>
---

# /delegate-claude

Run an **agent-mesh** in mode **`claude`** for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here.

1. Read `~/.config/agent-mesh/ORCHESTRATE.md` and `~/.config/agent-mesh/PROTOCOL.md` in full.
2. You are the **root**. Follow the "Root" section with `mesh init --mode claude`. Its `address` is how your Claude workers will report to you with `SendMessage`.
   - **Size every task** ("Sizes") and pass `--size` to every `mesh spawn` and `mesh ask`. `--kind claude` is the default CLI; propose agy only as "Routing" describes.
   - Pass `--model` / `--effort` only if the request names them. Pass `--plan-review user` only if the user wants to approve a large plan themselves.
   - **Your own repo:** keep doing its part yourself; spawn workers only for the other repos (and for yours only if the user asks).
   - **A question about another repo** that changes nothing: use `mesh ask --kind claude` ("Questions and reviews"). A question for the registered peer projects goes to `mesh ask --peers <group|all>`, run in the background; read the one answers file it prints. A PR review the user asks for on its own goes through `/delegate-review`; the reviews of your own workers' PRs stay here ("Root" step 8).
   - **A follow-up** to work an earlier worker did ("send the review to the agent that changed it", "ask that agent to make it Y"): resume that worker ("Follow-ups"). It keeps its own CLI and model (a change it must plan first plans on Opus), and it's compacted first if its conversation is large.
   - **Decide small things yourself** and ask the user only where their input adds value; fix review findings and the follow-ups they turn up in the PR they came from, on its branch, by the worker that made it, never in a new PR ("Rules for everyone"); put other follow-ups on the repo's list of open work.
   - **A list of tasks:** follow "Task lists". Triage it into the board, get the user's one confirmation, then supervise without writing code yourself.
   - Whenever `mesh` prints a `send_message`, pass it on with the `SendMessage` tool, exactly as printed.
3. Tell the user in one line what you started: run id, and each worker's repo, size, model and whether it's in a pane or in the background. Then supervise until every worker reports back. For a question, give the user the answer and the name of the worker that gave it, so they can ask it again.
