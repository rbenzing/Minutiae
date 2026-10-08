package sqlitedb_test

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// tableOf builds a database with one table t defined by sql, lets fill add
// rows, and returns the opened table.
func tableOf(t testing.TB, opts sqlitetest.Options, sql string, fill func(tb *sqlitetest.Table)) *sqlitedb.Table {
	t.Helper()
	if opts.PageSize == 0 {
		opts.PageSize = 1024
	}
	data := newBuilderDB(t, opts, func(b *sqlitetest.Builder) { fill(b.CreateTable("t", sql)) })
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// scanEach runs the scan and calls fn for every row; the scan must succeed and
// deliver at least one row.
func scanEach(t testing.TB, tb *sqlitedb.Table, fn func(i int, r sqlitedb.Row)) {
	t.Helper()
	i := 0
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		fn(i, r)
		i++
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if i == 0 {
		t.Fatal("Scan delivered no row")
	}
}

func TestColStateStringsAndKnown(t *testing.T) {
	all := []sqlitedb.ColState{
		sqlitedb.StatePresent, sqlitedb.StateNull, sqlitedb.StateAbsent, sqlitedb.StateDefaulted,
		sqlitedb.StateOmitted, sqlitedb.StateUnread, sqlitedb.StateClipped, sqlitedb.StateUndecodable, sqlitedb.StateLost,
	}
	names := []string{"present", "null", "absent", "defaulted", "omitted", "unread", "clipped", "undecodable", "lost"}
	known := map[sqlitedb.ColState]bool{sqlitedb.StatePresent: true, sqlitedb.StateNull: true, sqlitedb.StateDefaulted: true}
	for i, s := range all {
		if s.String() != names[i] || s.Known() != known[s] {
			t.Errorf("%d: String %q Known %v", i, s.String(), s.Known())
		}
	}
	if sqlitedb.ColState(200).String() == "" || sqlitedb.ColState(200).Known() {
		t.Error("an unknown state must not be known")
	}
}

func TestRowAccessorsByType(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b, c, d, e)", func(tt *sqlitetest.Table) {
		tt.Insert(1, int64(5), 2.5, "hi", []byte{1, 2}, nil)
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.Table() != "t" || r.NumCols() != 5 || r.Flags() != 0 || r.ExtraValues() != 0 || r.Recovered() != nil {
			t.Errorf("table %q cols %d flags %v extra %d rec %v", r.Table(), r.NumCols(), r.Flags(), r.ExtraValues(), r.Recovered())
		}
		if id, ok := r.Rowid(); id != 1 || !ok {
			t.Errorf("Rowid = %d, %v", id, ok)
		}
		if v, ok := r.Int(0); v != 5 || !ok {
			t.Errorf("Int(0) = %d, %v", v, ok)
		}
		if v, ok := r.Float(1); v != 2.5 || !ok {
			t.Errorf("Float(1) = %v, %v", v, ok)
		}
		if v, ok := r.Text(2); string(v) != "hi" || !ok {
			t.Errorf("Text(2) = %q, %v", v, ok)
		}
		if v, ok := r.RawText(2); string(v) != "hi" || !ok {
			t.Errorf("RawText(2) = %q, %v", v, ok)
		}
		if v, ok := r.Blob(3); string(v) != "\x01\x02" || !ok {
			t.Errorf("Blob(3) = %v, %v", v, ok)
		}
		if !r.IsNull(4) || r.State(4) != sqlitedb.StateNull {
			t.Errorf("column 4: IsNull %v state %v", r.IsNull(4), r.State(4))
		}
		// No coercion: only the matching kind answers.
		if _, ok := r.Int(1); ok {
			t.Error("Int of a real answered")
		}
		if _, ok := r.Float(0); ok {
			t.Error("Float of an integer answered")
		}
		if _, ok := r.Text(3); ok {
			t.Error("Text of a blob answered")
		}
		if _, ok := r.Blob(2); ok {
			t.Error("Blob of a text answered")
		}
		if _, ok := r.RawText(0); ok {
			t.Error("RawText of an integer answered")
		}
		if _, ok := r.Int(4); ok {
			t.Error("Int of NULL answered")
		}
		if r.IsNull(0) || r.IsNull(2) {
			t.Error("IsNull true for a value")
		}
		for col, want := range []sqlitefile.Kind{sqlitefile.KindInt, sqlitefile.KindFloat, sqlitefile.KindText, sqlitefile.KindBlob, sqlitefile.KindNull} {
			if r.Kind(col) != want {
				t.Errorf("Kind(%d) = %v, want %v", col, r.Kind(col), want)
			}
		}
		for col := 0; col < 4; col++ {
			if r.State(col) != sqlitedb.StatePresent || r.Unknown(col) {
				t.Errorf("column %d: state %v unknown %v", col, r.State(col), r.Unknown(col))
			}
		}
		if r.Value(0).Int != 5 || r.Value(2).Len != 2 {
			t.Errorf("Value = %+v / %+v", r.Value(0), r.Value(2))
		}
		rg, role := r.Range()
		if role != parse.RoleDB || rg.Offset <= 0 || rg.Length <= 0 || r.Loc().File != sqlitefile.FileDB {
			t.Errorf("Range = %+v %q, Loc %+v", rg, role, r.Loc())
		}
		if rg != (records.Range{Offset: r.Loc().Offset, Length: r.Loc().Length}) {
			t.Errorf("Range %+v differs from Loc %+v", rg, r.Loc())
		}
	})
}

func TestAbsentColumnReportsAbsentEverywhere(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, int64(5))
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		for _, col := range []int{-1, 1, 99, math.MaxInt32} {
			if r.State(col) != sqlitedb.StateAbsent || !r.Unknown(col) || r.IsNull(col) || r.Kind(col) != sqlitefile.KindNull {
				t.Errorf("col %d: state %v unknown %v null %v kind %v", col, r.State(col), r.Unknown(col), r.IsNull(col), r.Kind(col))
			}
			if _, ok := r.Int(col); ok {
				t.Errorf("Int(%d) answered", col)
			}
			if _, ok := r.Float(col); ok {
				t.Errorf("Float(%d) answered", col)
			}
			if _, ok := r.Text(col); ok {
				t.Errorf("Text(%d) answered", col)
			}
			if _, ok := r.Blob(col); ok {
				t.Errorf("Blob(%d) answered", col)
			}
			if _, ok := r.RawText(col); ok {
				t.Errorf("RawText(%d) answered", col)
			}
			if r.Value(col).Kind != sqlitefile.KindNull || r.Value(col).Len != 0 || r.Value(col).Int != 0 {
				t.Errorf("Value(%d) = %+v", col, r.Value(col))
			}
		}
	})
}

func TestRowidAliasReadsAsRowid(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(id integer primary key, a)", func(tt *sqlitetest.Table) {
		tt.Insert(-5, nil, "neg")
		tt.Insert(9, nil, "pos")
	})
	want := []int64{-5, 9}
	scanEach(t, tb, func(i int, r sqlitedb.Row) {
		if v, ok := r.Int(0); v != want[i] || !ok || r.State(0) != sqlitedb.StatePresent || r.IsNull(0) {
			t.Errorf("row %d: Int(0) = %d, %v state %v", i, v, ok, r.State(0))
		}
		if id, ok := r.Rowid(); id != want[i] || !ok {
			t.Errorf("Rowid = %d, %v", id, ok)
		}
	})
}

func TestRowidAliasOfShortRecordIsPresent(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(id integer primary key, a)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(7, nil, nil)
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if v, ok := r.Int(0); v != 7 || !ok || r.State(0) != sqlitedb.StatePresent {
			t.Errorf("Int(0) = %d, %v state %v", v, ok, r.State(0))
		}
		if r.State(1) != sqlitedb.StateDefaulted || !r.IsNull(1) {
			t.Errorf("state(1) = %v null %v", r.State(1), r.IsNull(1))
		}
	})
}

// shortRecord is a record of one stored int8 column, value 5.
func shortRecord(tt *sqlitetest.Table) { tt.InsertRaw(1, []uint64{1}, []byte{5}) }

func TestShortRecordsTakeColumnDefaults(t *testing.T) {
	const sql = "create table t(a, b text default 'dx', c integer default 7, d real default 1.5, e default null, f, g integer default '8')"
	for _, enc := range []int{1, 2, 3} {
		tb := tableOf(t, sqlitetest.Options{Encoding: enc}, sql, shortRecord)
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			if v, ok := r.Int(0); v != 5 || !ok || r.State(0) != sqlitedb.StatePresent {
				t.Errorf("enc %d: a = %d, %v", enc, v, ok)
			}
			if v, ok := r.Text(1); string(v) != "dx" || !ok || r.State(1) != sqlitedb.StateDefaulted {
				t.Errorf("enc %d: b = %q, %v state %v", enc, v, ok, r.State(1))
			}
			if v, ok := r.Int(2); v != 7 || !ok || r.State(2) != sqlitedb.StateDefaulted {
				t.Errorf("enc %d: c = %d, %v", enc, v, ok)
			}
			if v, ok := r.Float(3); v != 1.5 || !ok || r.State(3) != sqlitedb.StateDefaulted {
				t.Errorf("enc %d: d = %v, %v", enc, v, ok)
			}
			for _, col := range []int{4, 5} {
				if !r.IsNull(col) || r.State(col) != sqlitedb.StateDefaulted || r.Unknown(col) {
					t.Errorf("enc %d: col %d null %v state %v", enc, col, r.IsNull(col), r.State(col))
				}
			}
			if v, ok := r.Int(6); v != 8 || !ok {
				t.Errorf("enc %d: g (affinity applied to the default) = %d, %v", enc, v, ok)
			}
			if r.Flags()&sqlitedb.FlagShortRecord == 0 || r.Flags()&sqlitedb.FlagUnknownValues != 0 || r.ExtraValues() != 0 {
				t.Errorf("enc %d: flags %b extra %d", enc, r.Flags(), r.ExtraValues())
			}
		})
	}
}

func TestShortRecordNonLiteralDefaultIsOmittedNotZero(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b default (1+1), c default current_timestamp)", shortRecord)
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		for _, col := range []int{1, 2} {
			if r.State(col) != sqlitedb.StateOmitted || !r.Unknown(col) || r.IsNull(col) {
				t.Errorf("col %d: state %v null %v", col, r.State(col), r.IsNull(col))
			}
			if _, ok := r.Int(col); ok {
				t.Errorf("Int(%d) answered for an omitted default", col)
			}
			if _, ok := r.Text(col); ok {
				t.Errorf("Text(%d) answered for an omitted default", col)
			}
		}
		if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
			t.Errorf("flags %b lack unknown-values", r.Flags())
		}
	})
}

func TestExtraValuesAreFlagged(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1, 1, 1, 1}, []byte{1, 2, 3, 4})
		tt.InsertRaw(2, []uint64{1, 1}, []byte{1, 2})
	})
	scanEach(t, tb, func(i int, r sqlitedb.Row) {
		if r.NumCols() != 2 {
			t.Errorf("NumCols = %d", r.NumCols())
		}
		if i == 0 {
			if r.Flags()&sqlitedb.FlagExtraValues == 0 || r.ExtraValues() != 2 {
				t.Errorf("flags %b extra %d", r.Flags(), r.ExtraValues())
			}
			if r.State(2) != sqlitedb.StateAbsent {
				t.Errorf("an extra value is exposed: %v", r.State(2))
			}
			if v, _ := r.Int(1); v != 2 {
				t.Errorf("b = %d", v)
			}
		} else if r.Flags()&sqlitedb.FlagExtraValues != 0 || r.ExtraValues() != 0 {
			t.Errorf("a full record: flags %b extra %d", r.Flags(), r.ExtraValues())
		}
	})
}

func TestRecordLengthMismatchIsFlagged(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{0}, []byte{0}) // a NULL and a stray byte
		tt.Insert(2, "ok", int64(2))
	})
	scanEach(t, tb, func(i int, r sqlitedb.Row) {
		got := r.Flags()&sqlitedb.FlagLengthMismatch != 0
		if got != (i == 0) {
			t.Errorf("row %d: length-mismatch flag = %v", i, got)
		}
	})
}

func TestVirtualGeneratedColumnIsOmittedNotNull(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, v as (a+1) virtual, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1, 1}, []byte{10, 20})
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if x, _ := r.Int(0); x != 10 {
			t.Errorf("a = %d", x)
		}
		if x, _ := r.Int(2); x != 20 {
			t.Errorf("b = %d (the virtual column must not shift the stored ones)", x)
		}
		if r.State(1) != sqlitedb.StateOmitted || r.IsNull(1) || !r.Unknown(1) {
			t.Errorf("v: state %v null %v", r.State(1), r.IsNull(1))
		}
		if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
			t.Errorf("flags %b", r.Flags())
		}
	})
}

func TestOmittedOverCapValueIsOmittedWithLen(t *testing.T) {
	const n = 4<<20 + 1
	tb := tableOf(t, sqlitetest.Options{PageSize: 4096}, "create table t(a, b)", func(tt *sqlitetest.Table) {
		tt.Insert(1, strings.Repeat("x", n), int64(3))
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.State(0) != sqlitedb.StateOmitted || r.IsNull(0) {
			t.Errorf("state %v", r.State(0))
		}
		if _, ok := r.Text(0); ok {
			t.Error("Text answered for an omitted value")
		}
		if _, ok := r.RawText(0); ok {
			t.Error("RawText answered for an omitted value")
		}
		if v := r.Value(0); v.Len != n || v.Kind != sqlitefile.KindText || !v.Omitted {
			t.Errorf("Value = Len %d kind %v omitted %v", v.Len, v.Kind, v.Omitted)
		}
		if x, ok := r.Int(1); x != 3 || !ok {
			t.Errorf("b = %d, %v", x, ok)
		}
	})
}

func TestUnreadValueInDamagedOverflowChain(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tt := b.CreateTable("t", "create table t(a, b)")
	tt.Insert(1, strings.Repeat("x", 12000), int64(0))
	pages := tt.Overflow(1)
	data := b.Bytes()
	binary.BigEndian.PutUint32(data[int(pages[0]-1)*1024:], 0xffffffff) // the chain ends in nowhere
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.State(1) != sqlitedb.StateUnread || r.IsNull(1) || !r.Unknown(1) {
			t.Errorf("b: state %v null %v", r.State(1), r.IsNull(1))
		}
		if _, ok := r.Int(1); ok {
			t.Error("Int answered for an unread value")
		}
		if r.State(0).Known() {
			t.Errorf("a (cut chain): state %v", r.State(0))
		}
		if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
			t.Errorf("flags %b", r.Flags())
		}
	})
}

func TestUtf16UndecodableTextKeepsRawBytes(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{Encoding: 2}, "create table t(a, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{19, 17}, []byte{0x41, 0x00, 0x42, 0x00, 0xd8}) // odd length; a lone high surrogate
		tt.Insert(2, "ok", "é€")
	})
	scanEach(t, tb, func(i int, r sqlitedb.Row) {
		if i == 0 {
			if r.State(0) != sqlitedb.StateUndecodable || r.State(1) != sqlitedb.StateUndecodable || !r.Unknown(0) {
				t.Errorf("states %v %v", r.State(0), r.State(1))
			}
			if _, ok := r.Text(0); ok {
				t.Error("Text answered for undecodable text")
			}
			if v, ok := r.RawText(0); !ok || string(v) != "A\x00B" {
				t.Errorf("RawText(0) = %q, %v", v, ok)
			}
			if v, ok := r.RawText(1); !ok || string(v) != "\x00\xd8" {
				t.Errorf("RawText(1) = %q, %v", v, ok)
			}
			if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
				t.Errorf("flags %b", r.Flags())
			}
			return
		}
		if v, ok := r.Text(1); !ok || string(v) != "é€" || r.State(1) != sqlitedb.StatePresent {
			t.Errorf("Text(1) = %q, %v state %v", v, ok, r.State(1))
		}
		if v, ok := r.RawText(1); !ok || string(v) != "\xe9\x00\xac\x20" {
			t.Errorf("RawText(1) = %q (the stored UTF-16LE bytes)", v)
		}
	})
}

func TestInvalidUtf8SurvivesAsBytes(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{17}, []byte{0xff, 0xfe}) // text of two bytes
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if v, ok := r.Text(0); !ok || string(v) != "\xff\xfe" || r.State(0) != sqlitedb.StatePresent {
			t.Errorf("Text = %q, %v state %v", v, ok, r.State(0))
		}
	})
}

func TestRealPromotionAndNaN(t *testing.T) {
	nan := make([]byte, 8)
	binary.BigEndian.PutUint64(nan, math.Float64bits(math.NaN()))
	tb := tableOf(t, sqlitetest.Options{}, "create table t(r real, n)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1, 7}, append([]byte{3}, nan...))
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if v, ok := r.Float(0); v != 3 || !ok {
			t.Errorf("a stored integer in a REAL column = %v, %v", v, ok)
		}
		if _, ok := r.Int(0); ok {
			t.Error("Int answered for a promoted real")
		}
		// A stored NaN reads as NULL, as the engine reads it.
		if _, ok := r.Float(1); ok || !r.IsNull(1) || r.State(1) != sqlitedb.StateNull {
			t.Errorf("NaN: IsNull %v state %v", r.IsNull(1), r.State(1))
		}
	})
}

func TestRowIsAParseRow(t *testing.T) {
	var _ parse.Row = sqlitedb.Row{}
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, nil)
		tt.Insert(2, int64(1))
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		var pr parse.Row = r
		for col := -1; col < 3; col++ {
			st, ok := sqlitedb.StateOf(pr, col)
			if !ok || st != r.State(col) {
				t.Errorf("StateOf(%d) = %v, %v; State = %v", col, st, ok, r.State(col))
			}
		}
	})
	if _, ok := sqlitedb.StateOf(otherRow{}, 0); ok {
		t.Error("StateOf answered for a foreign row")
	}
	// The zero Row is safe to use.
	var z sqlitedb.Row
	if z.Table() != "" || z.NumCols() != 0 || z.State(0) != sqlitedb.StateAbsent || z.IsNull(0) {
		t.Error("zero Row misbehaves")
	}
	if _, ok := z.Rowid(); ok {
		t.Error("zero Row has a rowid")
	}
}

type otherRow struct{}

func (otherRow) Table() string                          { return "x" }
func (otherRow) Rowid() (int64, bool)                   { return 0, false }
func (otherRow) IsNull(int) bool                        { return false }
func (otherRow) Int(int) (int64, bool)                  { return 0, false }
func (otherRow) Float(int) (float64, bool)              { return 0, false }
func (otherRow) Text(int) ([]byte, bool)                { return nil, false }
func (otherRow) Blob(int) ([]byte, bool)                { return nil, false }
func (otherRow) Range() (records.Range, parse.FileRole) { return records.Range{}, parse.RoleDB }

func TestRowValueCostCoversAValue(t *testing.T) {
	if got := unsafe.Sizeof(sqlitefile.Value{}); got > sqlitedb.RowValueCost {
		t.Errorf("sizeof(Value) = %d is over RowValueCost %d", got, sqlitedb.RowValueCost)
	}
}

func TestOneExtraValueIsFlagged(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1, 1, 1}, []byte{1, 2, 3})
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.Flags()&sqlitedb.FlagExtraValues == 0 || r.ExtraValues() != 1 || r.Flags()&sqlitedb.FlagShortRecord != 0 {
			t.Errorf("flags %b extra %d", r.Flags(), r.ExtraValues())
		}
	})
}
