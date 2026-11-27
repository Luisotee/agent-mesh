# Model tiers per role, reviewers, and the root's weight (2026-11-26)

A research round on three questions: which model tier each role (orchestrator, planner, executor, reviewer) should run on to be good while cheap; whether the reviewer setup (the other CLI plus one OpenCode free model per PR) is the right one; and where the root's tokens go. It builds on `README.md` in this folder (ledger #4, #11, #12 and "Settled"): don't re-propose what is settled there without new evidence.

Our numbers are from 2026-11-25 onwards only: the mesh changed a lot on 11-24 and 11-25. The web evidence was gathered by Sonnet subagents. Every benchmark below is vendor-run unless marked otherwise.

## Answer

| Role | Tier (Claude names) | Why |
|---|---|---|
| Orchestrator (root) | Strong (Opus), kept lean | It decomposes the work and judges the results. A weak manager over a weak worker scored 42% on SWE-bench Lite, below the weak model working alone (44%). A strong manager over a weak worker scored 62%, above the strong model alone (60%) ([arXiv 2603.26458](https://arxiv.org/abs/2603.26458), 2026-03). Its cost is context × calls, not its tier, so the lever is its weight (below). |
| Planner | Strong (Opus) | A plan is short and decides the outcome; a bad plan does worse than none ([arXiv 2604.12147](https://arxiv.org/abs/2604.12147), 2026-04). In Aider's tests a strong architect with a cheap editor matched or beat the strong model alone ([Aider](https://aider.chat/2024/09/26/architect.html), 2024-09; [R1 + Sonnet](https://aider.chat/2025/01/24/r1-sonnet.html), 2025-01). Ours: Opus planning cost $21 of $772. |
| Executor | Mid (Sonnet) by default; Haiku for mechanical, fully specified edits; Opus only for ambiguous or risky work | Most tokens are spent here. Large workers were 46% of the spend. The pairing pays only when the plan is concrete. Measure cost per solved task, not per token: a cheaper model that reads 1.9× more can cost more ([Arize](https://arize.com/blog/how-cheap-models-changed-multi-agent-economics/), opinion). |
| Reviewer | Mid (Sonnet / agy hydrogen) from another family than the author; strong for large or risky PRs | Sonnet 5.5 is within 2 F1 points of Opus 5.5 on Qodo's benchmark (54.9% vs 56.8%) at 73% of the cost ([Qodo](https://qodo.ai/ai-code-review-benchmark/), 2026-09/10). A weak extra reviewer adds findings the root must verify (below). |

Execution already follows this split: since 11-25, 12 of the 17 large workers executed on Sonnet. The biggest ones ran on Sonnet all the way through: `daemon-core` $65, `switchover` $39, `lean-sessions` $38 and `desktop` $33. Opus ran only in planning, except for `agent-set` ($7, all Opus). Ledger #11 has landed in practice.

## Our spend, 2026-11-25 → 11-26

`mesh cost --since 2026-11-25`: **$771.58** at API prices.

| Kind | $ | % |
|---|---|---|
| Large workers (pane + subagents) | 356 | 46 |
| Root (+ subagents) | 162 | 21 |
| User sessions | 136 | 18 |
| Claude reviewers (+ subagents) | 64 | 8.3 |
| Small workers, peers, askers, other | 53 | 6.9 |

- The Claude reviewers ran 33 sessions at about $1.90 each. Half of that is the subagents `/code-review` starts.
- OpenCode ran 52 reviews and agy 16; neither has a dollar price.

## Reviewers

### Our verdicts (all recorded verdicts; every one is from 11-25 onwards)

| Reviewer model | Reviews | Accepted | Rejected | Rejected % | Blocking |
|---|---|---|---|---|---|
| Sonnet | 14 | 72 | 12 | 14 | 3 |
| gemini hydrogen (high) | 11 | 38 | 4 | 9 | 6 |
| Opus | 7 | 56 | 5 | 8 | 15 |
| gemini neon (high) | 2 | 12 | 0 | 0 | 7 |
| opencode/beam-free | 12 | 8 | 16 | 67 | 0 |
| opencode/flint-40b-free | 11 | 15 | 12 | 44 | 1 |
| opencode/deepseek-v4.1-flash-free | 3 | 10 | 1 | 9 | 2 |

On the same PRs the free models found little:
- On #150, #151 and #154, Sonnet had 7–9 findings accepted per review and beam-free 0–1.
- On #113, Opus had 8 accepted and beam-free 1.
- On #134 and #136, hydrogen had 4–5 accepted, while flint had 0 accepted and 4 rejected.

A rejected finding is not free: the Opus root reads and checks it.

These numbers are not randomized. Opus reviewed the large PRs, which inflates its blocking count. DeepSeek's n is 3.

### Evidence

- **A second reviewer helps only if it is strong.** In a controlled study (900 sessions, 150 planted errors), one same-model plus one cross-model review found 56.7% of the errors, against 42.7% for two same-model reviews. A lightweight cross-model reviewer did no better than a same-model one. A fresh same-model review alone had F1 no different from a cross-model one ([arXiv 2610.01471](https://arxiv.org/abs/2610.01471), 2026-10).
- **Findings across models overlap only partly.** The cross-family Jaccard was about 0.37 ([Zenodo preprint](https://zenodo.org/records/20519585), 2026-06, one organisation, not peer reviewed).
- **A weaker reviewer anchors on the author.** Review beat the stronger model alone in only 9 of 96 generator-reviewer directions, at 2.6–2.8× the cost ([arXiv 2610.15897](https://arxiv.org/abs/2610.15897), 2026). This was answer verification, not diff review.
- **More reviewers raise false alarms as well as coverage.** A validator is what cuts them ([arXiv 2505.17928](https://arxiv.org/abs/2505.17928); [SWR-Bench](https://arxiv.org/pdf/2509.01494)). Anthropic reports under 1% of its Code Review findings marked incorrect, from verification, not from more reviewers ([Claude blog](https://claude.com/blog/code-review), own figure).
- **Higher effort trades precision for recall.** In Qodo's benchmark, raising reasoning effort raised recall and lowered precision.

### OpenCode's free models (Zen listing fetched 2026-11-26)

| Model | What it is | Vendor numbers | As reviewer |
|---|---|---|---|
| `deepseek-v4.1-flash-free` | DeepSeek V4.1 Flash, 552B MoE, 8–16B active | Terminal-Bench 2.1 90.6, DeepSWE 74.2 ([arXiv 2609.19969](https://arxiv.org/pdf/2609.19969)) | **Keep.** Best benchmarks and our best free verdicts (n=3). Zero retention, no training. |
| `mimo-v2.6-flash-free` | Xiaomi MiMo-V2.6-Flash, ~309B MoE, 15B active | none found for V2.6 | Maybe, as a trial only |
| `beam-free` | Reflection beam-1.0, 501B MoE, 23B active | SWE-bench Verified 81.6 (model card) | **Drop.** Strong benchmarks, but 67% of its findings were rejected here. |
| `flint-40b-free` | Recursive Flint, 40B MoE, 2.5B active | SWE-bench Verified 74.2 | **Drop.** Too small; 44% rejected. Its prompts may be used for training. |
| `hy4-flash-free` | Tencent Hy4 Flash, 12B active | Vals Index rank 33 of 39 | Drop |
| `nemotron-3.5-*-free` | NVIDIA, 3B / 12B active | SWE-bench Verified ~51.6 (Lightning) | Drop. Prompts are logged by NVIDIA. |
| `big-pickle`, `iceman-free`, `seaglass-alpha-free`, `tidepool-alpha-free` | unidentified or stealth models | none | Drop. Unmeasurable, and prompts are retained or used for training. |

### Decision (proposed; applied only on the user's yes)

- **One reviewer per PR, from the other CLI.** A Claude-authored PR gets agy: hydrogen (mid) for a small PR, neon (strong) for a large one. An agy-authored PR gets Claude: Sonnet (mid) for a small PR, Opus (strong) for a large one. If the other CLI is unavailable (agy out of quota), it falls back to the author's CLI in a fresh session. The 2610.01471 result says that loses little.
- **Two reviewers only for a very special PR** (security, money, data migrations, concurrency, a very large diff). Both must be strong and from different families. The root asks for them explicitly with `--reviewers agy,claude`, never by default.
- **OpenCode is not a default reviewer.** Its tier lists keep only `deepseek-v4.1-flash-free`, with no `free` fallback that could pick a weak model. Use it as an optional third reviewer, or as the reviewer when both other CLIs are at their limits. Keep collecting its verdicts: n=3 is too few to rely on.
- **This differs from ledger #12.** #12 said one cheap OpenCode reviewer for small PRs. Our verdicts say the cheap reviewer costs more in root verification than it saves.

The config this means in `~/.config/agent-mesh.json`:

```json
"review": {"reviewers": {"small": [["agy", "claude"]], "large": [["agy", "claude"]]}},
"tiers": {"opencode": {"mid": {"model": ["opencode/deepseek-v4.1-flash-free"]},
                       "strong": {"model": ["opencode/deepseek-v4.1-flash-free"]}}}
```

**Check:**
- Root-verified rejected findings per PR should fall, and accepted blocking findings per PR should not.
- Claude review $ per PR: Claude now reviews agy's PRs instead of OpenCode doing it, so expect about +$2 for each agy-authored PR.

## The root's weight

Measured over 18 root sessions since 2026-11-25 13:00 UTC: 2,133 calls and 4.39M chars of tool results, scripts in the session's scratchpad.

- **Baseline per session:** the first call already carries ~70k tokens, of which ~10k is CLAUDE.md files and memory; the rest is the system prompt, tools, skills and MCP listings. That is 44% of all cache reads. `root_reset.relaunch` is `false` in the config, so a reset is a `/clear` and keeps this baseline.
- **Context:** 169k tokens per call on average. 43% of calls run above 150k, and they hold 60% of the tokens read. Tokens above 150k are 19% of the total; above 120k, 29%.
- **Calls:** 80% of calls follow a tool result. That is about 4.9 calls per wake (300 user turns, 135 doorbells).

The biggest tool results (chars; divide by ~4 for tokens):

| Source | Chars | Share |
|---|---|---|
| `mesh inbox` (195 calls; full plan bodies 29 × 13.9k, `done` bodies 123 × 3.1k) | 1.09M | 25% |
| Reads of `reports/` | 254k | 5.8% |
| Repo docs, protocol and TODO via cat/sed | 244k | 5.6% |
| `git diff` / `git show` | 164k | 3.7% |
| Whole-file `Read` of `ORCHESTRATE.md` (4 × 33k; the file is 76.5 KB) | 133k | 3.0% |
| `mesh brief` (12 × 9.9k) | 119k | 2.7% |
| `git fetch` (50 calls) | 118k | 2.7% |
| `mesh protocol` (7 × 12.5k) | 88k | 2.0% |
| `mesh status` (43 × 1.1k) | 48k | 1.1% |

Caveat: "root-like" sessions were picked by mesh command count, so a user session that drove the mesh by hand may be in the set.

### Proposals, biggest first (savings are estimates against root cost; they overlap)

1. **`mesh inbox` prints a summary.** Show the header, a plan's changes, questions and risks, and a `done` report's first lines, plus the file path; the root reads the full text only when it acts on it. This touches `cmd_inbox` (`bin/mesh`). Plans and `done` reports are ~18% of the result chars, and each is re-read on every later call: **~8–10%**. Risk: approving a plan on a partial view, so keep the parts approval needs visible.
2. **Workers run and settle their own review round** (TODO, "Drop design §7's rule …"). The root loses the round's start, one wake, the report read and the per-finding check: **~10–15%**. Risk: no outside judge, so the root still checks what the worker disagreed with.
3. **Never `Read` `ORCHESTRATE.md` whole.** The card already says to read it in sections. Make `mesh protocol <part>` sections smaller (12.5k each today) and the card say "never the whole file": **~3–5%**.
4. **Shorter call chains.** One `mesh check <worker>` (fetch, `diff --stat`, CI state) instead of separate `git fetch`, `diff` and `status` calls, and the next command printed in `inbox` and `watch` output: **~3–5%**.
5. **Design reading goes to an asker.** ~370k chars of `bin/mesh`, docs and TODO reads were the root investigating things itself. A `mesh ask` answers that in a few hundred chars: **~3–4%**.
6. **Reset earlier** (`root_reset.at` 150k → ~120k). Calls above 120k hold 29% of the tokens, but ledger #4 found that resetting at 200k cut context per call 18% and left $ per worker task flat. A reset re-reads `mesh brief`, notes and the 70k baseline. Try it only after 1–5, and measure $ per worker task, not context.

Not proposed here: root effort `medium` (~$38/wk, "Settled": the user kept high).

## The root's weight per worker task (measured 2026-11-27)

The "Proposals" above (the inbox summary, workers settling their reviews, smaller protocol parts, `mesh check`) have landed. This measures the root again and asks the open question, proposal 6: would an earlier reset (`root_reset.at` 150k → ~120k) cost less per worker task?

**Data:** the four sessions of this run's lead (run `r2lscbp`, the lead's transcripts in `~/.claude/projects/…-agent-mesh-lead/`; the lead is a Claude Code root on Opus 5.5 with `root_reset.at` 150000, which resets it with `/clear`), 2026-11-27 08:41 → 20:07 UTC: 737 calls. `scripts/rootweight.py` (stdlib) reads the transcripts: calls deduplicated by `message.id`, API-equivalent dollars at the prices of `README.md` ("Weighting"), a task counted for each `mesh send --type task`, `mesh spawn --task` and `mesh resume --task` the root ran, an answer for each `mesh send --type answer` (follow-up work), a report for each `done` or `failed` header in what `mesh inbox` printed.

| session | from → to (UTC) | calls | context/call | $ main | $ subagents | tasks | answers | reports read | reviews run |
|---|---|---|---|---|---|---|---|---|---|
| 1ea8d9cf | 2026-11-27T08:41 → 2026-11-27T12:01 | 161 | 109k | 7 | 0 | 8 | 1 | 14 | 0 |
| a3d30d65 | 2026-11-27T12:07 → 2026-11-27T14:56 | 174 | 127k | 7 | 0 | 14 | 8 | 10 | 0 |
| a808b12f | 2026-11-27T15:01 → 2026-11-27T17:40 | 167 | 110k | 6 | 0 | 5 | 19 | 12 | 0 |
| 3e7c963f | 2026-11-27T17:46 → 2026-11-27T20:07 | 235 | 139k | 10 | 0 | 17 | 12 | 12 | 5 |

All 4 sessions: 737 calls, $30 (main and subagents), 44 tasks sent, 40 answers, 48 reports read.
Per task sent: 17 calls, $0.7, 2.0M tokens read, 20k written, 5k output.
Per piece of work sent to a worker (task or answer): 9 calls, $0.4.

**What a worker task costs the root:** about 17 calls and **$0.7** (**$0.4** per piece of work, counting answers). The lead's four sessions cost **$30** in all, for 44 tasks, 40 answers and 48 reports read; no subagents. The earlier measurement (11-25 → 11-26: the root row of the spend table, $162, over the 2,133 calls of 18 root sessions) was **$9 a session**, 169k tokens of context a call, **$0.076 a call**; now it is **$7.5 a session**, 109–139k a call, **$0.04 a call**. Against the large workers' $3.5 a task (below, measured 11-26, not re-measured) the root is about a fifth of a task's cost.

**Where it goes:**

| session | first call | $ read | $ write | $ output | calls over 120k | tokens read over 120k | over 150k |
|---|---|---|---|---|---|---|---|
| 1ea8d9cf | 21k | 3.5 | 2.5 | 1.1 | 42% | 60% | 41% |
| a3d30d65 | 21k | 4.4 | 1.5 | 1.2 | 59% | 76% | 54% |
| a808b12f | 21k | 3.7 | 1.3 | 1.0 | 46% | 62% | 38% |
| 3e7c963f | 21k | 6.5 | 1.8 | 1.5 | 62% | 79% | 60% |

- **Cache reads are 49–66% of the dollars** (the longest session has the most), writes 18–35%, output 15–17%. The first call of every session carries only **21k** tokens (it was ~70k on 11-26: the lean launch and the smaller protocol parts), so the baseline is no longer most of what is read.
- **The weight is the tail of each session:** 42–62% of the calls are above 120k and hold 60–79% of the tokens read; above 150k, 38–60%. A session runs 107–135 calls from its start to 150k, where it resets.

**Would resetting earlier pay?** A session's cost per call grows with its context, so a session cut sooner costs less per call, and each cut repeats the start of a reset (measured: the first 8 calls of a session cost $0.31–0.36, about $0.1 more than 8 calls from the middle of one; the conversation's own state is what the notes and the brief carry back, and a reset also costs what the root must re-derive, which this does not price):

| reset at | calls per session | $ per call, no reset cost | $ per call with a reset's start | $ per session |
|---|---|---|---|---|
| 100k | 66 | 0.0300 | 0.0315 | 2.07 |
| 120k | 85 | 0.0316 | 0.0325 | 2.77 |
| 150k | 121 | 0.0363 | 0.0366 | 4.43 |

By this model 120k would cut the root's dollars per call by about **11%** ($0.0366 → $0.0325), 100k by 14%. In a worker task's terms that is $0.07 of the root's $0.7, **about 2% of what a task costs** with its worker, so the saving is small and a reset's other cost is not in it: each one loses working context, and the lead's own sessions show no sign that more resets lower the calls a task takes (7–18 calls a piece of work, in no order by context). Ledger #4 found the same at 200k: context per call fell 18% and $ per task stayed flat. **Keep `root_reset.at` at 150k.** The proposal stands answered unless the root's weight grows again (a larger first call, or calls above 150k holding more than 60% of the tokens).

Caveats: one run, one root (the lead supervises and writes no code; a root that also drives a user's session reads more); dollars are API-equivalent, not what the subscription counts; tasks and reports are counted by the commands and headers of the transcript, so a task sent by another route (a device, the app) is missed; the what-if cuts the sessions that happened to be recorded and assumes the calls of a shorter session cost what the same calls cost inside a longer one.

## Large workers (measured 2026-11-26)

The data covers 15 large Claude worker sessions since 11-25. They made 5,308 calls, ran 102 tasks and opened 44 PRs.

- **Spend:** $355 in the main transcripts plus $17 of subagents. That is $3.5 per task and $8.1 per PR.
- **Small workers in the same window:** 19 tasks and 9 PRs for $24.7, or $1.3 per task and $2.7 per PR. That includes #151 (+22K lines) for $2.35.
- **Prices:** the $ are at `mesh cost`'s `PRICES`, which charge Sonnet 5.5 cache reads $0.20/MTok. The current `claude-api` reference lists $0.10, so Sonnet's share is overstated. Over the window that is about $95 of the $772.

### Where the money goes

- **By token class:** cache reads $216 (61%), cache writes $75 (21%), output $63 (18%).
- **Large contexts:** calls above 200K context are 45% of the calls but 59% of the $. A task can grow from ~65K to 500K+ context, because compaction runs only at task boundaries.
- **By phase:** planning and waiting for approval cost $67 (19%), execution $213, and the self-review window $74.5 plus $17 of `/code-review` subagents.
- **Follow-up tasks:** the tasks after a worker's first cost $197, 55% of the total. 65 of them were review-fix tasks, at about $2.1 each ($139).
- **Context growth:** 10.8M tokens over the stretches between compactions (estimated at 3.6 chars per token):
  - Assistant output: 46%, of which thinking is 27%.
  - Tool results: 30%.
  - The rest, about 24%: attachments, skill bodies, task text and hook text.
- **Tool results by source:**
  - Bash file reads (sed, cat, awk): 42%.
  - grep and ls: 18%.
  - `Read`: 11%.
  - git: 10%.
  - Test output: 4%.
  - Re-reads of the same file are negligible (0.39M chars).
- **Test and build calls:** 1,358 of them cost $103, including 306 full-suite runs (~$21.5).

### Size

Six of the 15 should have been small. They cost $77 in all:
- mesh-fixes: 4 PRs, +1.5K lines.
- parity-tools: its plan was 2.5K chars, a formality.
- castanha group-schedules: +1.8K lines.
- castanha region-counts: +273/−32.
- agent-setup: +409/−52, all on Opus.
- curupira map-descriptions: +999/−139.

Their plan phases cost $22.6. Signs a task was sized large wrongly: one PR under ~1K changed lines with no new subsystem, or a plan of 10K+ characters for a diff that small.

### Cheaper models for execution

| Model | $ per MTok (input / cache read / output) | Terminal-Bench 4.0 | SWE-bench Pro |
|---|---|---|---|
| Opus 5.5 | 4 / 0.20 / 20 | 66.4% | 89.9% (secondary source) |
| Sonnet 5.5 | 2 / 0.10 / 10 | 70.6% | 81.3% |
| Haiku 5.5, prompt ≤100K | 0.10 / 0.01 / 0.50 | 39.2% | 64.8% (max effort) |
| Haiku 5.5, prompt >100K | 0.50 / 0.05 / 2.50 | | |
| deepseek-v4.1-flash (free) | 0 | 31.2% | none (DeepSWE 74.2%) |
| mimo-v2.6-flash (free) | 0 | 28.8% | none |

Sources: Anthropic's pricing page and the [Haiku 5.5 launch post](https://www.anthropic.com/news/claude-haiku-5-5) (2026-10-07); DeepSeek's [paper](https://arxiv.org/pdf/2609.19969). The other figures are vendor numbers relayed by secondary sources.

- **Haiku saves little at worker context sizes.** Large-worker contexts average 139–256K, where Haiku's cache read is only 2× cheaper than Sonnet's, not 10×. It is also 31 points behind Sonnet on Terminal-Bench 4.0. Anthropic itself places Haiku at narrow work: compaction, summaries and subagents.
  - Use it for the light tier: trivial tasks in fresh sessions that stay under 100K.
  - Use it for read-only subagents: an `Explore` subagent with `model: haiku`. The built-in Explore inherits the parent's model.
  - Don't use it for execution inside a long worker conversation.
- **The free models score below Haiku** on Terminal-Bench 4.0. OpenCode Zen's free limits are not published, users report 429 `FreeUsageLimitError`, and nobody has published long-run reliability data.
  - Use them for trivial, self-contained work checked by a test: docs, translations, version bumps, config. OpenCode's light tier already exists for this.
  - Restrict the tier to `deepseek-v4.1-flash-free`, which has zero retention, rather than `free`, which can pick a model that trains on the code.
  - Don't use them for multi-file work.
- **Haiku with an Opus advisor:** the advisor tool is in Claude Code (`/advisor opus`, `advisorModel`). Each call reads the whole transcript uncached at Opus rates, about $0.80 at 200K, and there are no 5.5-generation results. Try it only on a measured trial.

### Proposals, biggest first (estimated $ over this window; they overlap, about $100–150 in all)

1. **Keep self-review as it is.** Since 11-25, 50 `done` reports that give counts say self-review fixed 160 of 217 findings, about 3.2 per PR. These are the workers' own counts, unverified. At ~$2.1 per large PR ($91 over 44 PRs) it costs about the same as one review-fix task (~$2.1), and that task also brings a re-review and a root wake. Don't lower `review.self_level.large` either. A lower `/code-review` level returns fewer, higher-confidence findings, so it trades recall for precision. Precision is what the outside reviewers lack, because the root pays to verify each of their findings. In self-review the author checks a finding in its own warm context for almost nothing, so recall matters more there. The only cut worth making is skipping `/code-review` on docs-only diffs: a few dollars.
2. **Fewer review-fix round trips.** These are 65 tasks at $139. A worker that runs and settles its own review round fixes everything in one warm task (the TODO item on workers starting their own review round). Fewer noisy reviewers (the reviewer section above) also mean fewer fix tasks.
3. **Size by the change, not the topic.** Large only for several PRs, a new subsystem, or open design choices. Follow-up tasks always go direct, with no re-planning (`DIRECT_PREFIX`). Re-planning in follow-ups cost $33, and the six oversized workers' plan phases $22.6. Together that saves $30–40. It touches ORCHESTRATE's size table and its follow-up rule.
4. **Approve at `mid` unless the plan leaves something open.** Opus execution and follow-ups cost $61. That saves ~$13, or more at the corrected Sonnet read price.
5. **Narrower test runs and batched reads** (worker protocol text): $10–20. Run the affected tests while iterating and the full suite once before `done`.
6. ~~Light tier (Haiku) in a fresh session for chores and one-line review fixes.~~ Dropped: the worker that wrote the code already has its context cached. A follow-up there costs cache reads (~$0.025 per call at 250K, Sonnet's $0.10 read price), and a fresh agent would re-read the files and might misread why the code is the way it is. Resumes within the hour read warm (99 of 102, "Settled"). The fix tasks cost money because there are many of them, not because of the model: see #2.

Not proposed:
- **Mid-task compaction at ~200K:** "Settled" measured re-reads eating the saving. Re-propose only with new data.
- **Sonnet execution at `medium` effort:** Anthropic suggests medium for agentic coding on Sonnet 5.5, but "Settled" found no saving for small workers at medium. Run it only as a trial measured per task.
- **A fresh worker per follow-up:** about $14, and it breaks the same-conversation rule.
