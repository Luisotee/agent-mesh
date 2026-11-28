-- Decisions the lead recorded for the user (`RecordDecision`): what and why, kept so the run's overview and the lead's
-- brief list them. A run's about lines and headline live on the agent and run records (their proto blobs), not here.

CREATE TABLE run_decisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  data       BLOB NOT NULL          -- the RunDecision
);
CREATE INDEX run_decisions_run ON run_decisions (run_id, id);
