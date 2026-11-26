-- Asks and review rounds (`mesh ask`): one row per ask, its record a protobuf blob. A PR's earlier rounds are
-- found by its number and URL, any run; the open ones by status.

CREATE TABLE asks (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL,
  caller_id   TEXT NOT NULL,
  pr          TEXT NOT NULL,
  pr_number   INTEGER NOT NULL DEFAULT 0,
  status      INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  data        BLOB NOT NULL
);
CREATE INDEX asks_run ON asks (run_id, created_at);
CREATE INDEX asks_pr ON asks (pr, created_at);
CREATE INDEX asks_open ON asks (status) WHERE status = 1;
