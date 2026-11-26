-- Standing peers (`mesh add-peer`): cloned projects, each a read-only asker that keeps its conversation across
-- runs, and the groups they are kept in. Records are protobuf blobs.

CREATE TABLE peers (
  name        TEXT PRIMARY KEY,
  group_name  TEXT NOT NULL,
  data        BLOB NOT NULL
);
CREATE INDEX peers_group ON peers (group_name);

CREATE TABLE peer_groups (
  name  TEXT PRIMARY KEY,
  data  BLOB NOT NULL
);
