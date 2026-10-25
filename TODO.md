# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

- [ ] Live end-to-end test of 0.7 in herdr: a `/delegate-claude` task list with a trivial, a small and a large task (background workers reporting by SendMessage, a pane worker through plan → approve → PR, a reviewer per PR, `mesh conflicts`, the board), then `mesh ask` on a stopped background worker with compaction. Also the 0.6 `mesh ask` live test that never ran
- [ ] A Claude root that restarts gets a new socket, so its workers fall back to doorbells until a new run; let `mesh init` (or a small `mesh rebind`) refresh the root's `address` in a live run
- [ ] agy's context size is unknown to mesh, so `--compact auto` compacts every agy worker coming back from an earlier run; read it from `agy --output-format json` usage if agy ever exposes it for a stored conversation
