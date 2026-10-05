package sqlitefile_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func tableFixture() (*sqlitetest.Builder, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	people := b.CreateTable("People", "CREATE TABLE People(id INTEGER PRIMARY KEY, name TEXT UNIQUE, age INT)")
	for i := int64(1); i <= 300; i++ {
		people.Insert(i, nil, fmt.Sprintf("name-%03d", i), i%50)
	}
	b.CreateAutoIndex("sqlite_autoindex_People_1", "People", 1)
	b.CreateIndex("idx_age", "People", "CREATE INDEX idx_age ON People(age)", 2)
	wr := b.CreateTableWithoutRowid("wr", "CREATE TABLE wr(k TEXT PRIMARY KEY, v) WITHOUT ROWID", 1)
	for i := int64(1); i <= 40; i++ {
		wr.Insert(i, fmt.Sprintf("k%02d", i), i)
	}
	b.CreateTable("broken", "CREATE TABLE broken(a, b").Insert(1, "x", int64(2))
	b.AddSchemaRow("view", "v", "v", int64(0), "CREATE VIEW v AS SELECT 1")
	b.AddSchemaRow("table", "vt", "vt", int64(0), "CREATE VIRTUAL TABLE vt USING fts5(a)")
	return b, people
}

func TestTableRowsAndGet(t *testing.T) {
	b, people := tableFixture()
	data := b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	ctx := context.Background()

	tb, err := v.Table(ctx, "People")
	if err != nil {
		t.Fatal(err)
	}
	if tb.Name() != "People" || tb.RootPage() != people.Root() || !tb.Def().ParseOK || tb.Def().RowidAlias != 0 {
		t.Errorf("name %q root %d def %+v", tb.Name(), tb.RootPage(), tb.Def())
	}
	// Rows equals ScanTree of the root.
	var viaTable, viaScan []sqlitefile.Row
	if err := tb.Rows(ctx, func(r sqlitefile.Row) bool { viaTable = append(viaTable, r.Clone()); return true }); err != nil {
		t.Fatal(err)
	}
	viaScan = scanRows(t, v, people.Root(), sqlitefile.TableTree)
	if len(viaTable) != 300 || len(viaScan) != 300 {
		t.Fatalf("%d and %d rows", len(viaTable), len(viaScan))
	}
	for i := range viaScan {
		if viaTable[i].Rowid != viaScan[i].Rowid || sameValues(viaTable[i].Values, viaScan[i].Values) != nil || viaTable[i].Loc.Offset != viaScan[i].Loc.Offset {
			t.Fatalf("row %d differs between Rows and ScanTree", i)
		}
	}
	// Rows stops early.
	n := 0
	if err := tb.Rows(ctx, func(sqlitefile.Row) bool { n++; return n < 7 }); err != nil || n != 7 {
		t.Errorf("early stop: %d rows, %v", n, err)
	}
	// Get.
	for _, id := range []int64{1, 150, 300} {
		r, ok, err := tb.Get(ctx, id)
		if err != nil || !ok || r.Rowid != id {
			t.Fatalf("Get(%d): %v %v", id, ok, err)
		}
		if got := tb.Resolve(r); valStr(got[0]) != fmt.Sprintf("i:%d", id) || valStr(got[1]) != fmt.Sprintf("t:name-%03d", id) {
			t.Errorf("Get(%d) resolves to %v", id, got)
		}
	}
	if _, ok, err := tb.Get(ctx, 301); ok || err != nil {
		t.Errorf("Get of an absent rowid: %v %v", ok, err)
	}
	// Names: ASCII case-insensitive.
	for _, name := range []string{"people", "PEOPLE", "pEoPlE"} {
		if got, err := v.Table(ctx, name); err != nil || got.RootPage() != people.Root() {
			t.Errorf("Table(%q): %v", name, err)
		}
	}
	// WITHOUT ROWID: Rows reads the index-shaped tree; Get is refused.
	wr, err := v.Table(ctx, "wr")
	if err != nil {
		t.Fatal(err)
	}
	cnt := 0
	if err := wr.Rows(ctx, func(r sqlitefile.Row) bool {
		cnt++
		if r.HasRowid || len(wr.Resolve(r)) != 2 {
			t.Errorf("WITHOUT ROWID row %+v", r)
		}
		return true
	}); err != nil || cnt != 40 {
		t.Errorf("WITHOUT ROWID Rows: %d rows, %v", cnt, err)
	}
	if _, _, err := wr.Get(ctx, 1); !errors.Is(err, sqlitefile.ErrWithoutRowid) {
		t.Errorf("Get on WITHOUT ROWID: %v", err)
	}
	// An unparseable definition still reads, kind chosen from the page.
	br, err := v.Table(ctx, "broken")
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := resolvedRows(t, br); !slices.Equal(rows, []string{"1|t:x|i:2"}) {
		t.Errorf("broken: %q", rows)
	}
	if r, ok, err := br.Get(ctx, 1); err != nil || !ok || r.Rowid != 1 {
		t.Errorf("Get on an unparseable table: %v %v", ok, err)
	}
	// Unknown names and non-tables.
	for _, name := range []string{"nope", "", "idx_age", "v", "vt", "sqlite_autoindex_People_1"} {
		if _, err := v.Table(ctx, name); !errors.Is(err, sqlitefile.ErrNotFound) {
			t.Errorf("Table(%q): err %v, want ErrNotFound", name, err)
		}
	}
	// sqlite_master is the schema table under its old name.
	for _, name := range []string{"sqlite_master", "SQLITE_MASTER", "sqlite_schema"} {
		sm, err := v.Table(ctx, name)
		if err != nil {
			t.Fatalf("Table(%q): %v", name, err)
		}
		if sm.RootPage() != 1 || len(sm.Def().Columns) != 5 || sm.Def().Columns[3].Name != "rootpage" {
			t.Errorf("%s: root %d def %+v", name, sm.RootPage(), sm.Def())
		}
		rows := 0
		if err := sm.Rows(ctx, func(sqlitefile.Row) bool { rows++; return true }); err != nil || rows != 7 {
			t.Errorf("%s: %d rows, %v", name, rows, err)
		}
	}
}

func TestIndexEntries(t *testing.T) {
	b, people := tableFixture()
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	ctx := context.Background()
	for _, name := range []string{"idx_age", "sqlite_autoindex_People_1", "IDX_AGE"} {
		ix, err := v.Index(ctx, name)
		if err != nil {
			t.Fatalf("Index(%q): %v", name, err)
		}
		var rowids []int64
		count := 0
		err = ix.Entries(ctx, func(r sqlitefile.Row) bool {
			count++
			last := r.Values[len(r.Values)-1]
			if r.HasRowid || last.Kind != sqlitefile.KindInt {
				t.Errorf("%s: an entry of a rowid table ends with the rowid: %+v", name, r.Values)
			}
			rowids = append(rowids, last.Int)
			return true
		})
		if err != nil || count != 300 {
			t.Fatalf("%s: %d entries, %v: a full index has one entry per row", name, count, err)
		}
		slices.Sort(rowids)
		for i, id := range rowids {
			if id != int64(i+1) {
				t.Fatalf("%s: rowid %d at position %d", name, id, i)
			}
		}
	}
	ix, _ := v.Index(ctx, "idx_age")
	if d := ix.Def(); !d.ParseOK || d.Table != "People" || len(d.Columns) != 1 || d.Columns[0].Name != "age" {
		t.Errorf("def %+v", d)
	}
	auto, _ := v.Index(ctx, "sqlite_autoindex_People_1")
	if d := auto.Def(); !d.Auto || d.ParseOK {
		t.Errorf("auto def %+v", d)
	}
	// Entries are in index order: by age, then rowid.
	prev := int64(-1)
	_ = ix.Entries(ctx, func(r sqlitefile.Row) bool {
		if age := r.Values[0].Int; age < prev {
			t.Errorf("age %d after %d", age, prev)
		} else {
			prev = age
		}
		return true
	})
	for _, name := range []string{"nope", "", "People", "v", "vt"} {
		if _, err := v.Index(ctx, name); !errors.Is(err, sqlitefile.ErrNotFound) {
			t.Errorf("Index(%q): err %v, want ErrNotFound", name, err)
		}
	}
	_ = people
}

// TestTableAndIndexCancelAndBudget: a cancelled context ends Rows and Table
// lookups, and the books balance.
func TestTableAndIndexCancelAndBudget(t *testing.T) {
	b, _ := tableFixture()
	budget := newRecBudget(1 << 30)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: budget})
	ctx, cancel := context.WithCancel(context.Background())
	tb, err := v.Table(ctx, "People")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = tb.Rows(ctx, func(sqlitefile.Row) bool {
		if n++; n == 5 {
			cancel()
		}
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Rows after cancel: %v", err)
	}
	if _, err := v.Table(ctx, "People"); err != nil { // the schema is already cached: no I/O to cancel
		t.Logf("Table with a cancelled context: %v", err)
	}
	v.Release()
	budget.check(t)
}
