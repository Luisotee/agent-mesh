# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

- [ ] Mine similar projects for improvements, starting with [firstmate](https://github.com/kunchenguid/firstmate) (overlaps heavily: herdr panes, worktree per task, inbox plus doorbell, quota-aware dispatch). Candidates there (its composer-empty check and turn-end guard are done: `ring()` reads the pane, `mesh watch` nudges): wedge/stale-pane detection, reconciling run state after a restart. Then survey other multi-agent orchestrators for the same and note what to adopt here
- [ ] Live-test `--plan-review user` on plain `opus` (was `opusplan`): the plan dialog still shows `blocked` in herdr, and after approval the worker executes on Opus in the same pane
- [ ] herdr's agy manifest counts working only on the `esc to cancel` footer or a spinner, so a busy agy can read idle; report it upstream with a pane capture (mesh now works around it in `ring()` and the watch's grace)
