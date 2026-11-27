package store

import (
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// Held tasks (migration 0015): a task sent with `after`, kept until the task it names is reported done.

// PutHeldTask stores a held task.
func (t *Tx) PutHeldTask(h *v1.HeldTask) error {
	b, err := proto.Marshal(h)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO held_tasks (id, run_id, after_id, created_at, data) VALUES (?, ?, ?, ?, ?)`,
		h.Id, h.RunId, h.After, nanos(h.CreatedAt.AsTime()), b)
	return err
}

// HeldTasksAfter returns the tasks held for a task, oldest first: the order they were asked in.
func (r Reader) HeldTasksAfter(afterID string) ([]*v1.HeldTask, error) {
	return scanAll(r, `SELECT data FROM held_tasks WHERE after_id = ? ORDER BY created_at, id`,
		[]any{afterID}, func() *v1.HeldTask { return &v1.HeldTask{} })
}

// HeldTasks returns the held tasks of a run, or of every run when runID is empty, oldest first.
func (r Reader) HeldTasks(runID string) ([]*v1.HeldTask, error) {
	if runID == "" {
		return scanAll(r, `SELECT data FROM held_tasks ORDER BY created_at, id`, nil, func() *v1.HeldTask { return &v1.HeldTask{} })
	}
	return scanAll(r, `SELECT data FROM held_tasks WHERE run_id = ? ORDER BY created_at, id`,
		[]any{runID}, func() *v1.HeldTask { return &v1.HeldTask{} })
}

// GetHeldTask returns one held task; ErrNotFound when it is not held (any more).
func (r Reader) GetHeldTask(id string) (*v1.HeldTask, error) {
	return scanOne(r, `SELECT data FROM held_tasks WHERE id = ?`, []any{id}, &v1.HeldTask{})
}

// DeleteHeldTask drops a held task; ErrNotFound when it is not held (a release or a cancel got there first).
func (t *Tx) DeleteHeldTask(id string) error {
	res, err := t.tx.ExecContext(t.ctx, `DELETE FROM held_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotFound
	}
	return nil
}

// MessageByID returns a stored message by its id, whoever it is for; ErrNotFound when there is none.
func (r Reader) MessageByID(id string) (*v1.Message, error) {
	return scanOne(r, `SELECT data FROM messages WHERE id = ?`, []any{id}, &v1.Message{})
}
