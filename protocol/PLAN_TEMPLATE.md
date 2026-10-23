# Plan: <short title>

Repo: <path> · Branch: mesh/<run>/<slug> · Executor: <kind> <model>/<effort>

## Goal
One or two sentences: the outcome, not the steps.

## Setup
Commands to run first in the fresh worktree (e.g. `pnpm install --frozen-lockfile`, `uv sync`).

## Files
The files to create or change, with one line each on what changes. Name existing functions and utilities to reuse, with their paths.

## Invariants
Rules that must still hold afterwards: conventions from the repo's CLAUDE.md/AGENTS.md that this change touches, contracts with other services, security rules.

## Steps
1. …
2. …

## Done when
The exact command(s) that must pass, e.g. `pnpm test && pnpm build`, plus any manual check.

## Stop and ask if
- The plan is wrong about the code (a file or function isn't where it says).
- A step would touch files outside **Files**.
- <task-specific doubts the planner foresees>

## Out of scope
What not to do, even if it looks related.
