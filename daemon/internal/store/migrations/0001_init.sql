-- The first schema. Records are protobuf blobs (`data`) with the columns lookups need beside them, so a new
-- field needs no migration. Times are Unix nanoseconds.

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE runs (
  id         TEXT PRIMARY KEY,
  status     INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  data       BLOB NOT NULL
);

CREATE TABLE agents (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES runs(id),
  name        TEXT NOT NULL,
  parent_id   TEXT NOT NULL DEFAULT '',
  terminal_id TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  data        BLOB NOT NULL,
  UNIQUE (run_id, name)
);

CREATE TABLE messages (
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL,
  to_id        TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  delivered_at INTEGER,
  read_at      INTEGER,
  data         BLOB NOT NULL
);
CREATE INDEX messages_to ON messages (to_id, read_at, created_at);

-- Tokens are stored by their SHA-256; the secret itself is never kept.
CREATE TABLE tokens (
  hash       TEXT PRIMARY KEY,
  class      INTEGER NOT NULL,
  subject_id TEXT NOT NULL,
  name       TEXT NOT NULL,
  run_id     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE INDEX tokens_subject ON tokens (subject_id);

-- Writes already applied, by the actor's own mutation id, with the response to return to a retry.
CREATE TABLE mutations (
  actor_id   TEXT NOT NULL,
  id         TEXT NOT NULL,
  procedure  TEXT NOT NULL,
  response   BLOB,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (actor_id, id)
);

CREATE TABLE events (
  seq      INTEGER PRIMARY KEY AUTOINCREMENT,
  time     INTEGER NOT NULL,
  run_id   TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '',
  data     BLOB NOT NULL
);
CREATE INDEX events_run ON events (run_id, seq);
