package sqlitefile_test

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// ---- builders ----

const auto = -2 // RecordIndex placeholder: assigned in declaration order

func vNull() sqlitefile.Value { return sqlitefile.Value{} }
func vInt(n int64) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: n}
}

func vFloat(f float64) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: f}
}

func vText(s string) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s))}
}

func vBlob(b ...byte) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: b, Len: int64(len(b))}
}

func fcol(name string, aff sqlitefile.Affinity) sqlitefile.Column {
	return sqlitefile.Column{Name: name, Affinity: aff, RecordIndex: auto}
}

func notNull(c sqlitefile.Column) sqlitefile.Column { c.NotNull = true; return c }

func withDefault(c sqlitefile.Column) sqlitefile.Column {
	c.Default = sqlitefile.Default{Kind: sqlitefile.DefaultLiteral, Value: vInt(0)}
	return c
}

func withExprDefault(c sqlitefile.Column) sqlitefile.Column {
	c.Default = sqlitefile.Default{Kind: sqlitefile.DefaultExpr, Text: "CURRENT_TIMESTAMP"}
	return c
}

func pk(c sqlitefile.Column, ord int) sqlitefile.Column { c.PKOrdinal = ord; return c }

func recIdx(c sqlitefile.Column, i int) sqlitefile.Column { c.RecordIndex = i; return c }

func tableDef(without bool, alias int, cols ...sqlitefile.Column) *sqlitefile.TableDef {
	cs := append([]sqlitefile.Column(nil), cols...)
	n := 0
	for i := range cs {
		if cs[i].RecordIndex == auto {
			cs[i].RecordIndex = n
		}
		if cs[i].RecordIndex >= 0 {
			n++
		}
	}
	return &sqlitefile.TableDef{Columns: cs, WithoutRowid: without, RowidAlias: alias, StoredColumns: n, ParseOK: true}
}

func tableObj(name string, def *sqlitefile.TableDef) sqlitefile.SchemaObject {
	return sqlitefile.SchemaObject{Type: "table", Name: name, TblName: name, RootPage: 2, Table: def}
}

func indexObj(name, table string, cols ...sqlitefile.IndexColumn) sqlitefile.SchemaObject {
	return sqlitefile.SchemaObject{Type: "index", Name: name, TblName: table, RootPage: 3, Index: &sqlitefile.IndexDef{Table: table, Columns: cols, ParseOK: true}}
}

func schemaOf(objs ...sqlitefile.SchemaObject) *sqlitefile.Schema {
	return &sqlitefile.Schema{Objects: objs}
}

func names(s ...string) []string { return s }

// ---- table fit ----

func TestFitStrictAndLoose(t *testing.T) {
	I, T, B := sqlitefile.AffInteger, sqlitefile.AffText, sqlitefile.AffBlob
	tab := func(without bool, alias int, cols ...sqlitefile.Column) *sqlitefile.Schema {
		return schemaOf(tableObj("t", tableDef(without, alias, cols...)))
	}
	unparsed := tableObj("t", &sqlitefile.TableDef{RowidAlias: -1, ParseNote: "unparsed"})
	one := names("t")
	cases := []struct {
		name          string
		s             *sqlitefile.Schema
		v             []sqlitefile.Value
		strict, loose []string
	}{
		{"exact", tab(false, -1, fcol("a", I), fcol("b", T)), []sqlitefile.Value{vInt(1), vText("x")}, one, one},
		{"one value", tab(false, -1, fcol("a", I), notNull(fcol("b", T))), []sqlitefile.Value{vInt(1)}, nil, one},
		{"one extra value", tab(false, -1, fcol("a", I), fcol("b", T)), []sqlitefile.Value{vInt(1), vText("x"), vInt(3)}, nil, nil},
		{"no values", tab(false, -1, fcol("a", I)), nil, nil, nil},
		{"missing nullable", tab(false, -1, fcol("a", I), fcol("b", T), fcol("c", I)), []sqlitefile.Value{vInt(1), vText("x")}, one, one},
		{"missing defaulted not null", tab(false, -1, fcol("a", I), fcol("b", T), withDefault(notNull(fcol("c", I)))), []sqlitefile.Value{vInt(1), vText("x")}, one, one},
		{"missing not null no default", tab(false, -1, fcol("a", I), fcol("b", T), notNull(fcol("c", I))), []sqlitefile.Value{vInt(1), vText("x")}, nil, one},
		{"missing trailing rowid alias is not addable", tab(false, 1, fcol("a", I), fcol("k", I)), []sqlitefile.Value{vText("x")}, nil, one},
		{"missing not null expr default", tab(false, -1, fcol("a", I), fcol("b", T), withExprDefault(notNull(fcol("c", I)))), []sqlitefile.Value{vInt(1), vText("x")}, nil, one},
		{"addable column before a non-addable one", tab(false, -1, fcol("a", I), fcol("b", T), notNull(fcol("c", I))), []sqlitefile.Value{vInt(1)}, nil, one},
		{"not null violated", tab(false, -1, fcol("a", I), notNull(fcol("b", T))), []sqlitefile.Value{vInt(1), vNull()}, nil, one},
		{"rowid alias holds a value", tab(false, 0, fcol("a", I), fcol("b", T)), []sqlitefile.Value{vInt(5), vText("x")}, nil, one},
		{"rowid alias null", tab(false, 0, fcol("a", I), fcol("b", T)), []sqlitefile.Value{vNull(), vText("x")}, one, one},
		{"text column holds an integer", tab(false, -1, fcol("a", I), fcol("b", T)), []sqlitefile.Value{vInt(1), vInt(2)}, nil, nil},
		{"text column holds a float", tab(false, -1, fcol("b", T)), []sqlitefile.Value{vFloat(1.5)}, nil, nil},
		{"text column holds a blob", tab(false, -1, fcol("b", T)), []sqlitefile.Value{vBlob(1, 2)}, one, one},
		{"integer column holds text", tab(false, -1, fcol("a", I)), []sqlitefile.Value{vText("x")}, one, one},
		{"integer column holds a blob", tab(false, -1, fcol("a", I)), []sqlitefile.Value{vBlob(9)}, one, one},
		{"blob column holds anything", tab(false, -1, fcol("a", B), fcol("b", B), fcol("c", B), fcol("d", B)), []sqlitefile.Value{vInt(1), vFloat(2), vText("x"), vNull()}, one, one},
		{"numeric and real hold text", tab(false, -1, fcol("a", sqlitefile.AffNumeric), fcol("b", sqlitefile.AffReal)), []sqlitefile.Value{vText("x"), vText("y")}, one, one},
		{
			// declared (x TEXT, k INTEGER PRIMARY KEY) WITHOUT ROWID stores k first
			"without rowid honours record order",
			tab(true, -1, recIdx(fcol("x", T), 1), recIdx(pk(notNull(fcol("k", I)), 1), 0)),
			[]sqlitefile.Value{vInt(7), vText("x")},
			one, one,
		},
		{
			"without rowid declared order is not the stored order",
			tab(true, -1, recIdx(fcol("x", T), 1), recIdx(pk(notNull(fcol("k", I)), 1), 0)),
			[]sqlitefile.Value{vText("x"), vInt(7)},
			nil, nil,
		},
		{"without rowid key not null", tab(true, -1, recIdx(fcol("x", T), 1), recIdx(pk(notNull(fcol("k", I)), 1), 0)), []sqlitefile.Value{vNull(), vText("x")}, nil, one},
		{
			"virtual generated column is not stored",
			tab(false, -1, fcol("a", I), recIdx(fcol("g", T), -1), fcol("b", T)),
			[]sqlitefile.Value{vInt(1), vText("x")},
			one, one,
		},
		{"a table that failed to parse fits nothing", schemaOf(unparsed), []sqlitefile.Value{vInt(1)}, nil, nil},
		{"views and indexes are not tables", schemaOf(sqlitefile.SchemaObject{Type: "view", Name: "t"}), []sqlitefile.Value{vInt(1)}, nil, nil},
	}
	for _, c := range cases {
		if got := c.s.FitTables(c.v, sqlitefile.FitStrict); !reflect.DeepEqual(got, c.strict) {
			t.Errorf("%s: strict %v, want %v", c.name, got, c.strict)
		}
		if got := c.s.FitTables(c.v, sqlitefile.FitLoose); !reflect.DeepEqual(got, c.loose) {
			t.Errorf("%s: loose %v, want %v", c.name, got, c.loose)
		}
	}
}

// TestFitAmbiguousIsNone: two tables of one shape never yield a pick.
func TestFitAmbiguousIsNone(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	s := schemaOf(
		tableObj("a", tableDef(false, -1, fcol("x", I), fcol("y", T))),
		tableObj("b", tableDef(false, -1, fcol("x", I), fcol("y", T))),
		tableObj("c", tableDef(false, -1, fcol("x", I))),
	)
	v := []sqlitefile.Value{vInt(1), vText("q")}
	if got := s.FitTables(v, sqlitefile.FitStrict); !reflect.DeepEqual(got, names("a", "b")) {
		t.Errorf("strict %v, want a b in schema order", got)
	}
	if tb, basis := s.FitPage([][]sqlitefile.Value{v}); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("FitPage ambiguous: %q %q, want none", tb, basis)
	}
	// loose ambiguity too
	if tb, basis := s.FitPage([][]sqlitefile.Value{{vNull(), vNull()}}); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("FitPage loose ambiguous: %q %q", tb, basis)
	}
	// a unique strict match is fit, a unique loose one is guess
	u := schemaOf(
		tableObj("a", tableDef(false, -1, fcol("x", I), notNull(fcol("y", T)))),
		tableObj("c", tableDef(false, -1, fcol("x", I))),
	)
	if tb, basis := u.FitPage([][]sqlitefile.Value{{vInt(1), vText("q")}}); tb != "a" || basis != sqlitefile.BasisFit {
		t.Errorf("unique strict: %q %q, want a fit", tb, basis)
	}
	if tb, basis := u.FitPage([][]sqlitefile.Value{{vInt(1), vNull()}}); tb != "a" || basis != sqlitefile.BasisGuess {
		t.Errorf("unique loose: %q %q, want a guess", tb, basis)
	}
}

func TestFitPageIntersectsCells(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	s := schemaOf(
		tableObj("t1", tableDef(false, -1, fcol("a", I), fcol("b", T))),
		tableObj("t2", tableDef(false, -1, fcol("a", I), notNull(fcol("b", T)))),
	)
	row := func(n int64) []sqlitefile.Value { return []sqlitefile.Value{vInt(n), vText("x")} }
	if tb, basis := s.FitPage([][]sqlitefile.Value{row(1), row(2)}); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("two candidates remain: %q %q", tb, basis)
	}
	outlier := []sqlitefile.Value{vInt(3), vNull()} // not null in t2 is violated
	page := [][]sqlitefile.Value{row(1), nil, row(2), outlier, {}}
	if tb, basis := s.FitPage(page); tb != "t1" || basis != sqlitefile.BasisFit {
		t.Errorf("outlier removes t2: %q %q, want t1 fit", tb, basis)
	}
	// an outlier that fits no table removes everything
	if tb, basis := s.FitPage([][]sqlitefile.Value{row(1), {vText("z"), vInt(9), vInt(9)}}); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("no table fits every cell: %q %q", tb, basis)
	}
	// a later cell of another length removes the candidate that needs exactly that many
	if tb, basis := s.FitPage([][]sqlitefile.Value{row(1), {vInt(1)}}); tb != "t1" || basis != sqlitefile.BasisFit {
		t.Errorf("short cell: %q %q, want t1 fit (t2 needs its not null column)", tb, basis)
	}
	// nothing but non-record cells
	if tb, basis := s.FitPage([][]sqlitefile.Value{nil, {}}); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("no record cell: %q %q", tb, basis)
	}
	if tb, basis := s.FitPage(nil); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("no cell: %q %q", tb, basis)
	}
}

// ---- index fit ----

func TestFitIndexes(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	col := func(n string) sqlitefile.IndexColumn { return sqlitefile.IndexColumn{Name: n} }
	expr := sqlitefile.IndexColumn{Expr: true}
	rowid := schemaOf(
		tableObj("t", tableDef(false, -1, fcol("a", I), fcol("b", T), fcol("c", I))),
		indexObj("i_b", "t", col("b")),
		indexObj("i_ae", "t", col("a"), expr),
		sqlitefile.SchemaObject{Type: "index", Name: "sqlite_autoindex_t_1", TblName: "t", Index: &sqlitefile.IndexDef{Table: "t", Auto: true, Unique: true}},
		indexObj("orphan", "nosuch", col("a")),
		indexObj("badcol", "t", col("nosuch")),
	)
	without := schemaOf(
		tableObj("w", tableDef(true, -1,
			recIdx(pk(notNull(fcol("k1", T)), 1), 0), recIdx(pk(notNull(fcol("k2", I)), 2), 1), recIdx(fcol("v", I), 2))),
		indexObj("w_v", "w", col("v")),
		indexObj("w_k2", "w", col("k2")),
	)
	cases := []struct {
		name          string
		s             *sqlitefile.Schema
		v             []sqlitefile.Value
		strict, loose []string
	}{
		{"rowid table: column then integer rowid", rowid, []sqlitefile.Value{vText("x"), vInt(5)}, names("i_b"), names("i_b", "i_ae")},
		{"rowid not an integer", rowid, []sqlitefile.Value{vText("x"), vText("y")}, nil, names("i_b", "i_ae")},
		{"text column holds an integer", rowid, []sqlitefile.Value{vInt(1), vInt(5)}, nil, names("i_ae")},
		{"expression column accepts any class", rowid, []sqlitefile.Value{vInt(1), vBlob(1), vInt(5)}, names("i_ae"), names("i_ae")},
		{"too short for strict", rowid, []sqlitefile.Value{vText("x")}, nil, names("i_b", "i_ae")},
		{"too long", rowid, []sqlitefile.Value{vText("x"), vInt(1), vInt(2), vInt(3)}, nil, nil},
		{"no values", rowid, nil, nil, nil},
		{"without rowid: key, then the primary key columns", without, []sqlitefile.Value{vInt(1), vText("k"), vInt(2)}, names("w_v"), names("w_v")},
		{"without rowid: a primary key column in the index leaves the others", without, []sqlitefile.Value{vInt(2), vText("k")}, names("w_k2"), names("w_v", "w_k2")},
	}
	for _, c := range cases {
		if got := c.s.FitIndexes(c.v, sqlitefile.FitStrict); !reflect.DeepEqual(got, c.strict) {
			t.Errorf("%s: strict %v, want %v", c.name, got, c.strict)
		}
		if got := c.s.FitIndexes(c.v, sqlitefile.FitLoose); !reflect.DeepEqual(got, c.loose) {
			t.Errorf("%s: loose %v, want %v", c.name, got, c.loose)
		}
	}

	if ix, basis := rowid.FitIndexPage([][]sqlitefile.Value{{vText("x"), vInt(5)}, {vText("y"), vInt(6)}}); ix != "i_b" || basis != sqlitefile.BasisFit {
		t.Errorf("FitIndexPage: %q %q, want i_b fit", ix, basis)
	}
	if ix, basis := rowid.FitIndexPage([][]sqlitefile.Value{{vText("x"), vText("y")}}); ix != "" || basis != sqlitefile.BasisNone {
		t.Errorf("FitIndexPage loose ambiguous: %q %q", ix, basis)
	}
	if ix, basis := without.FitIndexPage([][]sqlitefile.Value{{vInt(1), vText("k"), vInt(2)}, {vInt(2), vText("k")}}); ix != "w_v" || basis != sqlitefile.BasisGuess {
		t.Errorf("index cells of two lengths: %q %q, want w_v guess (strict needs the exact length)", ix, basis)
	}
	if ix, basis := without.FitIndexPage([][]sqlitefile.Value{{vInt(1), vText("k"), vInt(2)}, nil}); ix != "w_v" || basis != sqlitefile.BasisFit {
		t.Errorf("FitIndexPage without rowid: %q %q", ix, basis)
	}
}

func TestFitDeterministic(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	var objs []sqlitefile.SchemaObject
	var want []string
	for i := range 30 {
		n := fmt.Sprintf("t%02d", i)
		want = append(want, n)
		objs = append(objs, tableObj(n, tableDef(false, -1, fcol("a", I), fcol("b", T))))
	}
	s := schemaOf(objs...)
	v := []sqlitefile.Value{vInt(1), vText("x")}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if got := s.FitTables(v, sqlitefile.FitStrict); !reflect.DeepEqual(got, want) {
					t.Errorf("order %v", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestFitUsesColumnCountIndex: a schema of 100,000 tables of 200 distinct
// widths; a strict call examines only the tables that could hold that many
// values, never every schema object.
func TestFitUsesColumnCountIndex(t *testing.T) {
	const perWidth, widths = 500, 200
	defs := make([]*sqlitefile.TableDef, widths+1)
	for w := 1; w <= widths; w++ {
		cols := make([]sqlitefile.Column, w)
		for i := range cols {
			cols[i] = notNull(fcol(fmt.Sprintf("c%d", i), sqlitefile.AffBlob))
		}
		defs[w] = tableDef(false, -1, cols...)
	}
	objs := make([]sqlitefile.SchemaObject, 0, perWidth*widths)
	for i := range perWidth * widths {
		w := 1 + i%widths
		objs = append(objs, tableObj(fmt.Sprintf("t%06d", i), defs[w]))
	}
	s := schemaOf(objs...)
	v := []sqlitefile.Value{vInt(1), vInt(2), vInt(3), vInt(4), vInt(5)}
	got, examined := sqlitefile.FitExamined(s, v, sqlitefile.FitStrict)
	if len(got) != perWidth {
		t.Errorf("%d tables fit, want %d (the tables of width 5)", len(got), perWidth)
	}
	if examined != perWidth {
		t.Errorf("examined %d tables, want exactly the %d of the same width", examined, perWidth)
	}
	for _, n := range got {
		var i int
		if _, err := fmt.Sscanf(n, "t%d", &i); err != nil || 1+i%widths != 5 {
			t.Fatalf("table %q is not of width 5", n)
		}
	}
}

// TestFitStepCapYieldsNoneWithLimitReached: a pathological page costs at most
// Limits.MaxFitSteps, then the answer is none and the limit is reported.
func TestFitStepCapYieldsNoneWithLimitReached(t *testing.T) {
	B := sqlitefile.AffBlob
	var objs []sqlitefile.SchemaObject
	for i := range 50 {
		objs = append(objs, tableObj(fmt.Sprintf("t%d", i), tableDef(false, -1, fcol("a", B), fcol("b", B), fcol("c", B))))
	}
	s := schemaOf(objs...)
	row := []sqlitefile.Value{vInt(1), vInt(2), vInt(3)}
	page := make([][]sqlitefile.Value, 400)
	for i := range page {
		page[i] = row
	}
	// without a cap every table fits every cell: ambiguous, not limited
	if tb, basis, limited := s.FitPageDetail(page); tb != "" || basis != sqlitefile.BasisNone || limited {
		t.Fatalf("uncapped: %q %q limited=%v", tb, basis, limited)
	}
	sqlitefile.SetFitLimit(s, 200)
	if tb, basis, limited := s.FitPageDetail(page); tb != "" || basis != sqlitefile.BasisNone || !limited {
		t.Errorf("capped: %q %q limited=%v, want none and limit-reached", tb, basis, limited)
	}
	if tb, basis := s.FitPage(page); tb != "" || basis != sqlitefile.BasisNone {
		t.Errorf("FitPage capped: %q %q", tb, basis)
	}
	// a single table that fits would have been found without the cap
	one := schemaOf(objs[0])
	sqlitefile.SetFitLimit(one, 200)
	if tb, basis, limited := one.FitPageDetail(page[:10]); tb != "t0" || basis != sqlitefile.BasisFit || limited {
		t.Errorf("small page under the cap: %q %q limited=%v", tb, basis, limited)
	}
}

// TestFitStepCapComesFromLimits: the schema a View reads carries
// Limits.MaxFitSteps, so the cap an examiner sets bounds the fit.
func TestFitStepCapComesFromLimits(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	b.CreateTable("t", "create table t(a integer, b text)").Insert(1, int64(1), "x")
	data := b.Bytes()
	page := [][]sqlitefile.Value{{vInt(1), vText("x")}}
	for _, c := range []struct {
		steps   int64
		limited bool
	}{{0, false}, {1, true}} {
		_, v := layoutOf(t, data, sqlitefile.Options{Limits: sqlitefile.Limits{MaxFitSteps: c.steps}})
		s, err := v.Schema(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		tb, basis, limited := s.FitPageDetail(page)
		if limited != c.limited || (!c.limited && (tb != "t" || basis != sqlitefile.BasisFit)) {
			t.Errorf("MaxFitSteps %d: %q %q limited=%v", c.steps, tb, basis, limited)
		}
	}
}
