# Choosing models and efforts in the mesh: research and recommendations

Written 2026-11-28 from research done on 2026-11-27 and 2026-11-28. This is a report, not an implementation: nothing in the code has changed. Section 7 lists the two host settings that did change.

Benchmark numbers are independent unless marked **(V)**, which means the model's vendor reported them. Our own numbers come from `mesh cost`, `mesh usage` and `mesh asks` on this host, for the week 2026-11-21 to 2026-11-28, when the agent-mesh apps were built.

## 1. Conclusions

1. **The plans are very different sizes, so a percentage means different amounts on each.**
   - **Claude Max 5x is the big pool.** A week of full app building used 77% of it: about 234M tokens plus 9.1G cache reads, about $2.2k at API prices.
   - **agy on Google AI Pro is small.** Its Gemini week ran out in about 1.5 days, after about 26M tokens plus 0.3G cache reads. That is roughly 3–13% of what a Claude week holds.
   - **OpenCode's free models have daily caps per model.** DeepSeek V4.1 Flash runs out at times.
2. **No free model reaches Sonnet 5.5.** The best one, DeepSeek V4.1 Flash, is level with Haiku 5.5 or a little better. Free models can replace Haiku and part of the routine work, not Sonnet's real execution.
3. **Opus was 63% of last week's Claude spend.** That includes the user's own sessions, the root, and direct tasks run on Opus. Moving execution and direct tasks to Sonnet, or to Opus at lower effort, saves more than any other change.
4. **agy's models are strong but scarce.** Neon and Carbon are close to Claude on coding and frontend, and Neon has our best review record. In agy, effort is quota: Neon at `xhigh` uses 2.3× the tokens of `high` for 2 more points.
5. **OpenCode's tier chains were in the wrong order.** beam-free was first and is the worst performer we use; DeepSeek, the best free model, was last. This is fixed on this host (section 7).
6. **Haiku is a poor asker.** It scores 34 on codebase Q&A against Sonnet's 64.
7. **Effort matters as much as the model.** Above `high`, tokens and time grow much faster than scores, on every vendor. `low` saves tokens but weakens long tool loops, checking and finding bugs. Two examples: Opus at `medium` beats Sonnet at `high`, and Sonnet at `medium` is weak.
8. **Drop the rule that frontend goes to Claude only.** Neon and Carbon are close to Claude on frontend benchmarks, so a vendor rule wastes the Claude pool. Route frontend by quality and headroom like any other work. agy's small plan still limits how much frontend it can take, and agy's work gets a Claude review.
9. **Large PRs and risky PRs are different things.** Size sets how many reviewers a PR gets and how deep the review goes. Risk (auth, money, data migrations, concurrency) sets how strong the reviewers are and whether a relay runs. A large routine PR doesn't need `strong` reviewers, and a 20-line auth change does.
10. **Don't subscribe to OpenCode Go.** On the $10 plan it would lift only about 3% of Claude's load (section 8). We stay on the free models.

## 2. The pools

| Pool | Plan | How big | What limits it |
|---|---|---|---|
| Claude | Max 5x | Big: a week of app building ≈ 77% of the week | 5-hour and 7-day windows; `quota_stop` 85 |
| agy, Gemini group (Neon, Carbon, Argon, Hydrogen, Flash) | Google AI Pro | Small: ≈ 3–13% of a Claude week; gone in ~1.5 days | One 5-hour and one weekly meter shared by the group |
| agy, Claude/GPT group (agy's own Opus and Sonnet) | Google AI Pro | Small, separate meter | Was at 0% for the week on 2026-11-27 |
| OpenCode free | none | No plan; caps per model | Per-model daily caps ("usage limit reached. It will reset in …"). Users report about 200 requests a day (unconfirmed), and calls from outside OpenCode are refused |

- **agy's quota seems to scale with tokens.** Google says quota "correlate[s] with the amount of work done by the agent", and thinking tokens bill as output. So on agy a higher effort costs quota directly. That last step is our inference.
- **OpenCode's free caps seem to count requests, not tokens** (unconfirmed). A higher effort then costs no more cap per call, but a longer agent run makes more calls.

## 3. How good each model is, by kind of work

**What the columns measure.** These are independent tests where every model runs in the same setup:

| Column | What it is | Closest mesh work |
|---|---|---|
| TB4.0 | Vals Terminal-Bench 4.0: long tasks done through tool calls | a worker's tool loop |
| AA agent | Artificial Analysis Coding Agent Index | a model inside a real coding CLI |
| WebDev | WebDev Arena: one-shot web pages, judged by people | frontend (but not edits in an existing codebase) |
| Q&A | Artificial Analysis SWE-Atlas: answering questions about a codebase | askers and peers |
| Review | CodeRabbit (CR) and Kodus: recall / precision | reviewers |

Recall is the share of real bugs a reviewer found; precision is the share of its findings that were real.

| Model (pool) | TB4.0 | AA agent | WebDev | Q&A | Review | Our review record |
|---|---|---|---|---|---|---|
| Fable 5.2 (Claude, `top`) | 65.2 | 58 | 1822 | – | – | – |
| Opus 5.5 (Claude) | 65.2 | 58 | 1820 | 66 | CR 62/67, Kodus 42/53 | 94% agreed, 1.8 blocking per review |
| Sonnet 5.5 (Claude) | 64.1 | 56 | 1773 (at `xhigh`) | 64 | CR 46/41, Kodus 37/57 | 96% agreed, 0.5 blocking per review |
| Carbon (agy) | 63.1 | 55 | 1769 | – | – | – |
| Neon (agy) | 61.6 | 59 | 1765 | – | – | 99% agreed, 2.0 blocking per review |
| Argon (agy) | 57.6 | 45.5 | – | – | – | – |
| Hydrogen (agy) | 54.0 | – | 1701 | – | – | – |
| DeepSeek V4.1 Flash (OpenCode free) | 42.3 (V) | 36.8 | – | – | Kodus 41/43 | – |
| Haiku 5.5 (Claude) | 36.4 | 30 | – | **34** | – | – |
| Gemini 3.8 Flash (agy) | 19.7 | – | – | – | – | – |
| hy4-flash-free (OpenCode free) | 11.8 (on TB4.1) | – | – | – | – | ~91% agreed, 0.8 blocking per review |
| seaglass-alpha-free (OpenCode free) | – | – | – | – | – | 89% agreed, 1.3 blocking per review, 3 of 11 timed out |
| beam-free (OpenCode free) | **3.0** | – | – | – | – | 76–82% agreed, 0.16 blocking per review, 6 of 37 timed out |

**Other test results:**
- Carbon ranks second on Vals' multi-step web-app test (49.7); Opus 5.5 scores 31.0 there.
- Carbon leads Google's own long-context test (V).
- Vendors' own scores often run 5–30 points above the independent ones. Sonnet 5.5's own 70.6 on TB4.0 was measured at 63.6 by Artificial Analysis.
- In our record, Opus and Neon reviewed the large PRs and the others mostly small ones, so blocking findings per review is not a straight ranking.

**What each model is good and weak at:**

- **Fable 5.2:** best for the hardest planning and "must be right first time" work. It is the most expensive per task (about $15 at Artificial Analysis), uses many tokens, and starts self-checking subagents at `xhigh`. Use it only when the user names it.
- **Opus 5.5:** the best all-rounder: planning, review (best precision), frontend. At `max` it overthinks and runs long.
- **Sonnet 5.5:** within about a point of Opus on TB4.0 at half the price, so it is the right executor. It is good for routine reviews but not the hardest cases. At `max` it timed out and made out-of-scope edits (V).
- **Haiku 5.5:** for searches, summaries and mechanical edits. It is weak in agent loops and at codebase questions. At `low` it skips checks and stops early (V).
- **Carbon:** Google's strongest model and the best of them at multi-step web apps. It is slow, about 36 minutes per task.
- **Neon:** the best value per task and our best reviewer. One hands-on test caught it coding against a retired API, inventing fallback data and rigging its own tests to pass, so a Claude model reviews its work.
- **Hydrogen:** a fast, cheap executor between Haiku and Sonnet. It is weak at frontend (#14 on WebDev Arena) and uses many tokens at `xhigh`.
- **Argon:** overtaken by Hydrogen and Neon.
- **Gemini 3.8 Flash:** not a coding agent. Use it only for trivial questions.
- **DeepSeek V4.1 Flash (free):** the strongest free model and the most used one on OpenCode. Its free route caps context at 200K and output at 32K. One report says it hallucinates (anecdotal). Data terms: zero retention, no training.
- **hy4-flash (free):** fast, with 1M context. It is weak in long tool loops, but does well in our reviews.
- **seaglass (free):** a stealth model with 1M context. It trains on your prompts (acceptable to the user) and has no benchmarks. Its record in our reviews is good.
- **mimo-v2.6-flash (free):** a reasonable executor, below DeepSeek. It had a tool-call loop bug in OpenCode at launch, fixed in September.
- **beam (free):** high vendor scores but 3.0 on the independent TB4.0, and weak in our reviews. Use it last.
- **Avoid in OpenCode:** DeepSeek V4 Pro and MiniMax-M3 score near zero there because of a tool-call bug (OpenCode issue #36354, closed as "not planned"). The free models also rotate: Tidepool has left, and nothing about how long they stay free is published.

## 4. Effort levels

### What each CLI offers on this host

These lists come from `agy models` and `opencode models --verbose`:

| Models | Levels |
|---|---|
| Opus, Sonnet, Haiku, Fable (Claude Code) | low, medium, high, xhigh, max |
| Neon, Hydrogen (agy) | low, medium, high, xhigh |
| Carbon, Argon, 3.8 Flash (agy) | low, medium, high |
| agy's own Opus and Sonnet | low, medium, high |
| DeepSeek V4.1 Flash (OpenCode) | low, high, max (= DeepSeek's effort 50, 75, 100), or thinking off |
| hy4-flash (OpenCode) | none, high |
| seaglass (OpenCode) | minimal, low, medium, high, xhigh, max |
| beam (OpenCode) | low, medium, high, xhigh, max |
| mimo (OpenCode) | no levels: thinking on by default |

When a model doesn't offer the requested level, our OpenCode adapter doesn't set one.

### Claude (Anthropic)

- **What effort does:** it governs all output: thinking, tool calls and text. Lower effort makes fewer and terser tool calls and can skip thinking. It is a behaviour signal, not a token budget.
- **Defaults:** Claude Code runs all four models at `medium`. The API uses `medium` for Opus and Haiku and `high` for Sonnet and Fable.

Scores below: CursorBench 4.1 as score, $ per task and tokens per task; Artificial Analysis (AA) as index and minutes per task.

| Effort | Opus 5.5 | Sonnet 5.5 | Haiku 5.5 | Fable 5.2 |
|---|---|---|---|---|
| low | 42.7%, $1.43, 19.5k · AA 35, 2.9 min | 31.8%, $0.44 · AA 26 | 27.8%, $0.08 · AA 19 | 53.4%, $4.26 · AA 54, 5.3 min |
| medium | 50.0%, $2.85, 39.7k · AA 48, 6.8 min | **34.3%**, $0.63 · AA 31 | 34.2%, $0.17 · AA 25 | 54.2%, $5.01 · AA 56 |
| high | 54.5%, $3.87, 56.2k · AA 52, 9.0 min | 43.8%, $1.41, 43.1k · AA 41, 6.9 min | 37.5%, $0.31 · AA 30 | 56.5%, $10.51 · AA 62, 14.6 min |
| xhigh | 56.7%, $6.54, 104.9k · AA 58, 15.5 min | 49.9%, $3.13, 112k · AA 50, 13.9 min | 45.2%, $0.56 · AA 35 | 58.8%, $17.84 · AA 62 |
| max | 58.1%, $11.88, 213k · AA 59, 24.7 min | 54.4%, $8.63, 342k · AA 55, 31.1 min | 48.4%, $1.14 · AA 37 | 61.3%, $48.11 · AA 65, 40.4 min |

Other per-effort evidence:
- **Opus 5.5 bug finding (V):** F1 of 0.489 at `low`, 0.827 at `medium` and 0.850 at `high`. `medium` gets 97% of `high`'s result for 85% of its cost, while `low` loses a third of the bugs.
- **Opus 5.5 run low, retry failures at high (V):** about 97% of tasks solved for about $0.17 each, against 95.3% for $0.29 with everything at `high`. It needs a reliable failure signal such as tests.
- **Sonnet 5.5 at `max` (V):** it scores below `xhigh` on FrontierCode (46.2 vs 52.1), because it starts its own review subagents, times out and edits out of scope.
- **Sonnet 5.5 on frontend:** WebDev Arena 1715 at `high` against 1773 at `xhigh`.
- **Low effort (V):** Sonnet at `low` can skip checking its change; Haiku at `low` skips searches and checks; Fable stops early at `low` and `medium`.
- **Opus `low` vs Sonnet `high`:** equal in score and cost.
- **Opus `medium` vs Sonnet:** Opus `medium` is about equal to Sonnet at `xhigh` (CursorBench 50.0 vs 49.9, at $2.85 vs $3.13) and clearly beats Sonnet at `high` (AA 48 vs 41).
- **Opus `xhigh` vs Fable `high`:** equal, for about 60% of the cost.
- **Prompt cache:** in Claude Code on a subscription, changing effort keeps the prompt cache on these models, but switching model always starts a fresh one. That backs the current rule of raising effort before switching model.

### agy (Gemini 4)

**Google's guidance (V):** use `high` by default on Neon and Hydrogen. Save `xhigh` for "hard, multi-step coding and agentic problems where high falls short", because it can overthink simple prompts. Pick the lowest level that meets your quality bar. Google's own agy defaults:
- the Orchestrator plans on Carbon `high` and executes on Hydrogen `high`;
- agy's Auto-mode review runs on Hydrogen `medium`.

Scores are Artificial Analysis index / output tokens across its whole test suite / seconds to first answer.

| Model | xhigh | high | medium | low |
|---|---|---|---|---|
| Neon | 62 / 283M / 65 s | **60 / 123M / 35 s** | 55 / 63M / 27 s | 44 / 28M / 19 s |
| Hydrogen | 54 / 304M / 48 s | **51 / 110M / 17 s** | 41 / 54M / 13 s | **27** / 20M / 7 s |
| Carbon | – | 60 / 205M / 83 s | 58 / 124M / 41 s | 54 / 84M / 13 s |
| Argon | – | 51 / 113M / 41 s | 48 / 82M / 9 s | – |
| 3.8 Flash | – | 31 / 126M / 23 s | 30 / 84M / 13 s | 22 / – / 4 s |

- **Neon:** `high` gives 60 for less than half the tokens of `xhigh` (62). Carbon at `high` scores the same 60 but uses 1.7× Neon's tokens.
- **Hydrogen at `low` scores 27, below Haiku**, yet it is what our agy light tier runs today.

### OpenCode free models

- **DeepSeek V4.1 Flash, measured by Artificial Analysis:**

  | Setting | Index | Output tokens | Seconds to first answer |
  |---|---|---|---|
  | Thinking off | 19 | 51M | 1 |
  | `high` | 29 | 134M | 19 |
  | `max` | 32 | 174M | 47 |

  DeepSeek's paper (V) says effort 60–80 recovers most of the accuracy at under half the tokens, so `high` (75) is the sweet spot. At `max`, answers average about 89k output tokens per task. That is above the free route's 32K output cap, so `max` may be cut short (our inference).
- **hy4-flash, mimo, beam and seaglass:** no measurements per level were found.

## 5. Recommendations

### Routing by role

The order within each cell is the order to try: first choice, then the next when its pool is short.

| Role | First choice | Fallback | Avoid |
|---|---|---|---|
| Planning, unclear work, bug of unknown cause | Opus `high` (`xhigh` for the hardest) | Neon `high` when agy has quota | free models, Haiku, Sonnet `medium` |
| Executing a clear plan (backend) | Sonnet `high` | Opus `medium` when it needs more. Hydrogen `high` when agy has quota | Opus `high` and up on small tasks; Sonnet `medium` |
| Frontend and design (**no longer Claude-only**) | Sonnet `xhigh` or Opus `medium` | Neon or Carbon `high` when agy has quota, with a Claude reviewer | free models, Hydrogen |
| Trivial and mechanical work | DeepSeek `low` or thinking off; hy4-flash `none` | Haiku `medium` | agy (its small pool); Hydrogen `low` |
| Asker or peer, short question | hy4-flash `high`, seaglass `high` (both 1M context; keeps DeepSeek's cap for reviews) | Sonnet `medium` | Haiku |
| Asker or peer, research across a repo | seaglass `high` or hy4-flash `high` | Sonnet `high`, or Opus `medium` for hard questions | Haiku |
| Small PR review | DeepSeek `high` | hy4-flash, seaglass | beam; Opus `low` |
| Large but routine PR review | DeepSeek `high` plus Sonnet `high` (two reviewer slots) | Neon `medium` in place of Sonnet when agy has quota | `strong` reviewers by default |
| Risky PR review (auth, money, data migrations, concurrency; any size) | Relay: Opus `high`, then another family (Neon `high` when agy has quota, else DeepSeek `high`), then Opus on the fixes | – | free models alone |
| Subagents (Claude Code's Agent tool) | Haiku for a search or a summary | Sonnet for reading code to a conclusion | the caller's Opus except for planning-grade work |
| `top` tier (Fable, only when the user names it) | Fable `medium` to plan, `high` for long unattended work | – | `xhigh` and `max` (2–5× the cost, self-checking subagents) |

Standing rules:
- **agy is a scarce pool.** Use it where it adds a second model family or relieves Claude: second reviews, risky-PR relays, planning or frontend when Claude is near its limit. Never use it for volume, and never for trivial work.
- **Free models first** wherever the table allows. When their caps run out, fall back to Haiku or Sonnet, never Opus.
- **Opus is for planning, risky work and hard reviews.** Most of its 63% share last week was work Sonnet, or Opus at lower effort, could have done.

### Effort

| Tier and CLI | Now | Recommended | Why |
|---|---|---|---|
| Claude `light` (Haiku) | medium | medium; `high` when the task must check its own work | Haiku `low` skips checks; `xhigh` costs more than free models that do the same work |
| Claude `mid` (Sonnet) | high | **high, never medium** | Sonnet `medium` 34.3% vs `high` 43.8% on CursorBench |
| Claude `strong` (Opus) | high | high for planning and risky work; **medium for execution** after a plan | Opus `medium` beats Sonnet `high`; `high` costs +36% for +4.5 points |
| Claude `top` (Fable) | high | medium to plan, high for long unattended work | Fable's docs; `low` and `medium` are close |
| agy `light` (Hydrogen) | low | **don't route light work to agy**; if it must, Hydrogen `medium` | Hydrogen `low` scores 27, below Haiku |
| agy `mid` (Hydrogen) | high | high | Google's executor default; `xhigh` costs 2.8× the tokens for 3 points |
| agy `strong` (Neon) | high | high; `xhigh` only when `high` failed | `xhigh` costs 2.3× the tokens for 2 points |
| OpenCode `light` (DeepSeek) | low | low, or thinking off | Fine for trivial work, and fastest |
| OpenCode `mid` and `strong` (DeepSeek) | high | high, never `max` | `max` likely hits the free route's 32K output cap and waits about 47 s |
| `review.effort` (one value for all) | high | **per model:** Opus `medium` (risky: `high`), Sonnet `high`, Neon `medium` (relay: `high`), DeepSeek `high` | Opus bug finding: `medium` 0.827 vs `high` 0.850 F1, vs `low` 0.489; Google reviews on `medium` |
| `max` on any model | – | never by default | Opus +2% score for 2× the tokens; Sonnet `max` worse than `xhigh`; overthinking |

How to move a worker up:
1. **Raise effort first,** which keeps Claude's prompt cache: Sonnet `high` → `xhigh`, Opus `medium` → `high`.
2. **Then switch model:** Sonnet `xhigh` → Opus `high`. Opus `medium` costs about the same as Sonnet `xhigh`, so a long piece of remaining work can go straight to Opus `medium`.
3. **For a task with reliable tests,** "run at `low`, retry failures at `high`" is the cheapest route measured (V). Consider it for test-backed mechanical work.

## 6. What to change in agent-mesh (not done)

1. **`ORCHESTRATE.md`, "Modes" and "Tiers":**
   - Name OpenCode as a normal worker CLI; today only `claude` and `agy` are listed.
   - Add the role table and the effort rules above, the scarce-agy rule, and free-models-first.
   - Split large from risky PRs in the review rules.
   - Remove any Claude-only wording about frontend.
   - Fix the subagent rule: Haiku only for searches and summaries.
2. **Host facts as settings, not protocol.** The protocol has to stay process-agnostic, so plan sizes and model strengths should live in settings:
   - add a leaf per CLI for its plan (for example `plans.<kind>`) and a strengths line per model family;
   - print both in `mesh brief`, where the root reads them every run.
3. **`mesh usage` should compare pools in one unit.** The daemon has each window's token counts and how far its percentage moved, so it can work out each plan's real size. It should show what's left as, say, "≈ 3% of a Claude week", and the pace (used vs. elapsed time): agy at 98% used with 6 days to go is a warning.
4. **A model list per role for askers and peers.** Today a peer ask always runs on the peer's registered CLI and tier list, and refuses `--model` and `--kind` (`daemon/internal/daemon/peers.go`, around line 796). All 40 peers are registered as Claude, and `edit-peer` changes only the description. This needs:
   - `roles.asker.<kind>.model` and `roles.peer.<kind>.model`, falling back to the tier list;
   - `edit-peer --kind` to move a peer to OpenCode. The move starts a fresh conversation, so the peer loses what it remembers;
   - a fallback: an answer that comes back weak is asked again on Sonnet.
5. **Effort per model for reviewers.** `review.effort` is one value today. Make it per CLI and per risk (for example `review.effort.<kind>`, and `high` for a relay).
6. **Shipped defaults in `daemon/internal/settings/schema.go`:**
   - OpenCode tiers: DeepSeek V4.1 Flash first, beam and flint last;
   - agy `light`: Hydrogen `medium`, or no agy light tier at all;
   - Opus execution after a plan at `medium` (an execution effort beside the tier at `mesh approve`);
   - review slots: small PR OpenCode then Claude; large PR OpenCode plus Claude; risky PR a relay.
7. **`mesh models --check`** should flag a free model that has left OpenCode's list (as Tidepool did), and a requested effort the model doesn't offer.
8. **Record effort.** Store each agent's effort with its tokens and review verdicts, and show verdicts per model and effort in the brief. Every review so far ran at `high`, so our own data can't compare effort levels yet. beam's 0.16 blocking findings per review would have been visible early.

## 7. Already changed on this host (settings, not code)

On 2026-11-28, following the user's "prioritise using it for PR reviews":
- `tiers.opencode.mid.model`
- `tiers.opencode.strong.model`

Both now list `opencode/deepseek-v4.1-flash-free`, then `hy4-flash-free`, `seaglass-alpha-free`, `mimo-v2.6-flash-free`, `beam-free` and finally `free`. To undo: `mesh unset --key tiers.opencode.mid.model` and `mesh unset --key tiers.opencode.strong.model`.

## 8. OpenCode Go (decided against)

**How the plan works.** Go is $10 a month (Go Plus $40). It is one shared monthly meter: "The paid models you use on Go share one allowance". Each model's monthly limit only sets how fast it drains that meter. A 5-hour window allows 20% of the month and a week allows 50%.

**What it would have done for us, priced against last week's work:**
- **The work the open models can do** is about 15–20% of Claude's load: small reviews, peers, askers, and backend execution of clear plans.
- **On GLM-5.3, the value pick:**
  - $10 Go lifts about 3% of Claude's load on average, and about 6–7% in a peak week.
  - Go Plus lifts about 6% on average, and about 13% in a peak week.
- **Kimi K3.1 Pro, the model closest to Sonnet,** allows only 147 requests per 5 hours.
- **DeepSeek V4.1 Flash** would cover all of that work within the meter, but it is already free.

So Go doesn't noticeably lengthen app-building weeks.

## 9. Gaps and caveats

- **Few independent measurements of research quality or tool-call reliability.** The closest are TB4.0 and the Artificial Analysis Coding Agent Index.
- **Benchmark versions differ across pages.** Compare scores within one test, not across tests.
- **The free stealth models (seaglass and others)** have no published benchmarks. Their place in the recommendations rests on our own review record.
- **No per-level data for hy4-flash, mimo, beam and seaglass.** Their effort recommendations are a default (`high`), not measured.
- **Antigravity doesn't say how effort maps to quota.** "Quota scales with tokens" is inferred from its billing.
- **The pool comparison rests on one week** of measurements on this host (`mesh cost`, `mesh usage`).

## 10. Sources

- **Coding benchmarks:**
  - https://vals.ai/benchmarks/terminal-bench-4
  - https://www.vals.ai/benchmarks/vals_index
  - https://www.vals.ai/benchmarks/vcb-1-100-v1-1
  - https://artificialanalysis.ai/articles/gemini-4-neon-intelligence-gain
  - https://artificialanalysis.ai/agents/coding-agents/comparisons/claude-code-vs-opencode
  - https://benchlm.ai/benchmarks/aacodingagents
  - https://arena.ai/leaderboard/code/webdev
  - https://cursor.com/evals
- **Review benchmarks:**
  - https://coderabbit.ai/blog/sonnet-5-5-model-review
  - https://www.coderabbit.ai/blog/opus-5-5-model-review
  - https://kodus.io/en/best-llms-for-code-review/
- **Claude effort:**
  - https://platform.claude.com/docs/en/build-with-claude/effort
  - https://platform.claude.com/docs/en/about-claude/models/optimizing-for-cost-and-intelligence
  - https://code.claude.com/docs/en/model-config
  - https://code.claude.com/docs/en/prompt-caching
  - https://www.anthropic.com/news/claude-opus-5-5
  - https://www.anthropic.com/news/claude-sonnet-5-5
  - https://www.anthropic.com/news/claude-haiku-5-5
  - https://artificialanalysis.ai/models/claude-opus-5-5-xhigh
  - https://artificialanalysis.ai/models/claude-sonnet-5-5
  - https://artificialanalysis.ai/models/claude-haiku-5-5
  - https://artificialanalysis.ai/models/claude-fable-5-2
- **Gemini effort:**
  - https://aistudio.google.com/docs/thinking
  - https://antigravity.google/docs/cli/commands/effort/
  - https://raw.githubusercontent.com/google-antigravity/antigravity-cli/main/CHANGELOG.md
  - https://artificialanalysis.ai/models/gemini-4-neon
  - https://artificialanalysis.ai/models/gemini-4-hydrogen
  - https://artificialanalysis.ai/models/gemini-4-carbon
  - https://artificialanalysis.ai/models/gemini-4-argon
  - https://www.antigravity.google/docs/plans
- **Gemini models:**
  - https://www.mindstudio.ai/blog/gemini-4-neon-vs-claude-opus-gpt-6-2
  - https://antigravity.google/blog/gemini-4-neon-in-google-antigravity/
- **OpenCode and its models:**
  - https://opencode.ai/docs/zen/
  - https://opencode.ai/docs/go/
  - https://opencode.ai/go
  - https://opencode.ai/data
  - https://github.com/anomalyco/models.dev/tree/dev/providers/opencode/models
  - https://github.com/anomalyco/opencode/issues/36354
  - https://github.com/anomalyco/opencode/issues/33495
  - https://github.com/anomalyco/opencode/issues/58205
  - https://artificialanalysis.ai/models/deepseek-v4-1-flash-1017
  - https://arxiv.org/html/2609.19969
  - https://www.morphllm.com/deepseek-v4-1-flash
  - https://artificialwatch.com/stealth-models
- **Our own data on this host:** `mesh cost --since 2026-11-21T06:00 --until 2026-11-28T06:00 --agents`, `mesh usage`, `mesh asks`, `agy models`, `opencode models --verbose`.
