-- Pull requests per agent (`mesh prs`, the brief): the last state GitHub gave for each PR an agent owns, kept so
-- a restart, a failed `gh` or a client reading later still has it. Records are protobuf blobs (PullRequest).

CREATE TABLE agent_prs (
  agent_id    TEXT NOT NULL,
  url         TEXT NOT NULL,
  data        BLOB NOT NULL,
  PRIMARY KEY (agent_id, url)
);
