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
2. `agg.py` loads both and classifies sessions (`kind()`) with prices (`cost()`); the others `exec` it: `turns.py` (per user turn / per task), `reads.py` (protocol and file reads), `events.py` then `reread.py` (re-reads after compaction, writes `events.pkl`), `meshtext.py` (inter-agent text's share), `prose.py` (per kind and window: chars per call of prose, tool inputs and file edits, the part of output an output style can shorten), `prefix_repo.py` (first-call context per repo and kind), `window.py` (one window's output style share, output and context per call, first-call prefix; tasks, plans, questions, `failed`, message and file sizes, reviewers and root wakes per PR, run events; with `mesh cost --agents --json`'s file, $ and calls per task).
3. A window: `SINCE=2026-10-28 UNTIL=2026-11-04` (ISO, UTC) in the environment sets it for `turns.py`, `reads.py`, `meshtext.py` and `window.py` (`prose.py` takes `WINDOWS=A..B,C..D`); unset, each keeps its first round's start and no end. `reads.py` still picks sessions by file mtime. `meshtext.py`'s prefix line uses the first round's skill description and snippet sizes.

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

Savings are per week at the baseline's volume, in API-equivalent dollars. "Check" is how to tell from the data. Fill "Landed" with the date and commit, and "Actual" a week after. All ten landed on 2026-10-28, so their effects can't be told apart; "Actual" is from the week of 10-28 → 11-04 (and 11-04 → 11-07 11:00 where noted), sourced in "Check 1" below.

| # | Change | Expected | Check | Landed | Actual |
|---|---|---|---|---|---|
| 1 | Claude Code's built-in `Concise` output style, `"outputStyle": "Concise"` in `~/.claude/settings.json` (all sessions) | ~2–3% (output −13%, reports −31% in the e2e) | output tokens per call by kind; report and plan sizes in `reports/`, `plans/`; calls per task unchanged | 2026-10-28 | Prose −11..−32% per call by kind, but output per call up (mix); reports not shorter; ~$4/wk (~0.5%). See "Check 1" |
| 2 | Fewer, lighter reviews: one reviewer for a small PR, Claude plus one for a large one; `/code-review` at medium for small | $90–120 (6–8%) plus ~⅓ of agy's review spend | reviewer $ per PR; reviewers per PR; agy review calls | 2026-10-28, PR #75 | Reviewers per PR 1.55 → 1.24 (1.04 from 11-04); Claude review $/wk −50% with PRs −59%, so per reviewed PR +22% ($2.59 → $3.16); agy review calls per PR ×2.2. No per-PR saving |
| 3 | One root wake per review round (one fan-out, one answers file) | part of $125 doorbell turns (~1.4 wakes per PR) | root turns started by a reviewer's `done` per PR | 2026-10-28, PR #75 | Root wakes by a reviewer's `done` per PR 1.31 → 0.46 (0.22 from 11-04); root doorbell turns $124 → $31/wk, $0.18 → $0.13 per worker task |
| 4 | Root reset at 200k when idle and owed nothing, default on, warm or cold | $30–45 | root mean context per call; reset count | 2026-10-28, PR #76 | Resets 4 → 12/wk; root context per call in its run 215k → 176k (−18%); root $ per worker task $0.35 → $0.38 (flat) |
| 5 | `promptCacheTtl: "5m"` for reviewers, peers, askers, small/bg workers, headless `-p` | ~$50 (3%): peers −19%, headless −20%, reviewers −6% | `ephemeral_5m` share per kind; cost per kind | 2026-10-28, PR #76 | 5m share: reviewers 87%, small bg 98%, peers 100%, askers 48%, headless 0%; written tokens per call rose (reviewers +73%, small bg +138%), write $ per call +17% and +50%. No saving shown; small bg likely worse |
| 6 | Concise English: agy "Writing" rule, `make-concise` skill, then this repo's templates and protocol files, then each repo's rules files | ~2% from the text (base ~9%, −25% on ~8% of it) on top of #1; repo rules files are 3.7% | first-call prefix tokens per kind and repo; inbox body sizes | 2026-10-28, PR #73 (agy rule, make-concise); PR #77 (this repo's files: already dense, ~1–3% smaller, so only a "write briefly" line in the plan, report and harvest guidance); city-tour-app, curupira-bot, castanha-bot and ai-boilerplate cut their `CLAUDE.md` under 24 KB the same night ("Check 2") | No prefix or message-size drop measurable (prefix grew 3–8k with Claude Code); no quality loss. See "Check 2" |
| 7 | Shorter mesh skill descriptions (6.7k chars machine-wide) | ~$12 | static prefix size | 2026-10-28, PR #73 (6,586 → 4,440 chars) | Not measurable (prefix grew); the descriptions are 5,837 chars again |
| 8 | Usage limits: wait for Claude's own auto-continue in panes; mesh continues agy (and `--bg`) itself at the reset | two root turns per limit hit (rare) | root turns after an exit 7 | 2026-10-28, PR #76 | Not exercised: no `continue` sent, 2 alerts all week |
| 9 | `mesh cost` (per-agent spend) | none directly: makes this file's checks one command | — | 2026-10-28, PR #74 | Used for this check; its peer row and `reviewers_per_pr` look wrong (see "Check 1") |
| 10 | Quota reserve: mesh starts nothing new on a quota once it is 90% used (`MESH_QUOTA_STOP`), for Claude's windows and agy's models (from the user, 2026-10-28) | none: keeps ~10% of each quota for the user's own sessions | no mesh spawn, ask or approve after a window passed 90% (`reserve` events, the usage endpoint's figures) | 2026-10-28, PR #76 | Holds: 3 `reserve` events (6 more after 11-04); no Claude agent started inside the 11-04 09:02–09:50 five-hour reserve |

Total expected: roughly $250–350 a week, 15–20% of the baseline.

### Check 1: the week of 2026-10-28 → 11-04 (measured 2026-11-07)

Everything in the ledger landed on 10-28, in eight hours, and Claude Code moved from 2.1.312 to 2.1.317 during the week, so no figure below belongs to one change alone. The week's work also differed: a third of the tasks and output-heavy city-tour-app UI tasks (file-edit chars per large-worker call ×3). Per-call and per-task figures carry that mix; the 3.5 days after (11-04 → 11-07 11:00, before run me608) are given beside them as a second sample.

**Commands**, from an empty scratch directory (`S=<repo>/docs/cost-research/scripts`):

```bash
mesh cost --since 2026-10-21 --until 2026-10-28 --agents --json > cost-b.json   # baseline; also without --json for the table
mesh cost --since 2026-10-28 --until 2026-11-04 --agents --json > cost-a.json
mesh cost --since 2026-11-04 --until 2026-11-07T11:00 --agents --json > cost-l.json
python3 $S/extract.py && cp $S/*.py .
SINCE=2026-10-21 UNTIL=2026-10-28 SKIP_RUNS=me608 python3 window.py cost-b.json   # and the other two windows
SINCE=2026-10-21 UNTIL=2026-10-28 python3 turns.py; SINCE=2026-10-21 UNTIL=2026-10-28 python3 meshtext.py   # and 10-28 → 11-04
WINDOWS=2026-10-21..2026-10-28,2026-10-28..2026-11-04,2026-11-04..2026-11-07T11:00 python3 prose.py
```

**Totals** (`mesh cost`): $1,527 → **$764** a week (−50%), $523 in the 3.5 days after (a $1,040/wk pace). Volume fell more: runs started 68 → 20, agents spawned 361 → 98, worker tasks 672 → 234 (192 in the 3.5 days). Mesh $ (every agent of a run) per worker task **$1.63 → $1.99** (+22%), $1.97 after 11-04. So per task, the week cost more, not less.

| `mesh cost` kind | $ | calls | $/call | ctx/call | out/call | 5m % | prefix | after 11-04: $/call, ctx, out |
|---|---|---|---|---|---|---|---|---|
| worker large pane | 460 → 244 | 6,399 → 2,286 | 0.072 → 0.107 | 179k → 238k | 797 → 1,426 | 0 → 0 | 50k → 53k | 0.069, 196k, 646 |
| user | 270 → 199 | 3,801 → 2,043 | 0.071 → 0.097 | 133k → 237k | 712 → 755 | 0 → 0 | 52k → 55k | 0.110, 227k, 561 |
| root | 233 → 89 | 3,366 → 1,327 | 0.069 → 0.067 | 215k → 176k | 530 → 553 | 0 → 0 | 60k → 56k | 0.060, 183k, 382 |
| reviewer | 212 → 81 | 3,408 → 982 | 0.062 → 0.083 | 113k → 141k | 702 → 1,261 | 0 → 87 | 49k → 53k | 0.056, 116k, 667 |
| reviewer subagents | 101 → 76 | 1,636 → 756 | 0.062 → 0.101 | 88k → 99k | 944 → 1,831 | 100 | — | 0.058, 102k, 844 |
| worker small bg | 71 → 42 | 1,048 → 474 | 0.067 → 0.088 | 139k → 141k | 658 → 1,017 | 0 → 98 | 49k → 55k | 0.071, 143k, 620 |
| peer | 54 → 2 | 683 → 15 | — | — | — | 0 → 100 | — | — |
| asker | 51 → 5 | 566 → 79 | 0.090 → 0.066 | 158k → 172k | 686 → 703 | 0 → 48 | 53k → 75k | — |
| headless | 33 → 5 | 434 → 54 | 0.076 → 0.085 | 99k → 83k | 1,294 → 1,066 | 0 → 0 | 99k → 52k | — |

`mesh cost`'s peer row is not the peers' spend: `classify.py` finds 353 peer-asker calls ($33, `turns.py`) that week, so resumed peer sessions land elsewhere (likely `user`), and `reviewers_per_pr` reads 1.0 in every window. Its `user` row also holds a root's calls outside its run's window: by `classify.py` roots were $375 → $234 and plain users $125 → $53.

**#1 Concise style** (`window.py`, `prose.py`): the query source (`repl_main_thread:outputStyle:Concise`) shows it on 88% of root calls, 99% of large workers', 95% of reviewers', 81% of small bg workers' and 88% of plain users' that week (100% after 11-04, small bg 53%); never on subagents, and on 4% of peers' (sessions from before 10-28). Prose chars per call (text blocks; thinking is redacted, so it is in output tokens but not here):

| Kind | Prose | Tool input (file edits) | Out tokens |
|---|---|---|---|
| root | 234 → 208 → 165 | 762 (127) → 767 (82) → 534 (19) | 628 → 661 → 493 |
| large pane worker | 103 → 70 → 67 | 1,221 (209) → 2,581 (641) → 898 (118) | 806 → 1,418 → 642 |
| small bg worker | 107 → 85 → 51 | 946 → 1,420 → 1,036 | 643 → 917 → 627 |
| reviewer | 123 → 110 → 117 | 662 → 1,008 → 621 | 700 → 1,244 → 664 |
| plain user | 244 → 168 → 361 (n=51) | 786 → 1,188 → 402 | 640 → 837 → 498 |

Prose is 5–15% of output tokens, so a 10–35% cut moves output per call by 1–4%, under the mix's swing. Priced as output plus its later cache writes and reads (Δtokens × ($20 + $8 + $0.20 × the session's remaining calls)), the week's shorter prose saved **~$3.7** (roots $1.5, large workers $1.2, users $0.7): ~0.5%, against 2–3% expected. Written files didn't shrink: large-worker `done` median 2,434 → 3,029 chars (2,444 after 11-04), small 1,792 → 2,130 (1,429), plans 11.7k → 13.5k (13.6k), reviewer `done` 5.3k → 7.6k (6.4k), `reports/` files median 3,870 → 4,247 bytes (3,124). Calls per task: large 28.1 → 30.1 (36.0), small 16.8 → 16.1 (23.7). The e2e's −13% output and −31% report came from a short task whose output was mostly prose; mesh workers' output is mostly tool input.

**#2 reviews** (`window.py`, PRs keyed by run, repo and number): 121 → 50 reviewed PRs (49 after 11-04); reviewer agents per PR 1.55 → 1.24 → 1.04 (2+ reviewers: 54 → 12 → 2 PRs); review tasks by CLI claude 140 / agy 74 / copilot 59 → 55 / 37 / 11 → 40 / 41 / 2. Claude review $ (reviewers plus their subagents) $313 → $158 a week, **$2.59 → $3.16 per reviewed PR** ($2.34 after 11-04); `mesh cost`'s $ per PR with a Claude reviewer $3.37 → $5.26 ($4.99), as Claude now reviews mostly large PRs. The `/code-review` subagents' output per call doubled (944 → 1,831). agy review calls per reviewed PR 18 → 40 (35), read tokens 182M → 226M a week: agy took the small PRs alone. Numbered findings per reviewer `done` 1.7 → 3.9 → 4.4, but PR #75 changed the review format, so not comparable.

**#3 one wake per round:** root wakes by a reviewer's `done` (`rung`/`native` to the root) per PR 1.31 → 0.46 → 0.22, `collected` (no wake) 0 → 1.26 → 1.41. Root turns started by a doorbell (`turns.py`, `classify.py` kinds) $124 (1,859 calls) → $31 (454): $0.18 → $0.13 per worker task.

**#4 root reset:** 4 → 12 resets a week (7 in the 3.5 days after). Root context per call in its run 215k → 176k (−18%), 183k after; by `classify.py` (whole root sessions) 189k → 230k → 220k. Root $ per worker task $0.35 → $0.38 → $0.33.

**#5 5m TTL:** the 5m share reached reviewers (87%, 100% after 11-04), small bg workers (98%, 100%), peers (100%) and askers (48%); headless stayed 0% (3 sessions, none mesh's). Cache-written tokens per call rose: reviewers 3,268 → 5,655 (4,143 after), small bg 3,384 → 8,066 (5,879) at an unchanged context (139k → 141k), so pauses over 5 minutes re-write the prefix more than the cheaper write saves: write $ per call reviewers $0.026 → $0.031 ($0.021), small bg $0.027 → $0.041 ($0.029).

**#8 limits:** no `continue` sent or `continue_due` set in either window; `alert` events 13 → 2 (1 after 11-04).

**#10 reserve:** 3 `reserve` events that week, 6 after (Claude's five-hour window on 11-04 09:02 and 09:07, reset 09:50; agy flash, argon and opus models). No Claude agent started between 09:02 and 09:50 (the next resume was 09:51); agy kept starting on models with quota.

### Check 2: Concise English (#6), the week before and after 2026-10-28

Same windows and commands as "Check 1", plus `WINDOWS=2026-10-21..2026-10-28,2026-10-28..2026-11-04,2026-11-04..2026-11-07T11:00 python3 prefix_repo.py` (first-call context per repo and kind). Beyond this repo's PRs #73 and #77, four repos cut their root `CLAUDE.md` under agy's 24,000-byte limit the same night: city-tour-app 48,711 → 23,634 bytes (585605f, 01:57 −03:00), curupira-bot 39,916 → 23,904 (4075e82), castanha-bot 47,852 → 23,892 (66eb08c), ai-boilerplate 43,952 → 23,690 (1089f5c). agent-mesh's grew 16,543 → 20,213 with that week's docs. It all landed with #1–#10, so nothing below is #6's alone.

- **Prefix (median first-call context):** by `mesh cost` kind, large workers 50k → 53k (57k after 11-04), roots 60k → 56k (59k), reviewers 49k → 53k (54k), small bg 49k → 55k (58k). By repo, city-tour-app's roots fell 64k → 55k (n=7, 6), about the ~9k tokens its cut removed, while its workers rose 49k → 55k (n=21, 31) and reviewers 49k → 54k. Claude Code went 2.1.312 → 2.1.319 over the two weeks and the prefix grew 3–8k with it, so the cut shows only where the rules file was most of the change. A city-tour-app worker's first call was 47.5k under the 48.7k-byte file, the same as agent-mesh's under 16.5k: the worktree workers' first call doesn't seem to carry the rules file at all, which is unexplained (see "Not established").
- **Output per call:** see "Check 1": prose per call −11..−32%, output tokens per call up with the week's mix (large workers 806 → 1,418, 642 after 11-04).
- **Calls and $ per task** (`window.py`): large change tasks 28.1 → 30.1 calls (36.0 after 11-04), $2.00 → $3.21 ($2.49); small 16.8 → 16.1 (23.7), $1.13 → $1.36 ($1.71); review tasks 10.4 → 9.5 (11.4), $0.65 → $0.79 ($0.64); root $ per worker task $0.35 → $0.38 ($0.33).
- **Message sizes** (median body chars, mesh's header stripped): written by roots, large tasks 2,088 → 2,328 (1,851), small tasks 2,285 → 1,619 (1,537), answers 408 → 564 (529); written by workers, large `done` 2,434 → 3,029 (2,444), plans 11,689 → 13,489 (13,567), small `done` 1,792 → 2,130 (1,429), reviewer `done` 5,331 → 7,648 (6,388), peer `done` 13,361 → 10,865 (n=32, 13). Inter-agent text (`meshtext.py`): agent-written 2.4% → 1.8% of spend, mesh-written 3.5% → 3.4%.
- **Quality:** change tasks 331 → 117 (104 after 11-04); per task, plans 0.27 → 0.19 (0.20), questions 0.02 → 0.01 (0), `failed` 1 → 2 (0); plans sent back (a root answer other than `approved`) 13 of 98 answers → 2 of 24 (0 of 21); watch `nudge`s 9 → 4 (6). Review findings aren't comparable (PR #75 changed the format). No loss visible.

**Result:** no measurable saving from the text itself. The rules-file cuts are real (~9k tokens off city-tour-app's roots; curupira-bot's roots 73k → 62k, n=1 and 4; too few castanha-bot and ai-boilerplate sessions to tell), but Claude Code's own prefix growth hides them in the per-kind medians, and the messages agents write didn't get shorter. The expected ~2% isn't confirmed or ruled out.

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
- Whether a worker's first call in a mesh worktree carries the repo's rules file: city-tour-app's and agent-mesh's workers both started at ~47.5k on 2026-10-21 → 10-27, under 48.7k and 16.5k-byte files ("Check 2").
