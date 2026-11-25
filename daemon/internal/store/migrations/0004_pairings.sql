-- One-time pairing tokens; only the SHA-256 hash is stored.
CREATE TABLE pairings (
  hash        TEXT PRIMARY KEY,
  id          TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL DEFAULT '',
  class       INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER,
  device_id   TEXT NOT NULL DEFAULT '',
  mutation_id TEXT NOT NULL DEFAULT '',
  created_by  BLOB NOT NULL
);
CREATE INDEX pairings_expires ON pairings (expires_at);
