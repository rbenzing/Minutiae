package sqlitefile_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// checkTableDef asserts the invariants every parsed definition holds, whatever
// the input: the record mapping is a bijection onto 0..StoredColumns-1 for the
// stored columns and -1 for the virtual generated ones, the rowid alias is a
// real single-column integer key of a rowid table, and a failed parse carries
// a note and no columns.
func checkTableDef(t testing.TB, sql string, def sqlitefile.TableDef, steps int) {
	t.Helper()
	if steps > 100*len(sql)+100 {
		t.Fatalf("%d steps for %d bytes of input", steps, len(sql))
	}
	if !def.ParseOK {
		if len(def.Columns) != 0 || def.ParseNote == "" || !noteShape.MatchString(def.ParseNote) || sqlShape.MatchString(def.ParseNote) || def.RowidAlias != -1 {
			t.Fatalf("failed parse: columns %d note %q alias %d", len(def.Columns), def.ParseNote, def.RowidAlias)
		}
		return
	}
	if def.ParseNote != "" {
		t.Fatalf("ParseOK with note %q", def.ParseNote)
	}
	if len(def.Columns) == 0 || len(def.Columns) > sqlitefile.DefaultLimits().MaxColumns {
		t.Fatalf("%d columns", len(def.Columns))
	}
	seen := make([]bool, def.StoredColumns)
	stored, pks := 0, 0
	for i, c := range def.Columns {
		if c.RecordIndex < -1 || c.RecordIndex >= def.StoredColumns {
			t.Fatalf("column %d: RecordIndex %d outside -1..%d", i, c.RecordIndex, def.StoredColumns-1)
		}
		if (c.RecordIndex == -1) != (c.Generated == sqlitefile.GenVirtual) {
			t.Fatalf("column %d: RecordIndex %d with Generated %d", i, c.RecordIndex, c.Generated)
		}
		if c.RecordIndex >= 0 {
			if seen[c.RecordIndex] {
				t.Fatalf("column %d: RecordIndex %d is used twice", i, c.RecordIndex)
			}
			seen[c.RecordIndex] = true
			stored++
		}
		if c.PKOrdinal < 0 || c.PKOrdinal > len(def.Columns) {
			t.Fatalf("column %d: PKOrdinal %d", i, c.PKOrdinal)
		}
		if c.PKOrdinal > 0 {
			pks++
		}
		if c.Affinity > sqlitefile.AffReal || c.Default.Kind > sqlitefile.DefaultExpr || c.Generated > sqlitefile.GenStored {
			t.Fatalf("column %d: enumeration out of range", i)
		}
	}
	if stored != def.StoredColumns {
		t.Fatalf("StoredColumns %d but %d columns are stored", def.StoredColumns, stored)
	}
	for k := 1; k <= pks; k++ { // 1..pks each exactly once
		n := 0
		for _, c := range def.Columns {
			if c.PKOrdinal == k {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("PKOrdinal %d held by %d columns", k, n)
		}
	}
	if def.WithoutRowid && pks == 0 {
		t.Fatal("WITHOUT ROWID without a key")
	}
	if def.RowidAlias != -1 {
		if def.RowidAlias < 0 || def.RowidAlias >= len(def.Columns) || def.WithoutRowid || pks != 1 ||
			def.Columns[def.RowidAlias].PKOrdinal != 1 || def.Columns[def.RowidAlias].RecordIndex < 0 {
			t.Fatalf("RowidAlias %d is not a single-column key of a rowid table", def.RowidAlias)
		}
	}
	if def.WithoutRowid { // the key columns come first in the record, in key order
		for _, c := range def.Columns {
			if c.PKOrdinal > 0 && c.RecordIndex != c.PKOrdinal-1 {
				t.Fatalf("WITHOUT ROWID key column %q has RecordIndex %d, want %d", c.Name, c.RecordIndex, c.PKOrdinal-1)
			}
		}
	}
}

func checkIndexDef(t testing.TB, sql string, def sqlitefile.IndexDef, steps int) {
	t.Helper()
	if steps > 100*len(sql)+100 {
		t.Fatalf("%d steps for %d bytes of input", steps, len(sql))
	}
	if !def.ParseOK && len(def.Columns) != 0 {
		t.Fatal("columns on a failed parse")
	}
	if def.ParseOK && (len(def.Columns) == 0 || len(def.Columns) > sqlitefile.DefaultLimits().MaxColumns) {
		t.Fatalf("%d columns", len(def.Columns))
	}
}

// FuzzCreateParse: the CREATE parsers never panic on any text, stay within
// 100 token steps per input byte, and every definition they return satisfies
// the invariants above. Seeds are the table-driven definitions.
func FuzzCreateParse(f *testing.F) {
	for _, c := range tableCases {
		f.Add(c.sql)
	}
	for _, c := range indexCases {
		f.Add(c.sql)
	}
	for _, s := range []string{
		"", "CREATE", "CREATE TABLE", "CREATE TABLE t(", "CREATE TABLE t(a", "CREATE VIRTUAL TABLE t USING m(a)",
		"CREATE TABLE t(a AS (", "CREATE TABLE t(a DEFAULT -", "CREATE TABLE t(a DEFAULT x'0", "CREATE TABLE t(a PRIMARY KEY(",
		"CREATE TABLE t(a, PRIMARY KEY(a, a))", "CREATE TABLE t(a PRIMARY KEY, PRIMARY KEY(a))", "CREATE INDEX i ON t((((((",
		"CREATE TABLE t(a REFERENCES o ON ON ON)", "CREATE TABLE t(a CHECK ())", "'\"`[", "CREATE TABLE \"\"(\"\")",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		def, virtual, steps := sqlitefile.ParseCreateTable(sql)
		checkTableDef(t, sql, def, steps)
		if virtual && def.ParseOK {
			t.Fatal("a virtual table is never ParseOK")
		}
		idef, isteps := sqlitefile.ParseCreateIndex(sql)
		checkIndexDef(t, sql, idef, isteps)
		// A small column cap must hold too.
		small, _, ssteps := sqlitefile.ParseCreateTableCols(sql, 3)
		if small.ParseOK && len(small.Columns) > 3 {
			t.Fatalf("%d columns with a cap of 3", len(small.Columns))
		}
		_ = ssteps
	})
}

// TestFuzzSeedsParse runs every seed through the same checks under plain go
// test (the fuzz seeds run there too; this one fails with a readable name).
func TestFuzzSeedsParse(t *testing.T) {
	for _, c := range tableCases {
		def, _, steps := sqlitefile.ParseCreateTable(c.sql)
		if !def.ParseOK {
			t.Errorf("%s: seed does not parse", c.name)
		}
		checkTableDef(t, c.sql, def, steps)
	}
}
