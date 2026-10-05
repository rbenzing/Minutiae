package sqlitefile_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// engineAccepts reports whether the engine accepts the statement on an empty
// database (the oracle for what the parser must accept).
func engineAccepts(t *testing.T, stmt string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(stmt)
	return err == nil
}

// TestParserRefusesWhatTheEngineRefuses: a definition the engine would refuse
// (duplicate column names, a STRICT column whose type is not INT, INTEGER,
// REAL, TEXT, BLOB or ANY, or has none) is not parsed, and the parser accepts
// what the engine accepts.
func TestParserRefusesWhatTheEngineRefuses(t *testing.T) {
	for _, c := range []struct {
		name, sql string
		ok        bool
	}{
		{"duplicate column", "CREATE TABLE t(a, a)", false},
		{"duplicate column by case", "CREATE TABLE t(a INT, A TEXT)", false},
		{"duplicate column quoted", `CREATE TABLE t("a", [A])`, false},
		{"duplicate column in a strict table", "CREATE TABLE t(a INT, a INT) STRICT", false},
		{"distinct non-ascii case is distinct", "CREATE TABLE t(é, É)", true},
		{"strict valid types", "CREATE TABLE t(a INT, b INTEGER, c REAL, d TEXT, e BLOB, f ANY) STRICT", true},
		{"strict valid types in lower case", "CREATE TABLE t(a int, b text) strict", true},
		{"strict varchar", "CREATE TABLE t(a VARCHAR(10)) STRICT", false},
		{"strict numeric", "CREATE TABLE t(a NUMERIC) STRICT", false},
		{"strict no type", "CREATE TABLE t(a) STRICT", false},
		{"strict two words", "CREATE TABLE t(a UNSIGNED INT) STRICT", false},
		{"strict float", "CREATE TABLE t(a FLOAT) STRICT", false},
		{"strict without rowid", "CREATE TABLE t(a INT PRIMARY KEY, b FLOAT) STRICT, WITHOUT ROWID", false},
		{"non-strict free types", "CREATE TABLE t(a, b VARCHAR(10), c UNSIGNED INT)", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := engineAccepts(t, c.sql); got != c.ok {
				t.Fatalf("engine accepts %v, the test expects %v: the probe is wrong", got, c.ok)
			}
			def, _, _ := sqlitefile.ParseCreateTable(c.sql)
			if def.ParseOK != c.ok {
				t.Errorf("parser ParseOK %v (note %q), the engine says %v", def.ParseOK, def.ParseNote, c.ok)
			}
		})
	}
}

// TestParserCorpusAgreesWithTheEngine: every statement the author wrote for the
// parser is also run through the engine. A statement the parser accepts must be
// accepted by the engine; one the parser refuses must be refused by the engine
// unless the parser merely cannot read it (a CREATE TABLE AS or a virtual
// table, which the engine accepts).
func TestParserCorpusAgreesWithTheEngine(t *testing.T) {
	for _, c := range tableCases {
		if !engineAccepts(t, c.sql) {
			t.Errorf("accepted by the parser, refused by the engine: %s (%s)", c.name, c.sql)
		}
	}
	cannotRead := map[string]bool{"ctas": true, "virtual": true, "empty": true} // valid for the engine, not a table definition
	for _, c := range unparseableCases {
		if cannotRead[c.note] || c.name == "not a table" {
			continue
		}
		if engineAccepts(t, c.sql) {
			t.Errorf("refused by the parser (%s), accepted by the engine: %s (%s)", c.note, c.name, c.sql)
		}
	}
}

// TestSchemaWarnsForRefusedDefinitions: such a table is listed unparsed and
// raises schema-sql-unparsed; so does every statement over the per-statement
// cap (a table and an index), which used to be silent apart from the limit.
func TestSchemaWarnsForRefusedDefinitions(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.AddSchemaRow("table", "dup", "dup", int64(0), "CREATE TABLE dup(a, A)")
	b.AddSchemaRow("table", "strict", "strict", int64(0), "CREATE TABLE strict(a FLOAT) STRICT")
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range s.Objects {
		if o.Table == nil || o.Table.ParseOK {
			t.Errorf("%s: parsed, but the engine refuses it", o.Name)
		}
	}
	n := 0
	for _, w := range v.Warnings() {
		if w.Code == sqlitefile.WarnSchemaSQLUnparsed {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d schema-sql-unparsed warnings, want 2: %v", n, v.Warnings())
	}
}

// unparsedFor counts the schema-sql-unparsed warnings that name obj.
func unparsedFor(v *sqlitefile.View, obj string) int {
	n := 0
	for _, w := range v.Warnings() {
		if w.Code == sqlitefile.WarnSchemaSQLUnparsed && strings.Contains(w.Msg, `"`+obj+`"`) {
			n++
		}
	}
	return n
}

// TestOverCapSQLRaisesTheUnparsedWarning: a statement that is not parsed because
// it is over the per-statement cap, or past the total cap, is never silent: it
// raises schema-sql-unparsed like any other definition left unparsed.
func TestOverCapSQLRaisesTheUnparsedWarning(t *testing.T) {
	pad := func(prefix string, n int) string { return prefix + strings.Repeat(" ", n-len(prefix)-1) + ")" }
	t.Run("per statement", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		b.CreateTable("t", "CREATE TABLE t(a)")
		b.CreateTable("over", pad("CREATE TABLE over(a", 301))
		b.CreateIndex("ix", "t", pad("CREATE INDEX ix ON t(a", 301), 0)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Limits: sqlitefile.Limits{MaxSchemaSQLBytes: 300}})
		defer v.Release()
		if _, err := v.Schema(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"over", "ix"} {
			if n := unparsedFor(v, name); n != 1 {
				t.Errorf("%s: %d schema-sql-unparsed warnings, want 1: %v", name, n, v.Warnings())
			}
		}
		if n := unparsedFor(v, "t"); n != 0 {
			t.Errorf("the small table raised %d unparsed warnings", n)
		}
	})
	t.Run("past the total", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		for i := range 8 {
			name := fmt.Sprintf("t%d", i)
			b.CreateTable(name, pad("CREATE TABLE "+name+"(a", 120))
		}
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Limits: sqlitefile.Limits{MaxSchemaSQLBytes: 1000, MaxSchemaTotalBytes: 3 * 120}})
		defer v.Release()
		if _, err := v.Schema(context.Background()); err != nil {
			t.Fatal(err)
		}
		for i := 3; i < 8; i++ {
			if n := unparsedFor(v, fmt.Sprintf("t%d", i)); n != 1 {
				t.Errorf("t%d past the total cap: %d unparsed warnings, want 1", i, n)
			}
		}
	})
}

// TestRootPageZeroErrorText: a table with rootpage 0 that is not a virtual
// table says what it is, not "virtual table".
func TestRootPageZeroErrorText(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.AddSchemaRow("table", "zero", "zero", int64(0), "CREATE TABLE zero(a)")
	b.AddSchemaRow("table", "vt", "vt", int64(0), "CREATE VIRTUAL TABLE vt USING fts5(a)")
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	_, err := v.Table(context.Background(), "zero")
	if !errors.Is(err, sqlitefile.ErrNotFound) || strings.Contains(err.Error(), "virtual") || !strings.Contains(err.Error(), "rootpage 0") {
		t.Errorf("rootpage 0: %v", err)
	}
	_, err = v.Table(context.Background(), "vt")
	if !errors.Is(err, sqlitefile.ErrNotFound) || !strings.Contains(err.Error(), "virtual table") {
		t.Errorf("a virtual table: %v", err)
	}
}

// TestSchemaRowOutOfKeyRangeIsMarked: a schema row the key range of page 1's
// interior page does not allow is delivered, as the engine walk does, and
// marked on its object like any other row.
func TestSchemaRowOutOfKeyRangeIsMarked(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	for i := range 40 {
		name := fmt.Sprintf("t%02d", i)
		b.CreateTable(name, "CREATE TABLE "+name+"(a)")
	}
	data := b.Bytes()
	root := pageAt(data, 512, 1)
	if root[100] != 0x05 {
		t.Fatalf("page 1 is not an interior table page (%#x)", root[100])
	}
	cellOff := int(root[112])<<8 | int(root[113]) // first pointer, header at 100, 12 bytes
	key := int(root[cellOff+4])
	if key >= 0x80 || key < 2 {
		t.Fatalf("first key %d", key)
	}
	root[cellOff+4] = byte(key - 1) // the last row of the first leaf now lies above its key
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Objects) != 40 {
		t.Fatalf("%d objects: the walk delivers every row", len(s.Objects))
	}
	for i, o := range s.Objects {
		if want := i+1 == key; o.KeyRangeViolation != want {
			t.Errorf("object %d (%s): KeyRangeViolation %v, want %v", i, o.Name, o.KeyRangeViolation, want)
		}
	}
}
