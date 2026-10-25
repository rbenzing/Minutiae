package cellx_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// cell is one value of a fakeRow.
type cell struct {
	kind string // "int", "real", "text", "blob", "null"
	i    int64
	f    float64
}

// fakeRow is a parse.Row of another implementation than sqlitedb: it answers from
// IsNull and the typed getters only, as a row without decoder state does.
type fakeRow struct {
	parse.Row
	cells []cell
}

func (r fakeRow) Table() string {
	return "t"
}

func (r fakeRow) Rowid() (int64, bool) {
	return 1, true
}

func (r fakeRow) Range() (records.Range, parse.FileRole) {
	return records.Range{}, parse.RoleDB
}

func (r fakeRow) IsNull(col int) bool {
	return r.cells[col].kind == "null"
}

func (r fakeRow) Int(col int) (int64, bool) {
	c := r.cells[col]
	return c.i, c.kind == "int"
}

func (r fakeRow) Float(col int) (float64, bool) {
	c := r.cells[col]
	return c.f, c.kind == "real"
}

func (r fakeRow) Text(col int) ([]byte, bool) {
	return []byte("5"), r.cells[col].kind == "text"
}

func (r fakeRow) Blob(col int) ([]byte, bool) {
	return []byte{1}, r.cells[col].kind == "blob"
}

// unreadRow is a real sqlitedb row recovered from a record cut off after its first
// value: column 2 is unread, which the decoder reports as not known.
func unreadRow(t testing.TB) sqlitedb.Row {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(id integer primary key, a real)")
	tb.Insert(1, 1, 2.5)
	data := b.Bytes()
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	table, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := int64(1)
	row, err := sqlitedb.FromRecovered(table, sqlitefile.RecoveredRow{
		Table: "t", Rowid: &id, Truncated: true,
		Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}
