-- Tasks held until another task is reported done (`SendMessage` with `after`): the request and who made it, kept until a
-- `done` answers the task it waits for, then sent through the whole send path as the sender and deleted. Nothing else
-- reads this table, so a held task is in no inbox, no doorbell and no event until it is sent.

CREATE TABLE held_tasks (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL,
  after_id    TEXT NOT NULL,        -- the message id of the task it waits for
  created_at  INTEGER NOT NULL,
  data        BLOB NOT NULL         -- the HeldTask
);
CREATE INDEX held_tasks_after ON held_tasks (after_id, created_at, id);
CREATE INDEX held_tasks_run ON held_tasks (run_id, created_at, id);
