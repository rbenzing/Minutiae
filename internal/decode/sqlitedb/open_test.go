package sqlitedb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func threeRowDB(t testing.TB) []byte {
	t.Helper()
	return newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a, b)")
		tb.Insert(1, int64(1), "one")
		tb.Insert(2, int64(2), "two")
		tb.Insert(3, int64(3), "three")
	})
}

func hasWarning(ws []sqlitedb.Warning, code string) *sqlitedb.Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

func TestOpenPlainDatabase(t *testing.T) {
	d := openBytes(t, threeRowDB(t), nil, nil, bigBudget())
	info := d.Info()
	if info.Header.PageSize != 1024 || info.Stored.PageSize != 1024 {
		t.Errorf("page size: header %d stored %d", info.Header.PageSize, info.Stored.PageSize)
	}
	if info.WAL != nil || info.Journal != nil {
		t.Errorf("WAL %v journal %v, want nil", info.WAL, info.Journal)
	}
	got, err := d.Tables(t.Context())
	if err != nil || !slices.Equal(got, []string{"t"}) {
		t.Errorf("Tables = %v, %v", got, err)
	}
}

func TestOpenRefusesNilBudget(t *testing.T) {
	db := threeRowDB(t)
	r := &readCounter{r: bytes.NewReader(db)}
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: r, DBSize: int64(len(db))}, nil)
	if !errors.Is(err, sqlitedb.ErrNoBudget) || d != nil {
		t.Fatalf("Open = %v, %v", d, err)
	}
	if r.reads != 0 {
		t.Errorf("%d reads before the refusal", r.reads)
	}
}

type readCounter struct {
	r     io.ReaderAt
	reads int
}

func (c *readCounter) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.r.ReadAt(p, off)
}

func TestOpenRefusesBadFiles(t *testing.T) {
	db := threeRowDB(t)
	rd := bytes.NewReader(db)
	n := int64(len(db))
	for _, c := range []struct {
		name string
		f    sqlitedb.Files
	}{
		{"nil DB", sqlitedb.Files{DBSize: n}},
		{"negative size", sqlitedb.Files{DB: rd, DBSize: -1}},
		{"negative WAL size", sqlitedb.Files{DB: rd, DBSize: n, WAL: rd, WALSize: -1}},
		{"negative journal size", sqlitedb.Files{DB: rd, DBSize: n, Journal: rd, JournalSize: -1}},
		{"nil WAL reader with size", sqlitedb.Files{DB: rd, DBSize: n, WALSize: 10}},
		{"nil journal reader with size", sqlitedb.Files{DB: rd, DBSize: n, JournalSize: 10}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := sqlitedb.Open(t.Context(), c.f, bigBudget())
			if !errors.Is(err, sqlitedb.ErrBadFiles) || d != nil {
				t.Errorf("Open = %v, %v", d, err)
			}
		})
	}
}

func TestOpenNotSQLite(t *testing.T) {
	junk := bytes.Repeat([]byte("this is plain text, not a database. "), 100)
	_, err := sqlitedb.Open(t.Context(), filesOf(junk, nil, nil), bigBudget())
	if !errors.Is(err, sqlitedb.ErrNotSQLite) || !errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, sqlitedb.ErrEncrypted) {
		t.Errorf("plain text is not encrypted: %v", err)
	}
}

func TestOpenLooksEncrypted(t *testing.T) {
	if !loadExpect(t, "encrypted-like").NotSQLite {
		t.Fatal("the oracle does not call encrypted-like a non-database")
	}
	db, _, _ := loadFixture(t, "encrypted-like")
	_, err := sqlitedb.Open(t.Context(), filesOf(db, nil, nil), bigBudget())
	if !errors.Is(err, sqlitedb.ErrEncrypted) || !errors.Is(err, sqlitedb.ErrNotSQLite) || !errors.Is(err, sqlitefile.ErrLooksEncrypted) {
		t.Errorf("err = %v", err)
	}
	_, err = sqlitedb.Open(t.Context(), filesOf([]byte{}, nil, nil), bigBudget())
	if !errors.Is(err, sqlitedb.ErrNotSQLite) || errors.Is(err, sqlitedb.ErrEncrypted) {
		t.Errorf("empty file: %v", err)
	}
}

// walDB is a WAL-mode database and a function building a WAL that commits two
// more rows over it.
func walDB(t testing.TB) (db []byte, wal func() []byte) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, int64(1), "one")
	snap := b.Snapshot()
	base := b.Bytes()
	base[18], base[19] = 2, 2
	tb.Insert(2, int64(2), "two")
	tb.Insert(3, int64(3), "three")
	return base, func() []byte {
		w := b.NewWAL(false, 0x1000, 0x2000, 0)
		w.Frame(1, snap.Page(1), 0)
		w.Frame(2, b.PageBytes(2), 2)
		return w.Bytes()
	}
}

func TestOpenCompanionCases(t *testing.T) {
	t.Run("committed WAL is used by live", func(t *testing.T) {
		db, wal := walDB(t)
		d := openBytes(t, db, wal(), nil, bigBudget())
		info := d.Info()
		if info.WAL == nil || info.WAL.FramesCommitted < 2 || !info.WAL.UsedByLive {
			t.Errorf("WAL info = %+v", info.WAL)
		}
	})
	t.Run("WAL with a flipped magic opens and warns", func(t *testing.T) {
		db, wal := walDB(t)
		w := wal()
		w[1] ^= 0x10
		d := openBytes(t, db, w, nil, bigBudget())
		if info := d.Info(); info.WAL == nil || info.WAL.HeaderValid {
			t.Errorf("WAL info = %+v", info.WAL)
		}
		if hasWarning(d.Warnings(), sqlitefile.WarnWALHeaderInvalid) == nil {
			t.Errorf("warnings = %+v", d.Warnings())
		}
	})
	t.Run("zero-length WAL is given to the reader as supplied", func(t *testing.T) {
		// A supplied companion is never skipped silently: the reader decides what
		// an empty WAL is. This pins its decision.
		db, _ := walDB(t)
		d := openBytes(t, db, []byte{}, nil, bigBudget())
		info := d.Info()
		if info.WAL == nil {
			t.Fatal("a supplied zero-length WAL was skipped")
		}
		if info.WAL.Present || info.WAL.UsedByLive {
			t.Errorf("WAL info = %+v", info.WAL)
		}
	})
	t.Run("hot journal fixture is applied and warned about", func(t *testing.T) {
		db, _, journal := loadFixture(t, "hot-journal")
		d := openBytes(t, db, nil, journal, bigBudget())
		info := d.Info()
		if info.Journal == nil || !info.Journal.Hot || !info.Journal.Applied {
			t.Fatalf("journal info = %+v", info.Journal)
		}
		if hasWarning(d.Warnings(), sqlitefile.WarnJournalHot) == nil {
			t.Errorf("warnings = %+v", d.Warnings())
		}
	})
	t.Run("same database without its journal", func(t *testing.T) {
		// The host owns bundle completeness (doc.go): a journal that was not
		// supplied is not guessed, so there is no journal and no warning.
		db, _, _ := loadFixture(t, "hot-journal")
		d := openBytes(t, db, nil, nil, bigBudget())
		if d.Info().Journal != nil {
			t.Errorf("journal info = %+v", d.Info().Journal)
		}
		if hasWarning(d.Warnings(), sqlitefile.WarnJournalHot) != nil {
			t.Errorf("unexpected journal-hot warning")
		}
	})
	t.Run("journal with zero records: the reader applies it, to nothing", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		b.CreateTable("t", "create table t(a, b)").Insert(1, int64(1), "one")
		snap := b.Snapshot()
		j := b.NewJournal(512, 0x1234, snap.Pages())
		d := openBytes(t, b.Bytes(), nil, j.Bytes(), bigBudget())
		info := d.Info()
		// The reader decides: a hot header with no records is "applied" with 0 records.
		if info.Journal == nil || !info.Journal.Applied || info.Journal.AppliedRecords != 0 {
			t.Errorf("journal info = %+v", info.Journal)
		}
	})
}

// hotJournalDB returns a database and a hot journal of the builder.
func hotJournalDB(t testing.TB, journalPageSize int, superJournal string) (db, journal []byte) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, int64(1), "one")
	snap := b.Snapshot()
	tb.Insert(2, int64(2), "two")
	db = b.Bytes()
	jb := b
	if journalPageSize != 1024 {
		jb = sqlitetest.New(sqlitetest.Options{PageSize: journalPageSize})
	}
	j := jb.NewJournal(512, 0x7777, snap.Pages())
	if journalPageSize == 1024 {
		j.Record(2, snap.Page(2))
	} else {
		j.Record(1, make([]byte, journalPageSize))
	}
	if superJournal != "" {
		j.SuperJournal(superJournal)
	}
	return db, j.Bytes()
}

func TestOpenSuperJournalIsNotAppliedAndSaysSo(t *testing.T) {
	db, journal := hotJournalDB(t, 1024, "x")
	d := openBytes(t, db, nil, journal, bigBudget())
	j := d.Info().Journal
	if j == nil || !j.HasSuperJournal || j.Applied || j.NotAppliedReason != "super-journal-unknown" {
		t.Fatalf("journal info = %+v", j)
	}
	w := hasWarning(d.Warnings(), sqlitefile.WarnJournalSuperUnknown)
	if w == nil || !strings.Contains(w.Msg, "not applied") {
		t.Errorf("warning = %+v", w)
	}
	if got, err := d.Tables(t.Context()); err != nil || !slices.Equal(got, []string{"t"}) {
		t.Errorf("Tables = %v, %v", got, err)
	}
}

func TestOpenHotJournalNotAppliedIsLiveUnavailable(t *testing.T) {
	db, journal := hotJournalDB(t, 2048, "")
	d, err := sqlitedb.Open(t.Context(), filesOf(db, nil, journal), bigBudget())
	if !errors.Is(err, sqlitedb.ErrLiveUnavailable) || !errors.Is(err, sqlitefile.ErrLiveUnavailable) || d != nil {
		t.Fatalf("Open = %v, %v", d, err)
	}
}

func TestOpenWALEngineRefusalIsErrEngineRefuses(t *testing.T) {
	db, wal := walDB(t)
	w := wal()
	binary.BigEndian.PutUint32(w[4:], 3007001) // a version the engine does not support
	c1, c2 := sqlitefile.WALChecksum(w[:24], false, 0, 0)
	binary.BigEndian.PutUint32(w[24:], c1)
	binary.BigEndian.PutUint32(w[28:], c2)
	d, err := sqlitedb.Open(t.Context(), filesOf(db, w, nil), bigBudget())
	if !errors.Is(err, sqlitedb.ErrEngineRefuses) || !errors.Is(err, sqlitefile.ErrEngineRefuses) || d != nil {
		t.Fatalf("Open = %v, %v", d, err)
	}
}

func TestOpenCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d, err := sqlitedb.Open(ctx, filesOf(threeRowDB(t), nil, nil), bigBudget())
	if !errors.Is(err, context.Canceled) || d != nil {
		t.Errorf("Open = %v, %v", d, err)
	}
}

func TestTablesListsOnlyBaseTables(t *testing.T) {
	db := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("alpha", "create table alpha(a, b)").Insert(1, int64(1), "x")
		b.CreateTable("beta", "create table beta(c)")
		b.CreateIndex("alpha_a", "alpha", "create index alpha_a on alpha(a)", 0)
		b.AddSchemaRow("view", "v", "v", int64(0), "create view v as select a from alpha")
		b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(x)")
	})
	d := openBytes(t, db, nil, nil, bigBudget())
	got, err := d.Tables(t.Context())
	if err != nil || !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("Tables = %v, %v", got, err)
	}
}

func TestReleaseIsIdempotentAndFreesCharges(t *testing.T) {
	tb := &testBudget{limit: 1 << 30}
	d, err := sqlitedb.Open(t.Context(), filesOf(threeRowDB(t), nil, nil), tb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Tables(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tb.peak <= 0 {
		t.Errorf("peak = %d, want the layer to have charged the budget", tb.peak)
	}
	d.Release()
	d.Release()
	if tb.used != 0 {
		t.Errorf("used = %d after Release, want 0", tb.used)
	}
}

// guardReader fails on any read that touches a byte at or past size.
type guardReader struct {
	t    *testing.T
	data []byte
	size int64
	bad  int
}

func (g *guardReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > g.size {
		g.bad++
		g.t.Errorf("read of %d bytes at %d reaches past the stated size %d", len(p), off, g.size)
		return 0, fmt.Errorf("read past the stated size")
	}
	return bytes.NewReader(g.data).ReadAt(p, off)
}

func TestOpenNeverReadsPastStatedSize(t *testing.T) {
	db := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= 60; i++ {
			tb.Insert(i, i, strings.Repeat("x", 100))
		}
	})
	if len(db) < 4*1024 {
		t.Fatalf("builder database is %d bytes", len(db))
	}
	g := &guardReader{t: t, data: db, size: 2 * 1024}
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: g, DBSize: g.size}, bigBudget())
	if err == nil {
		_, _ = d.Tables(t.Context())
		d.Release()
	}
	if g.bad != 0 {
		t.Errorf("%d reads past the stated size", g.bad)
	}
}

func TestFixtureHelpersRejectAStaleOracle(t *testing.T) {
	db, wal, journal, exp, err := readFixture(fixtureDir, "hot-journal")
	if err != nil {
		t.Fatal(err)
	}
	if wal != nil || journal == nil {
		t.Fatalf("unexpected companions: wal %d journal %d", len(wal), len(journal))
	}
	files := map[string][]byte{"hot-journal.db": db, "hot-journal.db-journal": journal}
	if err := checkFixtureHashes("hot-journal", files, exp.Generator.FileSHA256); err != nil {
		t.Fatalf("the genuine fixture is refused: %v", err)
	}
	stale := slices.Clone(db)
	stale[len(stale)/2] ^= 1
	files["hot-journal.db"] = stale
	if err := checkFixtureHashes("hot-journal", files, exp.Generator.FileSHA256); err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Errorf("a flipped byte is accepted: %v", err)
	}
	delete(files, "hot-journal.db-journal")
	if err := checkFixtureHashes("hot-journal", files, exp.Generator.FileSHA256); err == nil {
		t.Error("a missing file is accepted")
	}
}
