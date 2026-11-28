package store

// DeleteRun removes a run and everything the host keeps of it: its agents and their tokens, queued prompts, pull-request
// records and waiting rows, its messages, events, asks, held tasks and standalone worktree rows. It returns the ids of the
// agents it removed. The caller has checked that no agent of the run has a process, and removes the run's files. What
// is not tied to a run stays: the settings, the pull-request holds (kept by URL), the mutation log (pruned by age) and the
// peers (a peer's conversation is its own record).
func (t *Tx) DeleteRun(runID string) ([]string, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT id FROM agents WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	var agents []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		agents = append(agents, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range agents {
		for _, q := range []string{
			`DELETE FROM tokens WHERE subject_id = ?1`,
			`DELETE FROM prompts WHERE agent_id = ?1`,
			`DELETE FROM agent_prs WHERE agent_id = ?1`,
			`DELETE FROM waiting WHERE to_id = ?1 OR from_id = ?1`,
		} {
			if _, err := t.tx.ExecContext(t.ctx, q, id); err != nil {
				return nil, err
			}
		}
	}
	for _, q := range []string{
		`DELETE FROM messages WHERE run_id = ?`,
		`DELETE FROM events WHERE run_id = ?`,
		`DELETE FROM asks WHERE run_id = ?`,
		`DELETE FROM held_tasks WHERE run_id = ?`,
		`DELETE FROM worktrees WHERE run_id = ?`,
		`DELETE FROM agents WHERE run_id = ?`,
		`DELETE FROM runs WHERE id = ?`,
	} {
		if _, err := t.tx.ExecContext(t.ctx, q, runID); err != nil {
			return nil, err
		}
	}
	return agents, nil
}
