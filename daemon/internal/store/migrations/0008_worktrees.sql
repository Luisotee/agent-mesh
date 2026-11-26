-- Standalone run worktrees (`mesh worktree`): created with no agent in them, removed when clean by StopRun.
CREATE TABLE worktrees (
  id         TEXT PRIMARY KEY,
  run_id     TEXT NOT NULL REFERENCES runs(id),
  slug       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  data       BLOB NOT NULL,
  UNIQUE (run_id, slug)
);
CREATE INDEX worktrees_run ON worktrees (run_id, created_at);
