package sqlitefile_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// valStr renders a resolved value: N null, O omitted, i:int, f:float,
// t:text, b:hex.
func valStr(v sqlitefile.Value) string {
	switch {
	case v.Omitted:
		return "O"
	case v.Kind == sqlitefile.KindNull:
		return "N"
	case v.Kind == sqlitefile.KindInt:
		return fmt.Sprintf("i:%d", v.Int)
	case v.Kind == sqlitefile.KindFloat:
		return fmt.Sprintf("f:%g", v.Float)
	case v.Kind == sqlitefile.KindText:
		s, ok := v.Text()
		if !ok {
			return fmt.Sprintf("t?:%x", v.Bytes)
		}
		return "t:" + s
	case v.Kind == sqlitefile.KindBlob:
		return fmt.Sprintf("b:%x", v.Bytes)
	}
	return "?"
}

// resolvedRows returns, per row of the table in scan order, "rowid|v1|v2|..."
// (the rowid is 0 for a WITHOUT ROWID row) and the stored column count.
func resolvedRows(t testing.TB, tb *sqlitefile.Table) (out []string, stored []int) {
	t.Helper()
	err := tb.Rows(context.Background(), func(r sqlitefile.Row) bool {
		parts := []string{fmt.Sprint(r.Rowid)}
		for _, v := range tb.Resolve(r) {
			parts = append(parts, valStr(v))
		}
		out = append(out, strings.Join(parts, "|"))
		stored = append(stored, len(r.Values))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, stored
}

func openTable(t testing.TB, data []byte, name string) (*sqlitefile.Table, *sqlitefile.View) {
	t.Helper()
	_, v := openLive(t, data, sqlitefile.Options{})
	t.Cleanup(v.Release)
	tb, err := v.Table(context.Background(), name)
	if err != nil {
		t.Fatalf("Table(%q): %v", name, err)
	}
	return tb, v
}

// TestResolveEngineSemantics: Resolve reads a stored row as the engine does.
func TestResolveEngineSemantics(t *testing.T) {
	const aSQL = "CREATE TABLE a(id INTEGER PRIMARY KEY, x REAL, y TEXT DEFAULT 'dflt', z INTEGER DEFAULT '7', w DEFAULT (1+1), v INT AS (id*2), s INT AS (id+1) STORED, t TEXT DEFAULT 5)"
	build := func(enc int) []byte {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512, Encoding: enc})
		a := b.CreateTable("a", aSQL)
		a.Insert(1, nil, 3.5, "hello", int64(9), "w", int64(2), "tt")                 // a full record
		a.Insert(2, nil, int64(4), "y2", int64(1), nil, int64(3), nil)                // REAL holding an integer
		a.Insert(3, nil, 1.5)                                                         // a short record: defaults
		a.Insert(4, nil, 0.5, "y", int64(1), "w", int64(5), "t", "extra1", int64(99)) // longer than the table
		u := b.CreateTable("u", "CREATE TABLE u(a, b")                                // unparseable
		u.Insert(1, "x", int64(2))
		f := b.CreateTable("f", "CREATE TABLE f(n INTEGER, r REAL, tx TEXT, nu NUMERIC)")
		f.Insert(1, "5", int64(3), int64(9), int64(1)) // stored values are returned as stored, but REAL reads as a float
		w := b.CreateTableWithoutRowid("w", "CREATE TABLE w(a, b, c, PRIMARY KEY(c)) WITHOUT ROWID", 1)
		w.Insert(1, "c1", "a1", "b1") // the record holds the key first
		w2 := b.CreateTableWithoutRowid("w2", "CREATE TABLE w2(a, b, c, d, PRIMARY KEY(c, a)) WITHOUT ROWID", 2)
		w2.Insert(1, "c", "a", "b", "d")
		return b.Bytes()
	}
	for _, enc := range []int{1, 2, 3} {
		data := build(enc)
		check := func(t *testing.T, name string, want []string, wantStored []int) {
			t.Helper()
			tb, _ := openTable(t, data, name)
			got, stored := resolvedRows(t, tb)
			if !slices.Equal(got, want) {
				t.Errorf("encoding %d, table %s\n got  %q\n want %q", enc, name, got, want)
			}
			if wantStored != nil && !slices.Equal(stored, wantStored) {
				t.Errorf("encoding %d, table %s: Row.Values lengths %v, want %v (the extra values stay in Row.Values)", enc, name, stored, wantStored)
			}
		}
		t.Run(fmt.Sprintf("encoding %d rowid table", enc), func(t *testing.T) {
			check(t, "a", []string{
				"1|i:1|f:3.5|t:hello|i:9|t:w|O|i:2|t:tt",
				"2|i:2|f:4|t:y2|i:1|N|O|i:3|N",
				"3|i:3|f:1.5|t:dflt|i:7|O|O|N|t:5",
				"4|i:4|f:0.5|t:y|i:1|t:w|O|i:5|t:t",
			}, []int{7, 7, 2, 9})
		})
		t.Run(fmt.Sprintf("encoding %d unparseable table returns the stored values", enc), func(t *testing.T) {
			tb, _ := openTable(t, data, "u")
			if d := tb.Def(); d.ParseOK || len(d.Columns) != 0 {
				t.Fatalf("def %+v", d)
			}
			check(t, "u", []string{"1|t:x|i:2"}, []int{2})
		})
		t.Run(fmt.Sprintf("encoding %d stored values are not converted but REAL reads as a float", enc), func(t *testing.T) {
			check(t, "f", []string{"1|t:5|f:3|i:9|i:1"}, nil)
		})
		t.Run(fmt.Sprintf("encoding %d WITHOUT ROWID returns declared order", enc), func(t *testing.T) {
			check(t, "w", []string{"0|t:a1|t:b1|t:c1"}, []int{3})
			check(t, "w2", []string{"0|t:a|t:b|t:c|t:d"}, []int{4})
		})
	}
	t.Run("a UTF-16 default is stored in the database encoding", func(t *testing.T) {
		data := build(2)
		tb, _ := openTable(t, data, "a")
		err := tb.Rows(context.Background(), func(r sqlitefile.Row) bool {
			if r.Rowid != 3 {
				return true
			}
			y := tb.Resolve(r)[2]
			if y.Enc != sqlitefile.EncUTF16LE || len(y.Bytes) != 8 || y.Len != 8 || valStr(y) != "t:dflt" {
				t.Errorf("default text %+v", y)
			}
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Resolve of a row without a rowid alias value", func(t *testing.T) {
		tb, _ := openTable(t, build(1), "a")
		got := tb.Resolve(sqlitefile.Row{Rowid: 42, HasRowid: true, Values: []sqlitefile.Value{{}, {Kind: sqlitefile.KindInt, Int: 8}}})
		if valStr(got[0]) != "i:42" || valStr(got[1]) != "f:8" || len(got) != 8 {
			t.Errorf("%d values: %v", len(got), got)
		}
	})
}

// engineFile builds a database with the engine by running stmts and returns
// the file's bytes.
func engineFile(t *testing.T, stmts []string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "e.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			_ = db.Close()
			t.Fatalf("engine: %s: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func engineVal(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return fmt.Sprintf("i:%d", x)
	case float64:
		return fmt.Sprintf("f:%g", x)
	case string:
		return "t:" + x
	case []byte:
		return fmt.Sprintf("b:%x", x)
	}
	return fmt.Sprintf("?%T", v)
}

// engineRows returns the rows of query as strings, one per row.
func engineRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = engineVal(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSchemaMatchesEngine: for tables the engine itself created, altered and
// filled, the parsed definition equals the engine's own table_xinfo, every
// rowid-alias rule agrees with the engine's behaviour, and Resolve of every
// stored row (short records, defaults, generated columns, REAL, WITHOUT ROWID)
// equals what the engine selects. Only virtual generated and expression
// default columns (Omitted here) are not compared.
// aliasInsert holds one fixed insert per alias table (statements are literals so
// the single-writer scan never sees a table name built at run time).
var aliasInsert = map[string]string{
	"al1": "insert into al1(b) values (1)", "al2": "insert into al2(b) values (1)", "al3": "insert into al3(b) values (1)",
	"al4": "insert into al4(b) values (1)", "al5": "insert into al5(b) values (1)", "al6": "insert into al6(b) values (1)",
	"al7": "insert into al7(b) values (1)", "al8": "insert into al8(b) values (1)", "al9": "insert into al9(b) values (1)",
	"al10": "insert into al10(b) values (1)", "al11": "insert into al11(b) values (1)", "al12": "insert into al12(b) values (1)",
	"al14": "insert into al14(b) values (1)",
}

func TestSchemaMatchesEngine(t *testing.T) {
	stmts := []string{
		"create table t1(id integer primary key, a text, b real, c integer default 5, d numeric, e)",
		"insert into t1(a,b,c,d,e) values('x', 1, 2, '3', 4.5)",
		"insert into t1(a,b,c,d,e) values('y', 2.5, null, '4.5', 'z')",
		"insert into t1(id,a,b) values(10,'late',7)",
		"alter table t1 add column f text default 'late'",
		"alter table t1 add column g integer default '12'",
		"alter table t1 add column h real default 3",
		"alter table t1 add column h2 real default '3.5'",
		"alter table t1 add column i numeric default '2.0'",
		"alter table t1 add column j text default 5",
		"alter table t1 add column k blob default 7",
		"alter table t1 add column l default x'0aff'",
		"alter table t1 add column m default NULL",
		"alter table t1 add column n default -4",
		"alter table t1 add column o default 2.50",
		"alter table t1 add column p integer default true",
		"alter table t1 add column q text default 'it''s'",
		"insert into t1(a,f,g,h,i,j,k,l,m,n,o) values('full','F',1,2,3,4,5,6,7,8,9)",
		"create table g(a integer, b integer as (a*2), c text as (a||'x') stored, d, e integer as (a+1) virtual)",
		"insert into g(a,d) values(1,'p'),(2,null),(3,x'00')",
		"create table w(a text, b integer, c real, d, primary key(c, a)) without rowid",
		"insert into w values('k1', 1, 2.5, 'd1'),('k0', 2, 2.5, 'd0'),('k2', 3, 1, null)",
		"alter table w add column e text default 'we'",
		"insert into w values('k3', 4, 9, 'd3', 'e3')",
		"create table s(id integer primary key, i int, r real, t text, b blob, x any) strict",
		"insert into s values(1, 1, 2, 'a', x'01', 5),(2, 3, 4.5, 'b', x'', 'any text')",
		"create table sw(id integer primary key, v real default 1) without rowid",
		"insert into sw(id) values(7),(8)",
		"create table bigrow(id integer primary key, u real, p blob)",
		"insert into bigrow values(1, 1e300, zeroblob(10000)),(2, -0.0, x'ff'),(3, 3, null)",
	}
	aliasCases := []struct {
		name, cols string
		strict     bool
		alias      bool
	}{
		{"al1", "a INTEGER PRIMARY KEY, b", false, true},
		{"al2", "a INT PRIMARY KEY, b", false, false},
		{"al3", "a INTEGER PRIMARY KEY DESC, b", false, false}, // [M] pinned here
		{"al4", "a INTEGER PRIMARY KEY ASC, b", false, true},
		{"al5", "a INTEGER, b, PRIMARY KEY(a)", false, true},
		{"al6", "a INTEGER, b, PRIMARY KEY(a DESC)", false, true}, // [M] table-level DESC does not stop the alias
		{"al7", "a INTEGER, b INTEGER, PRIMARY KEY(a, b)", false, false},
		{"al8", "a integer primary key autoincrement, b", false, true},
		{"al9", "a INTEGER PRIMARY KEY, b ANY", true, true},
		{"al10", "a INT PRIMARY KEY, b ANY", true, false},
		{"al11", "a INTEGER PRIMARY KEY NOT NULL, b", false, true},
		{"al12", "a Integer Primary Key, b", false, true},
		{"al13", "a INTEGER PRIMARY KEY, b, PRIMARY KEY(b)", false, false},
		{"al14", "a INTEGER, b INTEGER, PRIMARY KEY(a, b)", true, false}, // two keys: the engine refuses it
	}
	for _, c := range aliasCases {
		if c.name == "al13" {
			continue
		}
		strict := ""
		if c.strict {
			strict = " strict"
		}
		stmts = append(stmts, fmt.Sprintf("create table %s(%s)%s", c.name, c.cols, strict))
		if !c.strict || c.alias { // a STRICT key that is no alias is NOT NULL: the insert is refused
			stmts = append(stmts, aliasInsert[c.name])
		}
	}
	data := engineFile(t, stmts)
	db := openEngine(t, writeTemp(t, data))

	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	ctx := context.Background()
	names := []string{"t1", "g", "w", "s", "sw", "bigrow"}
	for _, c := range aliasCases {
		if c.name != "al13" {
			names = append(names, c.name)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			tb, err := v.Table(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			def := tb.Def()
			if !def.ParseOK {
				t.Fatalf("not parsed: %q", def.ParseNote)
			}
			// table_xinfo: name, type, notnull, pk, hidden.
			info := engineRows(t, db, fmt.Sprintf("select name, type, \"notnull\", pk, hidden from pragma_table_xinfo('%s') order by cid", name))
			if len(info) != len(def.Columns) {
				t.Fatalf("engine %d columns, parsed %d", len(info), len(def.Columns))
			}
			for i, col := range def.Columns {
				hidden := "i:0"
				switch col.Generated {
				case sqlitefile.GenVirtual:
					hidden = "i:2"
				case sqlitefile.GenStored:
					hidden = "i:3"
				}
				nn := "i:0"
				if col.NotNull {
					nn = "i:1"
				}
				// pragma spells a standard type (INTEGER, TEXT, ...) in upper case: compare blind to case.
				want := fmt.Sprintf("t:%s|t:%s|%s|i:%d|%s", col.Name, col.DeclType, nn, col.PKOrdinal, hidden)
				if !strings.EqualFold(info[i], want) {
					t.Errorf("column %d: engine %q, parsed %q", i, info[i], want)
				}
			}
			// The rowid alias agrees with the engine's behaviour.
			for _, c := range aliasCases {
				if c.name != name {
					continue
				}
				engineAlias := false
				if c.strict && !c.alias {
					if _, err := db.Exec(aliasInsert[name]); err == nil || !strings.Contains(err.Error(), "NOT NULL") {
						t.Fatalf("the engine must refuse a NULL key in a STRICT table: %v", err)
					}
				} else {
					var isNull bool
					if err := db.QueryRow(fmt.Sprintf("select a is null from %s", name)).Scan(&isNull); err != nil {
						t.Fatal(err)
					}
					engineAlias = !isNull
				}
				if engineAlias != c.alias {
					t.Fatalf("test table error: the engine treats %s as alias=%v", c.cols, engineAlias)
				}
				if got := def.RowidAlias >= 0; got != c.alias {
					t.Errorf("%s: RowidAlias %d, the engine treats the key as alias=%v", c.cols, def.RowidAlias, c.alias)
				}
			}
			// Rows.
			order := "order by rowid"
			if def.WithoutRowid {
				order = ""
			}
			var want []string
			if def.WithoutRowid {
				var pk []string
				for i := 1; i <= 4; i++ {
					for _, col := range def.Columns {
						if col.PKOrdinal == i {
							pk = append(pk, `"`+col.Name+`"`)
						}
					}
				}
				order = "order by " + strings.Join(pk, ", ")
			}
			want = engineRows(t, db, fmt.Sprintf("select * from %s %s", name, order))
			got, _ := resolvedRows(t, tb)
			if len(got) != len(want) {
				t.Fatalf("%d rows read, the engine has %d", len(got), len(want))
			}
			for i := range want {
				g := strings.Split(got[i], "|")[1:] // drop the rowid
				w := strings.Split(want[i], "|")
				if len(g) != len(w) {
					t.Fatalf("row %d: %d columns against %d", i, len(g), len(w))
				}
				for j := range w {
					if g[j] == "O" { // not materialized: virtual generated or an expression default
						if def.Columns[j].Generated != sqlitefile.GenVirtual && def.Columns[j].Default.Kind != sqlitefile.DefaultExpr {
							t.Errorf("row %d column %q: omitted without cause", i, def.Columns[j].Name)
						}
						continue
					}
					if g[j] != w[j] {
						t.Errorf("row %d column %q (%s): read %s, engine %s", i, def.Columns[j].Name, def.Columns[j].DeclType, g[j], w[j])
					}
				}
			}
		})
	}
}
