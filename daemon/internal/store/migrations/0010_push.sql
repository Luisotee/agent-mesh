-- Push endpoints (UnifiedPush / WebPush), one per paired device: where meshd sends the notifications that reach a
-- phone with its app closed. `endpoint` is a capability URL and the keys decrypt nothing on their own (the
-- device's private key never leaves it). Deleted with the device's revocation, when the push service says the
-- endpoint is gone, or after it failed for a week.
CREATE TABLE push_endpoints (
  device_id     TEXT PRIMARY KEY,
  endpoint      TEXT NOT NULL UNIQUE,
  p256dh        BLOB NOT NULL,
  auth          BLOB NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_ok_at    INTEGER,
  last_error    TEXT NOT NULL DEFAULT '',
  failing_since INTEGER
);
