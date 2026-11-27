package store

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// PushRecord is a device's push endpoint (migration 0010).
type PushRecord struct {
	DeviceID     string
	Endpoint     string
	P256dh       []byte
	Auth         []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastOKAt     time.Time // zero: never
	LastError    string
	FailingSince time.Time // zero: not failing
	// Events is what the device chose to be pushed (migration 0014): notice kinds; empty follows the host's list.
	Events []string
}

// Proto is the record as `mesh devices` shows it: the endpoint's host only, since its URL is a capability.
func (p PushRecord) Proto() *v1.PushRegistration {
	out := &v1.PushRegistration{DeviceId: p.DeviceID, RegisteredAt: timestamppb.New(p.UpdatedAt), LastError: p.LastError, Events: p.Events}
	if u, err := url.Parse(p.Endpoint); err == nil {
		out.EndpointHost = u.Host
	}
	if !p.LastOKAt.IsZero() {
		out.LastOkAt = timestamppb.New(p.LastOKAt)
	}
	if !p.FailingSince.IsZero() {
		out.FailingSince = timestamppb.New(p.FailingSince)
	}
	return out
}

// PutPush sets a device's endpoint. An endpoint another device held moves to this one (a phone paired again has a
// new device id but the same endpoint: without the move every push would arrive twice). A changed endpoint starts
// with no error; the same one keeps its history.
func (t *Tx) PutPush(deviceID, endpoint string, p256dh, auth []byte, events []string) error {
	if _, err := t.tx.ExecContext(t.ctx, `DELETE FROM push_endpoints WHERE endpoint = ? AND device_id != ?`, endpoint, deviceID); err != nil {
		return err
	}
	now := nanos(t.now)
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO push_endpoints (device_id, endpoint, p256dh, auth, created_at, updated_at, events)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET
		  last_error    = CASE WHEN endpoint = excluded.endpoint THEN last_error ELSE '' END,
		  failing_since = CASE WHEN endpoint = excluded.endpoint THEN failing_since ELSE NULL END,
		  last_ok_at    = CASE WHEN endpoint = excluded.endpoint THEN last_ok_at ELSE NULL END,
		  endpoint = excluded.endpoint, p256dh = excluded.p256dh, auth = excluded.auth, updated_at = excluded.updated_at,
		  events = excluded.events`,
		deviceID, endpoint, p256dh, auth, now, now, strings.Join(events, ","))
	return err
}

// DeletePush removes a device's endpoint; endpoint, when not empty, must still be the one stored (a result for an
// endpoint the device has since replaced removes nothing).
func (t *Tx) DeletePush(deviceID, endpoint string) (bool, error) {
	q, args := `DELETE FROM push_endpoints WHERE device_id = ?`, []any{deviceID}
	if endpoint != "" {
		q += ` AND endpoint = ?`
		args = append(args, endpoint)
	}
	res, err := t.tx.ExecContext(t.ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkPush records a push's outcome for the endpoint it went to (nothing when the device has replaced it since):
// a success clears the error, a failure keeps the time the failures began.
func (t *Tx) MarkPush(deviceID, endpoint string, ok bool, errText string) error {
	now := nanos(t.now)
	var err error
	if ok {
		_, err = t.tx.ExecContext(t.ctx, `UPDATE push_endpoints SET last_ok_at = ?, last_error = '', failing_since = NULL
			WHERE device_id = ? AND endpoint = ?`, now, deviceID, endpoint)
	} else {
		_, err = t.tx.ExecContext(t.ctx, `UPDATE push_endpoints SET last_error = ?, failing_since = COALESCE(failing_since, ?)
			WHERE device_id = ? AND endpoint = ?`, errText, now, deviceID, endpoint)
	}
	return err
}

// NotePush records a push's error without counting the endpoint as failing (a push it will never take).
func (t *Tx) NotePush(deviceID, endpoint, errText string) error {
	_, err := t.tx.ExecContext(t.ctx, `UPDATE push_endpoints SET last_error = ? WHERE device_id = ? AND endpoint = ?`, errText, deviceID, endpoint)
	return err
}

const pushCols = `device_id, endpoint, p256dh, auth, created_at, updated_at, last_ok_at, last_error, failing_since, events`

// Push returns a device's endpoint, or ErrNotFound.
func (r Reader) Push(deviceID string) (PushRecord, error) {
	p, err := scanPush(r.q.QueryRowContext(r.ctx, `SELECT `+pushCols+` FROM push_endpoints WHERE device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return PushRecord{}, ErrNotFound
	}
	return p, err
}

// ListPush returns every endpoint whose device still has a live token, oldest first.
func (r Reader) ListPush() ([]PushRecord, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT `+pushCols+` FROM push_endpoints p
		WHERE EXISTS (SELECT 1 FROM tokens t WHERE t.subject_id = p.device_id AND t.revoked_at IS NULL)
		ORDER BY created_at, device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushRecord
	for rows.Next() {
		p, err := scanPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPush(sc interface{ Scan(...any) error }) (PushRecord, error) {
	var p PushRecord
	var created, updated int64
	var ok, failing sql.NullInt64
	var events string
	if err := sc.Scan(&p.DeviceID, &p.Endpoint, &p.P256dh, &p.Auth, &created, &updated, &ok, &p.LastError, &failing, &events); err != nil {
		return PushRecord{}, err
	}
	if events != "" {
		p.Events = strings.Split(events, ",")
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	if ok.Valid {
		p.LastOKAt = time.Unix(0, ok.Int64)
	}
	if failing.Valid {
		p.FailingSince = time.Unix(0, failing.Int64)
	}
	return p, nil
}
