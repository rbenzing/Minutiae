package sqlitedb_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// oneTableDB builds a database with the one table t defined by sql.
func oneTableDB(t testing.TB, sql string) *sqlitedb.DB {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", sql)
	})
	return openBytes(t, data, nil, nil, bigBudget())
}

func TestTableResolvesColumnsByName(t *testing.T) {
	d := oneTableDB(t, "create table t(id integer primary key, name text not null, n int default 5, d)")
	tb, err := d.Table(t.Context(), "T", []string{"name"}, []string{"n", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if tb.Name() != "t" || tb.WithoutRowid() || tb.RootPage() < 2 {
		t.Errorf("name %q without-rowid %v root %d", tb.Name(), tb.WithoutRowid(), tb.RootPage())
	}
	if tb.Col("NAME") != 1 || tb.Col("n") != 2 || tb.Col("nope") != -1 || tb.Col("") != -1 {
		t.Errorf("Col: %d %d %d", tb.Col("NAME"), tb.Col("n"), tb.Col("nope"))
	}
	cols := tb.Cols()
	if len(cols) != 4 {
		t.Fatalf("%d columns", len(cols))
	}
	if !cols[0].RowidAlias || cols[1].RowidAlias || !cols[1].NotNull || cols[1].Affinity != sqlitefile.AffText ||
		cols[2].Affinity != sqlitefile.AffInteger || cols[2].Default != sqlitefile.DefaultLiteral ||
		cols[3].Default != sqlitefile.DefaultNone || cols[3].DeclType != "" {
		t.Errorf("cols = %+v", cols)
	}
	cols[0].Name = "changed"
	if tb.Cols()[0].Name != "id" {
		t.Error("Cols returned the table's own slice")
	}
}

func TestColumnNamesCaseAndQuoting(t *testing.T) {
	d := oneTableDB(t, "create table t(\"a b\", \"é\", \"Ünï\", rowid text, [bracket], `tick`)")
	tb, err := d.Table(t.Context(), "t", []string{"a b", "é", "Ünï", "rowid", "bracket", "tick"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range []string{"A B", "é", "ÜNï", "ROWID", "Bracket", "TICK"} {
		if tb.Col(n) != i {
			t.Errorf("Col(%q) = %d, want %d", n, tb.Col(n), i)
		}
	}
	for _, n := range []string{"É", "ÜNÏ", "ünï"} {
		if tb.Col(n) != -1 {
			t.Errorf("Col(%q) = %d, want -1 (non-ASCII bytes compare exactly)", n, tb.Col(n))
		}
	}
}

func TestMissingNeedColumnIsUnsupportedSchema(t *testing.T) {
	d := oneTableDB(t, "create table t(a, b)")
	tb, err := d.Table(t.Context(), "t", []string{"b", "x", "A", "y", "x"}, nil)
	if tb != nil {
		t.Error("non-nil table")
	}
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(err, &us) || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Fatalf("err = %v", err)
	}
	if us.Table != "t" || !slices.Equal(us.Missing, []string{"x", "y"}) {
		t.Errorf("Table %q Missing %v", us.Table, us.Missing)
	}
}

func TestUnparsedDefinitionIsUnsupportedNotMissing(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.AddSchemaRow("table", "broken", "broken", int64(2), "create table broken(((")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "broken", nil, nil)
	var us *sqlitedb.UnsupportedSchemaError
	if tb != nil || !errors.As(err, &us) {
		t.Fatalf("Table = %v, %v", tb, err)
	}
	if len(us.Missing) != 0 || us.Table != "broken" || !strings.Contains(us.Reason, "definition not parsed") {
		t.Errorf("err = %+v", us)
	}
}

func TestNoSuchTable(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)")
		b.AddSchemaRow("view", "v", "v", int64(0), "create view v as select 1")
		b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(a)")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	for _, n := range []string{"nothing", "v", "vt", ""} {
		tb, err := d.Table(t.Context(), n, nil, nil)
		if tb != nil || !errors.Is(err, sqlitedb.ErrNoSuchTable) || errors.Is(err, sqlitedb.ErrCorrupt) {
			t.Errorf("Table(%q) = %v, %v", n, tb, err)
		}
	}
}

func TestTableNotFoundAfterDamagedSchemaIsNotAbsent(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)")
		b.AddSchemaRow("bogus", "x", "x", int64(0), nil)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	_, err := d.Table(t.Context(), "gone", nil, nil)
	if !errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Fatalf("err = %v", err)
	}
}

func TestImpossibleRootPageIsErrCorrupt(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.AddSchemaRow("table", "far", "far", int64(0x7ffffff0), "create table far(a)")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "far", nil, nil)
	if tb != nil || !errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Fatalf("Table = %v, %v", tb, err)
	}
}

func TestRowidAliasRules(t *testing.T) {
	cases := []struct {
		sql   string
		alias bool
	}{
		{"create table t(id integer primary key, x)", true},
		{"create table t(x, id INTEGER PRIMARY KEY)", true},
		{"create table t(id integer primary key desc, x)", false},
		{"create table t(id int primary key, x)", false},
		{"create table t(a integer, b integer, primary key(a, b))", false},
		{"create table t(id integer primary key, x) without rowid", false},
		{"create table t(x)", false},
	}
	for _, c := range cases {
		d := oneTableDB(t, c.sql)
		tb, err := d.Table(t.Context(), "t", nil, nil)
		if err != nil {
			t.Errorf("%q: %v", c.sql, err)
			continue
		}
		got := slices.ContainsFunc(tb.Cols(), func(ci sqlitedb.ColumnInfo) bool { return ci.RowidAlias })
		if got != c.alias {
			t.Errorf("%q: alias = %v, want %v", c.sql, got, c.alias)
		}
	}
	// Table-level primary key(id) on an integer column: the brief lists it as
	// "not an alias", but the library reports an alias (as the engine does).
	// The library is authoritative; Task 10 compares it with the engine.
	d := oneTableDB(t, "create table t(id integer, x, primary key(id))")
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil || !tb.Cols()[0].RowidAlias {
		t.Errorf("table-level primary key(id): %v, %v", tb, err)
	}
}

func TestWithoutRowidFlag(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("r", "create table r(a, b)")
		b.CreateTableWithoutRowid("w", "create table w(a primary key, b) without rowid", 1)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	for n, want := range map[string]bool{"r": false, "w": true} {
		tb, err := d.Table(t.Context(), n, nil, nil)
		if err != nil || tb.WithoutRowid() != want {
			t.Errorf("%s: %v, %v", n, tb, err)
		}
	}
}

func TestVirtualGeneratedColumnIsMarkedVirtual(t *testing.T) {
	d := oneTableDB(t, "create table t(a, v as (a+1) virtual, s as (a+2) stored, b)")
	tb, err := d.Table(t.Context(), "t", []string{"a", "v", "s", "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := tb.Cols()
	if c[0].Virtual || !c[1].Virtual || c[2].Virtual || c[3].Virtual {
		t.Errorf("Virtual: %v %v %v %v", c[0].Virtual, c[1].Virtual, c[2].Virtual, c[3].Virtual)
	}
}

func TestDuplicateRequestedNamesResolveOnce(t *testing.T) {
	d := oneTableDB(t, "create table t(a, b)")
	tb, err := d.Table(t.Context(), "t", []string{"a", "A", "a"}, []string{"b", "B"})
	if err != nil || tb == nil || tb.Col("a") != 0 || tb.Col("b") != 1 {
		t.Fatalf("Table = %v, %v", tb, err)
	}
	_, err = d.Table(t.Context(), "t", []string{"z", "Z", "z"}, nil)
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(err, &us) || !slices.Equal(us.Missing, []string{"z"}) {
		t.Errorf("err = %v", err)
	}
}

func TestColumnCollationInfo(t *testing.T) {
	d := oneTableDB(t, "create table t(x text collate nocase, y text, z, primary key(y collate rtrim))")
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := tb.Cols()
	if !strings.EqualFold(c[0].Collation, "nocase") || c[0].Collation == "" {
		t.Errorf("x collation %q", c[0].Collation)
	}
	if !strings.EqualFold(c[1].Collation, "rtrim") || c[1].Collation == "" {
		t.Errorf("y collation %q", c[1].Collation)
	}
	if c[2].Collation != "" {
		t.Errorf("z collation %q", c[2].Collation)
	}
}

func TestTableAfterReleaseIsErrReleasedAndChargesNothing(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)")
	})
	bud := bigBudget()
	d := openBytes(t, data, nil, nil, bud)
	d.Release()
	if bud.used != 0 {
		t.Fatalf("used after Release = %d", bud.used)
	}
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if tb != nil || !errors.Is(err, sqlitedb.ErrReleased) {
		t.Fatalf("Table after Release = %v, %v", tb, err)
	}
	if bud.used != 0 {
		t.Errorf("Table after Release charged %d", bud.used)
	}
	d.Release() // a second Release is a no-op
	if bud.used != 0 {
		t.Errorf("used after second Release = %d", bud.used)
	}
	names, err := d.Tables(t.Context())
	if err != nil || !slices.Equal(names, []string{"t"}) {
		t.Errorf("Tables after Release = %v, %v", names, err)
	}
	if bud.used != 0 {
		t.Errorf("Tables charged %d", bud.used)
	}
}

func TestTableCancelledContextIsReturnedBeforeAnyWork(t *testing.T) {
	d := oneTableDB(t, "create table t(a)")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tb, err := d.Table(ctx, "t", nil, nil)
	if tb != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Table = %v, %v", tb, err)
	}
}

func TestKeyCollationWinsOverColumnCollation(t *testing.T) {
	d := oneTableDB(t, "create table t(x text collate nocase, primary key(x collate rtrim))")
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.Cols()[0].Collation; !strings.EqualFold(got, "rtrim") {
		t.Errorf("collation %q, want rtrim", got)
	}
}

func TestDeclTypeIsReported(t *testing.T) {
	d := oneTableDB(t, "create table t(a, b text, c varchar(10))")
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := tb.Cols()
	if c[0].DeclType != "" || c[1].DeclType != "text" || !strings.EqualFold(c[2].DeclType, "varchar(10)") {
		t.Errorf("DeclTypes %q %q %q", c[0].DeclType, c[1].DeclType, c[2].DeclType)
	}
}

func TestEmptyNeedNameIsMissing(t *testing.T) {
	d := oneTableDB(t, "create table t(a)")
	_, err := d.Table(t.Context(), "t", []string{""}, nil)
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(err, &us) || !slices.Equal(us.Missing, []string{""}) {
		t.Fatalf("err = %v", err)
	}
}
