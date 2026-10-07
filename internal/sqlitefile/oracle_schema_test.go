package sqlitefile_test

// Engine oracle, schema and layout (plan 3I, Task 13).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// liveFixture opens the engine-built database of scenario s in the library.
func liveFixture(t *testing.T, s liveScenario) (path string, data []byte, d *sqlitefile.DB, v *sqlitefile.View) {
	t.Helper()
	path = buildLiveDB(t, s)
	data = readFileRetry(t, path)
	d = openLibrary(t, data, nil, nil)
	v = d.Live()
	t.Cleanup(v.Release)
	return path, data, d, v
}

var schemaScenarios = []liveScenario{{4096, "UTF-8", 0}, {512, "UTF-16le", 1}}

// TestSchemaMatchesEngineXinfo (the brief's TestSchemaMatchesEngine; that name is taken by Task 5): pragma table_xinfo per table (names, declared types,
// not-null, default text, pk ordinal, hidden/generated flags) equals Def(); a
// rowid alias is detected exactly when the engine stores the column as NULL and
// reads it as the rowid.
func TestSchemaMatchesEngineXinfo(t *testing.T) {
	for _, s := range schemaScenarios {
		t.Run(s.String(), func(t *testing.T) {
			path, _, _, v := liveFixture(t, s)
			e := openEngine(t, path)
			ctx := context.Background()
			for _, n := range engineTableNames(t, e) {
				tb, err := v.Table(ctx, n)
				if err != nil {
					t.Fatalf("table %s: %v", n, err)
				}
				def := tb.Def()
				if !def.ParseOK {
					t.Errorf("table %s: definition not parsed (%s)", n, def.ParseNote)
					continue
				}
				xi := engineXinfo(t, e, n)
				if len(xi) != len(def.Columns) {
					t.Errorf("table %s: %d columns, engine %d", n, len(def.Columns), len(xi))
					continue
				}
				if wr := engineWithoutRowid(t, e, n); wr != def.WithoutRowid {
					t.Errorf("table %s: WithoutRowid %v, engine %v", n, def.WithoutRowid, wr)
				}
				for i, c := range xi {
					lc := def.Columns[i]
					where := fmt.Sprintf("table %s column %d (%s)", n, i, c.name)
					if lc.Name != c.name {
						t.Errorf("%s: name %q", where, lc.Name)
					}
					if !strings.EqualFold(strings.TrimSpace(lc.DeclType), strings.TrimSpace(c.typ)) {
						t.Errorf("%s: declared type %q, engine %q", where, lc.DeclType, c.typ)
					}
					if lc.NotNull != c.notnull {
						t.Errorf("%s: not-null %v, engine %v", where, lc.NotNull, c.notnull)
					}
					if lc.PKOrdinal != c.pk {
						t.Errorf("%s: pk ordinal %d, engine %d", where, lc.PKOrdinal, c.pk)
					}
					if c.dflt.Valid != (lc.Default.Kind != sqlitefile.DefaultNone) {
						t.Errorf("%s: default kind %d, engine default %v", where, lc.Default.Kind, c.dflt)
					} else if c.dflt.Valid && strings.TrimSpace(lc.Default.Text) != strings.TrimSpace(c.dflt.String) {
						t.Errorf("%s: default text %q, engine %q", where, lc.Default.Text, c.dflt.String)
					}
					wantGen := map[int]sqlitefile.GenKind{0: sqlitefile.GenNone, 2: sqlitefile.GenVirtual, 3: sqlitefile.GenStored}[c.hidden]
					if lc.Generated != wantGen {
						t.Errorf("%s: generated %d, engine hidden %d", where, lc.Generated, c.hidden)
					}
					if (lc.RecordIndex < 0) != (c.hidden == 2) {
						t.Errorf("%s: record index %d, engine hidden %d", where, lc.RecordIndex, c.hidden)
					}
				}
			}
			checkRowidAlias(t, e, v)
		})
	}
}

// checkRowidAlias asks the engine, on the open database, which of the integer
// primary key tables treat the column as the rowid: a NULL inserted into an
// alias becomes the new rowid, into a non-alias it stays NULL.
func checkRowidAlias(t *testing.T, e *sql.DB, v *sqlitefile.View) {
	t.Helper()
	ctx := context.Background()
	alias := map[string]bool{}
	for _, n := range []string{"al", "dsc", "sq", "over", "overt"} {
		mustExec(t, e, mustSQL(aliasInsertSQL, n))
		var isNull int
		if err := e.QueryRow(mustSQL(aliasProbeSQL, n)).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		alias[n] = isNull == 0
	}
	if !alias["al"] || alias["dsc"] || !alias["sq"] || !alias["over"] || !alias["overt"] {
		t.Fatalf("engine probe: alias %v", alias)
	}
	for _, n := range engineTableNames(t, e) {
		tb, err := v.Table(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		def := tb.Def()
		if got := def.RowidAlias >= 0; got != alias[n] {
			t.Errorf("table %s: rowid alias %v, engine %v", n, got, alias[n])
		}
	}
}

// TestLayoutMatchesDbstat: for every page dbstat reaches, the page type and the
// owning object agree with Layout.Class and Owner; no page is an orphan; the
// freelist equals PRAGMA freelist_count.
func TestLayoutMatchesDbstat(t *testing.T) {
	for _, s := range schemaScenarios {
		t.Run(s.String(), func(t *testing.T) {
			path, _, _, v := liveFixture(t, s)
			e := openEngine(t, path)
			ctx := context.Background()
			lay, err := v.Layout(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sch, err := v.Schema(ctx)
			if err != nil {
				t.Fatal(err)
			}
			class := map[string]sqlitefile.PageClass{"internal": sqlitefile.ClassBTreeInterior, "leaf": sqlitefile.ClassBTreeLeaf, "overflow": sqlitefile.ClassOverflow}
			rows, err := e.Query("select name, pageno, pagetype from dbstat")
			if err != nil {
				t.Fatal(err)
			}
			seen := map[uint32]bool{}
			for rows.Next() {
				var name, typ string
				var pg uint32
				if err := rows.Scan(&name, &pg, &typ); err != nil {
					t.Fatal(err)
				}
				seen[pg] = true
				if pg == 0 || pg > lay.Addressable {
					t.Errorf("dbstat page %d outside 1..%d", pg, lay.Addressable)
					continue
				}
				if lay.Class[pg] != class[typ] {
					t.Errorf("page %d (%s): Layout class %s, dbstat %s", pg, name, lay.Class[pg], typ)
				}
				o := lay.Owner[pg]
				got := ""
				switch {
				case o == 1:
					got = "sqlite_schema"
				case o >= 2 && int(o)-2 < len(sch.Objects):
					got = sch.Objects[o-2].Name
				}
				if !strings.EqualFold(got, name) {
					t.Errorf("page %d: Layout owner %q, dbstat %q", pg, got, name)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			var freelist uint32
			for pg := uint32(1); pg <= lay.Addressable; pg++ {
				if seen[pg] {
					continue
				}
				switch lay.Class[pg] {
				case sqlitefile.ClassFreelistTrunk, sqlitefile.ClassFreelistLeaf:
					freelist++
				case sqlitefile.ClassPtrmap, sqlitefile.ClassLockByte:
				default:
					t.Errorf("page %d is %s but dbstat does not reach it and it is not free or ptrmap", pg, lay.Class[pg])
				}
			}
			want, err := e.Query("pragma freelist_count")
			if err != nil {
				t.Fatal(err)
			}
			var fc uint32
			if want.Next() {
				_ = want.Scan(&fc)
			}
			_ = want.Close()
			if freelist != fc {
				t.Errorf("freelist pages %d, engine freelist_count %d", freelist, fc)
			}
			if len(lay.Orphans) != 0 || len(lay.Problems) != 0 {
				t.Errorf("orphans %v problems %v", lay.Orphans, lay.Problems)
			}
		})
	}
}

// TestIndexEntriesMatchEngine: the number of entries of every non-partial index
// equals the engine's count through INDEXED BY.
func TestIndexEntriesMatchEngine(t *testing.T) {
	for _, s := range schemaScenarios {
		t.Run(s.String(), func(t *testing.T) {
			path, _, _, v := liveFixture(t, s)
			e := openEngine(t, path)
			rows, err := e.Query("select name, tbl_name, sql from sqlite_schema where type = 'index'")
			if err != nil {
				t.Fatal(err)
			}
			type ix struct{ name, table, sql string }
			var idx []ix
			for rows.Next() {
				var x ix
				var q sql.NullString
				if err := rows.Scan(&x.name, &x.table, &q); err != nil {
					t.Fatal(err)
				}
				x.sql = q.String
				idx = append(idx, x)
			}
			_ = rows.Close()
			if len(idx) < 4 {
				t.Fatalf("only %d indexes", len(idx))
			}
			ctx := context.Background()
			checked := 0
			for _, x := range idx {
				if strings.Contains(strings.ToLower(x.sql), " where ") {
					continue // a partial index holds only some rows: not counted this way
				}
				var want int
				q := "select count(*) from " + quoteIdent(x.table) + " indexed by " + quoteIdent(x.name)
				if err := e.QueryRow(q).Scan(&want); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				ixv, err := v.Index(ctx, x.name)
				if err != nil {
					t.Fatalf("index %s: %v", x.name, err)
				}
				got := 0
				if err := ixv.Entries(ctx, func(sqlitefile.Row) bool { got++; return true }); err != nil {
					t.Fatalf("entries of %s: %v", x.name, err)
				}
				if got != want {
					t.Errorf("index %s: %d entries, engine %d", x.name, got, want)
				}
				checked++
			}
			if checked < 3 {
				t.Errorf("only %d indexes compared", checked)
			}
		})
	}
}
