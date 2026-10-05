package sqlitefile_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// engineRowsWithWAL puts a copy of db and wal side by side, opens the copy in
// the engine (which recovers the WAL by the same scan the library models) and
// returns the rows of table t.
func engineRowsWithWAL(t *testing.T, db, wal []byte) [][]any {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "w.db")
	db = bytes.Clone(db)
	db[18], db[19] = 2, 2 // WAL mode
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	e := openEngine(t, p)
	return engineQuery(t, e, "select rowid, a, b from t order by rowid")
}

// TestBuilderWALMatchesEngine: a builder database plus a builder WAL (two
// commits, then a reset generation that leaves stale frames behind) is read by
// the engine as the library's scan says it is applied: the engine's rows equal
// the builder's inputs at the last commit; a flipped checksum bit stops the
// engine at the previous commit. The first test that breaks the builder/reader
// circle for WALs.
func TestBuilderWALMatchesEngine(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	var rows builderRows
	add := func(from, to int64) {
		for id := from; id <= to; id++ {
			text := fmt.Sprintf("row%d-%s", id, longText(120, byte(id)))
			tb.Insert(id, id*3, text)
			rows.add(id, id*3, text)
		}
	}
	snapshotRows := func() [][]any { return append([][]any(nil), rows...) }
	add(1, 20)
	snap0, db0 := b.Snapshot(), b.Bytes()
	w := b.NewWAL(false, 0x1000, 0x2000, 0)
	add(21, 40)
	b.CommitTo(w, snap0)
	rows1 := snapshotRows()
	snap1 := b.Snapshot()
	add(41, 60)
	b.CommitTo(w, snap1)
	rows2 := snapshotRows()
	snap2, db2 := b.Snapshot(), b.Bytes()
	full := w.Bytes()

	scan, err := sqlitefile.ScanWAL(bytes.NewReader(full), int64(len(full)), 1024, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !scan.Info.HeaderValid || scan.Info.FramesValid != scan.Info.FrameSlots || scan.Info.LastCommit != scan.Info.FrameSlots || scan.Info.Commits != 2 {
		t.Fatalf("the library does not see two clean commits: %+v", scan.Info)
	}
	commit1 := uint32(0)
	for _, f := range scan.Frames {
		if f.DBSize != 0 && commit1 == 0 {
			commit1 = f.Slot
		}
	}

	t.Run("both commits", func(t *testing.T) {
		expectRows(t, "two commits", engineRowsWithWAL(t, db0, full), rows2)
	})
	t.Run("a flipped checksum bit stops the engine at the previous commit", func(t *testing.T) {
		d := bytes.Clone(full)
		last := int(scan.Info.LastCommit)
		d[32+(last-1)*(24+1024)+16] ^= 1
		bad, err := sqlitefile.ScanWAL(bytes.NewReader(d), int64(len(d)), 1024, sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if bad.Info.LastCommit != commit1 || bad.Frames[last-1].State != sqlitefile.FrameBroken {
			t.Fatalf("the library says: %+v", bad.Info)
		}
		expectRows(t, "one commit", engineRowsWithWAL(t, db0, d), rows1)
	})
	t.Run("a reset generation leaves stale frames the engine ignores", func(t *testing.T) {
		w.Reset(0x1001, 0x2002)
		add(61, 62)
		b.CommitTo(w, snap2)
		rows3 := snapshotRows()
		d := w.Bytes()
		s, err := sqlitefile.ScanWAL(bytes.NewReader(d), int64(len(d)), 1024, sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Info.FramesStale == 0 || s.Info.FramesValid == 0 || s.Info.FramesValid >= s.Info.FrameSlots || len(s.Info.Generations) < 2 {
			t.Fatalf("the scenario must leave stale frames: %+v", s.Info)
		}
		expectRows(t, "reset generation", engineRowsWithWAL(t, db2, d), rows3)
	})
}
