-- Merges the user holds (`HoldPullRequest`), by the pull request's normalized URL (`PRKey`) so a hold outlives every
-- `agent_prs` row of the PR: a row dropped by a clean read of GitHub and made again for another agent is held as before.

CREATE TABLE pr_holds (
  key TEXT NOT NULL PRIMARY KEY
);
