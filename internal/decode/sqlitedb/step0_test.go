package sqlitedb_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// B21: on a row whose record does not account for its bytes, a column the
// record does not hold is never given its default.
func TestLengthMismatchMissingColumnsAreUnreadNotDefaulted(t *testing.T) {
	const sql = "create table t(a, b default 3, c text default 'x')"
	for name, fill := range map[string]func(*sqlitetest.Table){
		"stray byte after one column":  func(tt *sqlitetest.Table) { tt.InsertRaw(1, []uint64{8}, []byte{0, 0}) },
		"header with no columns":       func(tt *sqlitetest.Table) { tt.InsertRaw(1, nil, []byte{9}) },
		"stray byte after two columns": func(tt *sqlitetest.Table) { tt.InsertRaw(1, []uint64{8, 1}, []byte{0, 4, 0}) },
	} {
		t.Run(name, func(t *testing.T) {
			tb := tableOf(t, sqlitetest.Options{}, sql, fill)
			scanEach(t, tb, func(_ int, r sqlitedb.Row) {
				if r.Flags()&sqlitedb.FlagLengthMismatch == 0 {
					t.Fatalf("flags %b lack length-mismatch", r.Flags())
				}
				unread := 0
				for col := range 3 {
					if r.State(col) == sqlitedb.StateDefaulted {
						t.Errorf("col %d is Defaulted on a length-mismatch row", col)
					}
					if r.State(col) == sqlitedb.StateUnread {
						unread++
						if !r.Unknown(col) || r.IsNull(col) {
							t.Errorf("col %d unread but unknown %v null %v", col, r.Unknown(col), r.IsNull(col))
						}
						if _, ok := r.Int(col); ok {
							t.Errorf("Int(%d) answered for an unread column", col)
						}
						if _, ok := r.Text(col); ok {
							t.Errorf("Text(%d) answered for an unread column", col)
						}
					}
				}
				if unread == 0 {
					t.Error("no column is Unread")
				}
				if r.Flags()&sqlitedb.FlagUnknownValues == 0 {
					t.Errorf("flags %b lack unknown-values", r.Flags())
				}
			})
		})
	}
}

// B21: columns that the damaged record does hold keep their values.
func TestLengthMismatchKeepsPresentColumns(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, b default 3)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1}, []byte{5, 0}) // a = 5, one stray byte
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if v, ok := r.Int(0); v != 5 || !ok || r.State(0) != sqlitedb.StatePresent {
			t.Errorf("a = %d, %v state %v", v, ok, r.State(0))
		}
		if r.State(1) != sqlitedb.StateUnread {
			t.Errorf("b state %v, want unread", r.State(1))
		}
	})
}

// B22: a virtual generated column has its own state and does not make the row
// "unknown"; a stored generated column reads normally.
func TestVirtualGeneratedColumnHasItsOwnState(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{}, "create table t(a, v as (a+1) virtual, s as (a+2) stored, b)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{1, 1, 1}, []byte{10, 12, 20})
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.State(1) != sqlitedb.StateGenerated || r.IsNull(1) || !r.Unknown(1) || r.State(1).Known() {
			t.Errorf("v: state %v null %v unknown %v", r.State(1), r.IsNull(1), r.Unknown(1))
		}
		if _, ok := r.Int(1); ok {
			t.Error("Int answered for a virtual generated column")
		}
		if x, ok := r.Int(2); x != 12 || !ok || r.State(2) != sqlitedb.StatePresent {
			t.Errorf("stored generated column: %d, %v, state %v", x, ok, r.State(2))
		}
		if x, _ := r.Int(3); x != 20 {
			t.Errorf("b = %d", x)
		}
		if r.Flags()&sqlitedb.FlagUnknownValues != 0 {
			t.Errorf("flags %b: a virtual generated column must not set unknown-values", r.Flags())
		}
	})
}

// B22: another unknown column still sets the flag next to a generated one.
func TestUnknownFlagStillSetBesideGeneratedColumn(t *testing.T) {
	tb := tableOf(t, sqlitetest.Options{Encoding: 2}, "create table t(a, v as (1) virtual)", func(tt *sqlitetest.Table) {
		tt.InsertRaw(1, []uint64{19}, []byte{0x41, 0x00, 0x42, 0x00, 0xd8})
	})
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		if r.State(0) != sqlitedb.StateUndecodable || r.Flags()&sqlitedb.FlagUnknownValues == 0 {
			t.Errorf("state %v flags %b", r.State(0), r.Flags())
		}
	})
}

// B25: a panic inside a Scan callback is recovered into ErrInternal and the
// charges are given back.
func TestScanRecoversCallbackPanic(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, "abc")
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanEach(t, tb, func(int, sqlitedb.Row) {}) // warm the page cache the view keeps
	base := b.used
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error { panic("boom") })
	if !errors.Is(err, sqlitedb.ErrInternal) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Scan = %v, want ErrInternal mentioning the panic", err)
	}
	if b.used != base {
		t.Errorf("budget used %d after the panic, want %d", b.used, base)
	}
	n := 0
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { n++; return nil }); err != nil || n != 1 {
		t.Errorf("Scan after a panic: n %d err %v", n, err)
	}
}

// panicBudget panics on its armed request.
type panicBudget struct {
	testBudget
	armAt, calls int
}

func (b *panicBudget) Alloc(n int64) error {
	b.calls++
	if b.calls == b.armAt {
		panic("budget exploded")
	}
	return b.testBudget.Alloc(n)
}

// B25: a panic in the budget during a scan is recovered as well.
func TestScanRecoversBudgetPanic(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, int64(1))
	})
	pb := &panicBudget{testBudget: testBudget{limit: 1 << 40}}
	d := openBytes(t, data, nil, nil, pb)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pb.armAt = pb.calls + 1
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil })
	if !errors.Is(err, sqlitedb.ErrInternal) || !strings.Contains(err.Error(), "budget exploded") {
		t.Fatalf("Scan = %v, want ErrInternal mentioning the panic", err)
	}
}

// B26: ErrStop has errors.Is semantics: a callback may wrap it, and the scan
// still ends with a nil result. Any other error is returned.
func TestScanStopMayBeWrapped(t *testing.T) {
	tb := bigTable(t)
	n := 0
	err := tb.Scan(t.Context(), func(sqlitedb.Row) error {
		n++
		return fmt.Errorf("enough: %w", sqlitedb.ErrStop)
	})
	if err != nil || n != 1 {
		t.Errorf("wrapped ErrStop: n %d err %v, want 1 and nil", n, err)
	}
	other := errors.New("other")
	err = tb.Scan(t.Context(), func(sqlitedb.Row) error { return fmt.Errorf("x: %w", other) })
	if !errors.Is(err, other) {
		t.Errorf("a different error was swallowed: %v", err)
	}
}

// raiseFirstRootKey adds delta to the first separator key of the interior
// root page of tb (one-byte varint, kept one byte) and returns the old key.
func raiseFirstRootKey(t *testing.T, b *sqlitetest.Builder, tb *sqlitetest.Table, delta byte) int64 {
	t.Helper()
	ps := b.PageSize()
	p := b.PageBytes(tb.Root())
	if p[0] != 5 {
		t.Fatalf("root page type %d is not an interior table page", p[0])
	}
	ptr := int(p[12])<<8 | int(p[13]) // the first cell pointer
	key := p[ptr+4]
	if key >= 0x80 || key+delta >= 0x80 {
		t.Fatalf("key byte %#x does not keep a one-byte encoding", key)
	}
	b.Patch(int(tb.Root()-1)*ps+ptr+4, key+delta)
	return int64(key)
}

// B27: a rowid outside the key range its position promises is flagged on a
// live row.
func TestScanFlagsKeyRangeViolation(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 100; i++ {
		tb.Insert(i, strings.Repeat("k", 60))
	}
	if tb.Depth() < 2 {
		t.Fatalf("depth %d", tb.Depth())
	}
	old := raiseFirstRootKey(t, b, tb, 3)
	d := openBytes(t, b.Bytes(), nil, nil, bigBudget())
	tt, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []int64
	total := 0
	if err := tt.Scan(t.Context(), func(r sqlitedb.Row) error {
		total++
		if r.Flags()&sqlitedb.FlagKeyRange != 0 {
			id, _ := r.Rowid()
			flagged = append(flagged, id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []int64{old + 1, old + 2, old + 3}
	if total != 100 || !slices.Equal(flagged, want) {
		t.Errorf("rows %d, flagged %v, want %v", total, flagged, want)
	}
}

// B27: overflow pages that lie in another file or frame than the cell set
// FlagOverflowMixed on a live row.
func TestScanFlagsOverflowMixed(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	blob := make([]byte, 5000)
	for i := range blob {
		blob[i] = byte(i * 31)
	}
	tb.Insert(1, int64(3), blob)
	tb.Insert(2, int64(6), []byte("x"))
	db := bytes.Clone(b.Bytes())
	db[18], db[19] = 2, 2
	s0 := b.Snapshot()
	w := b.NewWAL(false, 1, 2, 0)
	nb := bytes.Clone(blob)
	for i := 1100; i < len(nb); i++ { // only the overflow pages change
		nb[i] ^= 0x5a
	}
	tb.Update(1, int64(3), nb)
	b.CommitTo(w, s0)
	d := openBytes(t, db, w.Bytes(), nil, bigBudget())
	tt, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	scanEach(t, tt, func(_ int, r sqlitedb.Row) {
		id, _ := r.Rowid()
		mixed := r.Flags()&sqlitedb.FlagOverflowMixed != 0
		seen[id] = mixed
		if mixed != r.Loc().OverflowMixed {
			t.Errorf("rowid %d: flag %v and Loc.OverflowMixed %v disagree", id, mixed, r.Loc().OverflowMixed)
		}
	})
	if !seen[1] || seen[2] || len(seen) != 2 {
		t.Errorf("overflow-mixed by rowid: %v, want only row 1", seen)
	}
}

// B28: the per-column charge covers the value, the state byte and the slice
// header of the decoded text.
func TestRowValueCostCoversDecodedTextSlice(t *testing.T) {
	need := unsafe.Sizeof(sqlitefile.Value{}) + unsafe.Sizeof(sqlitedb.StateNull) + unsafe.Sizeof([]byte(nil))
	if need > sqlitedb.RowValueCost {
		t.Errorf("value + state + text slice = %d is over RowValueCost %d", need, sqlitedb.RowValueCost)
	}
}

// B28: empty UTF-16 text is an answered, empty value, not a missing one.
func TestEmptyUtf16TextIsAnsweredNotMissing(t *testing.T) {
	for _, enc := range []int{2, 3} {
		tb := tableOf(t, sqlitetest.Options{Encoding: enc}, "create table t(a, b text default '')", func(tt *sqlitetest.Table) {
			tt.Insert(1, "", "")
			tt.InsertRaw(2, []uint64{13}, nil) // an empty text, then a short record: b is defaulted
		})
		scanEach(t, tb, func(_ int, r sqlitedb.Row) {
			for col := range 2 {
				v, ok := r.Text(col)
				if !ok || v == nil || len(v) != 0 {
					t.Errorf("enc %d: Text(%d) = %#v, %v, want an empty non-nil slice", enc, col, v, ok)
				}
			}
		})
	}
}

// B25: the library already turns a panic inside its walk into an error, so the
// recovery in Scan itself is reached by a panic outside the walk: a Table that
// was not made by DB.Table.
func TestScanOnZeroTableIsErrInternalNotAPanic(t *testing.T) {
	var tb sqlitedb.Table
	err := tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil })
	if !errors.Is(err, sqlitedb.ErrInternal) {
		t.Fatalf("Scan on a zero Table = %v, want ErrInternal", err)
	}
}
