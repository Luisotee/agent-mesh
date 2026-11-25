-- An agent with a reopen asked for and not done yet (its record's pending_reopen), so the daemon finds them
-- without reading every agent.

ALTER TABLE agents ADD COLUMN reopen_pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX agents_reopen_pending ON agents (reopen_pending) WHERE reopen_pending = 1;
