package sqlitedb_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// describe renders everything a Row answers, so two rows can be compared.
func describe(r sqlitedb.Row) string {
	var sb strings.Builder
	id, ok := r.Rowid()
	fmt.Fprintf(&sb, "%s rowid=%d,%v flags=%b extra=%d loc=%+v;", r.Table(), id, ok, r.Flags(), r.ExtraValues(), r.Loc())
	for c := range r.NumCols() {
		i, iok := r.Int(c)
		f, fok := r.Float(c)
		tx, tok := r.Text(c)
		bl, bok := r.Blob(c)
		rt, rtok := r.RawText(c)
		fmt.Fprintf(&sb, "[%d %v null=%v i=%d,%v f=%v,%v t=%q,%v b=%x,%v raw=%q,%v]", c, r.State(c), r.IsNull(c), i, iok, f, fok, tx, tok, bl, bok, rt, rtok)
	}
	return sb.String()
}

// stepTable is a table of 5000 rows in 512-byte pages (depth 3): a Get and a
// scan touch many different pages.
func stepTable(t testing.TB, opts sqlitetest.Options) (*sqlitetest.Builder, *sqlitetest.Table) {
	t.Helper()
	opts.PageSize = 512
	b := sqlitetest.New(opts)
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 5000; i++ {
		tb.Insert(i, i*3, fmt.Sprintf("row-%05d-%s", i, strings.Repeat("x", 40)))
	}
	if tb.Depth() < 3 {
		t.Fatalf("depth %d", tb.Depth())
	}
	return b, tb
}

func tableFrom(t testing.TB, data []byte, name string) *sqlitedb.Table {
	t.Helper()
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestGetFindsAndMissesCleanly(t *testing.T) {
	b, _ := stepTable(t, sqlitetest.Options{})
	tb := tableFrom(t, b.Bytes(), "t")
	want := map[int64]string{}
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		id, _ := r.Rowid()
		want[id] = describe(r)
	})
	for _, id := range []int64{1, 2, 77, 2500, 4999, 5000} {
		r, ok, err := tb.Get(t.Context(), id)
		if err != nil || !ok {
			t.Fatalf("Get(%d) = ok %v err %v", id, ok, err)
		}
		if got := describe(r); got != want[id] {
			t.Errorf("Get(%d) differs from the scan:\n got %s\nwant %s", id, got, want[id])
		}
		if r.Table() != "t" {
			t.Errorf("Table = %q", r.Table())
		}
	}
	for _, id := range []int64{0, -1, 5001, 1 << 40, -(1 << 40)} {
		r, ok, err := tb.Get(t.Context(), id)
		if err != nil || ok || r.NumCols() != 0 {
			t.Errorf("Get(%d) = %d cols, ok %v, err %v; want a clean miss", id, r.NumCols(), ok, err)
		}
	}
}

func TestGetOnDamagedPathIsAnErrorNotAbsent(t *testing.T) {
	b, tb := stepTable(t, sqlitetest.Options{})
	data := b.Bytes()
	// Swap the first two cell pointers of the interior root: its keys are no
	// longer in order, so no search through it can prove a rowid absent.
	root := data[int(tb.Root()-1)*512:][:512]
	if root[0] != 5 {
		t.Fatalf("root is not an interior table page (%d)", root[0])
	}
	root[12], root[13], root[14], root[15] = root[14], root[15], root[12], root[13]
	tt := tableFrom(t, data, "t")
	errs := 0
	for id := int64(1); id <= 5000; id++ {
		_, ok, err := tt.Get(t.Context(), id)
		switch {
		case err == nil && !ok:
			t.Fatalf("Get(%d): a clean absent for a rowid that exists", id)
		case err != nil:
			errs++
			if !errors.Is(err, sqlitedb.ErrCorrupt) {
				t.Fatalf("Get(%d): %v, want ErrCorrupt", id, err)
			}
		}
	}
	if errs == 0 {
		t.Error("no Get reported the damaged page")
	}
}

func TestGetWithoutRowidTable(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTableWithoutRowid("w", "create table w(a primary key, b) without rowid", 1).Insert(1, "k", "v")
	})
	tb := tableFrom(t, data, "w")
	_, ok, err := tb.Get(t.Context(), 1)
	if !errors.Is(err, sqlitedb.ErrWithoutRowid) || ok {
		t.Errorf("Get on WITHOUT ROWID = %v, %v", ok, err)
	}
}

func TestGetAfterReleaseIsErrReleased(t *testing.T) {
	b, _ := stepTable(t, sqlitetest.Options{})
	bud := bigBudget()
	d := openBytes(t, b.Bytes(), nil, nil, bud)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	if _, ok, err := tb.Get(t.Context(), 1); !errors.Is(err, sqlitedb.ErrReleased) || ok || bud.used != 0 {
		t.Errorf("Get after Release = %v, %v (used %d)", ok, err, bud.used)
	}
}

// Review Focus 1: a Get made inside a Scan callback must not disturb the
// outer row, whose bytes alias the reader's memory.
func TestGetInsideScanKeepsOuterRowValid(t *testing.T) {
	b, _ := stepTable(t, sqlitetest.Options{})
	tb := tableFrom(t, b.Bytes(), "t")
	n := 0
	err := tb.Scan(t.Context(), func(outer sqlitedb.Row) error {
		before := describe(outer)
		for _, id := range []int64{4000, 17, 2500, 4999} {
			inner, ok, err := tb.Get(t.Context(), id)
			if err != nil || !ok {
				return fmt.Errorf("inner Get(%d): %v %v", id, ok, err)
			}
			if got, _ := inner.Rowid(); got != id {
				return fmt.Errorf("inner Get(%d) returned rowid %d", id, got)
			}
		}
		if after := describe(outer); after != before {
			return fmt.Errorf("the outer row changed:\n before %s\n after  %s", before, after)
		}
		n++
		if n == 300 {
			return sqlitedb.ErrStop
		}
		return nil
	})
	if err != nil || n != 300 {
		t.Fatalf("n %d err %v", n, err)
	}
}

func TestCloneOutlivesScan(t *testing.T) {
	blob := bytes.Repeat([]byte{0xab, 0xcd}, 1500) // spills to overflow pages
	for _, enc := range []int{1, 2, 3} {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: enc}, func(b *sqlitetest.Builder) {
			tt := b.CreateTable("t", "create table t(a, b, c, d)")
			for i := int64(1); i <= 40; i++ {
				tt.Insert(i, i, fmt.Sprintf("héllo %d", i), blob, nil)
			}
		})
		d := openBytes(t, data, nil, nil, bigBudget())
		tb, err := d.Table(t.Context(), "t", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var originals []string
		var clones []sqlitedb.Row
		if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
			originals = append(originals, describe(r))
			c, err := r.Clone()
			if err != nil {
				return err
			}
			// Scribble over the reader's bytes: the clone must not alias them.
			if bl, ok := r.Blob(2); ok {
				clear(bl)
			}
			clones = append(clones, c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(clones) != 40 {
			t.Fatalf("enc %d: %d rows", enc, len(clones))
		}
		// Another scan after the first has ended: the clones are still intact.
		scanEach(t, tb, func(int, sqlitedb.Row) {})
		for i, c := range clones {
			if got := describe(c); got != originals[i] {
				t.Fatalf("enc %d row %d: the clone changed:\n got  %s\n want %s", enc, i, got, originals[i])
			}
			if bl, ok := c.Blob(2); !ok || !bytes.Equal(bl, blob) {
				t.Fatalf("enc %d row %d: blob lost (%d bytes, %v)", enc, i, len(bl), ok)
			}
			if tx, ok := c.Text(1); !ok || string(tx) != fmt.Sprintf("héllo %d", i+1) {
				t.Fatalf("enc %d row %d: Text(1) = %q, %v", enc, i, tx, ok)
			}
		}
	}
}

// TestCloneAnswersLikeTheOriginal: a clone answers exactly what the row it came from answered.
func TestCloneAnswersLikeTheOriginal(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, a, b, c, d default 'dflt', v as (1) virtual)")
		tt.Insert(1, nil, int64(7), 2.5, "text", []byte{9, 8})
		tt.Insert(2, nil, nil, nil, "", nil)
		tt.InsertRaw(3, []uint64{8, 1}, []byte{0, 0, 7}) // a length mismatch
	})
	tb := tableFrom(t, data, "t")
	n := 0
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		c, err := r.Clone()
		if err != nil {
			return err
		}
		if a, b := describe(r), describe(c); a != b {
			return fmt.Errorf("clone differs:\n row   %s\n clone %s", a, b)
		}
		if c.Flags() != r.Flags() || c.Loc().Offset != r.Loc().Offset {
			return errors.New("flags or Loc differ")
		}
		n++
		return nil
	}); err != nil || n != 3 {
		t.Fatalf("n %d err %v", n, err)
	}
	if z, err := (sqlitedb.Row{}).Clone(); err != nil || z.NumCols() != 0 || z.Table() != "" {
		t.Errorf("clone of the zero Row = %v, %v", z, err)
	}
}

func TestCloneIsChargedAndRefusedWhole(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a, b)").Insert(1, bytes.Repeat([]byte{1}, 3000), "tail")
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var row sqlitedb.Row
	var clone sqlitedb.Row
	var charged int64
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		row = r
		before := b.used
		var err error
		if clone, err = r.Clone(); err != nil {
			return err
		}
		charged = b.used - before
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := int64(2*sqlitedb.RowValueCost + 3000 + 4); charged < want {
		t.Errorf("a clone of 2 columns and 3004 value bytes was charged %d, want at least %d", charged, want)
	}
	if _, ok := clone.Blob(0); !ok {
		t.Fatal("clone lost its blob")
	}
	_ = row
	// The charge stays until the database is released, then is given back.
	if b.used < charged {
		t.Errorf("used %d fell below the clone's charge %d while the DB is open", b.used, charged)
	}
	d.Release()
	if b.used != 0 {
		t.Errorf("used %d after Release", b.used)
	}

	// A refusing budget: the clone fails with the budget's error and charges nothing.
	cb := &chargeBudget{testBudget: testBudget{limit: 1 << 40}}
	d2 := openBytes(t, data, nil, nil, cb)
	tb2, err := d2.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tb2.Scan(t.Context(), func(r sqlitedb.Row) error {
		cb.limit = cb.used // nothing more may be charged
		before := cb.used
		c, cerr := r.Clone()
		if !errors.Is(cerr, parse.ErrBudget) {
			t.Errorf("Clone under a full budget = %v", cerr)
		}
		if c.NumCols() != 0 || c.Table() != "" {
			t.Error("a refused Clone returned a partial copy")
		}
		if cb.used != before {
			t.Errorf("a refused Clone charged %d", cb.used-before)
		}
		cb.limit = 1 << 40
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCloneAfterReleaseIsErrReleased(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var row sqlitedb.Row
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		var err error
		row, err = r.Clone()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d.Release()
	if _, err := row.Clone(); !errors.Is(err, sqlitedb.ErrReleased) || b.used != 0 {
		t.Errorf("Clone after Release = %v (used %d)", err, b.used)
	}
	// The released row still answers.
	if v, ok := row.Int(0); v != 1 || !ok {
		t.Errorf("Int(0) = %d, %v", v, ok)
	}
}

func TestRangeCoversCellOnlyAndOverflowIsFlagged(t *testing.T) {
	const ps = 1024
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tt := b.CreateTable("t", "create table t(a, b)")
	blob := bytes.Repeat([]byte{7}, 5000)
	tt.Insert(1, int64(3), blob)
	tt.Insert(2, int64(4), "small")
	ov := tt.Overflow(1)
	if len(ov) < 4 {
		t.Fatalf("scenario needs an overflow chain, has %v", ov)
	}
	cell, page, off := tt.CellBytes(1)
	tb := tableFrom(t, b.Bytes(), "t")
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		rng, role := r.Range()
		if role != parse.RoleDB {
			t.Errorf("role %q", role)
		}
		id, _ := r.Rowid()
		if id != 1 {
			if r.Loc().OverflowTotal != 0 || len(r.Loc().Overflow) != 0 {
				t.Errorf("rowid %d: overflow listed", id)
			}
			return
		}
		if rng.Offset != int64(page-1)*ps+int64(off) || rng.Length != int64(len(cell)) {
			t.Errorf("Range = %+v, want the cell at %d, %d bytes", rng, int64(page-1)*ps+int64(off), len(cell))
		}
		if rng.Length >= int64(len(blob)) {
			t.Errorf("Range length %d spans the overflow value", rng.Length)
		}
		l := r.Loc()
		if l.OverflowTotal != len(ov) || len(l.Overflow) != len(ov) {
			t.Fatalf("overflow total %d listed %d, builder says %d", l.OverflowTotal, len(l.Overflow), len(ov))
		}
		for i, p := range l.Overflow {
			if p.Page != ov[i] {
				t.Errorf("overflow %d is page %d, builder says %d", i, p.Page, ov[i])
			}
			if p.At.File != sqlitefile.FileDB || p.At.Offset != int64(p.Page-1)*ps {
				t.Errorf("overflow %d lies at %+v", i, p.At)
			}
		}
		if r.Flags()&sqlitedb.FlagOverflowMixed != 0 {
			t.Error("overflow-mixed on a plain database")
		}
	})
}

func TestRangeOfZeroRowHasNoRole(t *testing.T) {
	rng, role := (sqlitedb.Row{}).Range()
	if role != "" || rng.Offset != 0 || rng.Length != 0 {
		t.Errorf("Range of the zero Row = %+v, %q", rng, role)
	}
}

func TestLocatorFormat(t *testing.T) {
	type tcase struct {
		name  string
		table string
		rowid int64
		want  string
	}
	long := strings.Repeat("é", 600)
	cases := []tcase{
		{"plain", "t", 5, "sqlite:table=t;rowid=5"},
		{"separators", "a b;c=d", 1, "sqlite:table=a%20b%3Bc%3Dd;rowid=1"},
		{"non-ascii", "é", 2, "sqlite:table=%C3%A9;rowid=2"},
		{"unreserved stay", "A-z_0.9~", 3, "sqlite:table=A-z_0.9~;rowid=3"},
		{"negative rowid", "t", -7, "sqlite:table=t;rowid=-7"},
		{"percent", "100%", 4, "sqlite:table=100%25;rowid=4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
				b.CreateTable(c.table, `create table "`+c.table+`"(a)`).Insert(c.rowid, int64(1))
			})
			tb := tableFrom(t, data, c.table)
			scanEach(t, tb, func(_ int, r sqlitedb.Row) {
				got, ok := r.Locator()
				if !ok || got != c.want {
					t.Errorf("Locator = %q, %v; want %q", got, ok, c.want)
				}
				if !strings.HasPrefix(got, "sqlite:") {
					t.Errorf("%q does not start with sqlite:", got)
				}
			})
		})
	}
	t.Run("a name over the 1024-byte limit has no locator", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
			b.CreateTable(long, `create table "`+long+`"(a)`).Insert(1, int64(1))
		})
		tb := tableFrom(t, data, long)
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if got, ok := r.Locator(); ok || got != "" {
				t.Errorf("Locator = %q, %v; want none for a %d-byte result", got, ok, len(got))
			}
			// the rest of the row is unaffected
			if id, ok := r.Rowid(); id != 1 || !ok {
				t.Errorf("Rowid = %d, %v", id, ok)
			}
		})
	})
	t.Run("the limit is 1024 bytes inclusive", func(t *testing.T) {
		// "sqlite:table=" (13) + name + ";rowid=1" (8): a name of 1003 letters fits exactly.
		for _, c := range []struct {
			n  int
			ok bool
		}{{1003, true}, {1004, false}} {
			name := strings.Repeat("n", c.n)
			data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
				b.CreateTable(name, `create table "`+name+`"(a)`).Insert(1, int64(1))
			})
			tb := tableFrom(t, data, name)
			scanEach(t, tb, func(_ int, r sqlitedb.Row) {
				got, ok := r.Locator()
				if ok != c.ok || (ok && len(got) != 1024) {
					t.Errorf("name of %d: Locator %d bytes, ok %v", c.n, len(got), ok)
				}
			})
		}
	})
	t.Run("WITHOUT ROWID has no locator", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
			b.CreateTableWithoutRowid("w", "create table w(a primary key, b) without rowid", 1).Insert(1, "k", "v")
		})
		tb := tableFrom(t, data, "w")
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if got, ok := r.Locator(); ok || got != "" {
				t.Errorf("Locator = %q, %v", got, ok)
			}
		})
	})
	t.Run("a recovered row has no locator", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
			b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
		})
		tb := tableFrom(t, data, "t")
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if _, ok := r.Locator(); !ok {
				t.Fatal("the live row has no locator")
			}
			if got, ok := sqlitedb.MarkRecovered(r).Locator(); ok || got != "" {
				t.Errorf("recovered Locator = %q, %v", got, ok)
			}
		})
	})
	t.Run("the zero Row has no locator", func(t *testing.T) {
		if got, ok := (sqlitedb.Row{}).Locator(); ok || got != "" {
			t.Errorf("Locator = %q, %v", got, ok)
		}
	})
}

func TestRowsAreDeterministic(t *testing.T) {
	scanAll := func(t *testing.T, data, wal, journal []byte, table string) []string {
		t.Helper()
		d := openBytes(t, data, wal, journal, bigBudget())
		tb, err := d.Table(t.Context(), table, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			loc, _ := r.Locator()
			rng, role := r.Range()
			out = append(out, describe(r)+loc+fmt.Sprint(rng, role))
		})
		return out
	}
	b, _ := stepTable(t, sqlitetest.Options{Encoding: 3})
	db := b.Bytes()
	first := scanAll(t, db, nil, nil, "t")
	if second := scanAll(t, db, nil, nil, "t"); !slices.Equal(first, second) {
		t.Error("two opens of the same bytes give different rows")
	}
	d := openBytes(t, db, nil, nil, bigBudget())
	tb, _ := d.Table(t.Context(), "t", nil, nil)
	var again []string
	for range 2 {
		again = again[:0]
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			loc, _ := r.Locator()
			rng, role := r.Range()
			again = append(again, describe(r)+loc+fmt.Sprint(rng, role))
		})
		if !slices.Equal(first, again) {
			t.Error("two scans of one table give different rows")
		}
	}
	wdb, wwal, _ := loadFixture(t, "wal-uncheckpointed")
	if !slices.Equal(scanAll(t, wdb, wwal, nil, "kv"), scanAll(t, wdb, wwal, nil, "kv")) {
		t.Error("the WAL fixture differs between opens")
	}
	hdb, _, hj := loadFixture(t, "hot-journal")
	if !slices.Equal(scanAll(t, hdb, nil, hj, "b"), scanAll(t, hdb, nil, hj, "b")) {
		t.Error("the journal fixture differs between opens")
	}
}

// A row from Get is owned: scribbling over its bytes changes nothing the next
// Get returns.
func TestGetRowIsOwned(t *testing.T) {
	b, _ := stepTable(t, sqlitetest.Options{})
	tb := tableFrom(t, b.Bytes(), "t")
	first, ok, err := tb.Get(t.Context(), 10)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := describe(first)
	txt, ok := first.Text(1)
	if !ok {
		t.Fatal("no text")
	}
	clear(txt)
	second, ok, err := tb.Get(t.Context(), 10)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := describe(second); got != want {
		t.Errorf("the second Get saw the first row's scribbling:\n got  %s\n want %s", got, want)
	}
}
