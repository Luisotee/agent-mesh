# Harvest brief

The template for step 4 of `harvest-peers`. Every ask starts a fresh asker with no memory of an earlier round, so there is **one** brief, for every peer: on a first round cut the "What changed on our side" section, on a later one fill it from the last answers. Fill every `<…>`, keep the other sections, cut what doesn't apply, and copy from the heading down to the end (this note is for you, not the peers). One brief goes to every peer it names at once, so the per-project focus is a list at the end.

---

# Research question: what should <repo> adopt from this project, and which of our bugs would it have caught?

You are in a clone of a third-party project that shares some of <repo>'s mechanisms. I maintain **<repo>** (`<path>`): <what it is and the problem it solves, two or three sentences>. Read `<README and design doc paths>` there to know what we already do and which alternatives we rejected; `<main code paths>` is the code.

Our mechanics in one breath: <the 4 to 6 core mechanisms, each with the name of our function, file or doc section that does it>.

## What changed on our side (a later round only)

Since <date of the last round>: <a few lines on what landed, each with the file or function; what we adopted from the last answers, if anything>. Still open on our side: <the tracker's themes>. Already settled, so don't re-propose: <what the last answers said and what became of each, one line each>.

## What I want

Research this project's **source**, not only its README (READMEs oversell; behaviour lives in code). Then report, for each mechanism worth knowing about:

- **What it does** and **where** (file:line or function names).
- **How it differs from <repo>** (name our counterpart, or say we have none).
- **Verdict:** adopt / adapt / skip, and why. Respect our constraints: <language, dependencies, platform, design rules that must hold>.
- Rough cost of adopting it (small / medium / large) and what it would touch.

Cover the whole range of what we could take, not only new features:

1. **Features we lack** that would pay off here.
2. **Better ways to do what we already do:** the same mechanism done more simply, more robustly, or covering edge cases ours misses. Name our code it would replace or harden.
3. **Lessons from their history:** the clone has its git history, and `gh` works for their issues and PRs (read-only: post nothing). Look for bugs they fixed, races and failure modes they hit, features they removed or reverted, and pain points users filed. Say whether <repo> has the same bug or would hit it.
4. **Wording and interfaces:** how their docs, prompts, CLI or API are phrased, and whether ours would be better for it.
5. **Testing, install and UX:** how they test, install and show the user what's going on.
6. **Where we're already ahead:** briefly, so we know what not to change.
7. **Bug hunt.** For each of these, compare how your project handles the same thing, and look for cases ours misses: races, crashes, lost data, a failure mode your project fixed (its history and issues count, read-only) that ours would still hit. Read our code at the path given.
   - <mechanism>: ours is `<file>:<function>`. <the cases worth checking, e.g. "two senders at once", "the process restarted mid-write">.
   - …

Topics I care about most: <the open questions in this repo right now; its tracker's themes>.

Rank the findings, most valuable first. Say plainly what you could not confirm in the code. Don't pad: three strong findings beat ten weak ones. Write each one briefly: lead with the result, keep every fact, reason and caveat, and don't narrate your research. Change nothing in the clone, and say plainly when a section has nothing.

## Focus per project

Find your project's line:

- `<peer name>`: <what it is in one line; the mechanisms it shares with us; what to look at first (files, issues, a large repo: map it first)>.
- …
