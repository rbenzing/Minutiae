package sqlitefile_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func affLetter(a sqlitefile.Affinity) string {
	return map[sqlitefile.Affinity]string{
		sqlitefile.AffBlob: "B", sqlitefile.AffText: "T", sqlitefile.AffNumeric: "N",
		sqlitefile.AffInteger: "I", sqlitefile.AffReal: "R",
	}[a]
}

func defValueStr(v sqlitefile.Value) string {
	switch v.Kind {
	case sqlitefile.KindNull:
		return "NULL"
	case sqlitefile.KindInt:
		return fmt.Sprint(v.Int)
	case sqlitefile.KindFloat:
		return fmt.Sprintf("%gf", v.Float)
	case sqlitefile.KindText:
		return "'" + string(v.Bytes) + "'"
	case sqlitefile.KindBlob:
		return fmt.Sprintf("x'%X'", v.Bytes)
	}
	return "?"
}

// describeCol renders a column as name/decl/affinity[/nn][/pkN][/c=coll][/d=..][/gv|gs]/rN.
func describeCol(c sqlitefile.Column) string {
	s := fmt.Sprintf("%s/%s/%s", c.Name, c.DeclType, affLetter(c.Affinity))
	if c.NotNull {
		s += "/nn"
	}
	if c.PKOrdinal > 0 {
		s += fmt.Sprintf("/pk%d", c.PKOrdinal)
	}
	if c.Collation != "" {
		s += "/c=" + c.Collation
	}
	switch c.Default.Kind {
	case sqlitefile.DefaultLiteral:
		s += "/d=" + defValueStr(c.Default.Value)
	case sqlitefile.DefaultExpr:
		s += "/dX"
	}
	switch c.Generated {
	case sqlitefile.GenVirtual:
		s += "/gv"
	case sqlitefile.GenStored:
		s += "/gs"
	}
	return s + fmt.Sprintf("/r%d", c.RecordIndex)
}

func describeFlags(d sqlitefile.TableDef) string {
	s := fmt.Sprintf("alias=%d stored=%d", d.RowidAlias, d.StoredColumns)
	if d.WithoutRowid {
		s += " W"
	}
	if d.Strict {
		s += " S"
	}
	return s
}

type tableCase struct {
	name  string
	sql   string
	cols  []string
	flags string
}

// tableCases are the table-driven definitions of TestCreateTableParse; they
// also seed FuzzCreateParse.
var tableCases = []tableCase{
	{
		"identifier quotings", "CREATE TABLE t(\"a b\" INT, `c` TEXT, [d] REAL, 'e' BLOB, f)",
		[]string{"a b/INT/I/r0", "c/TEXT/T/r1", "d/REAL/R/r2", "e/BLOB/B/r3", "f//B/r4"},
		"alias=-1 stored=5",
	},
	{
		"doubled quotes", "CREATE TABLE \"t\"\"x\"(\"a\"\"b\" INT, `c``d` TEXT)",
		[]string{"a\"b/INT/I/r0", "c`d/TEXT/T/r1"},
		"alias=-1 stored=2",
	},
	{
		"comments inside", "CREATE TABLE t(a /* c1 */ INT, -- line\n b TEXT /* x */ , c /* ) */)",
		[]string{"a/INT/I/r0", "b/TEXT/T/r1", "c//B/r2"},
		"alias=-1 stored=3",
	},
	{
		"multi-word types and (n,m)", "CREATE TABLE t(a UNSIGNED BIG INT, b DECIMAL(10,5), c VARCHAR(255) NOT NULL, d DOUBLE PRECISION, e NATIVE CHARACTER(70))",
		[]string{"a/UNSIGNED BIG INT/I/r0", "b/DECIMAL(10,5)/N/r1", "c/VARCHAR(255)/T/nn/r2", "d/DOUBLE PRECISION/R/r3", "e/NATIVE CHARACTER(70)/T/r4"},
		"alias=-1 stored=5",
	},
	{
		"INTEGER PRIMARY KEY is the rowid alias", "CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		[]string{"id/INTEGER/I/pk1/r0", "v//B/r1"},
		"alias=0 stored=2",
	},
	{
		"INT PRIMARY KEY is not", "CREATE TABLE t(id INT PRIMARY KEY, v)",
		[]string{"id/INT/I/pk1/r0", "v//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"INTEGER PRIMARY KEY DESC is not [M]", "CREATE TABLE t(id INTEGER PRIMARY KEY DESC, v)",
		[]string{"id/INTEGER/I/pk1/r0", "v//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"INTEGER PRIMARY KEY ASC is", "CREATE TABLE t(id INTEGER PRIMARY KEY ASC, v)",
		[]string{"id/INTEGER/I/pk1/r0", "v//B/r1"},
		"alias=0 stored=2",
	},
	{
		"lower case and AUTOINCREMENT", "create table t(id integer primary key autoincrement, v)",
		[]string{"id/integer/I/pk1/r0", "v//B/r1"},
		"alias=0 stored=2",
	},
	{
		"table-level PRIMARY KEY over an INTEGER column", "CREATE TABLE t(a INTEGER, b, PRIMARY KEY(a))",
		[]string{"a/INTEGER/I/pk1/r0", "b//B/r1"},
		"alias=0 stored=2",
	},
	{
		"table-level PRIMARY KEY DESC is still an alias [M]", "CREATE TABLE t(a INTEGER, b, PRIMARY KEY(a DESC))",
		[]string{"a/INTEGER/I/pk1/r0", "b//B/r1"},
		"alias=0 stored=2",
	},
	{
		"STRICT key column is NOT NULL", "CREATE TABLE t(a INT PRIMARY KEY, b ANY) STRICT",
		[]string{"a/INT/I/nn/pk1/r0", "b/ANY/B/r1"},
		"alias=-1 stored=2 S",
	},
	{
		"STRICT rowid alias is not NOT NULL", "CREATE TABLE t(a INTEGER PRIMARY KEY, b ANY) STRICT",
		[]string{"a/INTEGER/I/pk1/r0", "b/ANY/B/r1"},
		"alias=0 stored=2 S",
	},
	{
		"STRICT composite key is NOT NULL", "CREATE TABLE t(a INTEGER, b INTEGER, PRIMARY KEY(a, b)) STRICT",
		[]string{"a/INTEGER/I/nn/pk1/r0", "b/INTEGER/I/nn/pk2/r1"},
		"alias=-1 stored=2 S",
	},
	{
		"table-level AUTOINCREMENT", "CREATE TABLE t(a INTEGER, PRIMARY KEY(a AUTOINCREMENT))",
		[]string{"a/INTEGER/I/pk1/r0"},
		"alias=0 stored=1",
	},
	{
		"composite PRIMARY KEY is not an alias", "CREATE TABLE t(a INTEGER, b INTEGER, PRIMARY KEY(a, b))",
		[]string{"a/INTEGER/I/pk1/r0", "b/INTEGER/I/pk2/r1"},
		"alias=-1 stored=2",
	},
	{
		"composite key order is the constraint's", "CREATE TABLE t(a INTEGER, b INTEGER, PRIMARY KEY(b, a))",
		[]string{"a/INTEGER/I/pk2/r0", "b/INTEGER/I/pk1/r1"},
		"alias=-1 stored=2",
	},
	{
		"CONSTRAINT names", "CREATE TABLE t(a INTEGER CONSTRAINT pk PRIMARY KEY, b TEXT CONSTRAINT nn NOT NULL CONSTRAINT u UNIQUE, CONSTRAINT c1 CHECK (a > 0), CONSTRAINT fk FOREIGN KEY (b) REFERENCES o(x))",
		[]string{"a/INTEGER/I/pk1/r0", "b/TEXT/T/nn/r1"},
		"alias=0 stored=2",
	},
	{
		"every column constraint", "CREATE TABLE t(a INTEGER NOT NULL DEFAULT 5 COLLATE NOCASE UNIQUE CHECK (a > 0 AND (a < 10)) REFERENCES o(x) ON DELETE CASCADE ON UPDATE SET NULL MATCH FULL DEFERRABLE INITIALLY DEFERRED, b)",
		[]string{"a/INTEGER/I/nn/c=NOCASE/d=5/r0", "b//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"conflict clauses", "CREATE TABLE t(a INT NOT NULL ON CONFLICT REPLACE, b TEXT PRIMARY KEY ON CONFLICT FAIL, c UNIQUE ON CONFLICT IGNORE)",
		[]string{"a/INT/I/nn/r0", "b/TEXT/T/pk1/r1", "c//B/r2"},
		"alias=-1 stored=3",
	},
	{
		"REFERENCES then NOT NULL", "CREATE TABLE t(a INT REFERENCES o NOT DEFERRABLE INITIALLY IMMEDIATE NOT NULL, b)",
		[]string{"a/INT/I/nn/r0", "b//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"REFERENCES actions", "CREATE TABLE t(a INT REFERENCES o(x) ON DELETE RESTRICT ON UPDATE NO ACTION ON DELETE SET DEFAULT, b)",
		[]string{"a/INT/I/r0", "b//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"WITHOUT ROWID with out-of-order PRIMARY KEY", "CREATE TABLE t(a, b, c, d, PRIMARY KEY(c, a)) WITHOUT ROWID",
		[]string{"a//B/nn/pk2/r1", "b//B/r2", "c//B/nn/pk1/r0", "d//B/r3"},
		"alias=-1 stored=4 W",
	},
	{
		"WITHOUT ROWID column-level key", "CREATE TABLE t(a TEXT PRIMARY KEY, b) WITHOUT ROWID",
		[]string{"a/TEXT/T/nn/pk1/r0", "b//B/r1"},
		"alias=-1 stored=2 W",
	},
	{
		"STRICT", "CREATE TABLE t(a INT, b TEXT, c ANY) STRICT",
		[]string{"a/INT/I/r0", "b/TEXT/T/r1", "c/ANY/B/r2"},
		"alias=-1 stored=3 S",
	},
	{
		"WITHOUT ROWID, STRICT", "CREATE TABLE t(a INTEGER PRIMARY KEY, b ANY) WITHOUT ROWID, STRICT",
		[]string{"a/INTEGER/I/nn/pk1/r0", "b/ANY/B/r1"},
		"alias=-1 stored=2 W S",
	},
	{
		"STRICT, WITHOUT ROWID", "CREATE TABLE t(a ANY PRIMARY KEY, b ANY) STRICT, WITHOUT ROWID",
		[]string{"a/ANY/B/nn/pk1/r0", "b/ANY/B/r1"},
		"alias=-1 stored=2 W S",
	},
	{
		"generated virtual shifts later columns", "CREATE TABLE t(a INT, b INT GENERATED ALWAYS AS (a*2) VIRTUAL, c TEXT)",
		[]string{"a/INT/I/r0", "b/INT/I/gv/r-1", "c/TEXT/T/r1"},
		"alias=-1 stored=2",
	},
	{
		"generated stored and bare AS", "CREATE TABLE t(a INT, b INT AS (a+1) STORED, c AS (a+2), d, e GENERATED ALWAYS AS (a))",
		[]string{"a/INT/I/r0", "b/INT/I/gs/r1", "c//B/gv/r-1", "d//B/r2", "e//B/gv/r-1"},
		"alias=-1 stored=3",
	},
	{
		"default literals", "CREATE TABLE t(a DEFAULT 5, b DEFAULT -7, c DEFAULT 2.5, d DEFAULT 'it''s', e DEFAULT x'0AFF', f DEFAULT NULL, g DEFAULT TRUE, h DEFAULT FALSE, i DEFAULT +3, j DEFAULT -2.5e1, k DEFAULT 0x10, l DEFAULT abc)",
		[]string{
			"a//B/d=5/r0", "b//B/d=-7/r1", "c//B/d=2.5f/r2", "d//B/d='it's'/r3", "e//B/d=x'0AFF'/r4", "f//B/d=NULL/r5",
			"g//B/d=1/r6", "h//B/d=0/r7", "i//B/d=3/r8", "j//B/d=-25f/r9", "k//B/d=16/r10", "l//B/d='abc'/r11",
		},
		"alias=-1 stored=12",
	},
	{
		"default expressions", "CREATE TABLE t(a DEFAULT CURRENT_TIMESTAMP, b DEFAULT (1+1), c DEFAULT (datetime('now')), d DEFAULT CURRENT_DATE, e DEFAULT CURRENT_TIME)",
		[]string{"a//B/dX/r0", "b//B/dX/r1", "c//B/dX/r2", "d//B/dX/r3", "e//B/dX/r4"},
		"alias=-1 stored=5",
	},
	{
		"default then constraint", "CREATE TABLE t(a INTEGER DEFAULT 5 NOT NULL, b INT DEFAULT -5 NOT NULL, c DEFAULT 'x' UNIQUE)",
		[]string{"a/INTEGER/I/nn/d=5/r0", "b/INT/I/nn/d=-5/r1", "c//B/d='x'/r2"},
		"alias=-1 stored=3",
	},
	{
		"default string with a parenthesis", "CREATE TABLE t(a TEXT DEFAULT ')', b)",
		[]string{"a/TEXT/T/d=')'/r0", "b//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"COLLATE", "CREATE TABLE t(a TEXT COLLATE NOCASE, b COLLATE \"RTRIM\")",
		[]string{"a/TEXT/T/c=NOCASE/r0", "b//B/c=RTRIM/r1"},
		"alias=-1 stored=2",
	},
	{
		"IF NOT EXISTS, TEMP, temp.t", "CREATE TEMP TABLE IF NOT EXISTS temp.t(a)",
		[]string{"a//B/r0"},
		"alias=-1 stored=1",
	},
	{
		"TEMPORARY and quoted schema", "CREATE TEMPORARY TABLE \"temp\".\"t\"(a)",
		[]string{"a//B/r0"},
		"alias=-1 stored=1",
	},
	{
		"keyword column names, quoted", "CREATE TABLE t(\"primary\" INT, \"default\" TEXT, \"key\", \"select\" PRIMARY KEY)",
		[]string{"primary/INT/I/r0", "default/TEXT/T/r1", "key//B/r2", "select//B/pk1/r3"},
		"alias=-1 stored=4",
	},
	{
		"keyword-like bare column names", "CREATE TABLE t(key INT, action TEXT, \"order\", \"constraint\" INT, \"unique\")",
		[]string{"key/INT/I/r0", "action/TEXT/T/r1", "order//B/r2", "constraint/INT/I/r3", "unique//B/r4"},
		"alias=-1 stored=5",
	},
	{
		"unicode names", "CREATE TABLE t(名前 TEXT, \"naïve\" INT, é)",
		[]string{"名前/TEXT/T/r0", "naïve/INT/I/r1", "é//B/r2"},
		"alias=-1 stored=3",
	},
	{
		"semicolon and trailing comment", "CREATE TABLE t(a); -- done",
		[]string{"a//B/r0"},
		"alias=-1 stored=1",
	},
	{
		"table constraints with nested parentheses and strings", "CREATE TABLE t(a, b, UNIQUE(a, b), CHECK (a IN (')', '(') AND b <> ')'))",
		[]string{"a//B/r0", "b//B/r1"},
		"alias=-1 stored=2",
	},
	{
		"columns named like types", "CREATE TABLE t(text TEXT, integer INTEGER PRIMARY KEY)",
		[]string{"text/TEXT/T/r0", "integer/INTEGER/I/pk1/r1"},
		"alias=1 stored=2",
	},
	{
		"NOT NULL alias", "CREATE TABLE t(a INTEGER PRIMARY KEY NOT NULL)",
		[]string{"a/INTEGER/I/nn/pk1/r0"},
		"alias=0 stored=1",
	},
	{
		"NULL constraint", "CREATE TABLE t(a INT NULL, b TEXT NULL ON CONFLICT FAIL)",
		[]string{"a/INT/I/r0", "b/TEXT/T/r1"},
		"alias=-1 stored=2",
	},
	{
		"WITHOUT ROWID with generated columns", "CREATE TABLE t(a PRIMARY KEY, b, c AS (b) STORED, d AS (a) VIRTUAL) WITHOUT ROWID",
		[]string{"a//B/nn/pk1/r0", "b//B/r1", "c//B/gs/r2", "d//B/gv/r-1"},
		"alias=-1 stored=3 W",
	},
	{
		"blob default lower case", "CREATE TABLE t(a BLOB DEFAULT x'ab')",
		[]string{"a/BLOB/B/d=x'AB'/r0"},
		"alias=-1 stored=1",
	},
	{
		"whitespace between name and parenthesis", "CREATE TABLE t (\n\ta\tINT,\r\n\tb TEXT\n)\n",
		[]string{"a/INT/I/r0", "b/TEXT/T/r1"},
		"alias=-1 stored=2",
	},
	{
		"large integer default becomes real", "CREATE TABLE t(a DEFAULT 9223372036854775807, b DEFAULT -9223372036854775808, c DEFAULT 9223372036854775808)",
		[]string{"a//B/d=9223372036854775807/r0", "b//B/d=-9223372036854775808/r1", "c//B/d=9.223372036854776e+18f/r2"},
		"alias=-1 stored=3",
	},
}

func TestCreateTableParse(t *testing.T) {
	if len(tableCases) < 40 {
		t.Fatalf("%d cases, want about 40", len(tableCases))
	}
	for _, c := range tableCases {
		t.Run(c.name, func(t *testing.T) {
			def, virtual, _ := sqlitefile.ParseCreateTable(c.sql)
			if virtual {
				t.Fatal("not a virtual table")
			}
			if !def.ParseOK || def.ParseNote != "" {
				t.Fatalf("ParseOK %v note %q", def.ParseOK, def.ParseNote)
			}
			var got []string
			for _, col := range def.Columns {
				got = append(got, describeCol(col))
			}
			if strings.Join(got, "|") != strings.Join(c.cols, "|") {
				t.Errorf("columns\n got  %q\n want %q", got, c.cols)
			}
			if f := describeFlags(def); f != c.flags {
				t.Errorf("flags %q, want %q", f, c.flags)
			}
		})
	}
}

func TestCreateTableParseDefaultValues(t *testing.T) {
	def, _, _ := sqlitefile.ParseCreateTable("CREATE TABLE t(a DEFAULT x'0aFF', b DEFAULT 'é', c DEFAULT (1+1))")
	if !def.ParseOK || len(def.Columns) != 3 {
		t.Fatalf("%+v", def)
	}
	if v := def.Columns[0].Default.Value; v.Kind != sqlitefile.KindBlob || len(v.Bytes) != 2 || v.Bytes[0] != 0x0a || v.Bytes[1] != 0xff || v.Len != 2 {
		t.Errorf("blob default %+v", v)
	}
	if v := def.Columns[1].Default.Value; v.Kind != sqlitefile.KindText || string(v.Bytes) != "é" || v.Len != 2 || v.Enc != sqlitefile.EncUTF8 {
		t.Errorf("text default %+v", v)
	}
	if d := def.Columns[2].Default; d.Kind != sqlitefile.DefaultExpr || d.Text != "(1+1)" || d.Value.Kind != sqlitefile.KindNull {
		t.Errorf("expression default %+v", d)
	}
}

type indexCase struct {
	name    string
	sql     string
	table   string
	unique  bool
	partial bool
	cols    string // name[!]/desc/collation; "!" marks an expression
	notOK   bool
}

var indexCases = []indexCase{
	{name: "plain", sql: "CREATE INDEX i ON t(a)", table: "t", cols: "a"},
	{
		name: "unique, if not exists, schema, quoting, desc, collation",
		sql:  "CREATE UNIQUE INDEX IF NOT EXISTS main.i ON \"t\"(a DESC, b COLLATE NOCASE, c COLLATE RTRIM ASC)", table: "t", unique: true, cols: "a/desc,b//NOCASE,c//RTRIM",
	},
	{name: "partial", sql: "CREATE INDEX i ON t(a) WHERE a > 5 AND b IS NOT NULL", table: "t", partial: true, cols: "a"},
	{name: "partial with a parenthesis in a string", sql: "CREATE INDEX i ON t(a) WHERE x = ')'", table: "t", partial: true, cols: "a"},
	{name: "expression keys", sql: "CREATE INDEX i ON t(lower(name), a+b, \"c\" DESC)", table: "t", cols: "!,!,c/desc"},
	{name: "expression with collation and DESC", sql: "CREATE INDEX i ON t(lower(name) COLLATE NOCASE DESC)", table: "t", cols: "!/desc/NOCASE"},
	{name: "bracket and backtick quoting", sql: "CREATE INDEX [i x] ON [t y](`a b`)", table: "t y", cols: "a b"},
	{name: "nested expression", sql: "CREATE INDEX i ON t((a+(b*2)), c)", table: "t", cols: "!,c"},
	{name: "comments", sql: "CREATE /* x */ INDEX i -- y\n ON t(a /* ) */, b)", table: "t", cols: "a,b"},
	{name: "semicolon", sql: "CREATE INDEX i ON t(a);", table: "t", cols: "a"},
	{name: "lower case", sql: "create unique index i on t(a asc)", table: "t", unique: true, cols: "a"},
	{name: "no columns", sql: "CREATE INDEX i ON t()", notOK: true},
	{name: "truncated", sql: "CREATE INDEX i ON t(", notOK: true},
	{name: "no table", sql: "CREATE INDEX i", notOK: true},
	{name: "not an index", sql: "CREATE TABLE t(a)", notOK: true},
	{name: "empty", sql: "", notOK: true},
	{name: "unbalanced expression", sql: "CREATE INDEX i ON t(lower((a)", notOK: true},
}

func describeIndexCols(d sqlitefile.IndexDef) string {
	var parts []string
	for _, c := range d.Columns {
		s := c.Name
		if c.Expr {
			s = "!"
		}
		if c.Desc || c.Collation != "" {
			s += "/"
			if c.Desc {
				s += "desc"
			}
			if c.Collation != "" {
				s += "/" + c.Collation
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

func TestCreateIndexParse(t *testing.T) {
	for _, c := range indexCases {
		t.Run(c.name, func(t *testing.T) {
			def, _ := sqlitefile.ParseCreateIndex(c.sql)
			if c.notOK {
				if def.ParseOK || len(def.Columns) != 0 {
					t.Fatalf("expected a parse failure: %+v", def)
				}
				return
			}
			if !def.ParseOK {
				t.Fatalf("not parsed: %+v", def)
			}
			if def.Table != c.table || def.Unique != c.unique || def.Partial != c.partial || def.Auto {
				t.Errorf("table %q unique %v partial %v auto %v", def.Table, def.Unique, def.Partial, def.Auto)
			}
			if got := describeIndexCols(def); got != c.cols {
				t.Errorf("columns %q, want %q", got, c.cols)
			}
		})
	}
}

var (
	noteShape = regexp.MustCompile(`^[a-z][a-z-]{0,15}$`)
	allNotes  = map[string]bool{"empty": true, "not-create": true, "truncated": true, "unbalanced": true, "syntax": true, "limit": true, "depth": true, "ctas": true, "virtual": true, "columns": true, "pk": true}
)

// TestCreateParseUnparseable: text the parser cannot read sets ParseOK=false
// with a short token note, returns no columns, and never panics or fails.
var unparseableCases = []struct{ name, sql, note string }{
	{"empty", "", "empty"},
	{"blank", " \t\r\n ", "empty"},
	{"only a comment", "-- nothing\n/* here */", "empty"},
	{"garbage", "garbage here", "not-create"},
	{"not a table", "CREATE VIEW v AS SELECT 1", "not-create"},
	{"truncated list", "CREATE TABLE t(a, b", "truncated"},
	{"truncated after name", "CREATE TABLE t", "truncated"},
	{"truncated after open", "CREATE TABLE t(", "truncated"},
	{"truncated option", "CREATE TABLE t(a) WITHOUT", "truncated"},
	{"truncated default", "CREATE TABLE t(a DEFAULT", "truncated"},
	{"unbalanced check", "CREATE TABLE t(a CHECK ((a > 0)", "unbalanced"},
	{"extra close", "CREATE TABLE t(a))", "syntax"},
	{"bad option", "CREATE TABLE t(a) FOO", "syntax"},
	{"empty item", "CREATE TABLE t(a, ,b)", "syntax"},
	{"no columns", "CREATE TABLE t()", "columns"},
	{"create as select", "CREATE TABLE t AS SELECT 1", "ctas"},
	{"two primary keys", "CREATE TABLE t(a INT PRIMARY KEY, b INT PRIMARY KEY)", "pk"},
	{"primary key of an unknown column", "CREATE TABLE t(a, PRIMARY KEY(zz))", "pk"},
	{"without rowid but no key", "CREATE TABLE t(a) WITHOUT ROWID", "pk"},
	{"unterminated string", "CREATE TABLE t(a DEFAULT 'abc", "syntax"},
	{"unterminated identifier", "CREATE TABLE t(\"a INT)", "syntax"},
	{"unterminated bracket", "CREATE TABLE t([a INT)", "syntax"},
	{"NUL byte", "CREATE TABLE t(a\x00 INT)", "syntax"},
	{"constraint without a name", "CREATE TABLE t(a CONSTRAINT", "truncated"},
	{"bad generated", "CREATE TABLE t(a AS b)", "syntax"},
	{"foreign key without references", "CREATE TABLE t(a, FOREIGN KEY (a)", "truncated"},
}

func TestCreateParseUnparseable(t *testing.T) {
	for _, c := range unparseableCases {
		t.Run(c.name, func(t *testing.T) {
			def, virtual, steps := sqlitefile.ParseCreateTable(c.sql)
			if virtual || def.ParseOK || def.ParseNote != c.note || len(def.Columns) != 0 || def.RowidAlias != -1 {
				t.Fatalf("virtual %v ParseOK %v note %q columns %d alias %d, want note %q", virtual, def.ParseOK, def.ParseNote, len(def.Columns), def.RowidAlias, c.note)
			}
			if steps > 100*len(c.sql)+100 {
				t.Errorf("%d steps for %d bytes", steps, len(c.sql))
			}
		})
	}
	t.Run("virtual table", func(t *testing.T) {
		for _, sql := range []string{"CREATE VIRTUAL TABLE t USING fts5(a, b)", "create virtual table if not exists main.t using rtree(id, x0, x1)"} {
			def, virtual, _ := sqlitefile.ParseCreateTable(sql)
			if !virtual || def.ParseOK || def.ParseNote != "virtual" || len(def.Columns) != 0 {
				t.Errorf("%q: virtual %v %+v", sql, virtual, def)
			}
		}
	})
}

// TestCreateParseBounded: hostile text costs bounded work. Nesting a million
// deep, a huge column list, a flood of tokens and long junk all end quickly
// with a note, within 100 token steps per input byte, without recursion.
func TestCreateParseBounded(t *testing.T) {
	long := func(prefix, unit string, n int) string { return prefix + strings.Repeat(unit, n) }
	for _, c := range []struct {
		name string
		sql  string
	}{
		{"nesting a million deep", long("CREATE TABLE t(a CHECK ", "(", 1_000_000)},
		{"nesting deep and closed", long("CREATE TABLE t(a CHECK ", "(", 500_000) + strings.Repeat(")", 500_000) + ")"},
		{"a million columns", "CREATE TABLE t(" + strings.Repeat("a,", 1_000_000) + "a)"},
		{"many constraints on one column", long("CREATE TABLE t(a ", "NOT NULL ", 200_000) + ")"},
		{"many words in a type", long("CREATE TABLE t(a ", "INT ", 300_000) + ")"},
		{"junk", strings.Repeat("\x01\xff';\"[`(", 100_000)},
		{"unterminated comment", "CREATE TABLE t(a /*" + strings.Repeat("*", 1<<20)},
		{"deep index expression", long("CREATE INDEX i ON t(", "(", 1_000_000)},
		{"many index columns", "CREATE INDEX i ON t(" + strings.Repeat("a,", 500_000) + "a)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			def, _, steps := sqlitefile.ParseCreateTable(c.sql)
			if steps > 100*len(c.sql)+100 {
				t.Errorf("table parser: %d steps for %d bytes", steps, len(c.sql))
			}
			if def.ParseOK && def.StoredColumns > sqlitefile.DefaultLimits().MaxColumns {
				t.Errorf("%d stored columns past the cap", def.StoredColumns)
			}
			if !def.ParseOK && !allNotes[def.ParseNote] {
				t.Errorf("note %q", def.ParseNote)
			}
			idef, isteps := sqlitefile.ParseCreateIndex(c.sql)
			if isteps > 100*len(c.sql)+100 {
				t.Errorf("index parser: %d steps for %d bytes", isteps, len(c.sql))
			}
			if idef.ParseOK && len(idef.Columns) > sqlitefile.DefaultLimits().MaxColumns {
				t.Errorf("%d index columns past the cap", len(idef.Columns))
			}
		})
	}
	// The column cap is enforced (a lowered cap shows it cheaply) and exact:
	// cap columns parse, cap+1 do not.
	mk := func(n int) string {
		cols := make([]string, n)
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		return "CREATE TABLE t(" + strings.Join(cols, ",") + ")"
	}
	if def, _, _ := sqlitefile.ParseCreateTableCols(mk(50), 50); !def.ParseOK || len(def.Columns) != 50 {
		t.Errorf("exactly the cap must parse: %v %d", def.ParseOK, len(def.Columns))
	}
	if def, _, _ := sqlitefile.ParseCreateTableCols(mk(51), 50); def.ParseOK || def.ParseNote != "columns" {
		t.Errorf("one past the cap must fail with columns: %v %q", def.ParseOK, def.ParseNote)
	}
	// A deep but legal nesting inside the depth cap parses; past it, "depth".
	deep := func(n int) string {
		return "CREATE TABLE t(a CHECK " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n) + ")"
	}
	if def, _, _ := sqlitefile.ParseCreateTable(deep(100)); !def.ParseOK {
		t.Errorf("nesting 100 must parse: %q", def.ParseNote)
	}
	if def, _, _ := sqlitefile.ParseCreateTable(deep(100_000)); def.ParseOK || def.ParseNote != "depth" {
		t.Errorf("nesting 100000: ParseOK %v note %q, want depth", def.ParseOK, def.ParseNote)
	}
	// Every note is a short token.
	for _, sql := range []string{"", "x", "CREATE TABLE t(", "CREATE VIRTUAL TABLE v USING m"} {
		def, _, _ := sqlitefile.ParseCreateTable(sql)
		if !noteShape.MatchString(def.ParseNote) || sqlShape.MatchString(def.ParseNote) {
			t.Errorf("note %q is not a short token", def.ParseNote)
		}
	}
}
