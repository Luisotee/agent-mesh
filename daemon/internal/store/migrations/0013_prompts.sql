-- The prompts a device sent an agent's chat that couldn't be typed at once (`PromptAgent` while the agent worked,
-- started, waited at a prompt or had a draft in its box): held here, so they survive a restart, and typed one at a
-- time at the agent's next idle. A row leaves `queued` exactly once: to `typed` (the delivery claimed it, then typed
-- it) or to `withdrawn` (`WithdrawPrompt`), each a compare-and-set on the state, so a prompt is never both.

CREATE TABLE prompts (
  id          TEXT PRIMARY KEY,
  agent_id    TEXT NOT NULL,
  state       TEXT NOT NULL,          -- queued, typed, withdrawn
  created_at  INTEGER NOT NULL,
  done_at     INTEGER,                -- when it was typed or withdrawn
  data        BLOB NOT NULL           -- the QueuedPrompt
);
CREATE INDEX prompts_queued ON prompts (agent_id, state, created_at, id);
