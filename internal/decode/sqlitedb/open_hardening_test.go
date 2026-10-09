package sqlitedb_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func gzipBytes(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// copyFixtureTo copies the committed hot-journal fixture into a fresh
// directory; edit may change the decompressed bytes of a file or the oracle.
func copyFixtureTo(t testing.TB, edit func(name string, b []byte) []byte) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"hot-journal.db.gz", "hot-journal.db-journal.gz", "hot-journal.expect.json"} {
		var out []byte
		if base, ok := strings.CutSuffix(n, ".gz"); ok {
			plain, _, err := gunzipFixtureFile(filepath.Join(fixtureDir, n))
			if err != nil {
				t.Fatal(err)
			}
			out = gzipBytes(t, edit(base, plain))
		} else {
			raw, err := os.ReadFile(filepath.Join(fixtureDir, n))
			if err != nil {
				t.Fatal(err)
			}
			out = edit(n, raw)
		}
		if err := os.WriteFile(filepath.Join(dir, n), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadFixtureVerifiesHashesOnTheRealPath(t *testing.T) {
	same := func(_ string, b []byte) []byte { return b }
	if _, _, _, _, err := readFixture(copyFixtureTo(t, same), "hot-journal"); err != nil {
		t.Fatalf("a faithful copy is refused: %v", err)
	}
	flip := func(name string, b []byte) []byte {
		if name == "hot-journal.db" {
			b = bytes.Clone(b)
			b[len(b)/2] ^= 1
		}
		return b
	}
	if _, _, _, _, err := readFixture(copyFixtureTo(t, flip), "hot-journal"); err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Errorf("a flipped image byte is accepted: %v", err)
	}
	// A stale oracle: its recorded hash of the database no longer matches.
	staleOracle := func(name string, b []byte) []byte {
		if name != "hot-journal.expect.json" {
			return b
		}
		_, _, _, exp, err := readFixture(fixtureDir, "hot-journal")
		if err != nil {
			t.Fatal(err)
		}
		h := exp.Generator.FileSHA256["hot-journal.db"]
		flipped := "0" + h[1:]
		if h[0] == '0' {
			flipped = "1" + h[1:]
		}
		if !bytes.Contains(b, []byte(h)) {
			t.Fatal("oracle text does not hold the database hash")
		}
		return bytes.Replace(b, []byte(h), []byte(flipped), 1)
	}
	if _, _, _, _, err := readFixture(copyFixtureTo(t, staleOracle), "hot-journal"); err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Errorf("a stale oracle is accepted: %v", err)
	}
}

// boundReader fails any read that is negative or touches a byte at or past size.
type boundReader struct {
	t     *testing.T
	data  []byte
	size  int64
	reads int
	bad   int
}

func (g *boundReader) ReadAt(p []byte, off int64) (int, error) {
	g.reads++
	if off < 0 || off+int64(len(p)) > g.size {
		g.bad++
		g.t.Errorf("read of %d bytes at %d is outside the stated size %d", len(p), off, g.size)
		return 0, fmt.Errorf("read outside the stated size")
	}
	return bytes.NewReader(g.data).ReadAt(p, off)
}

func TestOpenNeverReadsOutsideStatedSizes(t *testing.T) {
	big := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= 60; i++ {
			tb.Insert(i, i, strings.Repeat("x", 100))
		}
	})
	walBase, walFn := walDB(t)
	wal := walFn()
	hjDB, hjJournal := hotJournalDB(t, 1024, "")
	frame := int64(24 + 1024)
	sizes := func(n int64, extra ...int64) []int64 {
		s := append([]int64{0, 1, 31, 32, 99, 100, 511, 512, 513, 1023, 1024, 2047, 2048, n - 1}, extra...)
		var out []int64
		for _, v := range s {
			if v >= 0 && v < n {
				out = append(out, v)
			}
		}
		return append(out, n) // the full size is always the last case
	}
	for _, kind := range []string{"database", "wal", "journal"} {
		var list []int64
		switch kind {
		case "database":
			list = sizes(int64(len(big)), 4096, 4097)
		case "wal":
			list = sizes(int64(len(wal)), 32+24, 32+frame-1, 32+frame, 32+frame+24, 32+2*frame-1)
		default:
			list = sizes(int64(len(hjJournal)), 28, 512+4+1024-1)
		}
		for i, sz := range list {
			dbData := hjDB
			switch kind {
			case "database":
				dbData = big
			case "wal":
				dbData = walBase
			}
			dbr := &boundReader{t: t, data: dbData, size: int64(len(dbData))}
			f := sqlitedb.Files{DB: dbr, DBSize: dbr.size}
			var guarded *boundReader
			switch kind {
			case "database":
				dbr.size, f.DBSize = sz, sz
				guarded = dbr
			case "wal":
				guarded = &boundReader{t: t, data: wal, size: sz}
				f.WAL, f.WALSize = guarded, sz
			default:
				guarded = &boundReader{t: t, data: hjJournal, size: sz}
				f.Journal, f.JournalSize = guarded, sz
			}
			d, err := sqlitedb.Open(t.Context(), f, bigBudget())
			if err == nil {
				if _, err := d.Tables(t.Context()); err != nil {
					t.Errorf("%s size %d: Tables: %v", kind, sz, err)
				}
				d.Release()
			}
			if i == len(list)-1 { // the full size
				if err != nil {
					t.Errorf("%s at its full size: Open: %v", kind, err)
				}
				if guarded.reads == 0 {
					t.Errorf("%s at its full size: the guarded reader was never read", kind)
				}
			}
		}
	}
}

func TestWrapErrMapsEveryLibrarySentinel(t *testing.T) {
	mapped := []struct{ lib, layer error }{
		{sqlitefile.ErrNotSQLite, sqlitedb.ErrNotSQLite},
		{sqlitefile.ErrCorrupt, sqlitedb.ErrCorrupt},
		{sqlitefile.ErrLiveUnavailable, sqlitedb.ErrLiveUnavailable},
		{sqlitefile.ErrEngineRefuses, sqlitedb.ErrEngineRefuses},
		{sqlitefile.ErrWithoutRowid, sqlitedb.ErrWithoutRowid},
		{sqlitefile.ErrInternal, sqlitedb.ErrInternal},
	}
	for _, m := range mapped {
		in := fmt.Errorf("context: %w", m.lib)
		got := sqlitedb.WrapErr(in)
		if !errors.Is(got, m.layer) || !errors.Is(got, m.lib) {
			t.Errorf("%v: got %v", m.lib, got)
		}
	}
	passthrough := []error{
		sqlitefile.ErrBudget, parse.ErrBudget, sqlitefile.ErrLimit, sqlitefile.ErrPageUnavailable,
		context.Canceled, context.DeadlineExceeded, errors.New("disk on fire"),
	}
	for _, p := range passthrough {
		in := fmt.Errorf("ctx: %w", p)
		if got := sqlitedb.WrapErr(in); got != in {
			t.Errorf("%v was rewritten to %v", p, got)
		}
	}
	// A budget error that also wraps ErrCorrupt must stay a budget error.
	mixed := fmt.Errorf("%w while reading: %w", sqlitefile.ErrBudget, sqlitefile.ErrCorrupt)
	if got := sqlitedb.WrapErr(mixed); got != mixed {
		t.Errorf("budget+corrupt rewritten: %v", got)
	}
	if sqlitedb.WrapErr(nil) != nil {
		t.Error("WrapErr(nil) != nil")
	}
}

func TestFailedOpenReturnsEveryCharge(t *testing.T) {
	db := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		for _, n := range []string{"a", "b", "c", "d"} {
			tb := b.CreateTable(n, "create table "+n+"(x, y)")
			for i := int64(1); i <= 40; i++ {
				tb.Insert(i, i, strings.Repeat("y", 80))
			}
		}
	})
	probe := &testBudget{limit: 1 << 40}
	d, err := sqlitedb.Open(t.Context(), filesOf(db, nil, nil), probe)
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	if probe.peak <= 0 {
		t.Fatal("Open charged nothing")
	}
	failed := 0
	for limit := int64(0); limit <= probe.peak; limit += max(1, probe.peak/64) {
		tb := &testBudget{limit: limit}
		d, err := sqlitedb.Open(t.Context(), filesOf(db, nil, nil), tb)
		if err == nil {
			d.Release()
		} else {
			failed++
			if !errors.Is(err, parse.ErrBudget) && !errors.Is(err, sqlitefile.ErrBudget) {
				t.Errorf("limit %d: err = %v, want a budget error unchanged", limit, err)
			}
			if d != nil {
				t.Errorf("limit %d: non-nil DB with an error", limit)
			}
		}
		if tb.used != 0 {
			t.Errorf("limit %d: %d bytes still charged (err %v)", limit, tb.used, err)
		}
	}
	if failed == 0 {
		t.Error("no limit made Open fail")
	}
	// A failure after the database opened (a refused WAL) also returns everything.
	wdb, wal := walDB(t)
	w := wal()
	binary.BigEndian.PutUint32(w[4:], 3007001)
	c1, c2 := sqlitefile.WALChecksum(w[:24], false, 0, 0)
	binary.BigEndian.PutUint32(w[24:], c1)
	binary.BigEndian.PutUint32(w[28:], c2)
	tb := &testBudget{limit: 1 << 30}
	if _, err := sqlitedb.Open(t.Context(), filesOf(wdb, w, nil), tb); err == nil {
		t.Fatal("refused WAL opened")
	}
	if tb.used != 0 {
		t.Errorf("%d bytes still charged after a refused WAL", tb.used)
	}
}

func TestInfoDistinguishesStoredFromLiveHeader(t *testing.T) {
	build := func(userVersion int32) *sqlitetest.Builder {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024, UserVersion: userVersion})
		b.CreateTable("t", "create table t(a, b)").Insert(1, int64(1), "one")
		return b
	}
	old := build(5)
	cur := build(7)
	j := cur.NewJournal(512, 0x4242, old.Snapshot().Pages())
	j.Record(1, old.PageBytes(1))
	d := openBytes(t, cur.Bytes(), nil, j.Bytes(), bigBudget())
	info := d.Info()
	if info.Journal == nil || !info.Journal.Applied {
		t.Fatalf("journal info = %+v", info.Journal)
	}
	if info.Stored.UserVersion != 7 || info.Header.UserVersion != 5 {
		t.Errorf("Stored.UserVersion %d (want 7), Header.UserVersion %d (want 5)", info.Stored.UserVersion, info.Header.UserVersion)
	}
}

func TestStatsBaselineIsTakenAtOpen(t *testing.T) {
	db, _, journal := loadFixture(t, "hot-journal")
	d := openBytes(t, db, nil, journal, bigBudget())
	if len(d.Warnings()) == 0 {
		t.Fatal("fixture opened without warnings; the test needs some")
	}
	if got := d.Stats().NewWarningsAtLeast; got != 0 {
		t.Errorf("NewWarningsAtLeast = %d right after Open, want 0 (warnings of Open are not new)", got)
	}
}

func TestInspectionWorksAfterRelease(t *testing.T) {
	db, _, journal := loadFixture(t, "hot-journal")
	d, err := sqlitedb.Open(t.Context(), filesOf(db, nil, journal), bigBudget())
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.Tables(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nw := len(d.Warnings())
	info := d.Info()
	d.Release()
	after, err := d.Tables(t.Context())
	if err != nil || strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("Tables after Release = %v, %v; before %v", after, err, before)
	}
	if len(d.Warnings()) != nw || d.Info().Header.PageSize != info.Header.PageSize {
		t.Error("Warnings or Info changed after Release")
	}
}
