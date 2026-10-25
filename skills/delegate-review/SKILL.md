---
name: delegate-review
description: >-
  agent-mesh: gets a pull request reviewed by one or more other agents, Claude, agy and Copilot, each running inside the
  PR's repo with its rules loaded and the PR head checked out. They change nothing and post nothing; you get their
  findings merged, each checked against the code, and reviewers you can ask again. Defaults to the current branch's PR
  and to the other vendor from the one you are (Claude ↔ agy); Copilot only when named; `all` is all three. Use it,
  whether the user types /delegate-review or not, for any PR review the user asks for, their own PR included
  ("review my PR", "have Copilot review PR 42", "second opinion from Gemini and GPT on this PR"). Vendor words name
  reviewers: Gemini, Flash, Argon → agy; GPT, Copilot → copilot; Opus, Sonnet, Fable → claude. Not for the reviews a
  `/delegate-*` run gives its own workers' PRs, and never when you are a mesh worker (your task came by a `📬 [mesh]` doorbell).
argument-hint: "[all | claude agy copilot (any of them)] [PR link or number]"
---

# /delegate-review

Get a review for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here.

1. **The PR:** the link or number given. Otherwise the current branch's PR (`gh pr view --json url -q .url`). If there is none, ask the user which PR.
2. **The reviewers:** every CLI named, in any order (vendor words as in the description), or all three for `all`. With none named, the other vendor from you: agy if you are Claude Code, claude if you are agy. Copilot reviews only when the user names it (or says `all`).
3. **The size:** `small` for a PR of up to a few hundred lines in a few files (`gh pr view <PR> --json additions,deletions,changedFiles`), `large` for anything bigger or riskier (auth, money, data migrations). Every reviewer gets the same size. The size picks the plan phase; Claude and agy reviewers run at high effort either way, each in a pane beside you.
4. **Run them** from the local checkout of the PR's repo (the current one, or ask the user where it is), one `mesh ask` per reviewer:

   ```bash
   mesh ask --repo <checkout> --review <PR> --kind <reviewer> --size <size> --name review-<number>-<reviewer>
   ```

   - **Several reviewers:** they run at the same time, so first make sure there is one run for them all. If `mesh whoami` fails, start one with `mesh init --mode <claude | agy: your own CLI> --request "review <PR>"`. The `--name` keeps their names and worktrees apart.
   - **You are Claude Code:** run each `mesh ask` with `run_in_background`, all in the same turn; each answer arrives as its notification. A Claude reviewer under a Claude root returns once its task is delivered, and its answer comes back by `SendMessage`; if that `mesh ask` prints a `send_message`, pass it on with `SendMessage` exactly as printed first. `mesh retire <name>` that reviewer once you have no follow-up.
   - **You are agy:** start the Claude and agy reviewers with `--no-wait`, then run the Copilot one (if any) in the foreground, then `mesh watch <name>` each pane reviewer that hasn't answered and read the answers with `mesh inbox`. `mesh retire <name>` each pane reviewer once you have no follow-up.
   - **Copilot** runs headless inside its `mesh ask` call, and its findings are what that call prints. While it works, a pane beside you shows what it's doing (`mesh peek --follow`) and closes when it's done.
   - Any other exit code is `mesh watch`'s (see "Questions and reviews" in `~/.config/agent-mesh/ORCHESTRATE.md`, the only section you need).
5. **Check every finding** before you report it: it's a claim, not an order. Trace it in the code or run the failing case, and judge the suggested fix. With several reviewers, merge the same issue found by more than one into one finding and say who found it; agreement makes a finding likelier, not true, so check it all the same. Then give the user each finding with your verdict (agree, agree with a different fix, or disagree and why), and the reviewers' names: `mesh ask <name> --body "…"` asks one again with everything it read. Fix nothing unless the user asks.
