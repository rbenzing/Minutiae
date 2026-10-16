package records_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func journalMode(t *testing.T, c *evidence.Case) string {
	t.Helper()
	var m string
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return tx.QueryRow("PRAGMA journal_mode").Scan(&m) }); err != nil {
		t.Fatal(err)
	}
	return m
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestRelaxedSyncIsExplicitAndAuditedAtStart: the mode is a Writer option, off by default, and an
// ingest that uses it says so in its records.ingest.start entry (D90 d).
func TestRelaxedSyncIsExplicitAndAuditedAtStart(t *testing.T) {
	c, a := setup(t)
	startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	d := auditOf(t, c, evidence.ActionIngestStart)[0].Details
	if _, has := d["sync"]; has {
		t.Fatalf("a default ingest records sync = %v: the full mode is the unmarked default", d["sync"])
	}
	c2, a2 := setup(t)
	startWriter(t, c2, testParser, records.WriterOptions{RelaxedSync: true}, a2.ID)
	d = auditOf(t, c2, evidence.ActionIngestStart)[0].Details
	if d["sync"] != "normal" {
		t.Fatalf("a relaxed ingest records sync = %v, want normal", d["sync"])
	}
}

// TestFullSyncWriterNeverLeavesTheRollbackJournal: without the option a batch changes nothing about the
// journal (D90 a).
func TestFullSyncWriterNeverLeavesTheRollbackJournal(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	add(t, w, recordstest.Records(a.ID, 4, 1))
	if m := journalMode(t, c); m != "delete" {
		t.Fatalf("journal_mode = %q after batches of a default writer, want delete", m)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRelaxedEndRestoresFullBeforeTheConclusion: a relaxed ingest runs its batches on the write-ahead
// log; End checkpoints it and restores FULL and the rollback journal BEFORE records.ingest.end is
// audited and before the run row exists (D90 b): the run is complete only after that durable step.
func TestRelaxedEndRestoresFullBeforeTheConclusion(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2, RelaxedSync: true}, a.ID)
	add(t, w, recordstest.Records(a.ID, 4, 1))
	if m := journalMode(t, c); m != "wal" {
		t.Fatalf("journal_mode = %q during a relaxed ingest, want wal", m)
	}
	var seen bool
	w.SetHook(func(point string) error {
		if point != "after-sync-restore" {
			return nil
		}
		seen = true
		if n := len(auditOf(t, c, evidence.ActionIngestEnd)); n != 0 {
			t.Errorf("%d records.ingest.end entries when the sync was restored, want 0: restore comes first", n)
		}
		if n := count(t, c, "record_runs"); n != 0 {
			t.Errorf("%d run rows when the sync was restored, want 0", n)
		}
		if m := journalMode(t, c); m != "delete" {
			t.Errorf("journal_mode = %q at the restore point, want delete", m)
		}
		if fileExists(filepath.Join(c.Dir, "artifacts.db-wal")) {
			t.Error("the write-ahead log still exists at the restore point")
		}
		return nil
	})
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("the after-sync-restore point never fired")
	}
	if res.Outcome != "complete" || res.Records != 4 {
		t.Fatalf("result %+v", res)
	}
	if rep := mustVerify(t, c); !rep.OK() || len(rep.Notices) != 0 {
		t.Fatalf("a completed relaxed ingest: problems %q notices %q", rep.Problems, rep.Notices)
	}
	rd, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := rd.Overview(ctx, records.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Gaps) != 0 || ov.Runs["complete"] != 1 {
		t.Fatalf("overview of a completed relaxed ingest: gaps %+v runs %v", ov.Gaps, ov.Runs)
	}
}

// TestRelaxedEndFailsClosedWhenTheRestoreFails: nothing is concluded (no records.ingest.end, no run
// row) when the restore step fails.
func TestRelaxedEndFailsClosedWhenTheRestoreFails(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2, RelaxedSync: true}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1))
	boom := errors.New("restore refused")
	w.SetHook(func(point string) error {
		if point == "after-sync-restore" {
			return boom
		}
		return nil
	})
	if _, err := w.End(ctx); !errors.Is(err, boom) {
		t.Fatalf("End = %v, want the restore failure", err)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestEnd)); n != 0 {
		t.Errorf("%d records.ingest.end entries after a failed restore", n)
	}
	if n := count(t, c, "record_runs"); n != 0 {
		t.Errorf("%d run rows after a failed restore", n)
	}
}

// powerLoss ends the case the way a power cut ends a relaxed ingest whose last commits were not on
// disk: the process is gone, artifacts.db is the image taken at the last synced point (snapshot), the
// write-ahead log is gone, and the audit log, which fsyncs every entry, still holds every entry. It
// returns the reopened case.
func powerLoss(t *testing.T, c *evidence.Case, w *records.Writer, snapshot []byte) *evidence.Case {
	t.Helper()
	w.Die()
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "artifacts.db")
	for _, p := range []string{db + "-wal", db + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(db, snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	return c2
}

// TestPowerLossMidRelaxedIngestIsIncompleteAndNamesTheLostBatches (D90 c): batches 3 and 4 are audited
// (and fsynced) but their rows are lost with the power; the ingest has no completion record. verify
// reports the ingest interrupted and names the audited-but-absent batches, never complete and never an
// integrity failure; records stats and show report the same.
func TestPowerLossMidRelaxedIngestIsIncompleteAndNamesTheLostBatches(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2, RelaxedSync: true}, a.ID)
	add(t, w, recordstest.Records(a.ID, 4, 1)) // batches 1 and 2
	if err := c.RestoreFullSync(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(c.Dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	add(t, w, recordstest.Records(a.ID, 4, 2)) // batches 3 and 4: audited, then relaxed commits
	id := w.IngestID()
	if n := len(auditOf(t, c, evidence.ActionBatch)); n != 4 {
		t.Fatalf("%d batch entries audited, want 4", n)
	}
	c2 := powerLoss(t, c, w, snapshot)

	rep, err := c2.Verify()
	if err != nil {
		t.Fatalf("verify after the loss: %v (an integrity failure must not mask the incomplete run)", err)
	}
	if rep.OK() {
		t.Fatal("verify passed after a power loss mid-ingest: the run is not complete")
	}
	expectProblems(t, rep, []string{"ingest " + quoted(id) + ": interrupted"})
	for _, b := range []string{"batch 3 of ingest " + quoted(id), "batch 4 of ingest " + quoted(id)} {
		if !hasNotice(rep, b, "audited but its rows are not in the database") {
			t.Errorf("no notice for %q: %q", b, rep.Notices)
		}
	}
	if hasNotice(rep, "batch 1 of ingest") || hasNotice(rep, "batch 2 of ingest") {
		t.Errorf("a surviving batch is reported absent: %q", rep.Notices)
	}
	if rep.RecordsChecked != 4 {
		t.Errorf("RecordsChecked = %d, want the 4 surviving records", rep.RecordsChecked)
	}

	rd, err := records.NewReader(c2)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := rd.Overview(ctx, records.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Runs["complete"] != 0 {
		t.Errorf("stats counts %d complete runs after the loss", ov.Runs["complete"])
	}
	if len(ov.Gaps) != 1 || ov.Gaps[0].IngestID != id || ov.Gaps[0].Recovered || !slices.Equal(ov.Gaps[0].AbsentBatches, []int{3, 4}) {
		t.Fatalf("stats gaps = %+v, want ingest %s unconcluded with batches 3 and 4 absent", ov.Gaps, id)
	}
	full, err := rd.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if full.Run != nil || full.Gap == nil || !slices.Equal(full.Gap.AbsentBatches, []int{3, 4}) {
		t.Fatalf("show of record 1: run %+v gap %+v, want no run row and a gap naming batches 3 and 4", full.Run, full.Gap)
	}

	// the next ingest recovers the dead one (audited): its run is interrupted, never complete, and the lost
	// batches stay named
	w2 := startWriter(t, c2, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	add(t, w2, recordstest.Records(a.ID, 1, 9))
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	rep = mustVerify(t, c2)
	if !rep.OK() || !hasNotice(rep, quoted(id), "recovered", "[3 4]") {
		t.Fatalf("after recovery: problems %q notices %q", rep.Problems, rep.Notices)
	}
	ov, err = rd.Overview(ctx, records.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Runs["interrupted"] != 1 || ov.Runs["complete"] != 1 {
		t.Errorf("runs after recovery = %v, want 1 interrupted and 1 complete (the new ingest)", ov.Runs)
	}
	if len(ov.Gaps) != 1 || !ov.Gaps[0].Recovered || !slices.Equal(ov.Gaps[0].AbsentBatches, []int{3, 4}) {
		t.Fatalf("gaps after recovery = %+v", ov.Gaps)
	}
	full, err = rd.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if full.Run == nil || full.Run.Outcome != "interrupted" || full.Gap == nil || !full.Gap.Recovered {
		t.Fatalf("show after recovery: run %+v gap %+v", full.Run, full.Gap)
	}
}

// TestLostRelaxedBatchIDsAreNeverReused: the ids of the lost batches were announced by the audit log.
func TestLostRelaxedBatchIDsAreNeverReused(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2, RelaxedSync: true}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1))
	if err := c.RestoreFullSync(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(c.Dir, "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	add(t, w, recordstest.Records(a.ID, 2, 2)) // ids 3..4
	c2 := powerLoss(t, c, w, snapshot)
	w2 := startWriter(t, c2, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{BatchRows: 2}, a.ID)
	add(t, w2, recordstest.Records(a.ID, 2, 3))
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	first := scalar[int64](t, c2, `SELECT min(id) FROM records WHERE batch_id IN (SELECT batch_id FROM record_batches WHERE ingest_id = ?)`, w2.IngestID())
	if first != 5 {
		t.Fatalf("the next ingest starts at id %d, want 5 (3 and 4 were announced)", first)
	}
}
