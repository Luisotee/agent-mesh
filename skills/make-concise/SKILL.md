---
name: make-concise
description: >-
  Rewrites a repo's agent instruction files (CLAUDE.md, AGENTS.md, GEMINI.md, their imports, or files you name) in
  concise plain English without losing a rule: every rule and its reason kept, code, paths, URLs and marked blocks
  untouched, each rules file under agy's 24,000-byte limit, checked by a before/after validator, plus a size test and
  one PR. Use it when the user or a task asks to shorten, tighten or make concise a repo's rules or instruction files.
  Run it in that repo, by its own agent. Not for code, READMEs meant for humans, or another repo's files.
argument-hint: "[files (default: the repo's rules files)]"
---

# /make-concise

Rewrite this repo's instruction files so agents read fewer tokens on every call, while every rule still holds. Shorter text that drops a rule's reason or an edge case costs more than it saves, so the bar is: **nothing a reader acts on is lost.**

Run it from inside the repo whose files you rewrite, as that repo's agent, so its own `CLAUDE.md` / `AGENTS.md` rules apply (branches, commits, PR process). One repo per run, one PR per repo.

**Target:** `$ARGUMENTS` when given, else the repo's rules files (step 1).

## Steps

1. **Find the files.** Tracked `CLAUDE.md`, `AGENTS.md` and `GEMINI.md` at any depth (`git ls-files '*CLAUDE.md' '*AGENTS.md' '*GEMINI.md'`), and the files they import with an `@path` line. Note each one's size, and which are symlinks (rewrite the target once) or `@` imports of another.
2. **Find what code matches.** Some text is read by code, not only by agents: a test that asserts a sentence, a script that greps for a heading, a tool that regenerates a block. Grep the repo's code, tests, scripts and CI for each file's name and for distinctive quoted text from it. Every string you find goes to the validator as `--keep "<text>"`. Marked blocks (below) are found by the validator itself.
3. **Back up.** Copy each file to a scratch directory outside the repo, at the same relative path (`<scratch>/before/CLAUDE.md`, `<scratch>/before/docs/x/AGENTS.md`). Never back up inside the repo.
4. **Rewrite** each file by these rules:
   - **Keep every rule and its reason.** Shorten the reason; never drop it. A rule without its "because" gets applied where it doesn't fit.
   - **Say each rule once.** A rule repeated in two places stays in the one where readers look for it.
   - **Keep examples that show different cases.** Drop an example only when another shows the same case.
   - **Plain, full sentences.** Keep articles and connectives ("because", "unless", "so"). No telegraphic fragments, no symbols or abbreviations standing in for words.
   - **Keep how strict a rule is.** "Never", "always", "must", "usually", "only if" and "may" say that; don't add or remove them.
   - **Cut** preamble, filler ("Note that", "It is important to", "Please make sure"), headings restated as sentences, and narration of history that no rule depends on.
   - **Verbatim:** code, commands, paths, URLs, inline code and fenced blocks. Keep headings that other files or code cite.
   - **Marked blocks stay byte-identical:** anything between `<!-- BEGIN:name -->` and `<!-- END:name -->` (or `name:begin` / `name:end`), such as the `nextjs-agent-rules` block that `next dev` regenerates, or a block an installer writes.
   - **Two real files stay two real files.** Where the repo has both `CLAUDE.md` and `AGENTS.md` as real files, rewrite each; don't replace one with a symlink or an `@AGENTS.md` import. That choice is the repo owner's: if the two have drifted apart, say so in the PR.
   - **Each rules file at most 24,000 bytes** (agy truncates a rules file past that, so its end is silently lost). If a file can't fit without dropping rules, move detail that is rarely needed into a document the file points to, and keep a one-line gist where it was. Pass that document to the validator with `--also <file>` (step 5), so what moved there counts as kept.
5. **Validate:** `python3 <this skill's directory>/validate.py <scratch>/before <repo root> [--also <new doc> …] [--keep "<text>" …]` (the directory is `~/.claude/skills/make-concise/`, `~/.gemini/config/skills/make-concise/`, or the agent-mesh plugin's `skills/make-concise/`). It compares every backed-up file with its rewrite and prints the sizes. Text moved from one rewritten file to another, or into an `--also` file, counts as kept; so does a path put into backticks or taken out of them. Text deleted from one file because another already says it gets a "kept only in" warning: make sure that file is where readers look for it.
   - **ERROR** (exit 1): a fenced block missing or changed; a marked block missing or changed; an inline code span, URL, path or `@` import that was there before and is now in no file; a `--keep` text gone; a rules file over 24,000 bytes. Fix each by putting the text back, not by arguing with the check.
   - **warn**: a bare file name gone (words like `Next.js` or `github.com` look like one, so check it was only filler); a code span, URL or path that wasn't there before (make sure you didn't invent it); a heading removed (make sure nothing cites it); changed line endings; a file that grew. A file that isn't UTF-8 is an error.
   - It checks prose paths only when they look like paths: a leading `~`, `/`, `./` or `../`, a trailing `/`, a file extension, or two or more slashes. Keep any other path in backticks, where it is checked as code.
6. **Check the meaning.** The validator can't. Read `git diff --word-diff` file by file: for each rule, reason and example in the old text, find where the new text says it. Put back anything you can't find.
7. **Add a size test** so the files stay under the limit, in the repo's own test runner. Python:

   ```python
   import subprocess
   from pathlib import Path

   ROOT = Path(__file__).resolve().parent.parent
   AGY_RULE_LIMIT = 24_000  # bytes; agy truncates a rule file past this


   def test_rules_files_fit_agy():
       names = subprocess.run(["git", "ls-files", "-z", "*CLAUDE.md", "*AGENTS.md", "*GEMINI.md"],
                              cwd=ROOT, capture_output=True, text=True, check=True).stdout.split("\0")
       for name in filter(None, names):
           path = ROOT / name
           if not path.is_file():  # deleted but not yet committed
               continue
           size = len(path.read_bytes())
           assert size <= AGY_RULE_LIMIT, f"{name} is {size} bytes, over agy's {AGY_RULE_LIMIT}-byte limit"
   ```

   JavaScript (`node:test`; adapt to the repo's runner):

   ```js
   import { test } from "node:test";
   import assert from "node:assert";
   import { execFileSync } from "node:child_process";
   import { existsSync, statSync } from "node:fs";
   import { join } from "node:path";

   const AGY_RULE_LIMIT = 24_000; // bytes; agy truncates a rule file past this
   const ROOT = execFileSync("git", ["rev-parse", "--show-toplevel"], { encoding: "utf8" }).trim();

   test("rules files fit agy", () => {
     const names = execFileSync("git", ["ls-files", "-z", "*CLAUDE.md", "*AGENTS.md", "*GEMINI.md"],
       { cwd: ROOT, encoding: "utf8" }).split("\0").filter(Boolean);
     for (const name of names) {
       if (!existsSync(join(ROOT, name))) continue; // deleted but not yet committed
       const size = statSync(join(ROOT, name)).size;
       assert.ok(size <= AGY_RULE_LIMIT, `${name} is ${size} bytes, over agy's ${AGY_RULE_LIMIT}-byte limit`);
     }
   });
   ```

   A repo with no tests gets the check wherever it runs checks (CI, a pre-commit hook); with none of those, say so in the PR instead of adding a runner.
8. **Open one PR** following the repo's rules, so its reviewers check the meaning. The body gives each file's size before and after, the validator's output, what was merged as duplicate or moved to another document, and any drift between `CLAUDE.md` and `AGENTS.md` you left for the owner.

## Rules

- Only the instruction files, their imports, the size test and what you moved out of them change. No code, no other docs.
- Don't run the rewrite on a file a tool regenerates whole; leave it and say so.
