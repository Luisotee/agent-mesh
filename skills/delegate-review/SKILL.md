---
name: delegate-review
description: >-
  agent-mesh: gets a pull request reviewed by other agents (Claude, agy, Copilot), each inside the PR's repo at the PR
  head; they change and post nothing, and you get their findings merged and checked against the code. Defaults to the
  current branch's PR and to reviewers by the PR's size and agy's quota: one for a small PR (agy, else Copilot, else
  Claude), Claude plus agy or Copilot for a large or risky PR, the same ones minus Copilot for a large PR's re-review
  (a small PR gets one round), all run by one command that wakes you once; `all` is all three. Use it, typed or not,
  for any PR review the user asks for, their own included ("review my PR", "have Copilot review PR 42", "second opinion
  from Gemini and GPT"). Gemini, Flash, Argon → agy; GPT, Copilot → copilot; Opus, Sonnet, Fable → claude. Not for the
  reviews a `/delegate-*` run gives its own workers' PRs, or a mesh worker (your task came by a `📬 [mesh]` doorbell).
argument-hint: "[all | claude agy copilot (any of them)] [PR link or number]"
---

# /delegate-review

Get a review for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here.

1. **The PR:** the link or number given. Otherwise the current branch's PR (`gh pr view --json url -q .url`). If there is none, ask the user which PR.
2. **The reviewers:** every CLI named, in any order (vendor words as in the description), as a comma list (`claude,copilot`), or `all` for all three. With none named, `auto`: mesh picks by the size (step 3) and agy's quota, one reviewer for `small` (agy while it has quota, else Copilot, else Claude), Claude plus one other for `large` (agy, else Copilot), and says on stderr what it picked and why. A small PR gets one round: after its fix, check the fix commits yourself (`git diff <reviewed head>..<new head>`) rather than asking for a re-review, unless the user asks for one. A re-review after a fix (the user asks you to look again at a PR these reviewers already reviewed, or a large PR's fix) is the same command again: `auto` asks the same reviewers again except Copilot, whose credits are kept for first looks at the PRs that matter.
3. **The size:** `small` for a PR of up to a few hundred lines in a few files (`gh pr view <PR> --json additions,deletions,changedFiles`), `large` for anything bigger or riskier (auth, money, data migrations). Every reviewer gets the same size. The size picks the plan phase and the level of a Claude reviewer's `/code-review` (`medium` for small, `high` for large); Claude and agy reviewers run at high effort either way, each in a pane beside you.
4. **Run them** from the local checkout of the PR's repo (the current one, or ask the user where it is), all in one command, a review round:

   ```bash
   mesh ask --repo <checkout> --review <PR> --size <size> --reviewers <auto | claude,agy,copilot (the named ones) | all>
   ```

   - It starts a run if there is none, runs every reviewer at once, waits until each has answered or alerted, prints one line per reviewer and the path of **one** answers file (`answers: …`), and closes the reviewers that answered. Read that file, not their inboxes.
   - **You are Claude Code:** run it with `run_in_background`; its notification is the whole round. **You are agy:** run it in the foreground.
   - **Copilot** runs headless inside the round; while it works, a pane beside you shows what it's doing (`mesh peek --follow`) and closes when it's done.
   - A reviewer that didn't answer (its line says why: hung, stopped, held at a startup screen, out of quota) stays open: follow its line, and its later answer comes to your inbox. `mesh retire <name>` it once you have no follow-up. Any other exit code is `mesh watch`'s (see "Questions and reviews" in `~/.config/agent-mesh/ORCHESTRATE.md`, the only section you need).
5. **Check every finding** before you report it: it's a claim, not an order. Trace it in the code or run the failing case, and judge the suggested fix. Keep each finding's blocking or advisory mark (a blocking one you can't reproduce is advisory at most). With several reviewers, merge the same issue found by more than one into one finding and say who found it; agreement makes a finding likelier, not true, so check it all the same. Then give the user each finding with your verdict (agree, agree with a different fix, or disagree and why), and the reviewers' names: `mesh ask <name> --body "…"` asks one again with everything it read. Fix nothing unless the user asks; a fix they ask for goes on the PR's own branch, not in a new PR.
