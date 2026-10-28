# Cost research

Where this machine's Claude Code and agy quota goes, what we decided to change, and what we expect each change to save. It exists so a later round can start from these numbers instead of re-deriving them, and so each expectation gets checked against what actually happened.

The first round ran on 2026-10-28 (run `mb3c1`). Its reports are under `~/.local/state/agent-mesh/mb3c1/reports/` (under `archive/mb3c1/` after `mesh stop`), and its raw per-call data under `data/` there.

## How to use this file

- **Checking an expectation:** re-run the scripts (below) over the week after a change landed, compare with the baseline, and fill the "Actual" column of the ledger. Say what else changed that week: several changes landing together can't be told apart, so note them.
- **A new round:** start from the baseline, the ledger and "Settled" below. Don't re-propose what is settled unless new evidence changes it, and say what that evidence is.
- The askers keep their context and can be asked again: `mesh ask agent-mesh-cost-research` (the measurements), `mesh ask agent-mesh-lang-research` (language, output styles, tools), and the `orchestrators` peers (`mesh ask --peers ccswarm-research,gastown-research,firstmate-research,rove-research,repowire-research`).

## Method

- **Source:** every Claude Code transcript in `~/.claude/projects/*/*.jsonl` and `*/*/subagents/*.jsonl` (819 transcripts, 29,881 API calls, 2026-09-16 → 10-28). Each assistant record's `message.usage` gives `input_tokens`, `cache_creation_input_tokens` (split into `ephemeral_1h` / `ephemeral_5m`), `cache_read_input_tokens` and `output_tokens`.
- **Dedup by `message.id`:** Claude Code writes one record per content block (thinking, tool use, text), each with the same usage. Summing per record triples the totals (rove #942, Gas Town PR #1772). The ids must be deduped across files too: a background swap or resume copies the conversation into a new session file with the same ids, and so does a `/resume` that forks. The first round's `extract.py` deduped per file and over-counted by 7.3% (2026-10-21 → 10-28: $1,639 per file, $1,527 across files); it now shares one `seen`, so a copied call is counted in whichever file `glob` lists first, and `mesh cost` gives it to the file run state names.
- **Classification:** a session is mapped to an agent through every `run.json` (live and archived runs: role, size, view, `read_only`, `peer`, `review_heads`), else by project directory (mesh worktrees, peer worktrees) and its first message.
- **Weighting:** API-equivalent dollars stand in for what the subscription limit counts, which is undocumented. Opus 5.5 per MTok: input $4, 1h cache write $8 (2×), 5m write $5 (1.25×), cache read $0.20 (0.05×), output $20. Sonnet 5.5: $2 / $4 / $2.50 / $0.20 / $10 (from the `claude-api` skill). So a written token costs 40 reads, an output token 100. Every main session writes 1h cache; subagents write 5m. The ranking holds as long as writes and output cost more than reads.
- **agy:** token counts only, from the step metadata in agy's conversation files (as `agy_context_tokens` reads them). Copilot not measured.

### Scripts

**`mesh cost` first** (design §19): `mesh cost --since <start> --until <end>` gives the week's total and, per session kind, sessions, calls, $, share, $ per call, context and output per call, the 5m share of cache writes, the median first-call context (the static prefix) and cold resumes; then reviews ($ per PR, Claude reviewers per PR), root resets and agy tokens by kind (`--agents` for per agent, `--json` for the rows, `mesh cost <run>` for one run with its plan/exec split). That answers the ledger's checks #1, #2 (Claude side), #4 (root context per call, resets), #5, #6 and #7. Over this baseline's week it gives $1,527, the study's own per-call data deduped across files (the $1,614 below is inflated by copied sessions, ~$1,504 corrected: +1.5%). Its kinds differ from the study's where it splits by time: a root's calls outside its run's window count as `user`, and copied calls go to the agent whose file they came from. The scripts below are for what it doesn't do: per turn and per task, reads, re-reads after compaction, inter-agent text, effort.

In [`scripts/`](scripts/), stdlib Python, run from an empty scratch directory (they read and write files in the current directory):

1. `python3 <repo>/docs/cost-research/scripts/extract.py`: writes `msgs.jsonl` (one row per deduped API call: session, time, model, effort, query source, input, cache write 1h/5m, cache read, output) and `sessions.jsonl` (one per transcript: project, entrypoint, first user message, compactions, mesh commands used). Copy `classify.py` and `agg.py` beside them.
2. `agg.py` loads both and classifies sessions (`kind()`) with prices (`cost()`); the others `exec` it: `turns.py` (per user turn / per task), `reads.py` (protocol and file reads), `events.py` then `reread.py` (re-reads after compaction, writes `events.pkl`), `meshtext.py` (inter-agent text's share).

`sessions.jsonl` holds each session's first message, so the data stays out of git. The 2026-10-28 snapshot is in the run's `data/cost/`, and the language study's texts, counts and e2e runs in `data/lang/`.

[`scripts/lang/`](scripts/lang/) is the language study: `count.sh` counts a text's tokens with a lean headless call (`claude -p --system-prompt "Reply tersely." --tools "" --strict-mcp-config --setting-sources "" --effort low`, whose base is a constant 610–614 tokens, so text tokens are exact), `gemma.py` counts with Gemma 3's tokenizer as a Gemini proxy, `e2e*.sh` run the end-to-end task (`WORK` is the work directory, default the current one), `e2e_check.py` is its 23-case checker and `e2e_analyze.py` prices the runs.

## Baseline: the week of 2026-10-21 → 10-27

**$1,614** API-equivalent as measured, **~$1,504** corrected for copied sessions (see "Dedup" above: −7.3%, which also lowers the kind rows below, `worker small bg` most of all; `mesh cost --since 2026-10-21 --until 2026-10-28` gives the corrected split) (89% of the 14-day total, $1,810), 90% of it mesh; the last 46 h alone were $893, so the pace was still rising. Usage at the time (`cachedUsageUtilization` in `~/.claude.json`): one `five_hour` and one weekly window; `seven_day_opus` and `seven_day_sonnet` null, so Sonnet draws the same bucket as Opus on this account.

- **Spend split:** 42% cache reads, 37% cache writes, 21% output.
- **Cost goes per call:** $0.06–0.08 per call in every kind; cost is context size × number of calls.
- **Static prefix:** ~55k tokens on every call before any work (median first call 48–55k, flat since 10-14). Of that, ~24k is unused by coding agents: the `Artifact` tool ~14k, claude.ai skill listing ~3.8k, claude.ai connectors ~2.8k, global `todoist` MCP ~2k.
- **Repo rules files** (`CLAUDE.md` / `AGENTS.md` with `@` imports) are 3.7% of the week (~$60): city-tour-app 48k chars ($27/wk), agent-mesh 16.5k ($20), career-ops 88k through `@AGENTS.md` ($5), walklore-landing 21k ($4).
- **Inter-agent text** is ~5.5% (mesh-written 3.3%: protocol and skill reads 1.6%, mesh command output 0.8%, prefix 0.6%, doorbells 0.3%; agent-written 2.2%: inbox bodies 1.4%, writing them 0.5%, plan/report files 0.3%).

| Session kind | $/wk | % | Sessions | Avg ctx/call | Notes |
|---|---|---|---|---|---|
| Large pane workers | 461 | 29 | 67 | 175k | 28% writes, 49% reads, 23% output |
| Mesh roots | 381 | 24 | 40 | 186k | 81% at high; doorbell turns $125, watch exits $33, user turns $223 |
| Claude reviewers | 190 | 12 | 91 | 108k | all at high |
| Their `/code-review` subagents | 98 | 6 | 92 | 86k | |
| Small bg workers | 125 | 8 | 35 | 130k | |
| Plain user sessions | 125 | 8 | — | 145k | |
| Peer askers | 97 | 6 | 21 | 151k | cold resumes |
| Other (unmapped workers, askers, headless `-p`, other subagents) | 137 | 8 | — | — | |

**Reviews** were 22% of Claude spend after the tiny-PR rule (17ef37b, 10-26): 50 PRs, $59 small (26 reviewers) and $134 large (25); $3.23 per PR over 14 days ($2.2 small, $4.5 large), 34% of it in `/code-review`'s subagents. Reviewer `done`s per PR: 1 (21 PRs), 2 (32), 3 (25), 4 (8), 5+ (5).

**agy since 10-14** (tokens; all its limits read 0% at the time):

| Kind | Conversations | Calls | Input | Output |
|---|---|---|---|---|
| Reviews | 55 | 3,761 | 324M | 2.88M |
| Mesh workers | 26 | 2,326 | 209M | 0.84M |
| User projects | 37 | 2,226 | 218M | 0.89M |
| Other | 22 | 502 | 49M | 0.25M |

## Ledger: changes and expected savings

Savings are per week at the baseline's volume, in API-equivalent dollars. "Check" is how to tell from the data. Fill "Landed" with the date and commit, and "Actual" a week after.

| # | Change | Expected | Check | Landed | Actual |
|---|---|---|---|---|---|
| 1 | Claude Code's built-in `Concise` output style, `"outputStyle": "Concise"` in `~/.claude/settings.json` (all sessions) | ~2–3% (output −13%, reports −31% in the e2e) | output tokens per call by kind; report and plan sizes in `reports/`, `plans/`; calls per task unchanged | 2026-10-28 | |
| 2 | Fewer, lighter reviews: one reviewer for a small PR, Claude plus one for a large one; `/code-review` at medium for small | $90–120 (6–8%) plus ~⅓ of agy's review spend | reviewer $ per PR; reviewers per PR; agy review calls | 2026-10-28, PR #75 | |
| 3 | One root wake per review round (one fan-out, one answers file) | part of $125 doorbell turns (~1.4 wakes per PR) | root turns started by a reviewer's `done` per PR | 2026-10-28, PR #75 | |
| 4 | Root reset at 200k when idle and owed nothing, default on, warm or cold | $30–45 | root mean context per call; reset count | | |
| 5 | `promptCacheTtl: "5m"` for reviewers, peers, askers, small/bg workers, headless `-p` | ~$50 (3%): peers −19%, headless −20%, reviewers −6% | `ephemeral_5m` share per kind; cost per kind | | |
| 6 | Concise English: agy "Writing" rule, `make-concise` skill, then this repo's templates and protocol files, then each repo's rules files | ~2% from the text (base ~9%, −25% on ~8% of it) on top of #1; repo rules files are 3.7% | first-call prefix tokens per kind and repo; inbox body sizes | 2026-10-28, PR #73 (agy rule, make-concise; templates and repos pending) | |
| 7 | Shorter mesh skill descriptions (6.7k chars machine-wide) | ~$12 | static prefix size | 2026-10-28, PR #73 (6,586 → 4,440 chars) | |
| 8 | Usage limits: wait for Claude's own auto-continue in panes; mesh continues agy (and `--bg`) itself at the reset | two root turns per limit hit (rare) | root turns after an exit 7 | | |
| 9 | `mesh cost` (per-agent spend) | none directly: makes this file's checks one command | — | 2026-10-28, PR #74 | |
| 10 | Quota reserve: mesh starts nothing new on a quota once it is 90% used (`MESH_QUOTA_STOP`), for Claude's windows and agy's models (from the user, 2026-10-28) | none: keeps ~10% of each quota for the user's own sessions | no mesh spawn, ask or approve after a window passed 90% (`reserve` events, the usage endpoint's figures) | 2026-10-28, PR #LIFECYCLE | |

Total expected: roughly $250–350 a week, 15–20% of the baseline.

## Settled: measured and not worth doing (don't re-propose without new evidence)

- **Compacting workers earlier** (`autoCompactWindow` 250k or 200k, or a lower `COMPACT_AT`): after a compaction a large worker re-read 53% of its reads (11 per window vs 2.7 uncompacted) and took 43 calls to `done` vs 20. Net −$25..+$13/wk at 250k, −$82..+$4 at 200k. Keep 300k (design §12). Roots differ (their re-reads are reports and status): +$43 at 250k, +$54 at 200k, hence ledger #4. n=4 worker compactions.
- **Peers between asks:** today's retire then cold resume costs $2.22 per ask ($1.04 of it the first call, ~170k context). Compacting warm before retire nets −$1.0..+$0.45 per ask and loses the research; a fresh peer per round costs $2.80 (+26%) and loses the depth. A follow-up does half a first brief's reads (8.8 vs 18). Gaps between asks: 5 of 22 within 20 minutes, the rest 1.8–50 h.
- **Claude Code's idle compaction** (`idleCompaction`, flag-gated, fires at 90% of a 1h TTL above 200k): never fired here; on would save ≤ $7/wk and loses detail on sessions that never resume. Left off. It can't run on a retired session anyway.
- **5m TTL for roots and large workers:** +7% and +8% (95–96 gaps a week of 5–60 minutes each). User sessions: −4% only, unpredictable pauses: stay 1h.
- **Effort:** the binary's `effort_cost_index` for Opus 5.5 is low 0.48 / medium 0.66 / high 1 per task (Sonnet 5.5: 0.49 / 0.54 / 1), but measured per turn or task: roots high vs medium ×1.12 (~$38/wk if roots went medium), user sessions no difference ($0.56 vs $0.57 per turn), small workers no saving, peers −15% at medium (n=8). Reviewers never ran at medium. Output per call is the same at both. The user kept `modelSettings` at high for now (2026-10-28).
- **Sonnet for peers and askers:** same weekly bucket as Opus here; ~29% cheaper per token (reads cost the same), ~$34/wk. Design §9's sweep: Sonnet at high ≈ Opus at low (87.6% vs 87.4%), so Opus at low is the equal-quality alternative. Not adopted for now (2026-10-28).
- **Trimming the static prefix** (`enableArtifact: false`, `disableClaudeAiConnectors: true`, the claude.ai skill listing, `todoist` per project): ~$134/wk (8.3%), ~$90 if only for mesh agents. Declined by the user (not worth losing them).
- **Workers' unused tools** (`AskUserQuestion`, `SendFeedback`, `ScheduleWakeup` in `--disallowedTools`, ~5.9k tokens): ~$20/wk. Declined: workers stay full Claude sessions. No worker ever called `ScheduleWakeup` or `CronCreate`.
- **Already fine:** the approve swap keeps the cache (all 64 high→medium swaps read warm); auto-retire at 15 minutes vs the 1h TTL (99 of 102 resumes within the hour read warm); workers read `ORCHESTRATE.md` in sections, never whole (0 of 272 worker transcripts); Stop-hook nudges ~$1/wk; watch-exit root turns $33/wk.
- **Prompt-cache keepalive:** rejected in run `m6f06` (`archive/m6f06/reports/agent-mesh-cache-keepalive.md`).

## Language

Same mesh text (protocol excerpt plus a report finding, 2,855 chars, identifiers and commands kept English), exact token counts:

| Text | Claude 5.5 (Opus = Sonnet) | vs EN | Gemma 3 (Gemini proxy) | vs EN |
|---|---|---|---|---|
| English | 1,073 | — | 805 | — |
| Terse English | 832 | −22% | 573 | −29% |
| Chinese | 1,296 | +21% | 878 | +9% |
| Japanese | 1,397 | +30% | 932 | +16% |
| Spanish | 1,395 | +30% | 881 | +9% |
| Portuguese (BR) | 1,373 | +28% | 897 | +11% |
| Russian | 1,449 | +35% | 987 | +23% |
| German | 1,669 | +56% | 946 | +18% |
| Korean | 1,742 | +62% | 997 | +24% |

- "Chinese is cheaper" is about characters (half as many), not tokens: Claude packs ~2.7 English chars per token and ~1.2 Chinese. It may hold on Chinese-built tokenizers (Qwen, DeepSeek), which we don't run.
- Anthropic's MMLU figures put every language 2–3 points below English. Claude Code's `language` setting makes agents write that language, code comments included.
- **End-to-end** (add `parse_size` to `python-humanize/humanize` with tests; Opus at medium; 3 runs each; 23-case checker):

| Task | $/run | Calls | Output | Report tokens | Checker |
|---|---|---|---|---|---|
| English | 0.557 (0.52–0.62) | 12.0 | 9,164 | 1,349 | 23/23 ×3 |
| Chinese | 0.570 (+2%) | 12.7 | 9,469 | 1,541 (+14%) | 23/23 ×3 |
| Terse English | 0.477 (−14%) | 9.7 | 6,856 (−25%) | 791 (−41%) | 23/23 ×3 |
| English, `Concise` output style | 0.540 (−3%) | 12.3 | 8,008 (−13%) | 931 (−31%) | 23/23 ×3 |

  A Chinese task makes the agent write Chinese (reports ~47% CJK, unasked, 14% more tokens). A terse task makes it terse, partly by skipping extra checks; the `Concise` style shortened the writing with the same calls and checks, which is why it was adopted over terse tasks.

- **Tools surveyed** (2026-10-28): the built-in `Concise` style (adopted); caveman (skip: its rules exempt files and agent-to-agent messages; independent tests found 4% over a plain "be concise" line); caveman-compress and squeez (skip: drop connectives, hedges and examples, where rules keep their "why"; caveman-compress's before/after validator is worth borrowing); Anthropic's `claude-md-management` (skip: additive, ignores `AGENTS.md`); token-optimizer (skip: hooks and a daemon, overlaps this study); agnix (lint only). agy has no concise mode.

## What the peers taught (2026-10-28 round)

`reports/peers-20261028T020813337474Z.md` in the run. The useful lessons, beyond the ledger:

- **Measure first.** ccswarm's budget cap counts bytes/4 of prompt text and misses most real spend; its "93% saving" was never measured and was later deleted. Gas Town's ledger overcounted cache tokens 1.76–3.26× until it deduped by `message.id`, and dropped multi-MB lines with a 64 KB buffer.
- **The expensive pattern is a large session resumed cold.** firstmate #7755: cache writes ~60% of spend, the supervisor ~20%, all workers plus reviews ~6%. Gas Town #4626: two leftover sessions at 456k and 249k cost $49 of a $113 day.
- **Never wake an LLM on a timer.** Gas Town's Boot triage agent ran every daemon tick until mechanical gates saved ~480 sessions a day; firstmate had ~21 no-op wake turns in 90 minutes. Mesh's watch, retirer and nudge are Python: keep it that way.
- **A per-worker self-poll can run unseen** (firstmate #3061: 131 no-op `/loop` ticks over two days). Not seen here.
- **Routing by an LLM barely beats a constant** (rove's classifier: 72.7% vs 48.9% for always `standard`); per-role downgrades in Gas Town apply to supervisors, never workers.

## Not established

- How the subscription limit weighs each token class (API prices used as the proxy).
- The quality cost of any change beyond the small e2e checks; the `Concise` style on harder tasks and on roots.
- agy's quota formula and its real tokenizer (agy was out of quota); Copilot.
- `--bg` workers and Claude's auto-continue; a headless `/compact` reading a session's warm cache.
- Token counts in the cost study are chars ÷ 2.7 where not counted exactly.
