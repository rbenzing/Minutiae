package dbrecover

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

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

// TestSchemaDerivedEngineRefusalsRefuseRecovery: the engine refuses a database
// for reasons that only show once the schema is read (a schema tree deeper than
// its cursor, a row whose name disagrees with its statement); recovery refuses
// it too, with the engine-refuses reason, and ingests nothing (LX-4 U43).
func TestSchemaDerivedEngineRefusalsRefuseRecovery(t *testing.T) {
	mismatch := func() []byte {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		b.CreateTable("t", "create table t(a)").Insert(1, "x")
		b.CreateTable("kipds", "create table kinds(a)")
		return b.Bytes()
	}
	for _, c := range []struct {
		name string
		db   []byte
		want string
	}{
		{"schema tree deeper than the engine follows", deepSchema(t, 24), "schema tree deeper than the engine follows"},
		{"row name disagrees with its statement", mismatch(), "kipds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cs := recordstest.NewCase(t)
			db := addFile(t, cs, "/data/x.db", c.db)
			_, err := Run(context.Background(), cs, Inputs{DB: db.ID}, Options{})
			if got := refusalReason(err); got != ReasonEngineRefuses {
				t.Fatalf("refusal reason %q (err %v), want %q", got, err, ReasonEngineRefuses)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v does not name %q", err, c.want)
			}
			noIngest(t, cs)
		})
	}
}

// TestOrphanIndexRefusesRecovery: an index naming no table is a schema-derived
// engine refusal like the others (LX-4 U48). PASSES ON ARRIVAL: the gate of U43
// already reads every schema-derived reason.
func TestOrphanIndexRefusesRecovery(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(a)").Insert(1, "x")
	b.CreateIndex("ix", "t", "create index ix on t(a)", 0)
	data := bytes.Replace(b.Bytes(), []byte("indexixt"), []byte("indexixz"), 1)
	cs := recordstest.NewCase(t)
	db := addFile(t, cs, "/data/x.db", data)
	_, err := Run(context.Background(), cs, Inputs{DB: db.ID}, Options{})
	if got := refusalReason(err); got != ReasonEngineRefuses {
		t.Fatalf("refusal reason %q (err %v), want %q", got, err, ReasonEngineRefuses)
	}
	noIngest(t, cs)
}

// TestUnreadableSchemaAtTheGateRefusesRecovery: a schema that cannot be read at
// the gate is a typed refusal (engine-refuses, naming the read error), never a
// pass: the read can succeed later, and the schema-derived reasons would then
// never have been checked (LX-4 U47).
func TestUnreadableSchemaAtTheGateRefusesRecovery(t *testing.T) {
	defer SetSchemaReadHookForTest(func() error { return errors.New("injected schema read failure") })()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(a)").Insert(1, "x")
	cs := recordstest.NewCase(t)
	db := addFile(t, cs, "/data/x.db", b.Bytes())
	_, err := Run(context.Background(), cs, Inputs{DB: db.ID}, Options{})
	if got := refusalReason(err); got != ReasonEngineRefuses {
		t.Fatalf("refusal reason %q (err %v), want %q", got, err, ReasonEngineRefuses)
	}
	if !errors.Is(err, sqlitefile.ErrEngineRefuses) || !strings.Contains(err.Error(), "injected schema read failure") {
		t.Errorf("err %v", err)
	}
	noIngest(t, cs)
}
