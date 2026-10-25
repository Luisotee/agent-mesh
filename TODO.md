# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

- [ ] Live end-to-end test of 0.7 in herdr: a `/delegate-claude` task list with a trivial, a small and a large task (background workers reporting by SendMessage, a pane worker through plan → approve → PR, a reviewer per PR, `mesh conflicts`, the board), then `mesh ask` on a stopped background worker with compaction. Also the 0.6 `mesh ask` live test that never ran, and check the side-pane layout (workers split beside the root, the fourth goes to the run tab, a swap stays put)
- [ ] agy's context size is unknown to mesh, so `--compact auto` compacts every agy worker coming back from an earlier run; read it from `agy --output-format json` usage if agy ever exposes it for a stored conversation
- [ ] A planned task sent with `mesh send --type task` to a live worker that has no plan phase (a small asker, a worker already in `exec`) gets no exec phase, so `mesh approve` keeps its model and effort (`mesh resume` already gives one)
- [ ] `mesh ask` exited 3 ("stopped working without sending a message") on an agy reviewer that was still mid-turn after a ~6-minute thought (run m2d4d, agent-mesh-review-6): find what herdr reported and make the watch tolerate long thinking
- [ ] Mine similar projects for improvements, starting with [firstmate](https://github.com/kunchenguid/firstmate) (overlaps heavily: herdr panes, worktree per task, inbox plus doorbell, quota-aware dispatch). Candidates there: composer-empty check before typing a doorbell, wedge/stale-pane detection, a turn-end guard so the root never stops blind with workers running, reconciling run state after a restart. Then survey other multi-agent orchestrators for the same and note what to adopt here
