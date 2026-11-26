-- Models held at a usage limit, persisted across daemon restarts. Times are Unix nanoseconds.
CREATE TABLE model_limits (
  kind       TEXT NOT NULL,
  model      TEXT NOT NULL,
  until      INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (kind, model)
);
