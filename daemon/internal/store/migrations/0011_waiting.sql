-- What waits on a lead (`ListWaiting`, the apps' badge): a plan or question sent to it that no answer to its
-- author followed. Kept as rows by PutMessage (a plan or question adds one on its first insert, an answer to the
-- author removes the author's earlier ones), so a listing costs what is open, not the run's whole history. The
-- bodies stay in `messages`. Filled once from the existing messages at the first Open after this migration.

CREATE TABLE waiting (
  message_id  TEXT PRIMARY KEY,
  to_id       TEXT NOT NULL,
  from_id     TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE INDEX waiting_to ON waiting (to_id, created_at, message_id);
CREATE INDEX waiting_from ON waiting (from_id, created_at, message_id);
