package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// deepSchemaRefusal is the Info.EngineRefuses reason of a schema table whose
// tree is deeper than the engine cursor follows.
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

// TestSchemaRowsBeyondEngineDepthAreFlagged: an object read from a schema tree
// deeper than the engine follows is flagged, and Info.EngineRefuses says so.
func TestSchemaRowsBeyondEngineDepthAreFlagged(t *testing.T) {
	data := deepSchema(t, 24)
	eng := openEngine(t, writeTemp(t, data))
	var n int
	if err := eng.QueryRow("select count(*) from sqlite_master").Scan(&n); err == nil {
		t.Logf("the engine reads the schema of %d objects", n)
	}
	_, v := openLive(t, data, sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var tObj *sqlitefile.SchemaObject
	for i := range s.Objects {
		if s.Objects[i].Name == "t" {
			tObj = &s.Objects[i]
		}
	}
	if tObj == nil || !tObj.BeyondEngineDepth {
		t.Fatalf("table t = %+v", tObj)
	}
	if !slices.Contains(v.Info().EngineRefuses, deepSchemaRefusal) {
		t.Errorf("EngineRefuses = %v", v.Info().EngineRefuses)
	}
	tb, err := v.Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if !tb.SchemaBeyondEngineDepth() {
		t.Error("Table.SchemaBeyondEngineDepth = false")
	}
}

// TestShallowSchemaIsNotFlagged (PASSES ON ARRIVAL): a schema within the
// engine cursor depth flags nothing and adds no refusal.
func TestShallowSchemaIsNotFlagged(t *testing.T) {
	data := deepSchema(t, 18) // the deepest leaf lies at depth 19
	_, v := openLive(t, data, sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Objects) == 0 {
		t.Fatal("no objects")
	}
	for _, o := range s.Objects {
		if o.BeyondEngineDepth {
			t.Errorf("%s is flagged", o.Name)
		}
	}
	if slices.Contains(v.Info().EngineRefuses, deepSchemaRefusal) {
		t.Errorf("EngineRefuses = %v", v.Info().EngineRefuses)
	}
	tb, err := v.Table(context.Background(), "t")
	if err != nil || tb.SchemaBeyondEngineDepth() {
		t.Errorf("Table t: %v, flag %v", err, tb != nil && tb.SchemaBeyondEngineDepth())
	}
}

// TestShallowDefinitionInADeepSchemaIsFlagged: the engine refuses the whole
// file when any part of the schema tree is deeper than its cursor, so a table
// whose own row lies shallow is flagged too (LX-4 U42).
func TestShallowDefinitionInADeepSchemaIsFlagged(t *testing.T) {
	_, v := openLive(t, deepSchema(t, 24), sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.BeyondEngineDepth {
		t.Error("Schema.BeyondEngineDepth = false")
	}
	for _, o := range s.Objects {
		if o.Name == "s" && o.BeyondEngineDepth {
			t.Error("the row of s lies at depth 2 and is not itself beyond the engine depth")
		}
	}
	tb, err := v.Table(context.Background(), "s")
	if err != nil || !tb.SchemaBeyondEngineDepth() {
		t.Errorf("Table s: %v, SchemaBeyondEngineDepth %v", err, tb != nil && tb.SchemaBeyondEngineDepth())
	}
	_, v = openLive(t, deepSchema(t, 18), sqlitefile.Options{})
	if s, err := v.Schema(context.Background()); err != nil || s.BeyondEngineDepth {
		t.Errorf("shallow schema: %v, BeyondEngineDepth %v", err, s != nil && s.BeyondEngineDepth)
	}
}
