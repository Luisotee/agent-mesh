package store

import "time"

// UsageSample is one reading of a plan's usage window.
type UsageSample struct {
	Pool   string
	At     time.Time
	Used   float64
	Resets time.Time // zero when the reset time is unknown
}

// PutUsageSample records a reading, unless the pool's last one is under `every` old and shows the same use (a
// reading nothing moved is no new point). Samples older than `keep` are dropped.
func (t *Tx) PutUsageSample(s UsageSample, every, keep time.Duration) error {
	var lastAt int64
	var lastUsed float64
	err := t.tx.QueryRowContext(t.ctx, `SELECT at, used FROM usage_samples WHERE pool = ? ORDER BY at DESC LIMIT 1`, s.Pool).Scan(&lastAt, &lastUsed)
	if err == nil {
		if gap := s.At.Sub(time.Unix(0, lastAt)); gap < 0 || (gap < every && lastUsed == s.Used) {
			return nil // a reading older than the last one, or one nothing moved
		}
	}
	var resets int64
	if !s.Resets.IsZero() {
		resets = s.Resets.UnixNano()
	}
	if _, err := t.tx.ExecContext(t.ctx, `INSERT OR REPLACE INTO usage_samples (pool, at, used, resets) VALUES (?, ?, ?, ?)`,
		s.Pool, s.At.UnixNano(), s.Used, resets); err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `DELETE FROM usage_samples WHERE at < ?`, s.At.Add(-keep).UnixNano())
	return err
}

// UsageSamples are a pool's readings since `since`, oldest first.
func (r Reader) UsageSamples(pool string, since time.Time) ([]UsageSample, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT pool, at, used, resets FROM usage_samples WHERE pool = ? AND at >= ? ORDER BY at`, pool, since.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSample
	for rows.Next() {
		var s UsageSample
		var at, resets int64
		if err := rows.Scan(&s.Pool, &at, &s.Used, &resets); err != nil {
			return nil, err
		}
		s.At = time.Unix(0, at)
		if resets != 0 {
			s.Resets = time.Unix(0, resets)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UsagePools are the pools that have readings.
func (r Reader) UsagePools() ([]string, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT DISTINCT pool FROM usage_samples ORDER BY pool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
