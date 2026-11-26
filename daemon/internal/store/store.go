// Package store keeps the host's state in one SQLite database: runs, agents, messages, tokens, applied
// mutations and the event log. Every change goes through Tx, which numbers the events it appends and hands
// them to OnCommit after the commit, in sequence order.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"

	_ "modernc.org/sqlite" // the pure-Go driver, so builds stay static
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound is returned by lookups that find nothing.
var ErrNotFound = errors.New("not found")

// Store is the open database.
type Store struct {
	path       string
	w          *sql.DB // one connection, BEGIN IMMEDIATE: the only writer
	r          *sql.DB // readers, which WAL lets run beside the writer
	wmu        sync.Mutex
	generation string
	now        func() time.Time

	// OnCommit receives each committed transaction's events, in sequence order, before the next write starts.
	OnCommit func([]*v1.Event)
}

// Open opens (creating or migrating) the database at path. A database with an older schema is backed up next
// to it before it is migrated.
func Open(path string) (*Store, error) {
	return OpenAt(path, time.Now)
}

// OpenAt is Open with a clock, for tests.
func OpenAt(path string, now func() time.Time) (*Store, error) {
	// The path goes into a URI: escape what a URI would read as its query, fragment or an escape.
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	dsn := func(lock string) string {
		return "file:" + uriPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)" +
			"&_pragma=synchronous(FULL)&_txlock=" + lock
	}
	w, err := sql.Open("sqlite", dsn("immediate"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w, path, now); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn("deferred"))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	s := &Store{path: path, w: w, r: r, now: now}
	if err := s.loadGeneration(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// Generation identifies this event log: a cursor from another generation can't be resumed.
func (s *Store) Generation() string { return s.generation }

func (s *Store) loadGeneration() error {
	err := s.r.QueryRow(`SELECT value FROM meta WHERE key = 'generation'`).Scan(&s.generation)
	if errors.Is(err, sql.ErrNoRows) {
		s.generation = ids.New("g", 12)
		_, err = s.w.Exec(`INSERT INTO meta (key, value) VALUES ('generation', ?)`, s.generation)
	}
	return err
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %q: name must start with its number", e.Name())
		}
		b, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{v, e.Name(), string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1, 2, …: found %s at %d", m.name, i+1)
		}
	}
	return out, nil
}

// SchemaVersion is the newest migration's number.
func SchemaVersion() int {
	ms, err := migrations()
	if err != nil {
		panic(err)
	}
	return len(ms)
}

func migrate(db *sql.DB, path string, now func() time.Time) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	return migrateWith(db, path, now, ms)
}

// migrateWith applies the migrations past the database's user_version, after backing up a database that
// already has a schema.
func migrateWith(db *sql.DB, path string, now func() time.Time, ms []migration) error {
	var have int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&have); err != nil {
		return err
	}
	if have > len(ms) {
		return fmt.Errorf("%s has schema %d, newer than this daemon's %d: upgrade the daemon", path, have, len(ms))
	}
	if have == len(ms) {
		return nil
	}
	if have > 0 {
		backup := fmt.Sprintf("%s.bak-v%d-%s", path, have, now().UTC().Format("20060102T150405"))
		if _, err := os.Stat(backup); err == nil {
			return fmt.Errorf("backup %s already exists", backup)
		}
		if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
			return fmt.Errorf("backing up before migrating: %w", err)
		}
	}
	for _, m := range ms[have:] {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// queryer is what both a transaction and the reader pool offer.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Reader reads committed state. Store's read methods use it; Tx embeds one over its own transaction.
type Reader struct {
	q   queryer
	ctx context.Context
}

// Read returns a Reader over the reader pool.
func (s *Store) Read(ctx context.Context) Reader { return Reader{q: s.r, ctx: ctx} }

// Snapshot runs fn in one read transaction, so everything it reads is from the same moment.
func (s *Store) Snapshot(ctx context.Context, fn func(Reader) error) error {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(Reader{q: tx, ctx: ctx})
}

// Tx is a write transaction.
type Tx struct {
	Reader
	tx  *sql.Tx
	s   *Store
	now time.Time
	// Stamped on every event appended in this transaction.
	Actor      *v1.Actor
	MutationID string
	events     []*v1.Event
}

// Now is the transaction's clock reading: every record it writes shares it.
func (t *Tx) Now() time.Time { return t.now }

// Write runs fn in a write transaction. The events fn appends are published through OnCommit once it commits.
func (s *Store) Write(ctx context.Context, actor *v1.Actor, mutationID string, fn func(*Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	sqlTx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// A no-op once committed; a panic in fn leaves no open transaction on the one writer connection.
	defer sqlTx.Rollback()
	t := &Tx{Reader: Reader{q: sqlTx, ctx: ctx}, tx: sqlTx, s: s, now: s.now(), Actor: actor, MutationID: mutationID}
	if err := fn(t); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	if s.OnCommit != nil && len(t.events) > 0 {
		s.OnCommit(t.events)
	}
	return nil
}

func nanos(t time.Time) int64 { return t.UnixNano() }

// ── events ──

// AppendEvent numbers ev, stamps it with the generation, time, actor and mutation id, and stores it.
func (t *Tx) AppendEvent(ev *v1.Event) (*v1.Event, error) {
	ev.Generation = t.s.generation
	ev.Time = timestamppb.New(t.now)
	if ev.Actor == nil {
		ev.Actor = t.Actor
	}
	if ev.MutationId == "" {
		ev.MutationId = t.MutationID
	}
	// The row is inserted first to get its number, then rewritten with the number inside.
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO events (time, run_id, agent_id, data) VALUES (?, ?, ?, x'')`,
		nanos(t.now), ev.RunId, ev.AgentId)
	if err != nil {
		return nil, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	ev.Seq = seq
	b, err := proto.Marshal(ev)
	if err != nil {
		return nil, err
	}
	if _, err := t.tx.ExecContext(t.ctx, `UPDATE events SET data = ? WHERE seq = ?`, b, seq); err != nil {
		return nil, err
	}
	t.events = append(t.events, ev)
	return ev, nil
}

// LastSeq is the newest event's number, 0 for an empty log.
func (r Reader) LastSeq() (int64, error) {
	var seq sql.NullInt64
	err := r.q.QueryRowContext(r.ctx, `SELECT MAX(seq) FROM events`).Scan(&seq)
	return seq.Int64, err
}

// EventsAfter returns up to limit events numbered after seq, oldest first, of one run when runID is set (and
// the global ones, which have no run).
func (r Reader) EventsAfter(seq int64, runID string, limit int) ([]*v1.Event, error) {
	query := `SELECT data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`
	args := []any{seq, limit}
	if runID != "" {
		query = `SELECT data FROM events WHERE seq > ? AND (run_id = ? OR run_id = '') ORDER BY seq LIMIT ?`
		args = []any{seq, runID, limit}
	}
	return scanAll(r, query, args, func() *v1.Event { return &v1.Event{} })
}

func scanAll[T proto.Message](r Reader, query string, args []any, mk func() T) ([]T, error) {
	rows, err := r.q.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		m := mk()
		if err := proto.Unmarshal(b, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanOne[T proto.Message](r Reader, query string, args []any, m T) (T, error) {
	var b []byte
	err := r.q.QueryRowContext(r.ctx, query, args...).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	return m, proto.Unmarshal(b, m)
}

// ── runs ──

// ErrExists is a create that found its row already there.
var ErrExists = errors.New("already exists")

// PutRun creates a run; a run with that id already there is ErrExists, never overwritten.
func (t *Tx) PutRun(run *v1.Run) error {
	b, err := proto.Marshal(run)
	if err != nil {
		return err
	}
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO runs (id, status, created_at, data) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`, run.Id, int32(run.Status), run.CreatedAt.AsTime().UnixNano(), b)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrExists
	}
	return err
}

// UpdateRun replaces a run's record (its status, say).
func (t *Tx) UpdateRun(run *v1.Run) error {
	b, err := proto.Marshal(run)
	if err != nil {
		return err
	}
	res, err := t.tx.ExecContext(t.ctx, `UPDATE runs SET status = ?, data = ? WHERE id = ?`, int32(run.Status), b, run.Id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// GetRun returns one run.
func (r Reader) GetRun(id string) (*v1.Run, error) {
	return scanOne(r, `SELECT data FROM runs WHERE id = ?`, []any{id}, &v1.Run{})
}

// ListRuns returns the runs, oldest first; only open ones unless all.
func (r Reader) ListRuns(all bool) ([]*v1.Run, error) {
	query := `SELECT data FROM runs WHERE status = ? ORDER BY created_at, id`
	args := []any{int32(v1.RunStatus_RUN_STATUS_OPEN)}
	if all {
		query, args = `SELECT data FROM runs ORDER BY created_at, id`, nil
	}
	return scanAll(r, query, args, func() *v1.Run { return &v1.Run{} })
}

// ── agents ──

// PutAgent inserts or replaces an agent.
func (t *Tx) PutAgent(a *v1.Agent) error {
	b, err := proto.Marshal(a)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO agents (id, run_id, name, parent_id, terminal_id, created_at, data,
		reopen_pending)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET run_id = excluded.run_id, name = excluded.name,
		parent_id = excluded.parent_id, terminal_id = excluded.terminal_id, data = excluded.data,
		reopen_pending = excluded.reopen_pending`,
		a.Id, a.RunId, a.Name, a.ParentId, a.TerminalId, a.CreatedAt.AsTime().UnixNano(), b, a.PendingReopen != nil)
	return err
}

// GetAgent returns one agent by id.
func (r Reader) GetAgent(id string) (*v1.Agent, error) {
	return scanOne(r, `SELECT data FROM agents WHERE id = ?`, []any{id}, &v1.Agent{})
}

// AgentsNamed returns the agents with this name in the given run, or in every open run when runID is empty.
func (r Reader) AgentsNamed(runID, name string) ([]*v1.Agent, error) {
	if runID != "" {
		return scanAll(r, `SELECT data FROM agents WHERE run_id = ? AND name = ?`, []any{runID, name},
			func() *v1.Agent { return &v1.Agent{} })
	}
	return scanAll(r, `SELECT a.data FROM agents a JOIN runs r ON r.id = a.run_id
		WHERE a.name = ? AND r.status = ? ORDER BY a.created_at`, []any{name, int32(v1.RunStatus_RUN_STATUS_OPEN)},
		func() *v1.Agent { return &v1.Agent{} })
}

// AgentsNamedAll returns every agent with this name across all runs (open and closed), newest first.
func (r Reader) AgentsNamedAll(name string) ([]*v1.Agent, error) {
	return scanAll(r, `SELECT data FROM agents WHERE name = ? ORDER BY created_at DESC, id DESC`, []any{name},
		func() *v1.Agent { return &v1.Agent{} })
}

// AgentsNamedClosed returns every agent with this name in closed runs, newest first.
func (r Reader) AgentsNamedClosed(name string) ([]*v1.Agent, error) {
	return scanAll(r, `SELECT a.data FROM agents a JOIN runs r ON r.id = a.run_id
		WHERE a.name = ? AND r.status = ? ORDER BY a.created_at DESC, a.id DESC`,
		[]any{name, int32(v1.RunStatus_RUN_STATUS_CLOSED)}, func() *v1.Agent { return &v1.Agent{} })
}

// AllAgents returns every agent across all runs (open and closed), oldest first.
func (r Reader) AllAgents() ([]*v1.Agent, error) {
	return scanAll(r, `SELECT data FROM agents ORDER BY created_at, id`, nil,
		func() *v1.Agent { return &v1.Agent{} })
}

// ListAgents returns a run's agents, or every open run's when runID is empty, oldest first.
func (r Reader) ListAgents(runID string) ([]*v1.Agent, error) {
	if runID != "" {
		return scanAll(r, `SELECT data FROM agents WHERE run_id = ? ORDER BY created_at, id`, []any{runID},
			func() *v1.Agent { return &v1.Agent{} })
	}
	return scanAll(r, `SELECT a.data FROM agents a JOIN runs r ON r.id = a.run_id WHERE r.status = ?
		ORDER BY a.created_at, a.id`, []any{int32(v1.RunStatus_RUN_STATUS_OPEN)},
		func() *v1.Agent { return &v1.Agent{} })
}

// DeleteAgent removes an agent that never ran (its launch failed), so its name can be used again. Its
// events stay.
func (t *Tx) DeleteAgent(id string) error {
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM agents WHERE id = ?`, id)
	return err
}

// AgentsWithUndelivered lists the agents that have messages whose doorbell hasn't rung.
func (r Reader) AgentsWithUndelivered() ([]string, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT DISTINCT to_id FROM messages WHERE delivered_at IS NULL AND read_at IS NULL`)
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

// AgentByTerminal returns the agent running in a terminal.
func (r Reader) AgentByTerminal(terminalID string) (*v1.Agent, error) {
	return scanOne(r, `SELECT data FROM agents WHERE terminal_id = ?`, []any{terminalID}, &v1.Agent{})
}

// AgentsWithPendingReopen lists the agents with a reopen asked for and not done yet, in any run.
func (r Reader) AgentsWithPendingReopen() ([]*v1.Agent, error) {
	return scanAll(r, `SELECT data FROM agents WHERE reopen_pending = 1 ORDER BY created_at, id`, nil,
		func() *v1.Agent { return &v1.Agent{} })
}

// AgentsWithTerminals returns every agent that has a terminal id, in any run.
func (r Reader) AgentsWithTerminals() ([]*v1.Agent, error) {
	return scanAll(r, `SELECT data FROM agents WHERE terminal_id != '' ORDER BY created_at, id`, nil,
		func() *v1.Agent { return &v1.Agent{} })
}

// ── messages ──

// PutMessage inserts or replaces a message.
func (t *Tx) PutMessage(m *v1.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	var delivered, read sql.NullInt64
	if m.DeliveredAt != nil {
		delivered = sql.NullInt64{Int64: m.DeliveredAt.AsTime().UnixNano(), Valid: true}
	}
	if m.ReadAt != nil {
		read = sql.NullInt64{Int64: m.ReadAt.AsTime().UnixNano(), Valid: true}
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO messages (id, run_id, to_id, created_at, delivered_at, read_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET delivered_at = excluded.delivered_at, read_at = excluded.read_at,
		data = excluded.data`,
		m.Id, m.RunId, m.ToId, m.CreatedAt.AsTime().UnixNano(), delivered, read, b)
	return err
}

// MarkDelivered records that a message's doorbell reached its agent, on the row as it is now: a read time set
// since the caller looked is kept. A message that is gone, or already marked, is left as it is.
func (t *Tx) MarkDelivered(id string) error {
	m, err := scanOne(t.Reader, `SELECT data FROM messages WHERE id = ?`, []any{id}, &v1.Message{})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.DeliveredAt != nil {
		return nil
	}
	m.DeliveredAt = timestamppb.New(t.now)
	return t.PutMessage(m)
}

// Inbox returns an agent's messages, oldest first: unread ones, or all.
func (r Reader) Inbox(agentID string, all bool) ([]*v1.Message, error) {
	query := `SELECT data FROM messages WHERE to_id = ? AND read_at IS NULL ORDER BY created_at, id`
	if all {
		query = `SELECT data FROM messages WHERE to_id = ? ORDER BY created_at, id`
	}
	return scanAll(r, query, []any{agentID}, func() *v1.Message { return &v1.Message{} })
}

// RunMessages returns a run's messages, oldest first, up to limit, after the message `after` (its id; empty
// starts at the oldest). An unknown `after` is ErrNotFound.
func (r Reader) RunMessages(runID, after string, limit int) ([]*v1.Message, error) {
	query := `SELECT data FROM messages WHERE run_id = ? ORDER BY created_at, id LIMIT ?`
	args := []any{runID, limit}
	if after != "" {
		var created int64
		if err := r.q.QueryRowContext(r.ctx, `SELECT created_at FROM messages WHERE id = ? AND run_id = ?`, after, runID).Scan(&created); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		query = `SELECT data FROM messages WHERE run_id = ? AND (created_at, id) > (?, ?) ORDER BY created_at, id LIMIT ?`
		args = []any{runID, created, after, limit}
	}
	return scanAll(r, query, args, func() *v1.Message { return &v1.Message{} })
}

// Undelivered returns an agent's messages whose doorbell hasn't reached it, oldest first.
func (r Reader) Undelivered(agentID string) ([]*v1.Message, error) {
	return scanAll(r, `SELECT data FROM messages WHERE to_id = ? AND delivered_at IS NULL AND read_at IS NULL
		ORDER BY created_at, id`, []any{agentID}, func() *v1.Message { return &v1.Message{} })
}

// ── tokens ──

// TokenRecord is what a token stands for.
type TokenRecord struct {
	Class     v1.ActorClass
	SubjectID string
	Name      string
	RunID     string
}

// PutToken stores a token's hash. A token of an agent class names its run: without one it would read every run.
func (t *Tx) PutToken(hash string, rec TokenRecord) error {
	switch rec.Class {
	case v1.ActorClass_ACTOR_CLASS_ROOT, v1.ActorClass_ACTOR_CLASS_WORKER,
		v1.ActorClass_ACTOR_CLASS_ASKER, v1.ActorClass_ACTOR_CLASS_REVIEWER:
		if rec.RunID == "" {
			return errors.New("a token for an agent needs a run id")
		}
	}
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO tokens (hash, class, subject_id, name, run_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, hash, int32(rec.Class), rec.SubjectID, rec.Name, rec.RunID, nanos(t.now))
	return err
}

// RevokeTokens revokes every token of a subject.
func (t *Tx) RevokeTokens(subjectID string) error {
	_, err := t.tx.ExecContext(t.ctx, `UPDATE tokens SET revoked_at = ? WHERE subject_id = ? AND revoked_at IS NULL`,
		nanos(t.now), subjectID)
	return err
}

// SetTokenClass changes the class of every live token of a subject: the class is read from the row on every
// call, so a running agent's role change applies to its process at once.
func (t *Tx) SetTokenClass(subjectID string, class v1.ActorClass) error {
	_, err := t.tx.ExecContext(t.ctx, `UPDATE tokens SET class = ? WHERE subject_id = ? AND revoked_at IS NULL`,
		int32(class), subjectID)
	return err
}

// TokenKnown reports whether a hash is stored at all, revoked or not.
func (r Reader) TokenKnown(hash string) (bool, error) {
	var n int
	err := r.q.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM tokens WHERE hash = ?`, hash).Scan(&n)
	return n > 0, err
}

// Token resolves a token hash; revoked tokens are ErrNotFound.
func (r Reader) Token(hash string) (TokenRecord, error) {
	var rec TokenRecord
	var class int32
	err := r.q.QueryRowContext(r.ctx, `SELECT class, subject_id, name, run_id FROM tokens
		WHERE hash = ? AND revoked_at IS NULL`, hash).Scan(&class, &rec.SubjectID, &rec.Name, &rec.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	rec.Class = v1.ActorClass(class)
	return rec, err
}

// HasActiveToken reports whether subjectID currently has at least one unrevoked token.
func (r Reader) HasActiveToken(subjectID string) (bool, error) {
	var n int
	err := r.q.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM tokens WHERE subject_id = ? AND revoked_at IS NULL`, subjectID).Scan(&n)
	return n > 0, err
}

// ── mutations ──

// Mutation returns the stored response of a write this actor already applied under this id.
func (r Reader) Mutation(actorID, id string) (procedure string, response []byte, err error) {
	err = r.q.QueryRowContext(r.ctx, `SELECT procedure, response FROM mutations WHERE actor_id = ? AND id = ?`,
		actorID, id).Scan(&procedure, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return procedure, response, err
}

// PutMutation records an applied write and its response.
func (t *Tx) PutMutation(actorID, id, procedure string, response []byte) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO mutations (actor_id, id, procedure, response, created_at)
		VALUES (?, ?, ?, ?, ?)`, actorID, id, procedure, response, nanos(t.now))
	return err
}

// PruneMutations forgets applied writes: hook reports (`hook-` ids, two per tool call, retried only within
// minutes) older than hookBefore, any other older than before. It returns how many rows went.
func (t *Tx) PruneMutations(hookBefore, before time.Time) (int64, error) {
	res, err := t.tx.ExecContext(t.ctx, `DELETE FROM mutations WHERE created_at < ? OR (id LIKE 'hook-%' AND created_at < ?)`,
		nanos(before), nanos(hookBefore))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ScrubPairingMutation keeps a CreatePairing mutation record (so retries with the same mutation_id replay
// instead of minting a fresh token) while clearing its secret fields (token, uri, relay_client_token).
func (t *Tx) ScrubPairingMutation(actorID, id string) error {
	if actorID == "" || id == "" {
		return nil
	}
	_, raw, err := t.Mutation(actorID, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp := &v1.CreatePairingResponse{}
	if err := proto.Unmarshal(raw, resp); err != nil {
		return err
	}
	resp.Token = ""
	resp.Uri = ""
	resp.RelayClientToken = ""
	scrubbed, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `UPDATE mutations SET response = ? WHERE actor_id = ? AND id = ?`,
		scrubbed, actorID, id)
	return err
}

// ── pairings & devices ──

// PairingRecord is one stored one-time pairing token (only its SHA-256 hash is kept).
type PairingRecord struct {
	Hash       string
	ID         string
	Name       string
	Class      v1.ActorClass
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     time.Time
	DeviceID   string
	MutationID string
	CreatedBy  *v1.Actor
}

// PutPairing stores a one-time pairing token hash.
func (t *Tx) PutPairing(rec PairingRecord) error {
	by, err := proto.Marshal(rec.CreatedBy)
	if err != nil {
		return err
	}
	if by == nil {
		by = []byte{}
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO pairings (hash, id, name, class, created_at, expires_at, mutation_id, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Hash, rec.ID, rec.Name, int32(rec.Class), nanos(rec.CreatedAt), nanos(rec.ExpiresAt), rec.MutationID, by)
	return err
}

// Pairing looks up a pairing record by token hash.
func (r Reader) Pairing(hash string) (PairingRecord, error) {
	var rec PairingRecord
	var class int32
	var created, expires int64
	var used sql.NullInt64
	var by []byte
	err := r.q.QueryRowContext(r.ctx, `SELECT hash, id, name, class, created_at, expires_at, used_at, device_id, mutation_id, created_by
		FROM pairings WHERE hash = ?`, hash).
		Scan(&rec.Hash, &rec.ID, &rec.Name, &class, &created, &expires, &used, &rec.DeviceID, &rec.MutationID, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	rec.Class = v1.ActorClass(class)
	rec.CreatedAt = time.Unix(0, created)
	rec.ExpiresAt = time.Unix(0, expires)
	if used.Valid {
		rec.UsedAt = time.Unix(0, used.Int64)
	}
	rec.CreatedBy = &v1.Actor{}
	if err := proto.Unmarshal(by, rec.CreatedBy); err != nil {
		return rec, err
	}
	return rec, nil
}

// MarkPairingUsed marks a pairing token as redeemed by deviceID.
func (t *Tx) MarkPairingUsed(hash, deviceID string) error {
	res, err := t.tx.ExecContext(t.ctx, `UPDATE pairings SET used_at = ?, device_id = ? WHERE hash = ? AND used_at IS NULL`,
		nanos(t.now), deviceID, hash)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// PrunePairings removes expired unused pairings (and scrubs their CreatePairing mutation responses).
func (t *Tx) PrunePairings(before time.Time) (int64, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT mutation_id, created_by FROM pairings WHERE expires_at < ? AND mutation_id != ''`,
		nanos(before))
	if err != nil {
		return 0, err
	}
	type mut struct{ id, actorID string }
	var muts []mut
	for rows.Next() {
		var id string
		var by []byte
		if err := rows.Scan(&id, &by); err != nil {
			rows.Close()
			return 0, err
		}
		a := &v1.Actor{}
		if proto.Unmarshal(by, a) == nil && a.Id != "" {
			muts = append(muts, mut{id: id, actorID: a.Id})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, m := range muts {
		if err := t.ScrubPairingMutation(m.actorID, m.id); err != nil {
			return 0, err
		}
	}
	res, err := t.tx.ExecContext(t.ctx, `DELETE FROM pairings WHERE expires_at < ?`, nanos(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListDevices lists device tokens (excluding excludeSubjectID, such as the host's own local device).
func (r Reader) ListDevices(includeRevoked bool, excludeSubjectID string) ([]*v1.Device, error) {
	query := `SELECT subject_id, name, class, created_at, revoked_at FROM tokens
		WHERE class IN (?, ?) AND subject_id != ?`
	if !includeRevoked {
		query += ` AND revoked_at IS NULL`
	}
	query += ` ORDER BY created_at, subject_id`
	rows, err := r.q.QueryContext(r.ctx, query,
		int32(v1.ActorClass_ACTOR_CLASS_DEVICE), int32(v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE), excludeSubjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*v1.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDevice returns one device by subject_id.
func (r Reader) GetDevice(id string) (*v1.Device, error) {
	d, err := scanDevice(r.q.QueryRowContext(r.ctx, `SELECT subject_id, name, class, created_at, revoked_at
		FROM tokens WHERE subject_id = ? AND class IN (?, ?) ORDER BY created_at DESC LIMIT 1`,
		id, int32(v1.ActorClass_ACTOR_CLASS_DEVICE), int32(v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func scanDevice(sc interface{ Scan(...any) error }) (*v1.Device, error) {
	var d v1.Device
	var class int32
	var created int64
	var revoked sql.NullInt64
	if err := sc.Scan(&d.Id, &d.Name, &class, &created, &revoked); err != nil {
		return nil, err
	}
	d.Class = v1.ActorClass(class)
	d.CreatedAt = timestamppb.New(time.Unix(0, created))
	if revoked.Valid {
		d.RevokedAt = timestamppb.New(time.Unix(0, revoked.Int64))
	}
	return &d, nil
}

// ── meta ──

// Meta reads a value of the meta table.
func (r Reader) Meta(key string) (string, error) {
	var v string
	err := r.q.QueryRowContext(r.ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// PutMeta writes a value of the meta table.
func (t *Tx) PutMeta(key, value string) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ── settings ──

// SettingRecord is one stored override: the JSON of its value, when it was set and by whom.
type SettingRecord struct {
	Key       string
	Value     string
	UpdatedAt time.Time
	UpdatedBy *v1.Actor
}

func scanSetting(sc interface{ Scan(...any) error }) (SettingRecord, error) {
	var rec SettingRecord
	var at int64
	var by []byte
	if err := sc.Scan(&rec.Key, &rec.Value, &at, &by); err != nil {
		return rec, err
	}
	rec.UpdatedAt = time.Unix(0, at)
	rec.UpdatedBy = &v1.Actor{}
	return rec, proto.Unmarshal(by, rec.UpdatedBy)
}

// Setting reads one override; a setting nobody set is ErrNotFound.
func (r Reader) Setting(key string) (SettingRecord, error) {
	rec, err := scanSetting(r.q.QueryRowContext(r.ctx,
		`SELECT key, value, updated_at, updated_by FROM settings WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	return rec, err
}

// Settings reads every override, by key.
func (r Reader) Settings() ([]SettingRecord, error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT key, value, updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettingRecord
	for rows.Next() {
		rec, err := scanSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// PutSetting stores an override, replacing any; the transaction's clock and actor are its time and author.
func (t *Tx) PutSetting(key, valueJSON string) error {
	by, err := proto.Marshal(t.Actor)
	if err != nil {
		return err
	}
	if by == nil {
		by = []byte{} // a write with no actor (the daemon's own): a nil blob would be NULL
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at,
		updated_by = excluded.updated_by`, key, valueJSON, nanos(t.now), by)
	return err
}

// DeleteSetting drops an override, so the setting is its default again.
func (t *Tx) DeleteSetting(key string) error {
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// ── model limits ──

// ModelLimit is one held model (or "*" for every model of a kind) until a reset time.
type ModelLimit struct {
	Kind      string
	Model     string
	Until     time.Time
	UpdatedAt time.Time
}

// PutModelLimit records or extends a usage-limit hold for (kind, model) until `until`.
func (t *Tx) PutModelLimit(kind, model string, until time.Time) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO model_limits (kind, model, until, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, model) DO UPDATE SET
			until = CASE WHEN excluded.until > model_limits.until THEN excluded.until ELSE model_limits.until END,
			updated_at = excluded.updated_at`,
		kind, model, nanos(until), nanos(t.now))
	return err
}

// ModelHold returns the latest active hold end (zero if none is after `now`) for (kind, model) counting a
// kind-wide "*" hold, and whether any hold record (active or expired) exists for either key.
func (r Reader) ModelHold(kind, model string, now time.Time) (until time.Time, known bool, err error) {
	rows, err := r.q.QueryContext(r.ctx, `SELECT until FROM model_limits WHERE kind = ? AND model IN (?, '*')`,
		kind, model)
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var u int64
		if err := rows.Scan(&u); err != nil {
			return time.Time{}, false, err
		}
		known = true
		t := time.Unix(0, u)
		if t.After(now) && t.After(until) {
			until = t
		}
	}
	return until, known, rows.Err()
}

// ModelLimits returns all recorded holds for kind (or every kind when empty).
func (r Reader) ModelLimits(kind string) ([]ModelLimit, error) {
	query := `SELECT kind, model, until, updated_at FROM model_limits ORDER BY kind, model`
	var args []any
	if kind != "" {
		query = `SELECT kind, model, until, updated_at FROM model_limits WHERE kind = ? ORDER BY model`
		args = []any{kind}
	}
	rows, err := r.q.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelLimit
	for rows.Next() {
		var l ModelLimit
		var u, upd int64
		if err := rows.Scan(&l.Kind, &l.Model, &u, &upd); err != nil {
			return nil, err
		}
		l.Until = time.Unix(0, u)
		l.UpdatedAt = time.Unix(0, upd)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Exec runs a statement in the transaction; for tests and one-off fixes.
func (t *Tx) Exec(query string, args ...any) error {
	_, err := t.tx.ExecContext(t.ctx, query, args...)
	return err
}

// ── asks ──

// PutAsk inserts or replaces an ask.
func (t *Tx) PutAsk(a *v1.Ask) error {
	b, err := proto.Marshal(a)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO asks (id, run_id, caller_id, pr, pr_number, status, created_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET status = excluded.status, data = excluded.data`,
		a.Id, a.RunId, a.CallerId, a.Pr, a.PrNumber, int32(a.Status), a.CreatedAt.AsTime().UnixNano(), b)
	return err
}

// GetAsk returns one ask by id.
func (r Reader) GetAsk(id string) (*v1.Ask, error) {
	return scanOne(r, `SELECT data FROM asks WHERE id = ?`, []any{id}, &v1.Ask{})
}

// OpenAsks lists the asks still waiting on members, oldest first.
func (r Reader) OpenAsks() ([]*v1.Ask, error) {
	return scanAll(r, `SELECT data FROM asks WHERE status = ? ORDER BY created_at, id`,
		[]any{int32(v1.AskStatus_ASK_STATUS_OPEN)}, func() *v1.Ask { return &v1.Ask{} })
}

// RunAsks lists a run's asks, oldest first, or every run's when runID is empty.
func (r Reader) RunAsks(runID string) ([]*v1.Ask, error) {
	if runID == "" {
		return scanAll(r, `SELECT data FROM asks ORDER BY created_at, id`, nil, func() *v1.Ask { return &v1.Ask{} })
	}
	return scanAll(r, `SELECT data FROM asks WHERE run_id = ? ORDER BY created_at, id`, []any{runID},
		func() *v1.Ask { return &v1.Ask{} })
}

// PRAsks lists the asks about one pull request (its URL), in any run, oldest first.
func (r Reader) PRAsks(pr string) ([]*v1.Ask, error) {
	return scanAll(r, `SELECT data FROM asks WHERE pr = ? ORDER BY created_at, id`, []any{pr},
		func() *v1.Ask { return &v1.Ask{} })
}

// NumberAsks lists the asks about a pull request number, in any repo, oldest first.
func (r Reader) NumberAsks(n int32) ([]*v1.Ask, error) {
	return scanAll(r, `SELECT data FROM asks WHERE pr_number = ? ORDER BY created_at, id`, []any{n},
		func() *v1.Ask { return &v1.Ask{} })
}

// ── peers ──

// PutPeer inserts or replaces a peer.
func (t *Tx) PutPeer(p *v1.Peer) error {
	b, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO peers (name, group_name, data) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET group_name = excluded.group_name, data = excluded.data`, p.Name, p.Group, b)
	return err
}

// DeletePeer removes a peer.
func (t *Tx) DeletePeer(name string) error {
	_, err := t.tx.ExecContext(t.ctx, `DELETE FROM peers WHERE name = ?`, name)
	return err
}

// GetPeer returns one peer by name.
func (r Reader) GetPeer(name string) (*v1.Peer, error) {
	return scanOne(r, `SELECT data FROM peers WHERE name = ?`, []any{name}, &v1.Peer{})
}

// ListPeers returns every peer (or a group's), by group and name.
func (r Reader) ListPeers(group string) ([]*v1.Peer, error) {
	if group != "" {
		return scanAll(r, `SELECT data FROM peers WHERE group_name = ? ORDER BY name`, []any{group},
			func() *v1.Peer { return &v1.Peer{} })
	}
	return scanAll(r, `SELECT data FROM peers ORDER BY group_name, name`, nil, func() *v1.Peer { return &v1.Peer{} })
}

// PutPeerGroup inserts or replaces a peer group.
func (t *Tx) PutPeerGroup(g *v1.PeerGroup) error {
	b, err := proto.Marshal(g)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO peer_groups (name, data) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET data = excluded.data`, g.Name, b)
	return err
}

// GetPeerGroup returns one group by name.
func (r Reader) GetPeerGroup(name string) (*v1.PeerGroup, error) {
	return scanOne(r, `SELECT data FROM peer_groups WHERE name = ?`, []any{name}, &v1.PeerGroup{})
}

// ListPeerGroups returns every group by name.
func (r Reader) ListPeerGroups() ([]*v1.PeerGroup, error) {
	return scanAll(r, `SELECT data FROM peer_groups ORDER BY name`, nil, func() *v1.PeerGroup { return &v1.PeerGroup{} })
}
