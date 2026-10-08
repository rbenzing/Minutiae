package sqlitefile_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func vtU16(b []byte) int    { return int(binary.BigEndian.Uint16(b)) }
func vtU32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// vtVarint decodes a varint (the test's own copy of the format rule).
func vtVarint(b []byte) (v uint64, n int) {
	for i := 0; i < 8; i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v<<8 | uint64(b[8]), 9
}

// genRow returns the declared columns of row i of the standard test table:
// every value kind, and an overflow blob on every 97th row.
func genRow(i int64) []any {
	var a any
	switch i % 7 {
	case 0:
		a = i * 1000003
	case 1:
		a = float64(i) + 0.25
	case 2:
		a = fmt.Sprintf("text-%d-é", i)
	case 3:
		a = bytes.Repeat([]byte{byte(i), 0, 0xff}, int(i%9))
	case 4:
		a = nil
	case 5:
		a = -i
	default:
		a = i % 2 // 0 and 1 use the constant serial types
	}
	var e any = []byte(nil)
	if i%97 == 0 {
		e = bytes.Repeat([]byte{byte(i), 0x5a}, 1500)
	}
	return []any{a, i * 3, fmt.Sprintf("row-%d", i), bytes.Repeat([]byte{byte(i)}, int(i%50)), e}
}

const stdTableSQL = "create table t(a, b, c, d, e)"

// stdTable builds the standard table with rows 1..n.
func stdTable(t *testing.T, o sqlitetest.Options, n int) (*sqlitetest.Builder, *sqlitetest.Table) {
	t.Helper()
	b := sqlitetest.New(o)
	tb := b.CreateTable("t", stdTableSQL)
	for i := int64(1); i <= int64(n); i++ {
		tb.Insert(i, genRow(i)...)
	}
	return b, tb
}

func valueIs(v sqlitefile.Value, want any) bool {
	switch w := want.(type) {
	case nil:
		return v.Kind == sqlitefile.KindNull
	case int64:
		return v.Kind == sqlitefile.KindInt && v.Int == w
	case int:
		return v.Kind == sqlitefile.KindInt && v.Int == int64(w)
	case float64:
		return v.Kind == sqlitefile.KindFloat && v.Float == w
	case string:
		s, ok := v.Text()
		return v.Kind == sqlitefile.KindText && ok && s == w
	case []byte:
		return v.Kind == sqlitefile.KindBlob && !v.Omitted && bytes.Equal(v.Bytes, w)
	}
	return false
}

func rowIs(r sqlitefile.Row, want []any) error {
	if len(r.Values) != len(want) {
		return fmt.Errorf("%d values, want %d", len(r.Values), len(want))
	}
	for i, w := range want {
		if !valueIs(r.Values[i], w) {
			return fmt.Errorf("column %d = %+v, want %v", i, r.Values[i], w)
		}
	}
	return nil
}

func openLive(t testing.TB, data []byte, o sqlitefile.Options) (*sqlitefile.DB, *sqlitefile.View) {
	t.Helper()
	db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, db.Live()
}

// scanRows scans the tree and returns owned copies of the rows.
func scanRows(t testing.TB, v *sqlitefile.View, root uint32, kind sqlitefile.BTreeKind) []sqlitefile.Row {
	t.Helper()
	var out []sqlitefile.Row
	err := v.ScanTree(context.Background(), root, kind, func(r sqlitefile.Row) bool {
		out = append(out, r.Clone())
		return true
	})
	if err != nil {
		t.Fatalf("ScanTree: %v", err)
	}
	return out
}

func rowids(rows []sqlitefile.Row) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.Rowid
	}
	return out
}

func viewWarns(v *sqlitefile.View, code string, page uint32) bool {
	for _, w := range v.Warnings() {
		if w.Code == code && (page == 0 || w.Page == page) {
			return true
		}
	}
	return false
}

func warnCodesOf(v *sqlitefile.View) []string {
	var out []string
	for _, w := range v.Warnings() {
		out = append(out, w.Code)
	}
	return out
}

// cellOffsets returns the cell pointer array of a non-first page.
func cellOffsets(p []byte) []int {
	hdr := 8
	if p[0] == 0x02 || p[0] == 0x05 {
		hdr = 12
	}
	n := vtU16(p[3:])
	out := make([]int, n)
	for i := range out {
		out[i] = vtU16(p[hdr+2*i:])
	}
	return out
}

// pageAt returns the live slice of page pgno of data.
func pageAt(data []byte, ps int, pgno uint32) []byte {
	return data[(int(pgno)-1)*ps : int(pgno)*ps]
}

// setChild points cell idx of interior page pgno at child.
func setChild(data []byte, ps int, pgno uint32, idx int, child uint32) {
	p := pageAt(data, ps, pgno)
	binary.BigEndian.PutUint32(p[cellOffsets(p)[idx]:], child)
}

// cellKey is the rowid key of interior table cell idx.
func cellKey(data []byte, ps int, pgno uint32, idx int) int64 {
	p := pageAt(data, ps, pgno)
	v, _ := vtVarint(p[cellOffsets(p)[idx]+4:])
	return int64(v)
}

// nthBudget refuses its n-th Alloc call (1-based) and every later one; it
// counts what is outstanding so a test can check the books balance.
type nthBudget struct {
	n, calls int
	used     int64
}

func (b *nthBudget) Alloc(n int64) error {
	b.calls++
	if b.n > 0 && b.calls >= b.n {
		return fmt.Errorf("%w: refused", sqlitefile.ErrBudget)
	}
	b.used += n
	return nil
}
func (b *nthBudget) Free(n int64) { b.used -= n }
