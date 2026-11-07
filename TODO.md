# TODO

Open work, kept short on purpose: whoever picks a task up works out the details. The reasons behind past decisions are in `docs/design.md`; the cost study's baseline, method, scripts and savings ledger are in `docs/cost-research/README.md`. If you have the peer projects registered (`mesh peers`), ask them before designing a mechanism they may have solved (see `CLAUDE.md`, "Peer projects").

- [ ] The CPU cap (`worker_cpus`, design §17): on a run with an agy worker, check that agy's own child processes see its mask (`taskset -p <pid>`); everything else was checked live in run `me608`.
- [ ] Auto-retire's viewer hold misses an attach from Claude Code's agent view (`claude agents`, then a session: it runs in that TUI's process). No outside signal in 2.1.319 (design §18, "Limits"); look again when Claude Code exposes its daemon's attach clients. (Without `/proc`, `ps` now covers `claude attach`.)
- [ ] `mesh cost`: its `peer` row missed most peer calls over 2026-10-28 → 11-04 (15 calls vs 353 by the cost study's `classify.py`; resumed peer sessions likely count as `user`), and `reviewers_per_pr` reads 1.0 in every window. See `docs/cost-research/README.md`, "Check 1".
- [ ] The 5m cache TTL (ledger #5) raised written tokens per call for small bg workers 3,384 → 8,066 at the same context, and their write $ per call +50% (+8% from 11-04): measure their gaps and decide whether they go back to 1h.
