package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// schemaFixture builds a database with tables, indexes, a view, a trigger, a
// virtual-table row and automatic indexes.
func schemaFixture(o sqlitetest.Options) (*sqlitetest.Builder, []string) {
	b := sqlitetest.New(o)
	people := b.CreateTable("people", "CREATE TABLE people(id INTEGER PRIMARY KEY, name TEXT UNIQUE, age INT)")
	for i := int64(1); i <= 20; i++ {
		people.Insert(i, nil, fmt.Sprintf("n%02d-é", i), i*3)
	}
	b.CreateAutoIndex("sqlite_autoindex_people_1", "people", 1)
	b.CreateIndex("idx_age", "people", "CREATE INDEX idx_age ON people(age DESC)", 2)
	wr := b.CreateTableWithoutRowid("wr", "CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID", 1)
	wr.Insert(1, "a", int64(1))
	wr.Insert(2, "b", int64(2))
	b.AddSchemaRow("view", "v", "v", int64(0), "CREATE VIEW v AS SELECT id FROM people")
	b.AddSchemaRow("trigger", "trg", "people", int64(0), "CREATE TRIGGER trg AFTER INSERT ON people BEGIN SELECT 1; END")
	b.AddSchemaRow("table", "vt", "vt", int64(0), "CREATE VIRTUAL TABLE vt USING fts5(a, b)")
	return b, []string{"people", "sqlite_autoindex_people_1", "idx_age", "wr", "v", "trg", "vt"}
}

func objNames(s *sqlitefile.Schema) []string {
	var out []string
	for _, o := range s.Objects {
		out = append(out, o.Name)
	}
	return out
}

func TestSchemaReadsSqliteSchema(t *testing.T) {
	for _, enc := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("encoding %d", enc), func(t *testing.T) {
			b, names := schemaFixture(sqlitetest.Options{PageSize: 512, Encoding: enc})
			data := b.Bytes()
			_, v := openLive(t, data, sqlitefile.Options{})
			defer v.Release()
			s, err := v.Schema(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := objNames(s); !slices.Equal(got, names) {
				t.Fatalf("objects %v, want %v in rowid order", got, names)
			}
			if s.Format != 4 || s.Cookie != v.Info().SchemaCookie {
				t.Errorf("format %d cookie %d", s.Format, s.Cookie)
			}
			wantType := []string{"table", "index", "index", "table", "view", "trigger", "table"}
			wantRoot := []uint32{b.Object("people").Root(), b.Object("sqlite_autoindex_people_1").Root(), b.Object("idx_age").Root(), b.Object("wr").Root(), 0, 0, 0}
			wantTbl := []string{"people", "people", "people", "wr", "v", "people", "vt"}
			for i, o := range s.Objects {
				if o.Type != wantType[i] || o.RootPage != wantRoot[i] || o.TblName != wantTbl[i] {
					t.Errorf("object %d: %s %q root %d tbl %q", i, o.Type, o.Name, o.RootPage, o.TblName)
				}
				// Loc reproduces the bytes of the schema row.
				cell := data[o.Loc.Offset : o.Loc.Offset+o.Loc.Length]
				p, n1 := vtVarint(cell)
				rowid, n2 := vtVarint(cell[n1:])
				if o.Loc.File != sqlitefile.FileDB || o.Loc.Page == 0 || !bytes.Contains(cell, []byte(o.Name)) && enc < 2 || int(rowid) != i+1 || int(p) != len(cell)-n1-n2 {
					t.Errorf("object %d: Loc %+v does not reproduce the row (payload %d rowid %d cell %d bytes)", i, o.Loc, p, rowid, len(cell))
				}
				if o.Type != "index" || o.SQL != "" { // the autoindex row stores NULL, not text
					if !strings.Contains(o.SQL, "CREATE") {
						t.Errorf("object %d %q: SQL %q", i, o.Name, o.SQL)
					}
				}
			}
			// Tables are parsed, with their derived facts; the view, the
			// trigger and the virtual table are not.
			people := s.Objects[0]
			if people.Table == nil || !people.Table.ParseOK || people.Table.RowidAlias != 0 || len(people.Table.Columns) != 3 || people.Virtual {
				t.Errorf("people: %+v", people.Table)
			}
			if wr := s.Objects[3]; wr.Table == nil || !wr.Table.WithoutRowid || !wr.Table.ParseOK {
				t.Errorf("wr: %+v", wr.Table)
			}
			if auto := s.Objects[1]; auto.Index == nil || !auto.Index.Auto || auto.Index.ParseOK || !auto.Index.Unique || auto.Index.Table != "people" || auto.SQL != "" {
				t.Errorf("autoindex: %+v sql %q", auto.Index, auto.SQL)
			}
			if idx := s.Objects[2]; idx.Index == nil || !idx.Index.ParseOK || idx.Index.Auto || idx.Index.Table != "people" ||
				len(idx.Index.Columns) != 1 || idx.Index.Columns[0].Name != "age" || !idx.Index.Columns[0].Desc {
				t.Errorf("idx_age: %+v", idx.Index)
			}
			for _, i := range []int{4, 5} {
				if o := s.Objects[i]; o.Table != nil || o.Index != nil || o.Virtual || o.SQL == "" {
					t.Errorf("object %q must carry its SQL and nothing parsed: %+v", o.Name, o)
				}
			}
			if vt := s.Objects[6]; !vt.Virtual || vt.Table == nil || vt.Table.ParseOK || vt.Table.ParseNote != "virtual" || vt.RootPage != 0 {
				t.Errorf("virtual table: %+v %+v", vt, vt.Table)
			}
			if !viewWarns(v, sqlitefile.WarnSchemaSQLUnparsed, 0) {
				t.Errorf("the virtual table row raises schema-sql-unparsed: %v", v.Warnings())
			}
			for _, w := range v.Warnings() {
				if w.Code == sqlitefile.WarnSchemaSQLUnparsed && !strings.Contains(w.Msg, "vt") {
					t.Errorf("only the virtual table is unparsed, got %v", w)
				}
			}
			// A second call serves the same schema without scanning again.
			reads := v.Stats().PageReads
			s2, err := v.Schema(context.Background())
			if err != nil || s2 != s || v.Stats().PageReads != reads {
				t.Errorf("second Schema call: same %v err %v reads %d -> %d", s2 == s, err, reads, v.Stats().PageReads)
			}
		})
	}
}

// TestSchemaMatchesEngineRows compares the schema table the engine reads from
// the builder's file with the objects read here (type, name, tbl_name,
// rootpage, sql).
func TestSchemaMatchesEngineRows(t *testing.T) {
	b, _ := schemaFixture(sqlitetest.Options{PageSize: 512})
	data := b.Bytes()
	db := openEngine(t, writeTemp(t, data))
	rows, err := db.Query("select type, name, tbl_name, rootpage, coalesce(sql, '<NULL>') from sqlite_schema order by rowid")
	if err != nil {
		t.Fatalf("the engine does not read the fixture's schema: %v", err)
	}
	defer func() { _ = rows.Close() }()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for rows.Next() {
		var typ, name, tbl, sqltext string
		var root int64
		if err := rows.Scan(&typ, &name, &tbl, &root, &sqltext); err != nil {
			t.Fatal(err)
		}
		if i >= len(s.Objects) {
			t.Fatalf("the engine has more rows than the %d objects read", len(s.Objects))
		}
		o := s.Objects[i]
		if sqltext == "<NULL>" {
			sqltext = ""
		}
		if o.Type != typ || o.Name != name || o.TblName != tbl || int64(o.RootPage) != root || o.SQL != sqltext {
			t.Errorf("row %d: engine (%s %s %s %d) read (%s %s %s %d)", i, typ, name, tbl, root, o.Type, o.Name, o.TblName, o.RootPage)
		}
		i++
	}
	if i != len(s.Objects) {
		t.Errorf("engine %d rows, reader %d objects", i, len(s.Objects))
	}
}

func TestSchemaRowValidation(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	good := b.CreateTable("good", "CREATE TABLE good(a)")
	good.Insert(1, int64(1))
	const okSQL = "CREATE TABLE x(a)"
	b.AddSchemaRow("tabel", "wrongtype", "wrongtype", int64(5), okSQL)               // type not one of the four
	b.AddSchemaRow(nil, "nulltype", "nulltype", int64(5), okSQL)                     // NULL type
	b.AddSchemaRow("table", nil, "nullname", int64(5), okSQL)                        // NULL name
	b.AddSchemaRow("table", int64(7), "intname", int64(5), okSQL)                    // name not text
	b.AddSchemaRow("table", "nulltbl", nil, int64(5), okSQL)                         // NULL tbl_name
	b.AddSchemaRow("table", "negroot", "negroot", int64(-3), okSQL)                  // negative rootpage
	b.AddSchemaRow("table", "textroot", "textroot", "5", okSQL)                      // rootpage not an integer
	b.AddSchemaRow("table", "floatroot", "floatroot", float64(5), okSQL)             // rootpage a float
	b.AddSchemaRow("table", "hugeroot", "hugeroot", int64(1)<<33, okSQL)             // rootpage past 32 bits
	b.AddSchemaRow("table", "intsql", "intsql", int64(5), int64(9))                  // sql neither text nor NULL
	b.AddSchemaRow("table", "short", "short")                                        // fewer than five columns
	b.AddSchemaRow("view", "fine", "fine", int64(0), "CREATE VIEW fine AS SELECT 1") // valid
	data := b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := objNames(s); !slices.Equal(got, []string{"good", "fine"}) {
		t.Fatalf("objects %v: every invalid row is skipped, the valid rows around them stay", got)
	}
	n := 0
	offsets := map[int64]bool{}
	for _, w := range v.Warnings() {
		if w.Code == sqlitefile.WarnSchemaRowInvalid {
			n++
			offsets[w.Offset] = true
		}
	}
	if n != 11 || len(offsets) != 11 {
		t.Errorf("%d schema-row-invalid warnings at %d offsets, want one per invalid row (11): %v", n, len(offsets), v.Warnings())
	}
}

func TestSchemaHostile(t *testing.T) {
	longTable := func(name string, n int) string { // a valid statement of exactly n bytes
		sql := "CREATE TABLE " + name + "(a"
		return sql + strings.Repeat(" ", n-len(sql)-1) + ")"
	}
	open := func(t *testing.T, lim sqlitefile.Limits, build func(b *sqlitetest.Builder)) (*sqlitefile.View, *sqlitefile.Schema) {
		t.Helper()
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		build(b)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Limits: lim})
		t.Cleanup(v.Release)
		s, err := v.Schema(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v, s
	}
	t.Run("one statement over MaxSchemaSQLBytes is not parsed", func(t *testing.T) {
		const capBytes = 300
		exact := longTable("exact", capBytes)
		over := longTable("over", capBytes+1)
		v, s := open(t, sqlitefile.Limits{MaxSchemaSQLBytes: capBytes}, func(b *sqlitetest.Builder) {
			b.CreateTable("exact", exact)
			b.CreateTable("over", over)
			b.CreateTable("small", "CREATE TABLE small(a)")
		})
		if len(s.Objects) != 3 {
			t.Fatalf("%d objects", len(s.Objects))
		}
		if o := s.Objects[1]; o.Table == nil || o.Table.ParseOK || o.Table.ParseNote != "limit" || len(o.Table.Columns) != 0 {
			t.Errorf("a statement of %d bytes against a cap of %d must not be parsed: %+v", len(over), capBytes, o.Table)
		}
		if o := s.Objects[2]; o.Table == nil || !o.Table.ParseOK {
			t.Errorf("the small table after it is parsed: %+v", o.Table)
		}
		if !viewWarns(v, sqlitefile.WarnLimitReached, 0) {
			t.Errorf("no limit-reached warning: %v", v.Warnings())
		}
		if o := s.Objects[0]; o.Table == nil || !o.Table.ParseOK {
			t.Errorf("a statement of exactly the cap (%d bytes) is parsed: %+v", len(exact), o.Table)
		}
	})
	t.Run("total over MaxSchemaTotalBytes stops parsing", func(t *testing.T) {
		const each = 120
		v, s := open(t, sqlitefile.Limits{MaxSchemaSQLBytes: 1000, MaxSchemaTotalBytes: 5 * each}, func(b *sqlitetest.Builder) {
			for i := 0; i < 12; i++ {
				b.CreateTable(fmt.Sprintf("t%02d", i), longTable(fmt.Sprintf("t%02d", i), each))
			}
		})
		if len(s.Objects) != 12 {
			t.Fatalf("%d objects: the objects stay listed", len(s.Objects))
		}
		parsed := 0
		for _, o := range s.Objects {
			if o.Table != nil && o.Table.ParseOK {
				parsed++
			}
		}
		if parsed != 5 {
			t.Errorf("%d tables parsed with a total cap of exactly 5 statements", parsed)
		}
		if last := s.Objects[11]; last.Table == nil || last.Table.ParseOK || last.Table.ParseNote != "limit" {
			t.Errorf("a statement past the total cap is not parsed: %+v", last.Table)
		}
		n := 0
		for _, w := range v.Warnings() {
			if w.Code == sqlitefile.WarnLimitReached && strings.Contains(w.Msg, "total") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d total-cap warnings, want exactly one: %v", n, v.Warnings())
		}
	})
	t.Run("MaxSchemaObjects", func(t *testing.T) {
		v, s := open(t, sqlitefile.Limits{MaxSchemaObjects: 5}, func(b *sqlitetest.Builder) {
			for i := 0; i < 20; i++ {
				b.CreateTable(fmt.Sprintf("t%02d", i), fmt.Sprintf("CREATE TABLE t%02d(a)", i))
			}
		})
		if len(s.Objects) != 5 {
			t.Errorf("%d objects, want the first 5", len(s.Objects))
		}
		if !viewWarns(v, sqlitefile.WarnLimitReached, 0) {
			t.Errorf("no limit-reached warning: %v", v.Warnings())
		}
	})
	t.Run("duplicate names: the first wins", func(t *testing.T) {
		v, s := open(t, sqlitefile.Limits{}, func(b *sqlitetest.Builder) {
			b.CreateTable("Dup", "CREATE TABLE Dup(first)")
			b.AddSchemaRow("table", "dup", "dup", int64(77), "CREATE TABLE dup(second)")
			b.AddSchemaRow("view", "DUP", "DUP", int64(0), "CREATE VIEW DUP AS SELECT 1")
			b.AddSchemaRow("trigger", "dup", "Dup", int64(0), "CREATE TRIGGER dup AFTER INSERT ON Dup BEGIN SELECT 1; END")
		})
		if got := objNames(s); !slices.Equal(got, []string{"Dup", "dup"}) { // the table and (a separate namespace) the trigger
			t.Fatalf("objects %v", got)
		}
		if s.Objects[0].Table.Columns[0].Name != "first" || s.Objects[1].Type != "trigger" {
			t.Errorf("the first definition must stay: %+v", s.Objects[0].Table)
		}
		n := 0
		for _, w := range v.Warnings() {
			if w.Code == sqlitefile.WarnSchemaDuplicate {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%d schema-duplicate warnings, want 2 (the table and the view): %v", n, v.Warnings())
		}
	})
	t.Run("an index and a table share a name", func(t *testing.T) {
		_, s := open(t, sqlitefile.Limits{}, func(b *sqlitetest.Builder) {
			b.CreateTable("same", "CREATE TABLE same(a)")
			b.AddSchemaRow("index", "SAME", "same", int64(9), "CREATE INDEX SAME ON same(a)")
		})
		if len(s.Objects) != 1 {
			t.Errorf("objects %v", objNames(s))
		}
	})
}

func TestSchemaBudgetBalancedAndCancelled(t *testing.T) {
	b, _ := schemaFixture(sqlitetest.Options{PageSize: 512})
	data := b.Bytes()
	budget := newRecBudget(1 << 30)
	_, v := openLive(t, data, sqlitefile.Options{Budget: budget})
	if _, err := v.Schema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if budget.used == 0 {
		t.Error("a held schema is charged to the budget")
	}
	v.Release()
	budget.check(t)

	// A refused charge fails the call and leaves nothing charged.
	for n := 1; n <= 12; n++ {
		nb := &nthBudget{n: n}
		_, v := openLive(t, data, sqlitefile.Options{Budget: nb})
		if _, err := v.Schema(context.Background()); err != nil && !errors.Is(err, sqlitefile.ErrBudget) {
			t.Fatalf("refusing charge %d: %v", n, err)
		}
		v.Release()
		if nb.used != 0 {
			t.Errorf("refusing charge %d left %d bytes charged after Release", n, nb.used)
		}
	}

	// A cancelled context returns the error and caches nothing.
	_, v = openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Schema(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if s, err := v.Schema(context.Background()); err != nil || len(s.Objects) != 7 {
		t.Errorf("after a cancelled call: %v %v", s, err)
	}
}

// TestSchemaOfAnEmptyAndDamagedDatabase: no schema rows is an empty schema,
// and a damaged schema page yields what can be read, with warnings.
func TestSchemaOfAnEmptyAndDamagedDatabase(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	if s, err := v.Schema(context.Background()); err != nil || len(s.Objects) != 0 {
		t.Errorf("empty schema: %v %v", s, err)
	}
	b2, _ := schemaFixture(sqlitetest.Options{PageSize: 512})
	data := b2.Bytes()
	_, clean := openLive(t, data, sqlitefile.Options{})
	defer clean.Release()
	cs, err := clean.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	badPage := cs.Objects[0].Loc.Page // a leaf of the schema table
	onIt := 0
	for _, o := range cs.Objects {
		if o.Loc.Page == badPage {
			onIt++
		}
	}
	copy(data[(int(badPage)-1)*512:], bytes.Repeat([]byte{0xff}, 8)) // that leaf's b-tree header
	_, v2 := openLive(t, data, sqlitefile.Options{})
	defer v2.Release()
	s, err := v2.Schema(context.Background())
	if err != nil {
		t.Fatalf("a damaged schema page is a warning, not an error: %v", err)
	}
	if len(s.Objects) != len(cs.Objects)-onIt || onIt == 0 || !viewWarns(v2, sqlitefile.WarnPageTypeInvalid, badPage) {
		t.Errorf("%d objects (want %d), warnings %v", len(s.Objects), len(cs.Objects)-onIt, v2.Warnings())
	}
}
