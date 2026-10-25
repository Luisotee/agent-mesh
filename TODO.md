# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

- [ ] agy's context size is unknown to mesh, so `--compact auto` compacts every agy worker coming back from an earlier run. Checked (Oct 2026): `agy --help` and `agy changelog` mention no usage or token counts, and a stored conversation (`~/.gemini/antigravity-cli/conversations/<id>.db`, SQLite with protobuf blobs in `steps` / `gen_metadata`) has no named token fields; read it once agy documents one (e.g. usage in `--output-format json`), rather than reverse-engineering the protobuf
- [ ] Mine similar projects for improvements, starting with [firstmate](https://github.com/kunchenguid/firstmate) (overlaps heavily: herdr panes, worktree per task, inbox plus doorbell, quota-aware dispatch). Candidates there (its composer-empty check and turn-end guard are done: `ring()` reads the pane, `mesh watch` nudges): wedge/stale-pane detection, reconciling run state after a restart. Then survey other multi-agent orchestrators for the same and note what to adopt here
- [ ] Live-test `--plan-review user` on plain `opus` (was `opusplan`): the plan dialog still shows `blocked` in herdr, and after approval the worker executes on Opus in the same pane
- [ ] herdr's agy manifest counts working only on the `esc to cancel` footer or a spinner, so a busy agy can read idle; report it upstream with a pane capture (mesh now works around it in `ring()` and the watch's grace)
