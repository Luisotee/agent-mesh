package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
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

func TestAPathWithURICharactersOpensThatFileInWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b?c%d")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "meshd.db")
	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	s.r.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if _, err := os.Stat(path); err != nil || mode != "wal" {
		t.Errorf("file %v, journal_mode %q", err, mode)
	}
}

func TestPutRunNeverOverwritesAndAWriterPanicLeavesNoOpenTransaction(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := timestamppb.New(fixed)
	if err := s.Write(ctx, nil, "", func(tx *Tx) error {
		return tx.PutRun(&v1.Run{Id: "r1", Title: "first", CreatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	err := s.Write(ctx, nil, "", func(tx *Tx) error {
		return tx.PutRun(&v1.Run{Id: "r1", Title: "second", CreatedAt: now})
	})
	if !errors.Is(err, ErrExists) {
		t.Errorf("second create: %v", err)
	}
	if run, _ := s.Read(ctx).GetRun("r1"); run.Title != "first" {
		t.Errorf("the run was overwritten: %v", run)
	}
	func() {
		defer func() { recover() }()
		s.Write(ctx, nil, "", func(tx *Tx) error {
			tx.PutRun(&v1.Run{Id: "r2", CreatedAt: now})
			panic("boom")
		})
	}()
	if err := s.Write(ctx, nil, "", func(tx *Tx) error { return tx.PutRun(&v1.Run{Id: "r3", CreatedAt: now}) }); err != nil {
		t.Errorf("a write after a panicked one: %v", err)
	}
	if _, err := s.Read(ctx).GetRun("r2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the panicked write's run: %v", err)
	}
}

func TestPruneMutationsKeepsRecentOnesAndForgetsOldHookOnes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := fixed.Add(-48 * time.Hour)
	write := func(at time.Time, ids ...string) {
		s.now = func() time.Time { return at }
		s.Write(ctx, nil, "", func(tx *Tx) error {
			for _, id := range ids {
				if err := tx.PutMutation("d1", id, "p", nil); err != nil {
					return err
				}
			}
			return nil
		})
	}
	write(old, "hook-old", "mine-old")
	write(fixed, "hook-new", "mine-new")
	s.Write(ctx, nil, "", func(tx *Tx) error {
		n, err := tx.PruneMutations(fixed.Add(-24*time.Hour), fixed.Add(-14*24*time.Hour))
		if err != nil || n != 1 {
			t.Errorf("pruned %d, %v", n, err)
		}
		return nil
	})
	for id, want := range map[string]bool{"hook-old": false, "mine-old": true, "hook-new": true, "mine-new": true} {
		if _, _, err := s.Read(ctx).Mutation("d1", id); (err == nil) != want {
			t.Errorf("%s kept=%v, want %v", id, err == nil, want)
		}
	}
}

func TestMarkDeliveredKeepsAReadTimeSetSinceTheSnapshot(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := timestamppb.New(fixed)
	s.Write(ctx, nil, "", func(tx *Tx) error {
		return tx.PutMessage(&v1.Message{Id: "m1", RunId: "r1", ToId: "a1", CreatedAt: now})
	})
	snapshot, _ := s.Read(ctx).Undelivered("a1")
	// The agent reads its inbox between the snapshot and the delivery's write.
	s.Write(ctx, nil, "", func(tx *Tx) error {
		m := snapshot[0]
		m.ReadAt = now
		return tx.PutMessage(m)
	})
	if err := s.Write(ctx, nil, "", func(tx *Tx) error { return tx.MarkDelivered("m1") }); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, nil, "", func(tx *Tx) error { return tx.MarkDelivered("gone") }); err != nil {
		t.Errorf("a vanished message: %v", err)
	}
	all, _ := s.Read(ctx).Inbox("a1", true)
	if len(all) != 1 || all[0].ReadAt == nil || all[0].DeliveredAt == nil {
		t.Errorf("message after delivery: %v", all)
	}
	if unread, _ := s.Read(ctx).Inbox("a1", false); len(unread) != 0 {
		t.Errorf("a read message turned unread: %v", unread)
	}
}

// A database made by the first release (schema 1) opens on the current one: its rows are kept, the settings
// table appears, and the first-release file is backed up.
func TestAVersionOneDatabaseGainsTheSettingsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateWith(db, path, clock, ms[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('daemon_id', 'dOld')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := s.Read(context.Background()).Meta("daemon_id"); err != nil || v != "dOld" {
		t.Errorf("kept row: %q, %v", v, err)
	}
	if recs, err := s.Read(context.Background()).Settings(); err != nil || len(recs) != 0 {
		t.Errorf("settings on a migrated database: %v, %v", recs, err)
	}
	if matches, _ := filepath.Glob(path + ".bak-v1-*"); len(matches) != 1 {
		t.Errorf("backups %v", matches)
	}
}

func TestSettingsRoundTripAndReplace(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	by := &v1.Actor{Id: "dev1", Class: v1.ActorClass_ACTOR_CLASS_DEVICE}
	put := func(key, value string) {
		t.Helper()
		if err := s.Write(ctx, by, "", func(tx *Tx) error { return tx.PutSetting(key, value) }); err != nil {
			t.Fatal(err)
		}
	}
	put("quota_stop", `75`)
	put("tiers.claude.mid.model", `["sonnet","haiku"]`)
	put("quota_stop", `60`)
	rec, err := s.Read(ctx).Setting("quota_stop")
	if err != nil || rec.Value != "60" || rec.UpdatedBy.GetId() != "dev1" || !rec.UpdatedAt.Equal(fixed) {
		t.Fatalf("%+v, %v", rec, err)
	}
	all, err := s.Read(ctx).Settings()
	if err != nil || len(all) != 2 || all[0].Key != "quota_stop" || all[1].Key != "tiers.claude.mid.model" {
		t.Fatalf("%+v, %v", all, err)
	}
	if err := s.Write(ctx, by, "", func(tx *Tx) error { return tx.DeleteSetting("quota_stop") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx).Setting("quota_stop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after a delete: %v", err)
	}
}

func TestVersionThreeDatabaseGainsPairingsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateWith(db, path, clock, ms[:3]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if devs, err := s.Read(context.Background()).ListDevices(true, "device-local"); err != nil || len(devs) != 0 {
		t.Fatalf("ListDevices after migration: %v, %v", devs, err)
	}
	if matches, _ := filepath.Glob(path + ".bak-v3-*"); len(matches) != 1 {
		t.Errorf("backups %v", matches)
	}
}

func TestPairingsAndDevicesRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	by := &v1.Actor{Id: "device-local", Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Name: "local"}

	pairResp, err := proto.Marshal(&v1.CreatePairingResponse{
		PairingId:        "p1",
		Token:            "amp_secret_1",
		Uri:              "agentmesh://pair?v=1&t=amp_secret_1",
		RelayClientToken: "amr_secret_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	expResp, err := proto.Marshal(&v1.CreatePairingResponse{
		PairingId:        "p-exp",
		Token:            "amp_secret_exp",
		Uri:              "agentmesh://pair?v=1&t=amp_secret_exp",
		RelayClientToken: "amr_secret_exp",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Write(ctx, by, "", func(tx *Tx) error {
		if err := tx.PutMutation("device-local", "mut-pair", "CreatePairing", pairResp); err != nil {
			return err
		}
		if err := tx.PutMutation("device-local", "mut-exp", "CreatePairing", expResp); err != nil {
			return err
		}
		if err := tx.PutPairing(PairingRecord{
			Hash:       "hpair1",
			ID:         "p1",
			Name:       "phone",
			Class:      v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE,
			CreatedAt:  fixed,
			ExpiresAt:  fixed.Add(10 * time.Minute),
			MutationID: "mut-pair",
			CreatedBy:  by,
		}); err != nil {
			return err
		}
		return tx.PutPairing(PairingRecord{
			Hash:       "hpair-exp",
			ID:         "p-exp",
			Name:       "expired-phone",
			Class:      v1.ActorClass_ACTOR_CLASS_DEVICE,
			CreatedAt:  fixed,
			ExpiresAt:  fixed.Add(1 * time.Minute),
			MutationID: "mut-exp",
			CreatedBy:  by,
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.Read(ctx).Pairing("hpair1")
	if err != nil || rec.ID != "p1" || rec.Class != v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE || !rec.UsedAt.IsZero() {
		t.Fatalf("Pairing = %+v, %v", rec, err)
	}

	err = s.Write(ctx, by, "", func(tx *Tx) error {
		if err := tx.MarkPairingUsed("hpair1", "dev1"); err != nil {
			return err
		}
		if err := tx.ScrubPairingMutation("device-local", rec.MutationID); err != nil {
			return err
		}
		if err := tx.PutToken("htok1", TokenRecord{Class: rec.Class, SubjectID: "dev1", Name: "Pixel"}); err != nil {
			return err
		}
		return tx.PutToken("hlocal", TokenRecord{Class: v1.ActorClass_ACTOR_CLASS_DEVICE, SubjectID: "device-local", Name: "local"})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := s.Read(ctx).Mutation("device-local", "mut-pair")
	if err != nil {
		t.Fatalf("mutation tombstone missing after scrub: %v", err)
	}
	var scrubbed v1.CreatePairingResponse
	if err := proto.Unmarshal(raw, &scrubbed); err != nil {
		t.Fatal(err)
	}
	if scrubbed.PairingId != "p1" || scrubbed.Token != "" || scrubbed.Uri != "" || scrubbed.RelayClientToken != "" {
		t.Errorf("scrubbed mutation = %+v, want PairingId=p1 and empty secrets", &scrubbed)
	}

	// PrunePairings also scrubs the expired pairing's mutation in place instead of deleting it.
	if err := s.Write(ctx, by, "", func(tx *Tx) error {
		n, err := tx.PrunePairings(fixed.Add(5 * time.Minute))
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("PrunePairings removed %d, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, expRaw, err := s.Read(ctx).Mutation("device-local", "mut-exp")
	if err != nil {
		t.Fatalf("expired pairing mutation tombstone missing: %v", err)
	}
	var expScrubbed v1.CreatePairingResponse
	if err := proto.Unmarshal(expRaw, &expScrubbed); err != nil {
		t.Fatal(err)
	}
	if expScrubbed.PairingId != "p-exp" || expScrubbed.Token != "" || expScrubbed.Uri != "" || expScrubbed.RelayClientToken != "" {
		t.Errorf("pruned scrubbed mutation = %+v, want PairingId=p-exp and empty secrets", &expScrubbed)
	}

	if active, err := s.Read(ctx).HasActiveToken("dev1"); err != nil || !active {
		t.Fatalf("HasActiveToken(dev1) before revoke = %v, %v; want true", active, err)
	}
	devs, err := s.Read(ctx).ListDevices(false, "device-local")
	if err != nil || len(devs) != 1 || devs[0].Id != "dev1" || devs[0].Name != "Pixel" {
		t.Fatalf("ListDevices = %+v, %v", devs, err)
	}
	if err := s.Write(ctx, by, "", func(tx *Tx) error { return tx.RevokeTokens("dev1") }); err != nil {
		t.Fatal(err)
	}
	if active, err := s.Read(ctx).HasActiveToken("dev1"); err != nil || active {
		t.Fatalf("HasActiveToken(dev1) after revoke = %v, %v; want false", active, err)
	}
	if active, _ := s.Read(ctx).ListDevices(false, "device-local"); len(active) != 0 {
		t.Errorf("revoked device still active: %+v", active)
	}
	if all, _ := s.Read(ctx).ListDevices(true, "device-local"); len(all) != 1 || all[0].RevokedAt == nil {
		t.Errorf("includeRevoked = %+v", all)
	}
}

func TestVersionFourDatabaseGainsModelLimitsTableAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateWith(db, path, clock, ms[:4]); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if matches, _ := filepath.Glob(path + ".bak-v4-*"); len(matches) != 1 {
		t.Errorf("backups %v", matches)
	}
	ctx := context.Background()
	until := fixed.Add(30 * time.Minute)
	if err := s.Write(ctx, nil, "", func(tx *Tx) error {
		if err := tx.PutModelLimit("claude", "sonnet", until); err != nil {
			return err
		}
		// Earlier until does not shrink a later hold; later until extends it.
		if err := tx.PutModelLimit("claude", "sonnet", fixed.Add(10*time.Minute)); err != nil {
			return err
		}
		return tx.PutModelLimit("agy", "*", fixed.Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	r := s.Read(ctx)
	if got, known, err := r.ModelHold("claude", "sonnet", fixed); err != nil || !known || !got.Equal(until) {
		t.Fatalf("ModelHold active = %v, %v, %v; want %v", got, known, err, until)
	}
	// Past expiration, until is zero but known stays true.
	if got, known, err := r.ModelHold("claude", "sonnet", until.Add(time.Second)); err != nil || !known || !got.IsZero() {
		t.Fatalf("ModelHold expired = %v, %v, %v", got, known, err)
	}
	// Kind-wide "*" hold applies to every model of that kind.
	if got, known, err := r.ModelHold("agy", "gemini-4-neon-high", fixed); err != nil || !known || !got.Equal(fixed.Add(time.Hour)) {
		t.Fatalf("ModelHold * = %v, %v, %v", got, known, err)
	}
	if all, err := r.ModelLimits(""); err != nil || len(all) != 2 {
		t.Fatalf("ModelLimits = %+v, %v", all, err)
	}
	// A hold the CLI answered past is deleted: only that (kind, model), and deleting a missing one is no error.
	if err := s.Write(ctx, nil, "", func(tx *Tx) error {
		if err := tx.DeleteModelLimit("agy", "*"); err != nil {
			return err
		}
		return tx.DeleteModelLimit("agy", "*")
	}); err != nil {
		t.Fatal(err)
	}
	if got, known, err := s.Read(ctx).ModelHold("agy", "gemini-4-neon-high", fixed); err != nil || known || !got.IsZero() {
		t.Fatalf("ModelHold after delete = %v, %v, %v", got, known, err)
	}
	if got, _, _ := s.Read(ctx).ModelHold("claude", "sonnet", fixed); !got.Equal(until) {
		t.Fatalf("another kind's hold changed: %v", got)
	}
}

func TestWorktreesRoundTripAndRejectDuplicates(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.Write(ctx, nil, "", func(tx *Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Title: "wt", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.Now()}); err != nil {
			return err
		}
		return tx.PutWorktree(&v1.Worktree{
			Id: "w1", RunId: "r1", Slug: "integ", Repo: "/repo", Path: "/wt/r1/repo-integ",
			Branch: "mesh/r1/integ", BaseCommit: "abc", CreatedAt: timestamppb.New(fixed),
		})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(ctx).GetWorktree("r1", "integ")
	if err != nil || got.Branch != "mesh/r1/integ" || got.BaseCommit != "abc" {
		t.Fatalf("GetWorktree = %+v, %v", got, err)
	}
	list, err := s.Read(ctx).ListWorktrees("r1")
	if err != nil || len(list) != 1 || list[0].Id != "w1" {
		t.Fatalf("ListWorktrees = %+v, %v", list, err)
	}
	err = s.Write(ctx, nil, "", func(tx *Tx) error {
		return tx.PutWorktree(&v1.Worktree{
			Id: "w2", RunId: "r1", Slug: "integ", Repo: "/repo", Path: "/wt/r1/repo-integ",
			Branch: "mesh/r1/integ", BaseCommit: "abc", CreatedAt: timestamppb.New(fixed),
		})
	})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate (run_id, slug): got %v, want ErrExists", err)
	}
	if err := s.Write(ctx, nil, "", func(tx *Tx) error {
		return tx.DeleteWorktree("w1")
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Read(ctx).ListWorktrees("r1"); err != nil || len(list) != 0 {
		t.Fatalf("ListWorktrees after delete = %+v, %v", list, err)
	}
}

func TestAfterCommitRunsOnlyForATransactionThatCommitted(t *testing.T) {
	s := open(t)
	var ran []string
	err := s.Write(context.Background(), nil, "", func(tx *Tx) error {
		tx.AfterCommit(func() { ran = append(ran, "rolled back") })
		return errors.New("no")
	})
	if err == nil || len(ran) != 0 {
		t.Fatalf("a failed write: %v, ran %v", err, ran)
	}
	if err := s.Write(context.Background(), nil, "", func(tx *Tx) error {
		tx.AfterCommit(func() { ran = append(ran, "committed") })
		return nil
	}); err != nil || len(ran) != 1 || ran[0] != "committed" {
		t.Fatalf("a write that committed: %v, ran %v", err, ran)
	}
}

func waitingMsg(id string, typ v1.MessageType, from, to string, at time.Time) *v1.Message {
	return &v1.Message{Id: id, RunId: "r1", ToId: to, Type: typ, From: &v1.Actor{Id: from}, CreatedAt: timestamppb.New(at)}
}

func waitingIDs(t *testing.T, s *Store, to string) []string {
	t.Helper()
	msgs, err := s.Read(context.Background()).Waiting(to)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.Id)
	}
	return ids
}

// An agent's answer closes only what its author asked that agent; the user's closes all of it.
func TestAnAgentsAnswerClosesOnlyWhatItsAuthorAskedIt(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put := func(m *v1.Message) {
		t.Helper()
		if err := s.Write(ctx, nil, "", func(tx *Tx) error { return tx.PutMessage(m) }); err != nil {
			t.Fatal(err)
		}
	}
	const q, ans = v1.MessageType_MESSAGE_TYPE_QUESTION, v1.MessageType_MESSAGE_TYPE_ANSWER
	put(waitingMsg("m1", q, "lead", "user", fixed))
	put(waitingMsg("m2", q, "lead", "boss", fixed.Add(time.Second)))
	a := waitingMsg("m3", ans, "w1", "lead", fixed.Add(2*time.Second))
	a.From.Class = v1.ActorClass_ACTOR_CLASS_WORKER
	put(a)
	if got := waitingIDs(t, s, "user"); len(got) != 1 {
		t.Fatalf("a worker's answer closed the lead's question to the user: %v", got)
	}
	d := waitingMsg("m4", ans, "dev", "lead", fixed.Add(3*time.Second))
	d.From.Class = v1.ActorClass_ACTOR_CLASS_DEVICE
	put(d)
	if got := waitingIDs(t, s, "user"); len(got) != 0 {
		t.Fatalf("the user's answer left it open: %v", got)
	}
}

// A plan or question waits until an answer to its author follows it; a re-put (read, delivered) changes nothing,
// and an answer that comes first doesn't clear a later message, even on the same nanosecond (id breaks the tie).
func TestWaitingIsWhatNoAnswerHasFollowed(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put := func(m *v1.Message) {
		t.Helper()
		if err := s.Write(ctx, nil, "", func(tx *Tx) error { return tx.PutMessage(m) }); err != nil {
			t.Fatal(err)
		}
	}
	const plan, q, ans, info = v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_ANSWER, v1.MessageType_MESSAGE_TYPE_INFO
	put(waitingMsg("m1", plan, "w1", "lead", fixed))
	put(waitingMsg("m2", q, "w2", "lead", fixed.Add(time.Second)))
	put(waitingMsg("m3", info, "w1", "lead", fixed.Add(2*time.Second)))
	if got := waitingIDs(t, s, "lead"); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("waiting = %v", got)
	}
	// The answer to w1 clears w1's plan, not w2's question.
	put(waitingMsg("a1", ans, "lead", "w1", fixed.Add(3*time.Second)))
	if got := waitingIDs(t, s, "lead"); len(got) != 1 || got[0] != "m2" {
		t.Fatalf("after the answer: %v", got)
	}
	// Reading the plan afterwards re-puts it: it does not come back.
	old, _ := s.Read(ctx).Inbox("lead", true)
	for _, m := range old {
		if m.Id == "m1" {
			m.ReadAt = timestamppb.New(fixed.Add(4 * time.Second))
			put(m)
		}
	}
	if got := waitingIDs(t, s, "lead"); len(got) != 1 || got[0] != "m2" {
		t.Fatalf("after a re-put: %v", got)
	}
	// A new plan from w1 waits again; an answer on the same nanosecond with a smaller id doesn't clear it.
	put(waitingMsg("m5", plan, "w1", "lead", fixed.Add(5*time.Second)))
	put(waitingMsg("m4", ans, "lead", "w1", fixed.Add(5*time.Second)))
	if got := waitingIDs(t, s, "lead"); len(got) != 2 || got[1] != "m5" {
		t.Fatalf("a tie: %v", got)
	}
	if got := waitingIDs(t, s, "w1"); len(got) != 0 {
		t.Errorf("another agent's waiting: %v", got)
	}
}

// A database from before migration 11 holds messages and no waiting rows: the first Open fills them in, the
// same way the live rule would have, and doesn't do it again.
func TestWaitingIsFilledFromAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.db")
	s, err := OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const plan, ans = v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_ANSWER
	s.Write(ctx, nil, "", func(tx *Tx) error {
		for _, m := range []*v1.Message{
			waitingMsg("m1", plan, "w1", "lead", fixed),
			waitingMsg("m2", ans, "lead", "w1", fixed.Add(time.Second)),
			waitingMsg("m3", plan, "w1", "lead", fixed.Add(2*time.Second)),
			waitingMsg("m4", plan, "w2", "lead", fixed.Add(3*time.Second)),
		} {
			if err := tx.PutMessage(m); err != nil {
				return err
			}
		}
		return nil
	})
	// Make it look like a database that has not been through the backfill.
	if _, err := s.w.Exec(`DELETE FROM waiting`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`DELETE FROM meta WHERE key = ?`, waitingBackfillKey); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := waitingIDs(t, s, "lead"); len(got) != 2 || got[0] != "m3" || got[1] != "m4" {
		t.Fatalf("after the backfill: %v", got)
	}
	// Not again: a row removed now stays removed across an Open.
	if _, err := s.w.Exec(`DELETE FROM waiting WHERE message_id = 'm4'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenAt(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := waitingIDs(t, s, "lead"); len(got) != 1 || got[0] != "m3" {
		t.Fatalf("after a second Open: %v", got)
	}
}

// A cached reader reads a run's whole message list once: a later read of it is the first one (a snapshot), a read
// with a limit or from a message is always fresh, and a plain reader or a Tx's own reader never caches.
func TestMessageCacheReadsARunsWholeListOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	put := func(id string) {
		t.Helper()
		if err := s.Write(ctx, nil, "", func(tx *Tx) error {
			return tx.PutMessage(&v1.Message{Id: id, RunId: "r1", ToId: "a1", CreatedAt: timestamppb.New(fixed)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("m1")
	cached := s.Read(ctx).WithMessageCache()
	first, err := cached.RunMessages("r1", "", -1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first read %v, %v", first, err)
	}
	put("m2")
	if again, _ := cached.RunMessages("r1", "", -1); len(again) != 1 || again[0] != first[0] {
		t.Fatalf("a second read of the run went to the database: %d messages", len(again))
	}
	if limited, _ := cached.RunMessages("r1", "", 10); len(limited) != 2 {
		t.Fatalf("a read with a limit is fresh: %d messages", len(limited))
	}
	if plain, _ := s.Read(ctx).RunMessages("r1", "", -1); len(plain) != 2 {
		t.Fatalf("a plain reader cached: %d messages", len(plain))
	}
	if other, _ := cached.RunMessages("r2", "", -1); len(other) != 0 {
		t.Fatalf("another run: %d messages", len(other))
	}
	err = s.Write(ctx, nil, "", func(tx *Tx) error {
		msgs, err := tx.RunMessages("r1", "", -1)
		if len(msgs) != 2 {
			t.Errorf("a Tx's reader saw %d messages, %v", len(msgs), err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
