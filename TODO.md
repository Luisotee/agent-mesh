# TODO

Open work, kept short on purpose: whoever picks a task up works out the details. The reasons behind past decisions are in `docs/design.md`; the cost study's baseline, method, scripts and savings ledger are in `docs/cost-research/README.md`. If you have the peer projects registered (`mesh peers`), ask them before designing a mechanism they may have solved (see `CLAUDE.md`, "Peer projects").

- [ ] Copilot workers, unverified live (the account had no Copilot subscription when it was built): a reopen on a named model, `write(<path>)` deny rules (to allow the report under `$(mesh dir)`), a `Stop` hook through `--plugin-dir`, and a reader for its `┃` input box (`typed_text` can't read it: a doorbell may type over a draft there, and a ring herdr reports `stalled` or `timeout` can't be confirmed in the pane, so it may queue a duplicate doorbell).
- [ ] `mesh cost`: count Copilot workers' calls (`session.usage_record` in their `events.jsonl`).
- [ ] One kind of agent, step 3: OpenCode as a worker kind (free-model rotation on `mesh switch`, policy leaves, review slots, its global rules).
- [ ] `review_head`'s move of a reviewer checkout: refresh the index (`git update-index -q --refresh`) before its `git reset --keep`; a clean reviewer worktree failed with `Entry bin/mesh not uptodate` after a power outage.
- [ ] `read_log` warns on every read about a log line a power cut left NUL-filled or torn (`m46c2/log.jsonl` line 24): seal or quarantine such lines once, so the warning comes once.
- [ ] `mesh cost`: agy workers whose entries have no `size` show as `agy worker ? pane` (1,064 calls over 2026-10-28 → 11-04): record or derive their size.
