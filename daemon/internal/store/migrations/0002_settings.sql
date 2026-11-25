-- Settings as host data: only the overrides are kept, a setting nobody set is its built-in default. `value` is
-- the JSON of the value, `updated_by` the Actor that set it (a protobuf blob). Times are Unix nanoseconds.

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  updated_by BLOB NOT NULL
);
