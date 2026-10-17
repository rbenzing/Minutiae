package rowrun_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// rowsOf scans table name of data and returns clones of its rows.
func rowsOf(t testing.TB, data []byte, name string) (*sqlitedb.Table, []sqlitedb.Row) {
	t.Helper()
	in := dbInput(t, data)
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	tb, err := d.Table(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []sqlitedb.Row
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		c, err := r.Clone()
		rows = append(rows, c)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tb, rows
}

func TestIntCellTellsNullMismatchAndMissingApart(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("u", "create table u(a)")
	tb.Insert(1, "x")
	tb.Insert(2, nil)
	tb.Insert(3, int64(5))
	_, rows := rowsOf(t, b.Bytes(), "u")
	want := []struct {
		v int64
		k rowrun.CellKind
	}{{0, rowrun.CellOther}, {0, rowrun.CellNull}, {5, rowrun.CellValue}}
	for i, w := range want {
		if v, k := rowrun.IntCell(rows[i], 0); v != w.v || k != w.k {
			t.Errorf("row %d: IntCell = %d, %d; want %d, %d", i+1, v, k, w.v, w.k)
		}
	}
	if _, k := rowrun.IntCell(rows[2], -1); k != rowrun.CellMissing {
		t.Errorf("column -1: kind %d, want CellMissing", k)
	}
	if _, k := rowrun.IntCell(rows[2], 7); k != rowrun.CellMissing && k != rowrun.CellUnknown {
		t.Errorf("column 7 (outside the table): kind %d", k)
	}
}

func TestTextCellAndFlag(t *testing.T) {
	_, rows := rowsOf(t, tinyDB(t, 2), "t")
	if s, k := rowrun.TextCell(rows[0], 2); string(s) != "row-1" || k != rowrun.CellValue {
		t.Errorf("TextCell = %q, %d", s, k)
	}
	if _, k := rowrun.TextCell(rows[0], 1); k != rowrun.CellOther {
		t.Errorf("an integer read as text: kind %d, want CellOther", k)
	}
	unknown := map[string]any{}
	rowrun.Flag(unknown, rows[0], "a", 1, rowrun.CellOther)
	rowrun.Flag(unknown, rows[0], "s", 2, rowrun.CellValue)
	rowrun.Flag(unknown, rows[0], "nope", -1, rowrun.CellMissing)
	if len(unknown) != 1 || unknown["a"] != "type-mismatch" {
		t.Errorf("unknown = %v, want only a: type-mismatch", unknown)
	}
}

func TestAnUnreadColumnIsUnknownNotNull(t *testing.T) {
	tb, _ := rowsOf(t, tinyDB(t, 1), "t")
	id := int64(1)
	// a recovered record cut off after its first value leaves the other columns unread
	row, err := sqlitedb.FromRecovered(tb, sqlitefile.RecoveredRow{
		Table: "t", Rowid: &id, Truncated: true,
		Values: []sqlitefile.Value{{Kind: sqlitefile.KindNull}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, k := rowrun.IntCell(row, 1); k != rowrun.CellUnknown || v != 0 {
		t.Errorf("IntCell of an unread column = %d, %d; want CellUnknown", v, k)
	}
	unknown := map[string]any{}
	rowrun.Flag(unknown, row, "a", 1, rowrun.CellUnknown)
	if unknown["a"] != "unread" {
		t.Errorf("Flag recorded %v, want a: unread", unknown)
	}
}

func TestResolveAndAtNeverGuessAColumn(t *testing.T) {
	c := rowrun.Cols{"a": 1}
	if c.At("a") != 1 || c.At("typo") != -1 {
		t.Errorf("At: a=%d typo=%d", c.At("a"), c.At("typo"))
	}
}

func TestDamageFlagsNamesOnlyDamage(t *testing.T) {
	_, rows := rowsOf(t, tinyDB(t, 1), "t")
	if f := rowrun.DamageFlags(rows[0]); f != nil {
		t.Errorf("a clean row has flags %v", f)
	}
	if f := rowrun.DamageFlags(stubRow{}); f != nil {
		t.Errorf("a row without flags has %v", f)
	}
}
