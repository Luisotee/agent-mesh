package store

import (
	"time"

	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// The prompts queue (migration 0013): what a device sent an agent's chat while it couldn't take a prompt, typed at
// its next idle. A prompt leaves `queued` once, by a compare-and-set: ClaimPrompt (to be typed) or WithdrawPrompt.

// QueuePrompt stores a prompt as queued.
func (t *Tx) QueuePrompt(p *v1.QueuedPrompt) error {
	b, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO prompts (id, agent_id, state, created_at, data) VALUES (?, ?, 'queued', ?, ?)`,
		p.Id, p.AgentId, nanos(p.CreatedAt.AsTime()), b)
	return err
}

// QueuedPrompts returns an agent's queued prompts, oldest first: the order they are typed in.
func (r Reader) QueuedPrompts(agentID string) ([]*v1.QueuedPrompt, error) {
	return scanAll(r, `SELECT data FROM prompts WHERE agent_id = ? AND state = 'queued' ORDER BY created_at, id`,
		[]any{agentID}, func() *v1.QueuedPrompt { return &v1.QueuedPrompt{} })
}

// AgentsWithQueuedPrompts lists the agents that have a prompt waiting to be typed.
func (r Reader) AgentsWithQueuedPrompts() ([]string, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT DISTINCT agent_id FROM prompts WHERE state = 'queued'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// leaveQueue moves a queued prompt of an agent to `to`, and returns it; ErrNotFound when it is not queued (any more).
func (t *Tx) leaveQueue(agentID, id, to string) (*v1.QueuedPrompt, error) {
	p, err := scanOne(t.Reader, `SELECT data FROM prompts WHERE id = ? AND agent_id = ? AND state = 'queued'`,
		[]any{id, agentID}, &v1.QueuedPrompt{})
	if err != nil {
		return nil, err
	}
	res, err := t.tx.ExecContext(t.ctx, `UPDATE prompts SET state = ?, done_at = ? WHERE id = ? AND state = 'queued'`,
		to, nanos(t.now), id)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, ErrNotFound
	}
	return p, nil
}

// ClaimPrompt takes a queued prompt for typing: from here it can't be withdrawn. ErrNotFound when it was withdrawn
// (or claimed) first.
func (t *Tx) ClaimPrompt(agentID, id string) (*v1.QueuedPrompt, error) {
	return t.leaveQueue(agentID, id, "typed")
}

// WithdrawPrompt takes a queued prompt back. ErrNotFound when it is not queued: typed (or being typed) already, or
// withdrawn before.
func (t *Tx) WithdrawPrompt(agentID, id string) (*v1.QueuedPrompt, error) {
	return t.leaveQueue(agentID, id, "withdrawn")
}

// RequeuePrompt puts a claimed prompt back at its place in the queue: its typing certainly never reached the terminal.
func (t *Tx) RequeuePrompt(id string) error {
	_, err := t.tx.ExecContext(t.ctx, `UPDATE prompts SET state = 'queued', done_at = NULL WHERE id = ? AND state = 'typed'`, id)
	return err
}

// PrunePrompts forgets prompts typed or withdrawn before a time: only the queue is ever read.
func (t *Tx) PrunePrompts(before time.Time) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx, `DELETE FROM prompts WHERE state != 'queued' AND done_at < ?`, nanos(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
