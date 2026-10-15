package sqlitedb_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// smallTable is a database of one table with n rows of text.
func smallTable(t testing.TB, n int) []byte {
	return newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= int64(n); i++ {
			tt.Insert(i, i, strings.Repeat("x", 30))
		}
	})
}

func TestBudgetExhaustedDuringOpenIsParseErrBudget(t *testing.T) {
	data := smallTable(t, 50)
	tb := &testBudget{limit: 1}
	_, err := sqlitedb.Open(t.Context(), filesOf(data, nil, nil), tb)
	if !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("Open under a 1-byte testBudget = %v, want parse.ErrBudget", err)
	}
	if tb.used != 0 {
		t.Errorf("a failed Open left %d bytes charged", tb.used)
	}
	host := parse.NewBudget(1)
	_, err = sqlitedb.Open(t.Context(), filesOf(data, nil, nil), host.View())
	if !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("Open under the real 1-byte budget = %v, want parse.ErrBudget", err)
	}
	if host.Used() != 0 {
		t.Errorf("a failed Open left %d bytes charged to the host", host.Used())
	}
}

func TestBudgetExhaustedDuringScanIsAnErrorNotATruncatedScan(t *testing.T) {
	data := smallTable(t, 200)
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {}) // warm: the library's pages are charged already
	base := b.used
	n := 0
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error {
		n++
		if n == 3 {
			b.limit = base // the next row cannot be charged
		}
		return nil
	})
	if !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("Scan = %v after %d rows, want parse.ErrBudget (a short scan must not look complete)", err, n)
	}
	if n != 3 {
		t.Errorf("%d rows delivered, want 3", n)
	}
	b.limit = 1 << 40
	// The refusal kept nothing: a fresh scan sees all 200 rows.
	count := 0
	scanEach(t, tb, func(int, sqlitedb.Row) { count++ })
	if count != 200 {
		t.Errorf("a later scan saw %d rows", count)
	}
}

// The decoder's own per-row charge (2 columns x RowValueCost) is refused while
// every charge of the library succeeds: the scan must end with ErrBudget, not
// look complete, and the callback must not run for the refused row.
func TestDecoderRowChargeRefusalIsAnErrorNotATruncatedScan(t *testing.T) {
	data := smallTable(t, 200)
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {}) // warm
	n := 0
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error {
		n++
		if n == 3 {
			b.refuse = 2 * sqlitedb.RowValueCost
		}
		return nil
	})
	if !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("Scan = %v after %d rows, want parse.ErrBudget (a truncated scan must not look complete)", err, n)
	}
	if n != 3 {
		t.Errorf("%d rows delivered, want 3", n)
	}
	b.refuse = 0
	count := 0
	scanEach(t, tb, func(int, sqlitedb.Row) { count++ })
	if count != 200 {
		t.Errorf("a later scan saw %d rows", count)
	}
}

func TestBudgetReturnsToZeroAfterRelease(t *testing.T) {
	data := smallTable(t, 50)
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	var rows []sqlitedb.Row
	g, ok, err := tb.Get(t.Context(), 7)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	rows = append(rows, g)
	n := 0
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		if n++; n > 10 {
			return sqlitedb.ErrStop
		}
		c, err := r.Clone()
		rows = append(rows, c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 11 {
		t.Fatalf("%d rows kept", len(rows))
	}
	if b.peak == 0 {
		t.Error("peak is 0: nothing was ever charged")
	}
	// Row.Release in any order, twice each, returns the rows' share.
	mid := b.used
	for i := len(rows) - 1; i >= 0; i-- {
		rows[i].Release()
		rows[i].Release()
	}
	if b.used >= mid {
		t.Errorf("releasing 11 rows returned nothing (%d -> %d)", mid, b.used)
	}
	d.Release()
	if b.used != 0 {
		t.Errorf("used = %d after Release, want 0", b.used)
	}
	// And again with the rows held until the DB is released.
	b2 := bigBudget()
	d2 := openBytes(t, data, nil, nil, b2)
	tb2, err := d2.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 10; id++ {
		r, _, err := tb2.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Clone(); err != nil {
			t.Fatal(err)
		}
	}
	d2.Release()
	if b2.used != 0 || b2.peak == 0 {
		t.Errorf("used %d peak %d after Release with rows held", b2.used, b2.peak)
	}
}

func TestScanChargesPerRowAndFreesAfterCallback(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		for i := int64(1); i <= 60; i++ {
			tt.Insert(i, strings.Repeat("y", 100), i)
		}
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {}) // the library's page cache is warm
	before := b.used
	var during []int64
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		during = append(during, b.used)
		_, _ = r.Text(0)
	})
	for i, u := range during {
		if u <= before {
			t.Fatalf("row %d: nothing charged while the callback ran (%d, base %d)", i, u, before)
		}
		// A row's charge varies with its data; a charge that is not freed
		// after its callback would add up to many times one row's.
		if u-before > 4*(during[0]-before) {
			t.Fatalf("row %d: %d bytes held over the base, row 0 held %d: the charge of a row was not freed after its callback", i, u-before, during[0]-before)
		}
	}
	if b.used != before {
		t.Errorf("used %d after the scan, want %d", b.used, before)
	}
}

func TestHugeColumnCountTable(t *testing.T) {
	cols := func(n int) string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("c%d", i)
		}
		return strings.Join(names, ",")
	}
	t.Run("1999 columns scan", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096}, func(b *sqlitetest.Builder) {
			vals := make([]any, 1999)
			for i := range vals {
				vals[i] = int64(i)
			}
			b.CreateTable("w", "create table w("+cols(1999)+")").Insert(1, vals...)
		})
		tb := tableFrom(t, data, "w")
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if r.NumCols() != 1999 {
				t.Errorf("NumCols = %d", r.NumCols())
			}
			if v, ok := r.Int(1998); !ok || v != 1998 {
				t.Errorf("Int(1998) = %d, %v", v, ok)
			}
		})
	})
	t.Run("2001 columns are unsupported", func(t *testing.T) {
		data := newBuilderDB(t, sqlitetest.Options{PageSize: 4096}, func(b *sqlitetest.Builder) {
			b.CreateTable("w", "create table w("+cols(2001)+")").Insert(1, int64(1))
		})
		d := openBytes(t, data, nil, nil, bigBudget())
		_, err := d.Table(t.Context(), "w", nil, nil)
		var us *sqlitedb.UnsupportedSchemaError
		if !errors.As(err, &us) || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
			t.Fatalf("Table = %v, want an *UnsupportedSchemaError", err)
		}
	})
}

func TestOpenOfHostileSizesNeverAllocatesBeyondBudget(t *testing.T) {
	const limit = 64 << 20
	base := smallTable(t, 300)
	mutate := func(f func(d []byte)) []byte {
		d := bytes.Clone(base)
		f(d)
		return d
	}
	cases := map[string][]byte{
		"header claims 2^32-1 pages": mutate(func(d []byte) { binary.BigEndian.PutUint32(d[28:], 0xFFFFFFFF) }),
		"freelist claims 2^32-1 pages": mutate(func(d []byte) {
			binary.BigEndian.PutUint32(d[32:], 2)
			binary.BigEndian.PutUint32(d[36:], 0xFFFFFFFF)
		}),
		"page size 65536 in a small file": mutate(func(d []byte) { binary.BigEndian.PutUint16(d[16:], 1) }),
		"page size 512 on 1024 pages":     mutate(func(d []byte) { binary.BigEndian.PutUint16(d[16:], 512) }),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			b := &testBudget{limit: limit}
			d, err := sqlitedb.Open(t.Context(), filesOf(data, nil, nil), b)
			if err == nil {
				for _, n := range []string{"t"} {
					if tb, terr := d.Table(t.Context(), n, nil, nil); terr == nil {
						_ = tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil })
						_, _, _ = tb.Get(t.Context(), 5)
					}
				}
				d.Release()
			}
			if b.peak >= limit {
				t.Errorf("peak %d reached the %d budget", b.peak, limit)
			}
			if b.used != 0 {
				t.Errorf("used %d after the work", b.used)
			}
		})
	}
}

// armedReader panics on every read once armed.
type armedReader struct {
	r     *bytes.Reader
	armed bool
}

func (a *armedReader) ReadAt(p []byte, off int64) (int, error) {
	if a.armed {
		panic("reader exploded")
	}
	return a.r.ReadAt(p, off)
}

func TestPanicInReaderBecomesErrInternal(t *testing.T) {
	data := smallTable(t, 400)
	files := func(r *armedReader) sqlitedb.Files {
		return sqlitedb.Files{DB: r, DBSize: int64(len(data))}
	}
	t.Run("during Open", func(t *testing.T) {
		b := bigBudget()
		_, err := sqlitedb.Open(t.Context(), files(&armedReader{r: bytes.NewReader(data), armed: true}), b)
		if !errors.Is(err, sqlitedb.ErrInternal) {
			t.Fatalf("Open = %v, want ErrInternal", err)
		}
		if b.used != 0 {
			t.Errorf("used %d after a panicking Open", b.used)
		}
	})
	t.Run("during Scan and Get", func(t *testing.T) {
		r := &armedReader{r: bytes.NewReader(data)}
		b := bigBudget()
		d, err := sqlitedb.Open(t.Context(), files(r), b)
		if err != nil {
			t.Fatal(err)
		}
		tb, err := d.Table(t.Context(), "t", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		n := 0
		err = tb.Scan(t.Context(), func(sqlitedb.Row) error { n++; return nil })
		if !errors.Is(err, sqlitedb.ErrInternal) {
			t.Errorf("Scan = %v after %d rows, want ErrInternal", err, n)
		}
		if _, ok, err := tb.Get(t.Context(), 399); ok || !errors.Is(err, sqlitedb.ErrInternal) {
			t.Errorf("Get = ok %v, %v, want ErrInternal", ok, err)
		}
		r.armed = false
		d.Release()
		if b.used != 0 {
			t.Errorf("used %d after the panics and Release", b.used)
		}
	})
}

func TestScanCallbackPanicIsNotSwallowed(t *testing.T) {
	b := bigBudget()
	d := openBytes(t, smallTable(t, 50), nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {})
	base := b.used
	n := 0
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error {
		if n++; n == 4 {
			panic("callback exploded")
		}
		return nil
	})
	if !errors.Is(err, sqlitedb.ErrInternal) || !strings.Contains(err.Error(), "callback exploded") {
		t.Fatalf("Scan = %v, want ErrInternal naming the panic", err)
	}
	if n != 4 {
		t.Errorf("%d callbacks ran, want the scan to stop at the panic", n)
	}
	if b.used != base {
		t.Errorf("used %d after the panic, want %d", b.used, base)
	}
	count := 0
	scanEach(t, tb, func(int, sqlitedb.Row) { count++ })
	if count != 50 {
		t.Errorf("a later scan saw %d rows", count)
	}
}
