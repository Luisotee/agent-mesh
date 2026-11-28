package store

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Decisions (migration 0016): what the lead decided for the user to see, newest last in the table.

// PutDecision stores a decision and returns it with its id.
func (t *Tx) PutDecision(runID string, d *v1.RunDecision) (*v1.RunDecision, error) {
	d.At = timestamppb.New(t.Now())
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO run_decisions (run_id, created_at, data) VALUES (?, ?, ?)`,
		runID, nanos(d.At.AsTime()), []byte{})
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	d.Id = id
	b, err := proto.Marshal(d)
	if err != nil {
		return nil, err
	}
	_, err = t.tx.ExecContext(t.ctx, `UPDATE run_decisions SET data = ? WHERE id = ?`, b, id)
	return d, err
}

// Decisions returns a run's newest decisions, newest first; limit 0 is all of them.
func (r Reader) Decisions(runID string, limit int) ([]*v1.RunDecision, error) {
	if limit <= 0 {
		limit = -1
	}
	return scanAll(r, `SELECT data FROM run_decisions WHERE run_id = ? ORDER BY id DESC LIMIT ?`,
		[]any{runID, limit}, func() *v1.RunDecision { return &v1.RunDecision{} })
}
