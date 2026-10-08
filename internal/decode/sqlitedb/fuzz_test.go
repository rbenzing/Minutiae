package sqlitedb_test

// FuzzSQLiteDB drives the whole read surface of the decoder over arbitrary
// database, WAL and journal bytes: Open, Info, Tables, Table, a bounded Scan,
// Get, Index, Rowids, Clone and recovered rows. The oracle is the contract:
// every error is typed (ErrInternal is a failure), every Row keeps its
// invariants, a live row's Range lies inside its file, Get agrees with Scan,
// an index finds the row it was built from, and the budget balances.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	fuzzBudget      = 64 << 20
	fuzzTables      = 16
	fuzzScanRows    = 2000
	fuzzGetRows     = 5
	fuzzCloneRows   = 3
	fuzzKeyRows     = 50
	fuzzHistoryRows = 200
	fuzzTimeout     = 30 * time.Second
	fuzzSeedCap     = 64 << 10
)

// fuzzSeed is one corpus entry. invalid marks a seed that is expected not to
// open and scan cleanly (an encrypted-looking file, a damaged first page).
type fuzzSeed struct {
	name             string
	db, wal, journal []byte
	mode             uint8
	invalid          bool
}

// trimZeros drops the zero bytes after the last non-zero byte.
func trimZeros(b []byte) []byte {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return b[:n]
}

// fixtureSeeds gunzips every committed 3I fixture (in memory only).
func fixtureSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.db.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures under %s: %v", fixtureDir, err)
	}
	slices.Sort(paths)
	var out []fuzzSeed
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".db.gz")
		s := fuzzSeed{name: name, mode: 1, invalid: name == "encrypted-like"}
		for _, f := range []struct {
			suffix string
			dst    *[]byte
		}{{".db", &s.db}, {".db-wal", &s.wal}, {".db-journal", &s.journal}} {
			b, ok, gerr := gunzipFixtureFile(filepath.Join(fixtureDir, name+f.suffix+".gz"))
			if gerr != nil {
				t.Fatal(gerr)
			}
			if ok {
				*f.dst = trimZeros(b)
			}
		}
		if len(s.db) > fuzzSeedCap || len(s.wal) > fuzzSeedCap || len(s.journal) > fuzzSeedCap {
			continue // large fixtures are covered by the hostile matrices; big seeds stalled the fuzz workers
		}
		out = append(out, s)
	}
	return out
}

// builderSeeds are small databases of the shapes the decoder has special
// handling for.
func builderSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	var out []fuzzSeed
	add := func(name string, o sqlitetest.Options, fill func(b *sqlitetest.Builder)) {
		out = append(out, fuzzSeed{name: name, db: newBuilderDB(t, o, fill), mode: 1})
	}
	add("short-records", sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, a text, b integer, c text)")
		tt.Insert(1, nil, "full", int64(7), "row")
		tt.InsertRaw(2, []uint64{0, 15, 1}, []byte{'x', 5}) // two of the three stored columns
		tt.InsertRaw(3, []uint64{0}, nil)                   // none of them
	})
	for _, enc := range []int{2, 3} {
		add(fmt.Sprintf("utf16-%d", enc), sqlitetest.Options{PageSize: 512, Encoding: enc}, func(b *sqlitetest.Builder) {
			tt := b.CreateTable("t", "create table t(id integer primary key, s text)")
			for i := int64(1); i <= 8; i++ {
				tt.Insert(i, nil, fmt.Sprintf("café-%d-世界", i))
			}
		})
	}
	add("overflow", sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, s text, b blob)")
		tt.Insert(1, nil, strings.Repeat("o", 2000), bytes.Repeat([]byte{7}, 1500))
		tt.Insert(2, nil, "short", []byte{1})
	})
	add("without-rowid", sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		tt := b.CreateTableWithoutRowid("w", "create table w(a primary key, b) without rowid", 1)
		for i := int64(1); i <= 6; i++ {
			tt.Insert(i, i*10, fmt.Sprintf("v%d", i))
		}
	})
	add("two-level-tree", sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, s text)")
		for i := int64(1); i <= 300; i++ {
			tt.Insert(i, nil, fmt.Sprintf("row-%03d-%s", i, strings.Repeat("p", 20)))
		}
		if tt.Depth() < 2 {
			t.Fatalf("the two-level seed is %d levels deep", tt.Depth())
		}
	})
	add("indexed-nocase", sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, name text collate nocase)")
		for i, s := range []string{"Alpha", "alpha", "ALPHA", "beta", "Beta", "Éclair", "éclair"} {
			tt.Insert(int64(i+1), nil, s)
		}
		b.CreateIndex("t_name", "t", "create index t_name on t(name)", 1)
	})

	// The WAL trio and the journal trio of the companion tests.
	ws := buildWAL(t, false)
	out = append(out, fuzzSeed{name: "wal-trio", db: ws.db, wal: ws.wal, mode: 1})
	wt := buildWAL(t, true)
	out = append(out, fuzzSeed{name: "wal-trio-uncommitted-tail", db: wt.db, wal: wt.wal, mode: 1})
	jb := companionBase(t)
	snap := jb.Snapshot()
	companionRewrite(jb, "new")
	j := jb.NewJournal(512, 0x1234, snap.Pages())
	for p := uint32(1); p <= snap.Pages(); p++ {
		if string(jb.PageBytes(p)) != string(snap.Page(p)) {
			j.Record(p, snap.Page(p))
		}
	}
	out = append(out, fuzzSeed{name: "journal-trio", db: jb.Bytes(), journal: j.Bytes(), mode: 1})
	return out
}

// flippedSeeds adds, for every seed, a variant with one byte of the database
// header (page 1) flipped, at an offset that cycles through the header fields.
func flippedSeeds(seeds []fuzzSeed) []fuzzSeed {
	offsets := []int{16, 18, 20, 24, 28, 40, 56, 60, 72, 96}
	var out []fuzzSeed
	for i, s := range seeds {
		off := offsets[i%len(offsets)]
		if off >= len(s.db) {
			continue
		}
		v := s
		v.name += fmt.Sprintf("-flip%d", off)
		v.db = slices.Clone(s.db)
		v.db[off] ^= 0xff
		v.mode = 0
		v.invalid = true
		out = append(out, v)
	}
	return out
}

func allFuzzSeeds(t testing.TB) []fuzzSeed {
	t.Helper()
	seeds := append(fixtureSeeds(t), builderSeeds(t)...)
	return append(seeds, flippedSeeds(seeds)...)
}

// openErrOK says an Open error is one the contract lists.
func openErrOK(err error) bool {
	for _, e := range []error{
		sqlitedb.ErrNotSQLite, sqlitedb.ErrEncrypted, sqlitedb.ErrCorrupt, sqlitedb.ErrLiveUnavailable,
		sqlitedb.ErrEngineRefuses, sqlitedb.ErrBadFiles, sqlitefile.ErrLimit, sqlitefile.ErrPageUnavailable,
		parse.ErrBudget, context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// fuzzTyped fails on an error that is not typed; ErrInternal is a failure.
func fuzzTyped(t testing.TB, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if errors.Is(err, sqlitedb.ErrInternal) {
		t.Errorf("%s: internal error: %v", what, err)
		return
	}
	for _, e := range typedErrors {
		if errors.Is(err, e) {
			return
		}
	}
	if errors.Is(err, sqlitedb.ErrKeyUndecidable) || errors.Is(err, sqlitefile.ErrPageUnavailable) {
		return
	}
	t.Errorf("%s: untyped error %T: %v", what, err, err)
}

// rowSig is a comparable rendering of a row's rowid and every column.
func rowSig(r sqlitedb.Row) string {
	var sb strings.Builder
	id, ok := r.Rowid()
	fmt.Fprintf(&sb, "%d/%v", id, ok)
	for i := range r.NumCols() {
		v := r.Value(i)
		fmt.Fprintf(&sb, "|%v:%d:%d:%x:%x:%d:%d", r.State(i), v.Kind, v.Int, v.Float, v.Bytes, v.Len, v.Enc)
	}
	return sb.String()
}

type keyProbe struct {
	rowid int64
	key   parse.JoinKey
}

// fuzzRun is the body of the fuzz target. It returns the number of rows the
// scans delivered (for the seed test's non-vacuity check).
func fuzzRun(t testing.TB, db, wal, journal []byte, mode uint8) (scanned int) {
	t.Helper()
	if len(wal) == 0 {
		wal = nil
	}
	if len(journal) == 0 {
		journal = nil
	}
	b := &testBudget{limit: fuzzBudget}
	ctx, cancel := context.WithTimeout(t.Context(), fuzzTimeout)
	defer cancel()
	defer func() {
		if b.used != 0 {
			t.Errorf("budget used %d after Release", b.used)
		}
		if b.peak > b.limit {
			t.Errorf("budget peak %d exceeds the limit %d", b.peak, b.limit)
		}
	}()

	d, err := sqlitedb.Open(ctx, filesOf(db, wal, journal), b)
	if err != nil {
		if !openErrOK(err) {
			t.Errorf("Open: unexpected error %T: %v", err, err)
		}
		return 0
	}
	defer d.Release()
	_ = d.Info()
	names, err := d.Tables(ctx)
	fuzzTyped(t, "Tables", err)
	sizes := map[parse.FileRole]int64{parse.RoleDB: int64(len(db)), parse.RoleWAL: int64(len(wal)), parse.RoleJournal: int64(len(journal))}
	if len(names) > fuzzTables {
		names = names[:fuzzTables]
	}
	opened := map[string]*sqlitedb.Table{}
	for _, n := range names {
		tb, terr := d.Table(ctx, n, nil, nil)
		fuzzTyped(t, "Table "+n, terr)
		if terr != nil {
			continue
		}
		opened[n] = tb

		sigs := map[int64][]string{}
		var probes []keyProbe
		var clones []sqlitedb.Row
		col0 := -1
		if cols := tb.Cols(); len(cols) > 0 && !cols[0].Virtual {
			col0 = 0
		}
		cnt := 0
		warn0 := len(d.Warnings())
		serr := tb.Scan(ctx, func(r sqlitedb.Row) error {
			checkRowInvariants(t, r)
			if rg, role := r.Range(); r.Recovered() == nil {
				size, known := sizes[role]
				if !known || rg.Offset < 0 || rg.Length <= 0 || rg.Offset > size || rg.Length > size-rg.Offset {
					t.Errorf("%s: Range %+v in role %q lies outside its file (size %d, known %v)", n, rg, role, size, known)
				}
			}
			if id, ok := r.Rowid(); ok {
				sigs[id] = append(sigs[id], rowSig(r))
				if col0 >= 0 && len(probes) < fuzzKeyRows {
					if key, kok, kerr := sqlitedb.KeyOf(r, col0); kerr == nil && kok {
						probes = append(probes, keyProbe{id, key})
					} else if kerr != nil {
						fuzzTyped(t, "KeyOf "+n, kerr)
					}
				}
			}
			if len(clones) < fuzzCloneRows {
				c, cerr := r.Clone()
				fuzzTyped(t, "Clone "+n, cerr)
				if cerr == nil {
					clones = append(clones, c)
					if rowSig(c) != rowSig(r) {
						t.Errorf("%s: Clone differs from its source", n)
					}
					checkRowInvariants(t, c)
				}
			}
			cnt++
			if cnt >= fuzzScanRows {
				return sqlitedb.ErrStop
			}
			return nil
		})
		fuzzTyped(t, "Scan "+n, serr)
		cleanScan := serr == nil && len(d.Warnings()) == warn0
		scanned += cnt
		for _, c := range clones {
			c.Release()
		}

		if !tb.WithoutRowid() {
			for id := int64(1); id <= fuzzGetRows; id++ {
				r, ok, gerr := tb.Get(ctx, id)
				fuzzTyped(t, fmt.Sprintf("Get %s %d", n, id), gerr)
				_, scanned := sigs[id]
				if scanned && gerr == nil && !ok {
					t.Errorf("%s: Get(%d) answers not found for a row the scan delivered", n, id)
				}
				if scanned && cleanScan && gerr != nil {
					t.Errorf("%s: Get(%d) failed after a clean scan: %v", n, id, gerr)
				}
				if gerr != nil || !ok {
					continue
				}
				checkRowInvariants(t, r)
				if want, seen := sigs[id]; seen && !slices.Contains(want, rowSig(r)) {
					t.Errorf("%s: Get(%d) differs from the scanned row", n, id)
				}
				r.Release()
			}
		}

		if col0 >= 0 {
			ix, ierr := tb.Index(ctx, tb.Cols()[col0].Name, 20000)
			fuzzTyped(t, "Index "+n, ierr)
			if ierr == nil {
				for _, p := range probes {
					ids, flags, rerr := ix.Rowids(p.key)
					if flags != 0 {
						continue // the collations or classes differ: no containment promise
					}
					fuzzTyped(t, "Rowids "+n, rerr)
					if rerr != nil {
						t.Errorf("%s: Rowids of the key of row %d: %v", n, p.rowid, rerr)
					} else if !slices.Contains(ids, p.rowid) {
						t.Errorf("%s: Rowids of the key of row %d does not contain it: %v", n, p.rowid, ids)
					}
				}
				ix.Release()
			}
		}
	}

	if mode&1 != 0 {
		fuzzHistory(ctx, t, d, db, wal, journal, opened)
	}
	return scanned
}

// fuzzHistory pushes up to fuzzHistoryRows history rows of a second reader
// through FromRecovered.
func fuzzHistory(ctx context.Context, t testing.TB, d *sqlitedb.DB, db, wal, journal []byte, opened map[string]*sqlitedb.Table) {
	t.Helper()
	lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		libTyped(t, "history Open", err)
		return
	}
	if wal != nil {
		if _, err := lib.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			libTyped(t, "history AttachWAL", err)
			return
		}
	}
	if journal != nil {
		if _, err := lib.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
			libTyped(t, "history AttachJournal", err)
			return
		}
	}
	h := lib.History()
	defer h.Release()
	n := 0
	_, herr := h.Rows(ctx, func(rr sqlitefile.RecoveredRow) bool {
		n++
		if rr.Index == "" && rr.Table != "" {
			tb, ok := opened[rr.Table]
			if !ok {
				var terr error
				if tb, terr = d.Table(ctx, rr.Table, nil, nil); terr != nil {
					fuzzTyped(t, "history Table "+rr.Table, terr)
					tb = nil
				}
				opened[rr.Table] = tb
			}
			if tb != nil {
				r, ferr := sqlitedb.FromRecovered(tb, rr)
				if ferr != nil {
					if !errors.Is(ferr, sqlitedb.ErrRowMismatch) {
						fuzzTyped(t, "FromRecovered "+rr.Table, ferr)
					}
				} else {
					checkRecoveredInvariants(t, r)
					r.Release()
				}
			}
		}
		return n < fuzzHistoryRows
	})
	libTyped(t, "History Rows", herr)
}

// libTyped fails on a second-reader (library) error that is not one of the
// library's or the decoder's typed errors.
func libTyped(t testing.TB, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, e := range []error{
		sqlitefile.ErrNotSQLite, sqlitefile.ErrLooksEncrypted, sqlitefile.ErrCorrupt, sqlitefile.ErrBudget,
		sqlitefile.ErrLimit, sqlitefile.ErrPageUnavailable, sqlitefile.ErrEngineRefuses, sqlitefile.ErrLiveUnavailable,
		sqlitefile.ErrAlreadyAttached,
	} {
		if errors.Is(err, e) {
			return
		}
	}
	if errors.Is(err, sqlitefile.ErrInternal) {
		t.Errorf("%s: internal error: %v", what, err)
		return
	}
	fuzzTyped(t, what, err)
}

func FuzzSQLiteDB(f *testing.F) {
	for _, s := range allFuzzSeeds(f) {
		f.Add(s.db, s.wal, s.journal, s.mode)
	}
	f.Fuzz(func(t *testing.T, db, wal, journal []byte, mode uint8) {
		fuzzRun(t, db, wal, journal, mode)
	})
}

// TestFuzzSeedsOpenAndScan: every seed but the intentionally invalid ones
// opens, lists a table and scans cleanly, so the fuzz target is not vacuous.
func TestFuzzSeedsOpenAndScan(t *testing.T) {
	seeds := allFuzzSeeds(t)
	valid := 0
	for _, s := range seeds {
		if s.invalid {
			continue
		}
		valid++
		t.Run(s.name, func(t *testing.T) {
			d := openBytes(t, s.db, s.wal, s.journal, bigBudget())
			names, err := d.Tables(t.Context())
			if err != nil || len(names) == 0 {
				t.Fatalf("Tables = %v, %v", names, err)
			}
			rows := 0
			for _, n := range names {
				tb, err := d.Table(t.Context(), n, nil, nil)
				if err != nil {
					t.Fatalf("Table %s: %v", n, err)
				}
				if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { rows++; return nil }); err != nil {
					t.Fatalf("Scan %s: %v", n, err)
				}
			}
			if rows == 0 {
				t.Errorf("the seed scans no row")
			}
			if got := fuzzRun(t, s.db, s.wal, s.journal, s.mode); got == 0 {
				t.Errorf("the fuzz body scanned no row")
			}
		})
	}
	if valid < 12 {
		t.Errorf("only %d valid seeds", valid)
	}
}
