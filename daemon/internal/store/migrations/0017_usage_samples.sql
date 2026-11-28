-- One reading of a plan's usage window per row, kept 45 days: what `mesh usage` needs to say how many tokens a percent
-- of that window is (the tokens counted between two readings of one reset period, over the percent they moved).
-- `pool` names the window ("claude:five_hour", "claude:seven_day", "agy:<group>:weekly"), `used` is percent used and
-- `resets` the window's reset time as unix nanoseconds, 0 when unknown.

CREATE TABLE usage_samples (
  pool    TEXT NOT NULL,
  at      INTEGER NOT NULL,
  used    REAL NOT NULL,
  resets  INTEGER NOT NULL,
  PRIMARY KEY (pool, at)
);
