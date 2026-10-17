package sqlitefile_test

// The declared sort order of primary-key columns (LX-4 Task 7, L105). The engine
// writes WITHOUT ROWID tables; the test reads the leaf entries in TREE ORDER with
// ScanTree (the engine's own order, independent of createparse) and derives for
// each key column whether the sequence ascends or descends. Column.PKDesc must
// equal that derived fact: this decides, for every form, whether the engine
// honours DESC for key order.

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// pkOrderDB builds a database file with the engine: the DDL, then the rows of
// insert (one argument list each, in the order given).
func pkOrderDB(t *testing.T, ddl, insert string, rows [][]any) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pk.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		t.Fatalf("%s: %v", ddl, err)
	}
	for _, r := range rows {
		if _, err := db.Exec(insert, r...); err != nil {
			_ = db.Close()
			t.Fatalf("%s %v: %v", insert, r, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return readFileRetry(t, path)
}

// derivedOrder reads the leaf entries of table t in tree order and says, for each of the first n
// columns, whether the column descends (within runs where the earlier columns are equal).
func derivedOrder(t *testing.T, data []byte, n int) (desc []bool, def sqlitefile.TableDef) {
	t.Helper()
	d := openLibrary(t, data, nil, nil)
	v := d.Live()
	t.Cleanup(v.Release)
	tb, err := v.Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	def = tb.Def()
	var entries [][]int64
	kind := sqlitefile.IndexTree
	if err := v.ScanTree(context.Background(), tb.RootPage(), kind, func(r sqlitefile.Row) bool {
		e := make([]int64, n)
		for i := range n {
			if r.Values[i].Kind != sqlitefile.KindInt {
				t.Fatalf("entry column %d is not an integer: %+v", i, r.Values[i])
			}
			e[i] = r.Values[i].Int
		}
		entries = append(entries, e)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("%d entries read", len(entries))
	}
	desc = make([]bool, n)
	for k := range n {
		up, down := 0, 0
		for i := 1; i < len(entries); i++ {
			same := true
			for j := range k {
				same = same && entries[i][j] == entries[i-1][j]
			}
			if !same {
				continue
			}
			switch {
			case entries[i][k] > entries[i-1][k]:
				up++
			case entries[i][k] < entries[i-1][k]:
				down++
			}
		}
		if (up > 0) == (down > 0) {
			t.Fatalf("column %d: %d rising and %d falling steps; the data does not show one order", k, up, down)
		}
		desc[k] = down > 0
	}
	return desc, def
}

func TestPrimaryKeyOrderIsRecorded(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a fixed shuffle of test data
	single := make([][]any, 0, 40)
	for _, i := range rng.Perm(40) {
		single = append(single, []any{i + 1, 0})
	}
	var pairs [][]any
	for _, p := range rng.Perm(40) {
		pairs = append(pairs, []any{p/8 + 1, p%8 + 1})
	}
	for _, tc := range []struct {
		name, ddl string
		rows      [][]any
		keyCols   []string
	}{
		{"table asc", "create table t(a, b, primary key(a)) without rowid", single, []string{"a"}},
		{"table desc", "create table t(a, b, primary key(a desc)) without rowid", single, []string{"a"}},
		{"table a desc, b", "create table t(a, b, c, primary key(a desc, b)) without rowid", pairs, []string{"a", "b"}},
		{"table a, b desc", "create table t(a, b, c, primary key(a, b desc)) without rowid", pairs, []string{"a", "b"}},
		{"table both desc", "create table t(a, b, c, primary key(a desc, b desc)) without rowid", pairs, []string{"a", "b"}},
		{"column level desc", "create table t(a int primary key desc, b) without rowid", single, []string{"a"}},
		{"column level asc", "create table t(a int primary key asc, b) without rowid", single, []string{"a"}},
		{"column level plain", "create table t(a int primary key, b) without rowid", single, []string{"a"}},
		{"desc with collate", "create table t(a, b, primary key(a collate binary desc)) without rowid", single, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ins := "insert into t(a, b) values(?, ?)"
			if len(tc.keyCols) == 2 {
				ins = "insert into t(a, b, c) values(?, ?, 0)"
			}
			data := pkOrderDB(t, tc.ddl, ins, tc.rows)
			desc, def := derivedOrder(t, data, len(tc.keyCols))
			if !def.ParseOK || !def.WithoutRowid {
				t.Fatalf("definition: ParseOK=%v WithoutRowid=%v (%s)", def.ParseOK, def.WithoutRowid, def.ParseNote)
			}
			for k, name := range tc.keyCols {
				var col *sqlitefile.Column
				for i := range def.Columns {
					if def.Columns[i].Name == name {
						col = &def.Columns[i]
					}
				}
				if col == nil || col.PKOrdinal != k+1 {
					t.Fatalf("key column %s: %+v", name, col)
				}
				if col.PKDesc != desc[k] {
					t.Errorf("column %s: PKDesc %v, the engine's tree order descends=%v", name, col.PKDesc, desc[k])
				}
			}
			for i := range def.Columns {
				if def.Columns[i].PKOrdinal == 0 && def.Columns[i].PKDesc {
					t.Errorf("non-key column %s has PKDesc", def.Columns[i].Name)
				}
			}
		})
	}
}

// A rowid table has no declared key order to record: PKDesc stays false.
func TestPrimaryKeyOrderIsFalseForARowidTable(t *testing.T) {
	for _, ddl := range []string{
		"create table t(a integer primary key desc, b)",
		"create table t(a, b, primary key(a desc))",
		"create table t(a int primary key desc, b)",
	} {
		d := openLibrary(t, pkOrderDB(t, ddl, "insert into t(a, b) values(?, ?)", [][]any{{1, 1}, {2, 2}}), nil, nil)
		v := d.Live()
		tb, err := v.Table(context.Background(), "t")
		if err != nil {
			t.Fatal(err)
		}
		def := tb.Def()
		if !def.ParseOK || def.WithoutRowid {
			t.Fatalf("%s: ParseOK=%v WithoutRowid=%v", ddl, def.ParseOK, def.WithoutRowid)
		}
		for _, c := range def.Columns {
			if c.PKDesc {
				t.Errorf("%s: column %s has PKDesc on a rowid table", ddl, c.Name)
			}
		}
		v.Release()
	}
}

// A column named twice in the key with two sort orders cannot be given one
// declared order: the table stays unparsed, as it does for two collations.
func TestPrimaryKeyWithTwoOrdersForOneColumnIsNotParsed(t *testing.T) {
	for _, tc := range []struct {
		ddl     string
		parseOK bool
	}{
		{"create table t(a, b, primary key(a, a desc)) without rowid", false},
		{"create table t(a, b, primary key(a desc, a)) without rowid", false},
		{"create table t(a, b, primary key(a desc, a desc)) without rowid", true},
		// A rowid table has no key order to record; the repeat is harmless there.
		{"create table t(a, b, primary key(a, a desc))", true},
	} {
		d := openLibrary(t, pkOrderDB(t, tc.ddl, "insert into t(a, b) values(?, ?)", [][]any{{1, 1}, {2, 2}}), nil, nil)
		v := d.Live()
		tb, err := v.Table(context.Background(), "t")
		if err != nil {
			t.Fatal(err)
		}
		if got := tb.Def().ParseOK; got != tc.parseOK {
			t.Errorf("%s: ParseOK %v, want %v", tc.ddl, got, tc.parseOK)
		}
		v.Release()
	}
}
