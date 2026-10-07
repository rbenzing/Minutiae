package sqlitefile_test

// Task 11 review fixes (step 0b): pins of the fit rules the review found
// unpinned, the explicit Origin of a recovered row and the bounded build of the
// fit index.

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestFitIndexRowidLastMustBeInteger: strict requires an integer rowid as the
// last value of an entry of a rowid table's index, whatever the class.
func TestFitIndexRowidLastMustBeInteger(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	s := schemaOf(
		tableObj("t", tableDef(false, -1, fcol("a", I), fcol("b", T))),
		indexObj("i_b", "t", sqlitefile.IndexColumn{Name: "b"}),
	)
	for _, c := range []struct {
		name   string
		last   sqlitefile.Value
		strict []string
	}{
		{"integer", vInt(5), names("i_b")},
		{"float", vFloat(5), nil},
		{"blob", vBlob(5), nil},
		{"null", vNull(), nil},
		{"text", vText("5"), nil},
	} {
		v := []sqlitefile.Value{vText("x"), c.last}
		if got := s.FitIndexes(v, sqlitefile.FitStrict); !reflect.DeepEqual(got, c.strict) {
			t.Errorf("%s: strict %v, want %v", c.name, got, c.strict)
		}
		if got := s.FitIndexes(v, sqlitefile.FitLoose); !reflect.DeepEqual(got, names("i_b")) {
			t.Errorf("%s: loose %v, want i_b", c.name, got)
		}
	}
}

// TestFitStoredGeneratedColumnIsNotAddable: ALTER TABLE cannot add a generated
// column, so a record that lacks a stored one is no strict fit; a record that
// lacks a nullable ordinary column is.
func TestFitStoredGeneratedColumnIsNotAddable(t *testing.T) {
	I := sqlitefile.AffInteger
	gen := func(k sqlitefile.GenKind) *sqlitefile.Schema {
		g := fcol("g", I)
		g.Generated = k
		return schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I), g)))
	}
	short := []sqlitefile.Value{vInt(1)}
	if got := gen(sqlitefile.GenStored).FitTables(short, sqlitefile.FitStrict); got != nil {
		t.Errorf("stored generated column missing: strict %v, want none", got)
	}
	if got := gen(sqlitefile.GenStored).FitTables(short, sqlitefile.FitLoose); !reflect.DeepEqual(got, names("t")) {
		t.Errorf("loose %v", got)
	}
	if got := gen(sqlitefile.GenNone).FitTables(short, sqlitefile.FitStrict); !reflect.DeepEqual(got, names("t")) {
		t.Errorf("ordinary nullable column missing: strict %v, want t", got)
	}
}

// TestFitIndexItemGuards: an index fits nothing when its definition did not
// parse, it has no columns, it is automatic, its table is missing or did not
// parse, it names a column the table lacks or an empty name; the rowid aliases
// are integer columns.
func TestFitIndexItemGuards(t *testing.T) {
	I, T := sqlitefile.AffInteger, sqlitefile.AffText
	col := func(n string) sqlitefile.IndexColumn { return sqlitefile.IndexColumn{Name: n} }
	table := tableObj("t", tableDef(false, -1, fcol("a", T)))
	mk := func(mut func(o *sqlitefile.SchemaObject)) *sqlitefile.Schema {
		ix := indexObj("ix", "t", col("a"))
		mut(&ix)
		return schemaOf(table, ix)
	}
	v := []sqlitefile.Value{vText("x"), vInt(1)}
	for _, c := range []struct {
		name string
		s    *sqlitefile.Schema
		want []string
		v    []sqlitefile.Value
	}{
		{"fits", mk(func(*sqlitefile.SchemaObject) {}), names("ix"), nil},
		{"definition did not parse", mk(func(o *sqlitefile.SchemaObject) { o.Index.ParseOK = false }), nil, nil},
		{"no columns", mk(func(o *sqlitefile.SchemaObject) { o.Index.Columns = nil; o.Index.ParseOK = true }), nil, []sqlitefile.Value{vInt(1)}},
		{"automatic", mk(func(o *sqlitefile.SchemaObject) { o.Index.Auto = true }), nil, nil},
		{"unknown column", mk(func(o *sqlitefile.SchemaObject) { o.Index.Columns = []sqlitefile.IndexColumn{col("zz")} }), nil, nil},
		{"table did not parse", schemaOf(unparsedTable(), indexObj("ix", "t", col("a"))), nil, nil},
	} {
		v := v
		if c.v != nil {
			v = c.v
		}
		if got := c.s.FitIndexes(v, sqlitefile.FitStrict); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// rowid, _rowid_ and oid name the integer rowid
	for _, n := range []string{"rowid", "_rowid_", "oid", "ROWID"} {
		s := schemaOf(table, indexObj("ix", "t", col(n)))
		if got := s.FitIndexes([]sqlitefile.Value{vInt(3), vInt(3)}, sqlitefile.FitStrict); !reflect.DeepEqual(got, names("ix")) {
			t.Errorf("%s: integer entry %v", n, got)
		}
		if got := s.FitIndexes([]sqlitefile.Value{vText("x"), vInt(3)}, sqlitefile.FitStrict); !reflect.DeepEqual(got, names("ix")) {
			t.Errorf("%s: any class is allowed by the loose fit rule of INTEGER columns: %v", n, got)
		}
	}
	_ = I
}

// TestFitSchemaGuards: a table whose record layout is invalid, a virtual table
// and a repeated table name fit nothing (the first of two same-named tables is
// the one an index is read against).
func TestFitSchemaGuards(t *testing.T) {
	I := sqlitefile.AffInteger
	bad := tableDef(false, -1, fcol("a", I))
	bad.Columns[0].RecordIndex = 5 // past StoredColumns
	if got := schemaOf(tableObj("t", bad)).FitTables([]sqlitefile.Value{vInt(1)}, sqlitefile.FitLoose); got != nil {
		t.Errorf("invalid layout fits %v", got)
	}
	virt := tableObj("v", tableDef(false, -1, fcol("a", I)))
	virt.Virtual = true
	if got := schemaOf(virt).FitTables([]sqlitefile.Value{vInt(1)}, sqlitefile.FitLoose); got != nil {
		t.Errorf("virtual table fits %v", got)
	}
	vix := indexObj("vix", "v", sqlitefile.IndexColumn{Name: "a"})
	if got := schemaOf(virt, vix).FitIndexes([]sqlitefile.Value{vInt(1), vInt(1)}, sqlitefile.FitLoose); got != nil {
		t.Errorf("index of a virtual table fits %v", got)
	}
	first := tableObj("t", tableDef(false, -1, fcol("a", I)))
	second := tableObj("t", tableDef(false, -1, fcol("a", I), fcol("only_second", I)))
	ix := indexObj("ix", "t", sqlitefile.IndexColumn{Name: "only_second"})
	if got := schemaOf(first, second, ix).FitIndexes([]sqlitefile.Value{vInt(1), vInt(1)}, sqlitefile.FitLoose); got != nil {
		t.Errorf("the index was read against the second table: %v", got)
	}
}

// TestFitStepCapBoundary: a table of two columns costs three steps (one per
// column and one for the item) at each of the two tiers: a cap of six is enough,
// a cap of five is not.
func TestFitStepCapBoundary(t *testing.T) {
	I := sqlitefile.AffInteger
	for _, c := range []struct {
		cap     int64
		limited bool
	}{{6, false}, {5, true}} {
		s := schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I), fcol("b", I))))
		sqlitefile.SetFitLimit(s, c.cap)
		tb, basis, limited := s.FitPageDetail([][]sqlitefile.Value{{vInt(1), vInt(2)}})
		if limited != c.limited || (!limited && (tb != "t" || basis != sqlitefile.BasisFit)) {
			t.Errorf("cap %d: %q %q limited %v", c.cap, tb, basis, limited)
		}
	}
}

// TestFitBuildIsBounded: a schema of many wide indexes does not cost unbounded
// work when the fit index is first built: past the step cap the index is cut and
// every fit says limit-reached, promptly.
func TestFitBuildIsBounded(t *testing.T) {
	I := sqlitefile.AffInteger
	cols := make([]sqlitefile.Column, 300)
	icols := make([]sqlitefile.IndexColumn, 300)
	for i := range cols {
		cols[i] = fcol(fmt.Sprintf("c%d", i), I)
		icols[i] = sqlitefile.IndexColumn{Name: fmt.Sprintf("c%d", i)}
	}
	objs := []sqlitefile.SchemaObject{tableObj("t", tableDef(false, -1, cols...))}
	for i := range 400 {
		objs = append(objs, indexObj(fmt.Sprintf("ix%d", i), "t", icols...))
	}
	s := schemaOf(objs...)
	sqlitefile.SetFitLimit(s, 1000)
	_, _, limited := s.FitIndexPageDetail([][]sqlitefile.Value{{vInt(1)}})
	if !limited {
		t.Error("a build over the step cap was not reported as limit-reached")
	}
	// a call no item can hold is not stopped by its own step cap: the cut build says so
	if _, _, limited := s.FitIndexPageDetail([][]sqlitefile.Value{make([]sqlitefile.Value, 400)}); !limited {
		t.Error("a cut fit index did not report limit-reached to a call that examines nothing")
	}
	if n := sqlitefile.FitBuildSteps(s); n > 64*1000+300 {
		t.Errorf("the build took %d steps for a cap of 1000 (build cap 64000)", n)
	}
}

// TestFitBuildChargesItsColumnScan: the build counts one step per index column
// it resolves (and per table column it indexes), so the work is visible and
// capped.
func TestFitBuildChargesItsColumnScan(t *testing.T) {
	I := sqlitefile.AffInteger
	cols := make([]sqlitefile.Column, 50)
	icols := make([]sqlitefile.IndexColumn, 50)
	for i := range cols {
		cols[i] = fcol(fmt.Sprintf("c%d", i), I)
		icols[i] = sqlitefile.IndexColumn{Name: fmt.Sprintf("c%d", i)}
	}
	objs := []sqlitefile.SchemaObject{tableObj("t", tableDef(false, -1, cols...))}
	for i := range 20 {
		objs = append(objs, indexObj(fmt.Sprintf("ix%d", i), "t", icols...))
	}
	s := schemaOf(objs...)
	if n := sqlitefile.FitBuildSteps(s); n < 20*50 {
		t.Errorf("the build charged %d steps for 1000 index columns", n)
	}
	if got := s.FitIndexes(make([]sqlitefile.Value, 51), sqlitefile.FitLoose); len(got) != 20 {
		t.Errorf("%d indexes fit, want 20", len(got))
	}
}

// unparsedTable is a table whose columns are known but whose definition is
// marked as not parsed.
func unparsedTable() sqlitefile.SchemaObject {
	d := tableDef(false, -1, fcol("a", sqlitefile.AffText))
	d.ParseOK = false
	return tableObj("t", d)
}
