package sqlitefile_test

// Engine probes (plan 3I, Task 2). Each test asks the pinned engine what it
// does where the format description was written from memory; the answer is
// recorded as a named constant below, and later tasks assert against those
// constants, so the engine, not memory, decides. A driver upgrade that
// changes an answer fails the probe, not silently a later task.

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Recorded engine facts (modernc.org/sqlite v1.60.1, measured by the probes
// below; see task-2-report.md).
const (
	engineHasDBStat = true // the dbstat virtual table is compiled in
	engineHasDBPage = true // the sqlite_dbpage virtual table is compiled in

	// A WAL next to a database whose header bytes 18 and 19 say "rollback":
	// does the engine still apply the committed WAL frames? Variant A: the
	// WAL holds no frame of page 1; variant B: it does.
	engineAppliesWALWithRollbackHeader          = true
	engineAppliesWALWithRollbackHeaderPage1WAL  = true
	engineWALPageSizeMismatchOutcome            = "ignored"                     // header-only patch (checksums no longer match)
	engineWALPageSizeMismatchValidFramesOutcome = "applied-first-db-page-bytes" // hand-built valid chain of another page size

	// With no trusted header page count, a trailing partial page counts as a
	// page for the engine (zero-padded); the reader counts whole pages only.
	enginePartialTrailingPageCounts = true

	// A WAL beside a zero-length database file.
	engineDeletesWALOnZeroLengthDB = true
	engineSeesWALOnZeroLengthDB    = false
)

// walFixture is an engine-written WAL-mode database kept open so that its
// -wal still holds committed frames.
type walFixture struct {
	dir, db, wal string
	conn         *sql.DB
	base         string // dump of the committed state that is in the db file
	full         string // dump including the frames only the WAL holds
}

// newWALFixture writes 50 rows, checkpoints them into the db file, then
// commits more rows that stay in the WAL (autocheckpoint off). With grow the
// extra rows are large, so the file grows and page 1 is rewritten into the
// WAL as well.
func newWALFixture(t *testing.T, grow bool) *walFixture {
	t.Helper()
	dir := t.TempDir()
	f := &walFixture{dir: dir, db: filepath.Join(dir, "w.db")}
	f.wal = f.db + "-wal"
	f.conn = openEngine(t, f.db)
	for _, q := range []string{
		"pragma journal_mode=wal",
		"pragma wal_autocheckpoint=0",
		"create table t(id integer primary key, v text)",
	} {
		if _, err := f.conn.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i := range 50 {
		if _, err := f.conn.Exec("insert into t(v) values (?)", "base"+string(rune('a'+i%26))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.conn.Exec("pragma wal_checkpoint(truncate)"); err != nil {
		t.Fatal(err)
	}
	f.base = engineDump(t, f.conn)
	for i := range 10 {
		v := "wal-only"
		if grow {
			v = strings.Repeat("g", 3000+i)
		}
		if _, err := f.conn.Exec("insert into t(v) values (?)", v); err != nil {
			t.Fatal(err)
		}
	}
	f.full = engineDump(t, f.conn)
	if f.base == f.full {
		t.Fatal("fixture error: WAL-only rows are invisible")
	}
	return f
}

// outcome classifies what an engine opened on dbPath shows, against the two
// reference dumps.
func (f *walFixture) outcome(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "error: " + err.Error()
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var rowCount int
	if err := db.QueryRow("select count(*) from t").Scan(&rowCount); err != nil {
		return "error: " + err.Error()
	}
	var dump string
	func() {
		defer func() {
			if r := recover(); r != nil {
				dump = "panic"
			}
		}()
		dump = engineDump(t, db)
	}()
	switch dump {
	case f.base:
		return "ignored"
	case f.full:
		return "applied"
	}
	return "other"
}

func patchFile(t *testing.T, path string, off int64, b ...byte) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

func TestOracleEngineCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.db")
	db := openEngine(t, path)
	for _, q := range []string{
		"pragma journal_mode=wal",
		"create table t(id integer primary key, v text)",
		"insert into t(v) values ('x')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, p := range []string{
		"page_size", "auto_vacuum", "encoding", "journal_mode", "wal_autocheckpoint", "cache_size",
		"cache_spill", "secure_delete", "freelist_count", "page_count", "wal_checkpoint", "integrity_check",
		"table_xinfo(t)",
	} {
		rows, err := db.Query("pragma " + p)
		if err != nil {
			t.Errorf("pragma %s: %v", p, err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Errorf("pragma %s: %v", p, err)
		}
		_ = rows.Close()
		if n == 0 {
			t.Errorf("pragma %s returned no row", p)
		}
	}
	if got := pragmaString(t, db, "integrity_check"); got != "ok" {
		t.Errorf("integrity_check = %q", got)
	}
	for name, want := range map[string]bool{"dbstat": engineHasDBStat, "sqlite_dbpage": engineHasDBPage} {
		var n int
		err := db.QueryRow("select count(*) from " + name).Scan(&n)
		if got := err == nil; got != want {
			t.Errorf("virtual table %s available = %v (err %v), recorded constant says %v", name, got, err, want)
		}
	}
}

func TestOracleCopyWhileOpen(t *testing.T) {
	t.Run("wal", func(t *testing.T) {
		f := newWALFixture(t, false)
		dst := t.TempDir()
		mode := copyFiles(t, dst, f.db, f.wal)
		t.Logf("WAL copy mode: %s", mode)
		if got := f.outcome(t, filepath.Join(dst, "w.db")); got != "applied" {
			t.Errorf("copy of an open WAL database: engine shows %q, want \"applied\"", got)
		}
	})
	t.Run("journal", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "j.db")
		db := openEngine(t, path)
		for _, q := range []string{
			"pragma journal_mode=delete",
			"create table t(id integer primary key, v blob)",
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		for range 400 {
			if _, err := db.Exec("insert into t(v) values (zeroblob(1000))"); err != nil {
				t.Fatal(err)
			}
		}
		committed := engineDump(t, db)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("pragma cache_size=10"); err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		// An oversized transaction: far more dirty pages than the cache
		// holds, so the engine spills them (journal first, then the file).
		if _, err := tx.Exec("update t set v = zeroblob(1001)"); err != nil {
			t.Fatal(err)
		}
		journal := path + "-journal"
		dst := t.TempDir()
		mode := copyFiles(t, dst, path, journal)
		t.Logf("journal copy mode: %s", mode)
		_ = tx.Rollback()

		jb, err := os.ReadFile(filepath.Join(dst, "j.db-journal"))
		if err != nil {
			t.Fatal(err)
		}
		if len(jb) == 0 || jb[0] == 0 {
			t.Fatalf("copied journal is not hot (size %d)", len(jb))
		}
		spilled, err := os.ReadFile(filepath.Join(dst, "j.db"))
		if err != nil {
			t.Fatal(err)
		}
		if string(spilled) == string(before) {
			t.Fatal("the transaction did not spill into the database file: probe is vacuous")
		}
		cp := openEngine(t, filepath.Join(dst, "j.db"))
		if got := engineDump(t, cp); got != committed {
			t.Errorf("a copy with its hot journal does not show the committed state")
		}
		if _, err := os.Stat(filepath.Join(dst, "j.db-journal")); err == nil {
			t.Log("note: the engine left the journal in place after recovery")
		}
	})
}

func TestEngineWALWithRollbackHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		grow bool
		want bool
	}{
		{"no page 1 frame", false, engineAppliesWALWithRollbackHeader},
		{"page 1 in the WAL", true, engineAppliesWALWithRollbackHeaderPage1WAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWALFixture(t, tc.grow)
			dst := t.TempDir()
			copyFiles(t, dst, f.db, f.wal)
			cp := filepath.Join(dst, "w.db")
			patchFile(t, cp, 18, 1, 1)
			got := f.outcome(t, cp)
			t.Logf("outcome with header bytes 18,19 = 1: %s", got)
			if applied := got == "applied"; applied != tc.want {
				t.Errorf("engine applies the WAL = %v (outcome %q), recorded constant says %v", applied, got, tc.want)
			}
		})
	}
}

// walSum is the WAL checksum over 8-byte units (own implementation: the probe
// must not share code with the library).
func walSum(b []byte, bigEndian bool, s0, s1 uint32) (uint32, uint32) {
	var bo binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		bo = binary.BigEndian
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += bo.Uint32(b[i:]) + s1
		s1 += bo.Uint32(b[i+4:]) + s0
	}
	return s0, s1
}

func TestEngineWALPageSizeMismatch(t *testing.T) {
	t.Run("header patched only", func(t *testing.T) {
		f := newWALFixture(t, false)
		dst := t.TempDir()
		copyFiles(t, dst, f.db, f.wal)
		w := filepath.Join(dst, "w.db-wal")
		data, err := os.ReadFile(w)
		if err != nil {
			t.Fatal(err)
		}
		binary.BigEndian.PutUint32(data[8:], 8192)
		c1, c2 := walSum(data[:24], data[3] == 0x83, 0, 0)
		binary.BigEndian.PutUint32(data[24:], c1)
		binary.BigEndian.PutUint32(data[28:], c2)
		if err := os.WriteFile(w, data, 0o600); err != nil { //nolint:gosec // a test file inside t.TempDir()
			t.Fatal(err)
		}
		got := f.outcome(t, filepath.Join(dst, "w.db"))
		t.Logf("outcome: %s", got)
		if got != engineWALPageSizeMismatchOutcome {
			t.Errorf("outcome %q, recorded constant says %q", got, engineWALPageSizeMismatchOutcome)
		}
	})
	t.Run("valid chain of another page size", func(t *testing.T) {
		f := newWALFixture(t, false)
		dst := t.TempDir()
		copyFiles(t, dst, f.db)
		dbBytes, err := os.ReadFile(filepath.Join(dst, "w.db"))
		if err != nil {
			t.Fatal(err)
		}
		const dbPage = 4096 // the engine's default
		const walPage = 8192
		w := make([]byte, 32, 32+24+walPage)
		binary.BigEndian.PutUint32(w[0:], 0x377f0682) // little-endian checksum words
		binary.BigEndian.PutUint32(w[4:], 3007000)
		binary.BigEndian.PutUint32(w[8:], walPage)
		binary.BigEndian.PutUint32(w[16:], 0x11111111) // salt-1
		binary.BigEndian.PutUint32(w[20:], 0x22222222) // salt-2
		c1, c2 := walSum(w[:24], false, 0, 0)
		binary.BigEndian.PutUint32(w[24:], c1)
		binary.BigEndian.PutUint32(w[28:], c2)
		fh := make([]byte, 24)
		binary.BigEndian.PutUint32(fh[0:], 2) // page 2
		binary.BigEndian.PutUint32(fh[4:], 2) // commit frame, database of 2 pages
		binary.BigEndian.PutUint32(fh[8:], 0x11111111)
		binary.BigEndian.PutUint32(fh[12:], 0x22222222)
		// The frame holds the table's leaf (page 2) with its first text value
		// changed, padded to the WAL's page size: if the engine applies it,
		// the change shows even though the sizes disagree.
		page := make([]byte, walPage)
		copy(page, dbBytes[dbPage:2*dbPage])
		at := strings.Index(string(page[:dbPage]), "base")
		if at < 0 {
			t.Fatal("fixture error: no text in page 2")
		}
		page[at] = 'X'
		s1, s2 := walSum(fh[:8], false, c1, c2)
		s1, s2 = walSum(page, false, s1, s2)
		binary.BigEndian.PutUint32(fh[16:], s1)
		binary.BigEndian.PutUint32(fh[20:], s2)
		w = append(append(w, fh...), page...)
		if err := os.WriteFile(filepath.Join(dst, "w.db-wal"), w, 0o600); err != nil {
			t.Fatal(err)
		}
		got := f.outcome(t, filepath.Join(dst, "w.db"))
		if got == "other" {
			// distinguish "applied with the db's page size" from "ignored"
			db := openEngine(t, filepath.Join(dst, "w.db"))
			var n int
			if err := db.QueryRow("select count(*) from t where v like 'Xase%'").Scan(&n); err == nil && n > 0 {
				got = "applied-first-db-page-bytes"
			}
		}
		t.Logf("outcome: %s", got)
		if got != engineWALPageSizeMismatchValidFramesOutcome {
			t.Errorf("outcome %q, recorded constant says %q", got, engineWALPageSizeMismatchValidFramesOutcome)
		}
	})
}

func TestEngineWALOnZeroLengthDB(t *testing.T) {
	// A WAL that holds everything (schema included): autocheckpoint is off
	// and nothing is checkpointed, so the db file itself carries no table.
	src := filepath.Join(t.TempDir(), "z.db")
	conn := openEngine(t, src)
	for _, q := range []string{
		"pragma journal_mode=wal",
		"pragma wal_autocheckpoint=0",
		"create table t(id integer primary key, v text)",
		"insert into t(v) values ('only in the wal')",
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dst := t.TempDir()
	copyFiles(t, dst, src+"-wal")
	dbPath := filepath.Join(dst, "z.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := openEngine(t, dbPath)
	var n int
	seen := db.QueryRow("select count(*) from t").Scan(&n) == nil && n > 0
	_, statErr := os.Stat(dbPath + "-wal")
	deleted := errors.Is(statErr, os.ErrNotExist)
	t.Logf("engine sees the WAL's rows: %v; WAL deleted by the engine: %v", seen, deleted)
	if seen != engineSeesWALOnZeroLengthDB {
		t.Errorf("engine sees WAL rows = %v, recorded constant says %v", seen, engineSeesWALOnZeroLengthDB)
	}
	if deleted != engineDeletesWALOnZeroLengthDB {
		t.Errorf("WAL deleted = %v, recorded constant says %v", deleted, engineDeletesWALOnZeroLengthDB)
	}
}
