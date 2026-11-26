---
name: delegate-review
description: >-
  agent-mesh (daemon): gets a pull request reviewed by other agents (Claude, agy, Copilot, OpenCode), each inside the PR's repo at the PR
  head; they change and post nothing, and you get their findings merged and checked against the code by a root the daemon runs.
  Defaults to the current branch's PR and to reviewers by the PR's size and the CLIs installed
  (`review.reviewers.<size>`), the same ones again for a re-review of a fix. Use it, typed or not,
  for any PR review the user asks for, their own included ("review my PR", "have Copilot review PR 42", "second opinion
  from Gemini"). Not for the reviews a `delegate` run gives its own workers' PRs, or for a mesh agent (your task came by a
  `📬 [mesh]` doorbell).
argument-hint: "[all | claude agy copilot opencode (any of them)] [PR link or number]"
---

# delegate-review

Get a review for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here. You are the user's own session, not an agent of the mesh: a reviewer started with no parent has nobody to report to, so the review runs under a **root** that starts the reviewers, collects their answers and checks each finding against the code.

1. **The PR:** the link or number given. Otherwise the current branch's PR (`gh pr view --json url -q .url`). If there is none, ask the user which PR.
2. **The reviewers:** only Claude and agy have daemon adapters today (Copilot and OpenCode come later: name them and the root says it can't, then picks from the two). Every CLI named (vendor words: Gemini, Hydrogen, Flash, Argon → agy; GPT, Copilot → copilot; OpenCode, Big Pickle, DeepSeek, GLM, free models → opencode; Opus, Sonnet, Fable → claude), `all` for every installed one, or none named: the root picks by the PR's size from `review.reviewers.<size>` (`mesh settings --prefix review`), what is installed and the quotas.
3. **The size:** `small` for a PR of up to a few hundred lines in a few files (`gh pr view <PR> --json additions,deletions,changedFiles`), `large` for anything bigger or riskier (auth, money, data migrations).
4. **Start the run and its root** as the `delegate` skill's steps 1 to 4 say (`mesh hello`, `mesh start-run`, the request file, `mesh spawn --role root`), with a request file saying: review `<PR link>` (size, reviewers or `auto`, tier `mid` or `strong` by the PR's risk); follow "Questions and reviews" in `ORCHESTRATE.md`; start the reviewers at once; check every finding against the code; give the user the merged, checked findings with each reviewer's name and your verdict on each (agree, agree with a different fix, or disagree and why); fix nothing unless the user asks. Pass the repo's checkout as `--repo`.
5. **Tell the user** the run id and the root's name; the root reports the findings to them in the app. A re-review after a fix is the same request again in the same run: `mesh send <root name> --type task --body "re-review <PR> at <new head>"`.
