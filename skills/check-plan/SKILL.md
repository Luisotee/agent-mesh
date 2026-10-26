---
name: check-plan
description: >-
  Checks a plan you wrote against the code before you send it or act on it: verifies its load-bearing claims, labels
  each one fact or guess, searches once for prior art, finds the strongest objection and the cheapest check for it,
  gives a verdict (GO / FIX / RETHINK), then fixes the plan and puts the open risks in it. Use it as a mesh worker
  before every `plan` you send, and whenever the user asks you to check, stress-test, question or grill your own plan.
  It writes nothing but the plan and asks the user nothing. Not for reviewing someone else's plan or a PR.
argument-hint: "[plan file; empty: the plan you just wrote]"
---

# /check-plan

Try to prove your own plan wrong before anyone else reads it, and keep what survives. Your loyalty is to the task's goal, not to the plan, the research behind it, or anything you concluded earlier in this conversation.

**Target:** `$ARGUMENTS` when given. Otherwise the plan file you just wrote (a mesh worker: `$(mesh dir)/plans/<your-name>.md`), else the plan in this conversation.

## Steps

0. **You are anchored.** You wrote this plan, so you'll read it as right. Re-derive each point from the code and the task, not from what you remember deciding.
1. **The goal.** Restate in one sentence what the task asks for, and how the plan will show it's met (its done-when). Does the plan answer that, or a drifted version of it? Is the request's premise true in the code: the bug happens, the field is missing, the API behaves as the task says? A false premise is the first finding.
2. **Load-bearing claims.** List the 3 to 7 claims the plan stands on: where the behaviour lives, the root cause, what the tests cover, that nothing already does this, how a library or API behaves, that a step is safe. Check each one now: read the line, run the command. Mark it:
   - **FACT**: seen, with its `file:line`, command and output, or doc.
   - **GUESS**: inferred, not seen. A guess the plan depends on gets checked now if that takes minutes; otherwise it goes in the risks, with the check that would settle it.
   Never let a guess turn into a fact by being repeated.
3. **Prior art, at least one search.** In the repo first: a function, flag or helper that already does it, a convention for it, an earlier attempt in `git log`. Then outside when the plan builds something new (a library, a package, an existing tool or skill). Name what you found and what is actually different. Reuse beats a new mechanism unless the difference matters.
4. **The strongest objection.** Build the best case against the plan, not a straw man. Use only the lenses that bite:
   - a much simpler path to the same result
   - the wrong layer: fixing a symptom, or something another layer already owns
   - a hidden cost: complexity, upkeep, latency, a migration, a new dependency
   - what breaks next: callers, other workers' changes, the contract with another repo
   - a rule in the repo's `CLAUDE.md` / `AGENTS.md` (or its design docs) that it breaks
   - a done-when that would pass even if the change were wrong
   Then the cheapest check that settles the objection. Run it if it's cheap.
5. **Verdict:**
   - **GO**: sound. Say which attacks it withstood.
   - **FIX**: a must-fix, resting on something you saw, that the plan can absorb. Fix it.
   - **RETHINK**: the approach can't satisfy a must-fix, or a much simpler path reaches the goal. Rewrite the plan; if the choice is your parent's or the user's (scope, a product decision), put it in the open questions with your recommendation.

## Rules

- **Don't perform harshness.** Inventing problems to look rigorous is as wrong as agreeing by reflex. A sound plan gets GO in a few lines.
- **A must-fix rests on something seen.** A risk you can only state about code you haven't read is a GUESS: check it or list it, don't rank it as a finding.
- **Credit the plan's own safeguards.** A risk its tests or done-when would catch, and that's cheap to undo, is a note, not a must-fix.
- **No invented numbers.** "We don't know yet" is an answer.
- **Scale to the plan.** A small plan gets a short check. Skip empty lenses; don't pad.
- The check ends when you apply it. It isn't a stance for the rest of the session.

## Apply

Edit the plan:

- fix what the check found wrong (steps, files, done-when);
- fill its `## Self-check` section (add one if it has none), in 3 to 8 lines: the verdict, the load-bearing claims with FACT or GUESS and where each was checked, the strongest objection and what settled it, the prior art found;
- put what is still open in "Open questions / risks": each risk with how you'd notice it, each decision with your recommendation.

Write nothing else: no other file, nothing in the repo, no ledger. Ask nobody: no questions to the user, no subagents, no other agents (a mesh worker never spawns one). A mesh worker then sends the plan as usual. Used by hand on a plan that isn't in a file of yours, give the result in your reply instead.
