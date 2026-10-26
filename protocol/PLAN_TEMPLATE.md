# Plan: <short title>

Repo: <path> · Branch: mesh/<run>/<slug> · Agent: <your name> (<model>/<effort>)

A group's plan covers all its tasks: Goal, Findings, Files and Steps get a subsection per task, and a "Maybe" task says do or decline, with the evidence.

## Goal
One or two sentences: the outcome, not the steps.

## Findings
What your research found, with file paths and function names: where the behaviour lives today, the root cause of a bug, how you reproduced it, what the existing tests cover. The reviewer hasn't read the code, so this is what they judge the plan by.

## Files
The files to create or change, with one line each on what changes. Name existing functions and utilities to reuse, with their paths.

## Invariants
Rules that must still hold afterwards: conventions from the repo's CLAUDE.md/AGENTS.md that this change touches, contracts with other services, security rules.

## Steps
1. …
2. …

## Done when
The exact command(s) that must pass, e.g. `pnpm test && pnpm build`, plus any manual check.

## Self-check
What the `check-plan` skill found: its verdict (GO / FIX / RETHINK), the load-bearing claims marked FACT (with where you saw it) or GUESS, the strongest objection and what settled it, and the prior art. A few lines; what's still open goes in the next section.

## Open questions / risks
- Decisions you'd like the reviewer to make, each with the options and your recommendation.
- What could go wrong, and how you'll notice.

## Out of scope
What you won't do, even if it looks related.
