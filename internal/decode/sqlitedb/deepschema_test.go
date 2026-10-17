package sqlitedb_test

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const deepSchemaRefusal = "schema tree deeper than the engine follows"

// deepSchema returns a database whose schema table (root page 1) is a degenerate
// tree of n interior pages: interior 1 is page 1 itself, interiors 2..n are pages
// 2..n, leaf i (1..n) is the left child of interior i and holds a view row v<i>,
// and the last leaf (page 2n+1) is the right child of interior n and holds the
// row of table t, whose root page is 2n+2 (a leaf with three rows). The leaf of
// table t lies at depth n+1.
func deepSchema(t testing.TB, n int) []byte {
	t.Helper()
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	data := append([]byte(nil), b.Bytes()[:100]...) // the file header only
	text := func(s string) (serial byte, body []byte) { return byte(13 + 2*len(s)), []byte(s) }
	schemaRow := func(typ, name, tbl string, root byte, sql string) []byte {
		var types, body []byte
		for _, s := range []string{typ, name, tbl} {
			st, bd := text(s)
			types, body = append(types, st), append(body, bd...)
		}
		types, body = append(types, 1), append(body, root)
		st, bd := text(sql)
		types, body = append(types, st), append(body, bd...)
		rec := append([]byte{byte(1 + len(types))}, types...)
		return append(rec, body...)
	}
	leafRows := func(rows ...[]byte) []byte { // each argument is one complete cell
		p := make([]byte, ps)
		off := ps
		var ptrs []int
		for _, c := range rows {
			off -= len(c)
			copy(p[off:], c)
			ptrs = append(ptrs, off)
		}
		p[0] = 0x0d
		binary.BigEndian.PutUint16(p[3:], uint16(len(rows)))
		binary.BigEndian.PutUint16(p[5:], uint16(off))
		for i, o := range ptrs {
			binary.BigEndian.PutUint16(p[8+2*i:], uint16(o))
		}
		return p
	}
	cell := func(rowid int, rec []byte) []byte {
		return append([]byte{byte(len(rec)), byte(rowid)}, rec...)
	}
	interior := func(base int, left, right uint32, key int) []byte {
		p := make([]byte, ps)
		c := binary.BigEndian.AppendUint32(nil, left)
		c = append(c, byte(key))
		off := ps - len(c)
		copy(p[off:], c)
		p[base] = 0x05
		binary.BigEndian.PutUint16(p[base+3:], 1)
		binary.BigEndian.PutUint16(p[base+5:], uint16(off))
		binary.BigEndian.PutUint32(p[base+8:], right)
		binary.BigEndian.PutUint16(p[base+12:], uint16(off))
		return p
	}
	for i := 1; i <= n; i++ {
		right := uint32(i + 1)
		if i == n {
			right = uint32(2*n + 1)
		}
		base := 0
		if i == 1 {
			base = 100
		}
		pg := interior(base, uint32(n+i), right, i)
		if i == 1 {
			pg = pg[100:] // page 1 keeps the file header in its first 100 bytes
		}
		data = append(data, pg...)
	}
	for i := 1; i <= n; i++ {
		name := "v" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		row := schemaRow("view", name, name, 0, "create view "+name+" as select 1")
		if i == 1 { // a table with a shallow definition (depth 2) in a schema that is deep elsewhere
			row = schemaRow("table", "s", "s", byte(2*n+3), "create table s(a)")
		}
		data = append(data, leafRows(cell(i, row))...)
	}
	data = append(data, leafRows(cell(n+1, schemaRow("table", "t", "t", byte(2*n+2), "create table t(a)")))...)
	data = append(data, leafRows(
		[]byte{2, 1, 2, 0}, []byte{2, 2, 2, 0}, []byte{2, 3, 2, 0})...)
	data = append(data, leafRows([]byte{2, 1, 2, 0}, []byte{2, 2, 2, 0})...) // table s
	binary.BigEndian.PutUint32(data[28:], uint32(len(data)/ps))
	copy(data[92:96], data[24:28])
	return data
}

func deepSchemaTable(t *testing.T, n int) (*sqlitedb.DB, *sqlitedb.Table) {
	t.Helper()
	d := openBytes(t, deepSchema(t, n), nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, tb
}

// TestRowsOfATableDefinedBeyondEngineDepthAreFlagged: the schema decides how a
// table is decoded; when its row lies in a schema tree the engine cannot open,
// every row of the table is flagged, Stats.RowsFlagged counts them and
// Info.Header.EngineRefuses says why (LX-4 U40).
func TestRowsOfATableDefinedBeyondEngineDepthAreFlagged(t *testing.T) {
	d, tb := deepSchemaTable(t, 24)
	if !tb.SchemaBeyondEngineDepth() {
		t.Error("Table.SchemaBeyondEngineDepth = false")
	}
	if !slices.Contains(d.Info().Header.EngineRefuses, deepSchemaRefusal) {
		t.Errorf("EngineRefuses = %v", d.Info().Header.EngineRefuses)
	}
	n := 0
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		n++
		if r.Flags()&sqlitedb.FlagBeyondEngineDepth == 0 {
			id, _ := r.Rowid()
			t.Errorf("rowid %d is not flagged", id)
		}
	})
	if n != 3 {
		t.Fatalf("scan delivered %d rows", n)
	}
	if s := d.Stats(); s.RowsFlagged != 3 {
		t.Errorf("RowsFlagged = %d", s.RowsFlagged)
	}
	r, ok, err := tb.Get(t.Context(), 2)
	if err != nil || !ok || r.Flags()&sqlitedb.FlagBeyondEngineDepth == 0 {
		t.Errorf("Get(2) = %v, %v flags %b", ok, err, r.Flags())
	}
	r.Release()
}

// PASSES ON ARRIVAL: a schema within the engine cursor depth flags nothing.
func TestShallowSchemaFlagsNoRow(t *testing.T) {
	d, tb := deepSchemaTable(t, 18)
	if tb.SchemaBeyondEngineDepth() || slices.Contains(d.Info().Header.EngineRefuses, deepSchemaRefusal) {
		t.Error("a shallow schema is reported as deep")
	}
	n := 0
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		n++
		if r.Flags() != 0 {
			t.Errorf("flags %b", r.Flags())
		}
	})
	if n != 3 || d.Stats().RowsFlagged != 0 {
		t.Errorf("rows %d, flagged %d", n, d.Stats().RowsFlagged)
	}
}

// TestRowsOfAShallowDefinitionInADeepSchemaAreFlagged: the engine refuses the
// whole file when any part of the schema tree is too deep, so every table is
// flagged, not only the one whose row lies deep (LX-4 U42).
func TestRowsOfAShallowDefinitionInADeepSchemaAreFlagged(t *testing.T) {
	d := openBytes(t, deepSchema(t, 24), nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tb.SchemaBeyondEngineDepth() {
		t.Error("Table.SchemaBeyondEngineDepth = false")
	}
	n := 0
	scanEach(t, tb, func(_ int, r sqlitedb.Row) {
		n++
		if r.Flags()&sqlitedb.FlagBeyondEngineDepth == 0 {
			t.Errorf("a row of s is not flagged")
		}
	})
	if n != 2 || d.Stats().RowsFlagged != 2 {
		t.Errorf("rows %d, flagged %d", n, d.Stats().RowsFlagged)
	}
}

// TestRecoveredRowOfADeepSchemaCarriesTheFlag: a recovered row is built through
// the same definition and carries the flag too (LX-4 U45), here for the table with a shallow row; a shallow schema
// does not add it.
func TestRecoveredRowOfADeepSchemaCarriesTheFlag(t *testing.T) {
	rr := sqlitefile.RecoveredRow{Table: "s", Rowid: ip(1), Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}}}
	for _, c := range []struct {
		n    int
		want bool
	}{{24, true}, {18, false}} {
		tb, err := openBytes(t, deepSchema(t, c.n), nil, nil, bigBudget()).Table(t.Context(), "s", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := sqlitedb.FromRecovered(tb, rr)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Flags()&sqlitedb.FlagBeyondEngineDepth != 0; got != c.want {
			t.Errorf("depth %d: flag %v, want %v", c.n, got, c.want)
		}
		r.Release()
	}
}
