package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

var fixed = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return fixed }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := OpenAt(filepath.Join(t.TempDir(), "meshd.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFreshDatabaseHasNewestSchemaAndKeepsItsGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.db")
	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.r.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != SchemaVersion() {
		t.Fatalf("user_version = %d, %v; want %d", v, err, SchemaVersion())
	}
	var mode string
	s.r.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	gen := s.Generation()
	s.Close()
	s2, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Generation() != gen || gen == "" {
		t.Errorf("generation %q after reopen, was %q", s2.Generation(), gen)
	}
	matches, _ := filepath.Glob(path + ".bak-*")
	if len(matches) != 0 {
		t.Errorf("an up-to-date database was backed up: %v", matches)
	}
}

func TestMigrationBacksUpFirstAndRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m1 := migration{1, "0001_a.sql", `CREATE TABLE a (x INTEGER); INSERT INTO a VALUES (7);`}
	m2 := migration{2, "0002_b.sql", `ALTER TABLE a ADD COLUMN y TEXT NOT NULL DEFAULT 'new';`}
	if err := migrateWith(db, path, clock, []migration{m1}); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(path + ".bak-*"); len(matches) != 0 {
		t.Fatalf("a new database was backed up: %v", matches)
	}
	if err := migrateWith(db, path, clock, []migration{m1, m2}); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-v1-20261125T120000"
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("no backup before migrating: %v", err)
	}
	old, _ := sql.Open("sqlite", "file:"+backup)
	defer old.Close()
	var x, v int
	if err := old.QueryRow(`SELECT x FROM a`).Scan(&x); err != nil || x != 7 {
		t.Errorf("backup holds x=%d, %v", x, err)
	}
	old.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 1 {
		t.Errorf("backup user_version = %d, want 1", v)
	}
	var y string
	if err := db.QueryRow(`SELECT y FROM a`).Scan(&y); err != nil || y != "new" {
		t.Errorf("migrated y=%q, %v", y, err)
	}
	if err := migrateWith(db, path, clock, []migration{m1}); err == nil {
		t.Error("a database newer than the daemon was accepted")
	}
}

func TestEventsAreNumberedAndPublishedInOrderAfterCommit(t *testing.T) {
	s := open(t)
	var published []*v1.Event
	s.OnCommit = func(evs []*v1.Event) { published = append(published, evs...) }
	ctx := context.Background()
	actor := &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Id: "d"}
	err := s.Write(ctx, actor, "mut1", func(tx *Tx) error {
		for _, run := range []string{"r1", "r2"} {
			if _, err := tx.AppendEvent(&v1.Event{RunId: run}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || published[0].Seq != 1 || published[1].Seq != 2 {
		t.Fatalf("published %v", published)
	}
	ev := published[0]
	if ev.Generation != s.Generation() || ev.MutationId != "mut1" || ev.Actor.GetId() != "d" || !ev.Time.AsTime().Equal(fixed) {
		t.Errorf("event not stamped: %v", ev)
	}
	// A failed transaction stores and publishes nothing.
	boom := errors.New("boom")
	if err := s.Write(ctx, actor, "", func(tx *Tx) error {
		tx.AppendEvent(&v1.Event{RunId: "r3"})
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Errorf("a rolled-back event was published")
	}
	r := s.Read(ctx)
	last, _ := r.LastSeq()
	if last != 2 {
		t.Errorf("last seq %d", last)
	}
	only, err := r.EventsAfter(0, "r2", 10)
	if err != nil || len(only) != 1 || only[0].Seq != 2 || only[0].MutationId != "mut1" {
		t.Errorf("EventsAfter(run r2) = %v, %v", only, err)
	}
}

func TestRecordsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := timestamppb.New(fixed)
	err := s.Write(ctx, nil, "", func(tx *Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Title: "t", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.PutRun(&v1.Run{Id: "r2", Status: v1.RunStatus_RUN_STATUS_CLOSED, CreatedAt: now}); err != nil {
			return err
		}
		for _, a := range []*v1.Agent{
			{Id: "a1", RunId: "r1", Name: "repo-x", CreatedAt: now},
			{Id: "a2", RunId: "r2", Name: "repo-x", CreatedAt: now},
		} {
			if err := tx.PutAgent(a); err != nil {
				return err
			}
		}
		if err := tx.PutMessage(&v1.Message{Id: "m1", RunId: "r1", ToId: "a1", Body: "hi", CreatedAt: now}); err != nil {
			return err
		}
		return tx.PutToken("h1", TokenRecord{Class: v1.ActorClass_ACTOR_CLASS_WORKER, SubjectID: "a1", Name: "repo-x", RunID: "r1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	r := s.Read(ctx)
	if open, _ := r.ListRuns(false); len(open) != 1 || open[0].Id != "r1" {
		t.Errorf("open runs %v", open)
	}
	if all, _ := r.ListRuns(true); len(all) != 2 {
		t.Errorf("all runs %v", all)
	}
	if named, _ := r.AgentsNamed("", "repo-x"); len(named) != 1 || named[0].Id != "a1" {
		t.Errorf("named across open runs %v", named)
	}
	if inbox, _ := r.Inbox("a1", false); len(inbox) != 1 || inbox[0].Body != "hi" {
		t.Errorf("inbox %v", inbox)
	}
	rec, err := r.Token("h1")
	if err != nil || rec.Class != v1.ActorClass_ACTOR_CLASS_WORKER || rec.RunID != "r1" {
		t.Errorf("token %v, %v", rec, err)
	}
	s.Write(ctx, nil, "", func(tx *Tx) error { return tx.RevokeTokens("a1") })
	if _, err := s.Read(ctx).Token("h1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still resolves: %v", err)
	}
	if _, err := r.GetAgent("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing agent: %v", err)
	}
}
