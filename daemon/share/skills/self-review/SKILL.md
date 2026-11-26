---
name: self-review
description: >-
  Reviews your own change before you report it: the diff from where your branch started, against the task or the
  approved plan, for correctness bugs, missing tests and broken repo rules, then fixes what holds up and pushes. Use it
  as a mesh worker after you publish a change and before you send `done`, and whenever the user asks you to review your
  own work before handing it over. Not for reviewing someone else's PR (that is an outside reviewer's job).
argument-hint: "[range; empty: the merge base with your branch's base..HEAD]"
---

# /self-review

You know this change best, so you are the cheapest reviewer of it, and the one most likely to share its blind spots. Look for what you would miss: read the diff as a stranger would, from the task, not from what you remember deciding.

**Target:** `$ARGUMENTS` when given. Otherwise `$(git merge-base HEAD <base>)..HEAD`, where `<base>` is the branch your work started from (a mesh worker: the `baseCommit` that `mesh agent <your name>` shows, else `origin/HEAD`).

## Steps

1. **The diff, whole.** `git diff <range> --stat`, then every hunk. A file you didn't mean to touch is the first finding.
2. **Against the task.** Each thing the task or approved plan asked for: is it there, and is it what was asked? Each thing the diff does that nobody asked for: drop it, or say why it had to change.
3. **Correctness.** For each changed function: the inputs it gets now (callers, the empty and the error case), what it returns, and what else reads what it writes. A bug is one you can state as "with input X, Y happens". A guess you can't make concrete in a minute is not a finding.
4. **Tests and checks.** The repo's own test, lint and type commands (its `CLAUDE.md` / `AGENTS.md`) pass. A behaviour the change adds has a test that would fail without it.
5. **Claude Code:** also run `/code-review <level> <range>` at the level your parent gave (mesh: `review.self_level.<size>`, from `mesh settings --prefix review.self_level`), never with `--fix`, `--comment` or `ultra`, and judge each of its findings as you would your own.

## Apply

- Fix each finding you agree with, as commits on your branch, and push. Leave a finding you disagree with, with one line on why.
- Rerun what step 4 ran.
- In your report (a mesh worker: the `done`), one line: `Self-review: N found, M fixed` and the ones left, each with why.

Write nothing else: no PR comments, no review posted. Ask nobody: no subagents on a model your parent didn't pick, no other agents (a mesh worker starts no change agents).
