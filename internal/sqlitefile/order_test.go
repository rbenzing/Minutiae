package sqlitefile_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func TestOrderValuesUndecidedCases(t *testing.T) {
	badUTF8 := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{0xff, 'a'}, Len: 2, Enc: sqlitefile.EncUTF8}
	lone := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{0x00, 0xd8}, Len: 2, Enc: sqlitefile.EncUTF16LE}
	odd := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{'a', 0, 'b'}, Len: 3, Enc: sqlitefile.EncUTF16LE}
	omitted := sqlitefile.Value{Kind: sqlitefile.KindText, Len: 5, Omitted: true}
	unread := sqlitefile.Value{Kind: sqlitefile.KindInt, Unread: true}
	clipped := sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte("ab"), Len: 9, Clipped: true}
	unknown := sqlitefile.Value{Kind: sqlitefile.Kind(99)}
	one := evInt(1)
	for _, tc := range []struct {
		name string
		a, b sqlitefile.Value
		coll string
	}{
		{"custom collation", evText("a"), evText("b"), "FANCY"},
		{"omitted", omitted, evText("a"), ""},
		{"omitted second", evText("a"), omitted, ""},
		{"unread", unread, one, ""},
		{"clipped", clipped, evBlob("ab"), ""},
		{"unknown kind", unknown, one, ""},
		{"unknown kind second", one, unknown, ""},
		{"invalid utf8 nocase", badUTF8, evText("a"), "NOCASE"},
		{"invalid utf8 rtrim", evText("a"), badUTF8, "RTRIM"},
		{"unpaired utf16 nocase", lone, evUTF16(true, "a"), "NOCASE"},
		{"unpaired utf16 rtrim", evUTF16(true, "a"), lone, "RTRIM"},
		{"odd utf16 nocase", odd, odd, "NOCASE"},
		{"utf16 against utf8 binary", evUTF16(true, "a"), evText("a"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c, st := sqlitefile.OrderValues(tc.a, tc.b, tc.coll); st != sqlitefile.OrderUndecided || c != 0 {
				t.Errorf("OrderValues = %d, %v; want 0, OrderUndecided", c, st)
			}
		})
	}
}

func TestOrderKeysCompoundAndDesc(t *testing.T) {
	cols := []sqlitefile.KeyColumn{{}, {Desc: true}, {Collation: "nocase"}}
	k := func(a int64, b int64, s string) []sqlitefile.Value {
		return []sqlitefile.Value{evInt(a), evInt(b), evText(s)}
	}
	for _, tc := range []struct {
		name string
		a, b []sqlitefile.Value
		c, n int
	}{
		{"equal", k(1, 2, "x"), k(1, 2, "X"), 0, 3},
		{"first column", k(1, 2, "x"), k(2, 2, "x"), -1, 0},
		{"second column is desc", k(1, 2, "x"), k(1, 3, "x"), 1, 1},
		{"third column nocase", k(1, 2, "a"), k(1, 2, "B"), -1, 2},
		{"first decides before the rest", k(2, 9, "z"), k(1, 0, "a"), 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n, st := sqlitefile.OrderKeys(tc.a, tc.b, cols)
			if st != sqlitefile.OrderOK || c != tc.c || n != tc.n {
				t.Errorf("OrderKeys = %d, %d, %v; want %d, %d, OrderOK", c, n, st, tc.c, tc.n)
			}
			c2, n2, _ := sqlitefile.OrderKeys(tc.b, tc.a, cols)
			if c2 != -tc.c || n2 != tc.n {
				t.Errorf("reversed: %d, %d", c2, n2)
			}
		})
	}
	// DESC inverts NULLs too: NULL sorts last in a descending column.
	if c, n, st := sqlitefile.OrderKeys([]sqlitefile.Value{{}}, []sqlitefile.Value{evInt(0)}, []sqlitefile.KeyColumn{{Desc: true}}); c != 1 || n != 0 || st != sqlitefile.OrderOK {
		t.Errorf("NULL against 0 under DESC = %d, %d, %v; want 1, 0, OrderOK", c, n, st)
	}
	// A key shorter than the columns is undecided, on either side.
	if _, _, st := sqlitefile.OrderKeys(k(1, 2, "x")[:2], k(1, 2, "x"), cols); st != sqlitefile.OrderUndecided {
		t.Errorf("short a: %v", st)
	}
	if _, _, st := sqlitefile.OrderKeys(k(1, 2, "x"), k(1, 2, "x")[:1], cols); st != sqlitefile.OrderUndecided {
		t.Errorf("short b: %v", st)
	}
	// Values past the columns are ignored.
	if c, n, st := sqlitefile.OrderKeys(k(1, 2, "x"), k(1, 2, "y"), cols[:2]); c != 0 || n != 2 || st != sqlitefile.OrderOK {
		t.Errorf("prefix compare = %d, %d, %v; want 0, 2, OrderOK", c, n, st)
	}
	// An undecided column after a decided difference does not matter; before it, it does.
	om := sqlitefile.Value{Kind: sqlitefile.KindInt, Omitted: true}
	if c, n, st := sqlitefile.OrderKeys([]sqlitefile.Value{evInt(1), om}, []sqlitefile.Value{evInt(2), om}, cols[:2]); c != -1 || n != 0 || st != sqlitefile.OrderOK {
		t.Errorf("decided first column = %d, %d, %v", c, n, st)
	}
	if _, n, st := sqlitefile.OrderKeys([]sqlitefile.Value{evInt(1), om}, []sqlitefile.Value{evInt(1), evInt(2)}, cols[:2]); st != sqlitefile.OrderUndecided || n != 1 {
		t.Errorf("undecided second column: n=%d, %v; want n=1, OrderUndecided", n, st)
	}
}

// PASSES ON ARRIVAL against the stub, which returns at once. Mutation: clone the
// bytes in the NOCASE path with slices.Clone and see it fail.
func TestOrderValuesIsBoundedForHugeKeys(t *testing.T) {
	const size = 4 << 20
	mk := func(last byte) sqlitefile.Value {
		b := bytes.Repeat([]byte("a"), size)
		b[size-1] = last
		return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: size, Enc: sqlitefile.EncUTF8}
	}
	a, b := mk('a'), mk('b')
	for _, coll := range []string{"", "BINARY", "NOCASE", "RTRIM"} {
		if c, st := sqlitefile.OrderValues(a, b, coll); st != sqlitefile.OrderOK || c != -1 {
			t.Fatalf("%q: OrderValues = %d, %v; want -1, OrderOK", coll, c, st)
		}
	}
	for _, coll := range []string{"", "NOCASE"} {
		if n := testing.AllocsPerRun(5, func() { sqlitefile.OrderValues(a, b, coll) }); n != 0 {
			t.Errorf("%q: %v allocations per comparison, want 0", coll, n)
		}
	}
}

func TestOrderValuesNaNIsNull(t *testing.T) {
	nan := evFloat(math.NaN())
	null := sqlitefile.Value{}
	others := []sqlitefile.Value{null, evInt(math.MinInt64), evInt(0), evFloat(-1e300), evFloat(0), evText(""), evText("a"), evBlob(""), evBlob("a")}
	if c, st := sqlitefile.OrderValues(nan, nan, ""); c != 0 || st != sqlitefile.OrderOK {
		t.Errorf("NaN against NaN = %d, %v; want 0, OrderOK", c, st)
	}
	for _, o := range others {
		cn, sn := sqlitefile.OrderValues(nan, o, "")
		cz, sz := sqlitefile.OrderValues(null, o, "")
		if cn != cz || sn != sz || sn != sqlitefile.OrderOK {
			t.Errorf("NaN against %+v: %d, %v; NULL gives %d, %v", o, cn, sn, cz, sz)
		}
		rn, _ := sqlitefile.OrderValues(o, nan, "")
		if rn != -cn {
			t.Errorf("%+v against NaN: %d, want %d", o, rn, -cn)
		}
	}
	if c, _ := sqlitefile.OrderValues(nan, evInt(math.MinInt64), ""); c != -1 {
		t.Errorf("NaN against MinInt64 = %d, want -1", c)
	}
}
