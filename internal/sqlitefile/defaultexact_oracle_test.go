package sqlitefile_test

// The engine decides what a column DEFAULT is worth (Task 1b, ruling U3): for a
// matrix of column declarations and literals, the value the reader delivers for
// a short record equals the engine's, bit for bit, whenever it is decided, and
// the cases it calls undecided are exactly the documented families.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

type defClass uint8

const (
	defExact      defClass = iota // converts as the engine does on every declaration
	defDDLInexact                 // a bare numeric literal that is not a float64 this reader reproduces: undecided everywhere
	defStrInexact                 // number-like text that is not reproduced: undecided where the affinity is numeric
	defReal                       // an exact REAL literal: undecided only where it would be rendered as TEXT
)

type defLit struct {
	lit   string
	class defClass
}

// defaultMatrix is the list of literals: the exact decimals and the inexact families
// of D2, and the integral reals around the INTEGER conversion edge (the engine's
// rule is ix > MinInt64 && ix < MaxInt64, so every integral real from 9.2e18 up to
// 2^63 is an integer for an INTEGER or NUMERIC column).
var defaultMatrix = []defLit{
	{"'0.5'", defExact},
	{"0.25", defReal},
	{"'12'", defExact},
	{"'1e3'", defExact},
	{"'  7  '", defExact},
	{"'-0'", defExact},
	{"7", defExact},
	{"1.5", defReal},
	{"'abc'", defExact},
	{"'7.0'", defExact},
	{"'.5'", defExact},
	{"'0.1'", defExact},
	{"0.1", defReal},
	{"-7", defExact},
	{"'+7'", defExact},
	{"'3.141592653589793238'", defStrInexact},
	{"'12345678901234567890'", defStrInexact},
	{"'1e23'", defStrInexact},
	{"'1e400'", defStrInexact},
	{"'0.12345678901234567'", defStrInexact},
	{"'1e-23'", defStrInexact},
	{"0.1234567890123456789", defDDLInexact},
	{"1e23", defDDLInexact},
	{"12345678901234567890", defDDLInexact},
	{"9223372036854775808", defDDLInexact},
	{"-0.1234567890123456789", defDDLInexact},
	{"'9223372036854775807'", defExact},
	{"9223372036854775807", defExact},
	{"-9223372036854775808", defExact},
	{"'-9223372036854775808'", defExact},
	{"'9223372036854775808'", defStrInexact},
	// integral reals in [9.2e18, 2^63) and their neighbours
	{"9.2e18", defReal},
	{"9.21e18", defReal},
	{"9.22e18", defReal},
	{"-9.22e18", defReal},
	{"9.2233720368547e18", defReal},
	{"9.3e18", defReal},
	{"-9.3e18", defReal},
	{"-9.2e18", defReal},
	{"'9.2e18'", defExact},
	{"'9.22e18'", defExact},
	{"'9.2233720368547e18'", defExact},
	{"'9.3e18'", defExact},
	{"'-9.22e18'", defExact},
	{"9223372036854775807.0", defDDLInexact},
	{"9.223372036854775807e18", defDDLInexact},
	{"'9223372036854775807.0'", defStrInexact},
	{"'9.223372036854775807e18'", defStrInexact},
	{"1e15", defReal},
	{"1e18", defReal},
	{"1e22", defReal},
	{"'1e22'", defExact},
	{"1e-5", defReal},
}

var defaultDecls = []string{"numeric", "integer", "real", "text", ""}

func isNumericDecl(d string) bool { return d == "numeric" || d == "integer" || d == "real" }

// defaultUndecidedBy is the documented rule for a literal of class k on a declaration.
func defaultUndecidedBy(k defClass, decl string) bool {
	switch k {
	case defDDLInexact:
		return true
	case defStrInexact:
		return isNumericDecl(decl)
	case defReal:
		return decl == "text"
	}
	return false
}

// bitsOf renders a value for exact comparison: kind, integer, float bits, text.
func bitsOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return fmt.Sprintf("i:%d", x)
	case float64:
		return fmt.Sprintf("f:%016x", math.Float64bits(x))
	case string:
		return "t:" + x
	case []byte:
		return fmt.Sprintf("b:%x", x)
	}
	return fmt.Sprintf("?%T", v)
}

func readerBits(v sqlitefile.Value) string {
	switch v.Kind {
	case sqlitefile.KindNull:
		return "N"
	case sqlitefile.KindInt:
		return fmt.Sprintf("i:%d", v.Int)
	case sqlitefile.KindFloat:
		return fmt.Sprintf("f:%016x", math.Float64bits(v.Float))
	case sqlitefile.KindText:
		s, _ := v.Text()
		return "t:" + s
	}
	return fmt.Sprintf("b:%x", v.Bytes)
}

// TestDefaultsMatchEngineMatrix and TestIntegralRealDefaultMatchesEngineKind:
// one engine-built database per declaration holds a short-record table with a
// column added by ALTER TABLE ADD COLUMN for every literal.
func TestDefaultsMatchEngineMatrix(t *testing.T) {
	decided, undecided := 0, 0
	for _, decl := range defaultDecls {
		path := filepath.Join(t.TempDir(), "m.db")
		db := openEngine(t, path)
		stmts := []string{"create table t(id integer primary key, a)", "insert into t(a) values(1), (2)"}
		for i, d := range defaultMatrix {
			stmts = append(stmts, fmt.Sprintf("alter table t add column c%d %s default %s", i, decl, d.lit))
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("engine, declaration %q: %s: %v", decl, s, err)
			}
		}
		cols := make([]string, len(defaultMatrix))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
		engine := engineDefaults(t, db, cols)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tab, _ := openTable(t, data, "t")
		def := tab.Def()
		if !def.ParseOK || len(def.Columns) != 2+len(defaultMatrix) {
			t.Fatalf("declaration %q: definition %+v", decl, def)
		}
		rows := 0
		if err := tab.Rows(context.Background(), func(r sqlitefile.Row) bool {
			vals := tab.Resolve(r)
			for i, d := range defaultMatrix {
				v := vals[2+i]
				got := v.DefaultUndecided != sqlitefile.DefaultReasonNone
				if got != defaultUndecidedBy(d.class, decl) {
					t.Errorf("declaration %q default %s: undecided = %v (reader %s); the documented rule says %v (engine %s)", decl, d.lit, got, valStr(v), !got, engine[i])
					continue
				}
				if got {
					undecided++
					if !v.Omitted || v.Kind != sqlitefile.KindNull {
						t.Errorf("declaration %q default %s: an undecided default must be Omitted and null-kinded: %+v", decl, d.lit, v)
					}
					continue
				}
				decided++
				if w := readerBits(v); w != engine[i] {
					t.Errorf("declaration %q default %s: reader %s, engine %s", decl, d.lit, w, engine[i])
				}
			}
			rows++
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Fatalf("declaration %q: %d rows, want 2", decl, rows)
		}
	}
	// Counts measured from this matrix (two short rows per cell; U23): not a ratio.
	t.Logf("matrix: %d literals x %d declarations x 2 rows: %d decided, %d undecided", len(defaultMatrix), len(defaultDecls), decided, undecided)
	if decided != wantDecidedCells || undecided != wantUndecidedCells {
		t.Errorf("decided %d undecided %d cells; the matrix was measured at %d and %d", decided, undecided, wantDecidedCells, wantUndecidedCells)
	}
}

// engineDefaults returns, for the columns, the engine's value of the first row.
func engineDefaults(t *testing.T, db *sql.DB, cols []string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("select %s from t order by id", strings.Join(cols, ", ")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var first []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		cur := make([]string, len(cols))
		for i, v := range vals {
			cur[i] = bitsOf(v)
		}
		if first == nil {
			first = cur
		} else {
			for i := range cur {
				if cur[i] != first[i] {
					t.Fatalf("the engine's two short rows differ at %s: %s and %s", cols[i], first[i], cur[i])
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return first
}

// TestIntegralRealDefaultMatchesEngineKind: a REAL literal that is integral and
// strictly inside (-2^63, 2^63) is an integer on an INTEGER or NUMERIC column
// (the engine's rule), including [9.2e18, 2^63); a REAL column keeps it a float.
func TestIntegralRealDefaultMatchesEngineKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db := openEngine(t, path)
	lits := []string{"9.2e18", "9.21e18", "9.22e18", "-9.22e18", "9.2233720368547e18", "9.3e18"}
	if _, err := db.Exec("create table t(id integer primary key, a)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("insert into t(a) values(1)"); err != nil {
		t.Fatal(err)
	}
	for i, l := range lits {
		for _, decl := range []string{"integer", "numeric", "real"} {
			if _, err := db.Exec(fmt.Sprintf("alter table t add column %s%d %s default %s", decl, i, decl, l)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := openEngine(t, path)
	tab, _ := openTable(t, data, "t")
	def := tab.Def()
	if err := tab.Rows(context.Background(), func(r sqlitefile.Row) bool {
		vals := tab.Resolve(r)
		for i, c := range def.Columns[2:] {
			var ev any
			if err := eng.QueryRow(fmt.Sprintf("select %s from t", c.Name)).Scan(&ev); err != nil {
				t.Fatal(err)
			}
			if got, want := readerBits(vals[2+i]), bitsOf(ev); got != want {
				t.Errorf("column %s: reader %s, engine %s", c.Name, got, want)
			}
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

// The cell counts the matrix above measures (see TestDefaultsMatchEngineMatrix).
const wantDecidedCells, wantUndecidedCells = 376, 154
