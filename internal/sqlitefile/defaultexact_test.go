package sqlitefile_test

// Column DEFAULT values convert only when the conversion is exact (ruling U3,
// Task 1b). A default the reader cannot reproduce exactly is delivered as not
// stored and undecided, with a typed reason (U21), never as a rounded number
// and never as the unconverted text.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// shortDefault builds a table t(a, c <decl> default <lit>) whose only record is
// short (it holds a alone) and returns what Resolve gives column c and the
// definition of c.
func shortDefault(t testing.TB, enc int, decl, lit string) (sqlitefile.Value, sqlitefile.Column) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024, Encoding: enc})
	tb := b.CreateTable("t", "create table t(a, c "+decl+" default "+lit+")")
	tb.InsertRaw(1, []uint64{1}, []byte{5})
	tab, _ := openTable(t, b.Bytes(), "t")
	var got sqlitefile.Value
	n := 0
	if err := tab.Rows(context.Background(), func(r sqlitefile.Row) bool {
		got = tab.Resolve(r)[1]
		n++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows", n)
	}
	d := tab.Def()
	if !d.ParseOK || len(d.Columns) != 2 {
		t.Fatalf("definition not parsed: %+v", d)
	}
	return got, d.Columns[1]
}

// TestColumnDefaultExactDecimalConverts (PASSES ON ARRIVAL: the exact cases
// already convert today; the mutation in the report proves the test can fail):
// defaults the reader reproduces exactly convert as the engine does.
func TestColumnDefaultExactDecimalConverts(t *testing.T) {
	type c struct{ lit, numeric, integer, real, text, none string }
	cases := []c{
		{"'0.5'", "f:0.5", "f:0.5", "f:0.5", "t:0.5", "t:0.5"},
		{"0.25", "f:0.25", "f:0.25", "f:0.25", "", "f:0.25"}, // a REAL on TEXT is Task 1b's undecided case
		{"'12'", "i:12", "i:12", "f:12", "t:12", "t:12"},
		{"'1e3'", "i:1000", "i:1000", "f:1000", "t:1e3", "t:1e3"},
		{"'  7  '", "i:7", "i:7", "f:7", "t:  7  ", "t:  7  "},
		{"'-0'", "i:0", "i:0", "f:0", "t:-0", "t:-0"},
		{"7", "i:7", "i:7", "f:7", "t:7", "i:7"},
		{"'abc'", "t:abc", "t:abc", "t:abc", "t:abc", "t:abc"},
	}
	for _, enc := range []int{1, 2, 3} {
		for _, k := range cases {
			for _, d := range []struct{ decl, want string }{
				{"numeric", k.numeric}, {"integer", k.integer}, {"real", k.real}, {"text", k.text}, {"", k.none},
			} {
				if d.want == "" {
					continue
				}
				v, _ := shortDefault(t, enc, d.decl, k.lit)
				if got := valStr(v); got != d.want || v.DefaultUndecided != sqlitefile.DefaultReasonNone {
					t.Errorf("enc %d: %q default %s: got %s (undecided %v), want %s", enc, d.decl, k.lit, got, v.DefaultUndecided, d.want)
				}
			}
		}
	}
}

// TestColumnDefaultInexactIsUndecided: a default the reader cannot convert
// exactly is Omitted and carries the reason; it is neither a rounded number nor
// the unconverted text.
func TestColumnDefaultInexactIsUndecided(t *testing.T) {
	strLits := []string{
		"'3.141592653589793238'", "'12345678901234567890'", "'1e23'", "'1e400'", "'0.12345678901234567'", "'-1e-400'",
	}
	for _, enc := range []int{1, 2, 3} {
		for _, decl := range []string{"numeric", "integer", "real"} {
			for _, lit := range strLits {
				v, _ := shortDefault(t, enc, decl, lit)
				if !v.Omitted || v.DefaultUndecided != sqlitefile.DefaultReasonInexactLiteral || v.Kind != sqlitefile.KindNull {
					t.Errorf("enc %d: %s default %s: %s (Omitted %v, reason %v); want an undecided inexact literal", enc, decl, lit, valStr(v), v.Omitted, v.DefaultUndecided)
				}
			}
		}
		// A string that looks like a number stays what it is on a column that keeps text.
		for _, decl := range []string{"text", ""} {
			for _, lit := range strLits {
				v, _ := shortDefault(t, enc, decl, lit)
				want := "t:" + strings.Trim(lit, "'")
				if got := valStr(v); got != want || v.DefaultUndecided != sqlitefile.DefaultReasonNone {
					t.Errorf("enc %d: %q default %s: got %s; want %s", enc, decl, lit, got, want)
				}
			}
		}
		// A REAL needed as TEXT: the engine's spelling of a real is not reproduced.
		v, _ := shortDefault(t, enc, "text", "0.25")
		if !v.Omitted || v.DefaultUndecided != sqlitefile.DefaultReasonInexactLiteral {
			t.Errorf("enc %d: a REAL default on a TEXT column: %s (reason %v); want undecided", enc, valStr(v), v.DefaultUndecided)
		}
	}
}

// TestExpressionDefaultHasTheTypedReason: an expression default is not evaluated;
// it is Omitted with its own reason, told apart from an inexact literal (U21).
func TestExpressionDefaultHasTheTypedReason(t *testing.T) {
	for _, lit := range []string{"(1+1)", "current_timestamp", "current_date"} {
		v, c := shortDefault(t, 1, "integer", lit)
		if !v.Omitted || v.DefaultUndecided != sqlitefile.DefaultReasonExpression {
			t.Errorf("default %s: Omitted %v reason %v; want an undecided expression", lit, v.Omitted, v.DefaultUndecided)
		}
		if c.Default.Kind != sqlitefile.DefaultExpr {
			t.Errorf("default %s: kind %v, want DefaultExpr", lit, c.Default.Kind)
		}
	}
	if got := sqlitefile.DefaultReasonExpression.String(); got != "default is an expression" {
		t.Errorf("reason text %q", got)
	}
	if sqlitefile.DefaultReasonInexactLiteral.String() == sqlitefile.DefaultReasonExpression.String() || sqlitefile.DefaultReasonNone.String() != "" {
		t.Error("the reasons are not told apart by their text")
	}
}

// TestDdlLiteralDefaultInexactIsNotRounded: a DDL decimal literal the reader
// cannot reproduce exactly is not evaluated: its source text is kept and its
// value is not a rounded float.
func TestDdlLiteralDefaultInexactIsNotRounded(t *testing.T) {
	inexact := []string{
		"0.1234567890123456789", "1e23", "12345678901234567890", "-0.1234567890123456789", "+1e400",
		"9223372036854775808", "9223372036854775807.0", "9.223372036854775807e18", "1e-23",
	}
	for _, lit := range inexact {
		v, c := shortDefault(t, 1, "", lit)
		if c.Default.Kind != sqlitefile.DefaultInexactLiteral || c.Default.Text != lit || c.Default.Value.Kind != sqlitefile.KindNull {
			t.Errorf("DEFAULT %s: kind %v text %q value %+v; want an inexact literal that keeps its source", lit, c.Default.Kind, c.Default.Text, c.Default.Value)
		}
		if !v.Omitted || v.DefaultUndecided != sqlitefile.DefaultReasonInexactLiteral {
			t.Errorf("DEFAULT %s: resolved %s (reason %v); want undecided inexact literal", lit, valStr(v), v.DefaultUndecided)
		}
	}
	exact := map[string]string{
		"0.5": "f:0.5", "7": "i:7", "-7": "i:-7", "1e3": "f:1000", "0x10": "i:16", "-0.25": "f:-0.25", "7.": "f:7",
		"-9223372036854775808": "i:-9223372036854775808", "9223372036854775807": "i:9223372036854775807",
	}
	for lit, want := range exact {
		v, c := shortDefault(t, 1, "", lit)
		if c.Default.Kind != sqlitefile.DefaultLiteral || valStr(v) != want || v.DefaultUndecided != sqlitefile.DefaultReasonNone {
			t.Errorf("DEFAULT %s: kind %v resolved %s; want a literal %s", lit, c.Default.Kind, valStr(v), want)
		}
	}
}

// TestDefaultTextOfFourMiBDigitsIsBounded: a digit string as a default is
// classified from its first 64 bytes (no allocation grows with it), and a
// definition over MaxSchemaSQLBytes is not parsed at all.
func TestDefaultTextOfFourMiBDigitsIsBounded(t *testing.T) {
	t.Run("under the cap: undecided, nothing proportional to the text is allocated", func(t *testing.T) {
		digits := strings.Repeat("7", 900<<10)
		b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		tb := b.CreateTable("t", "create table t(a, c integer default '"+digits+"')")
		tb.InsertRaw(1, []uint64{1}, []byte{5})
		tab, _ := openTable(t, b.Bytes(), "t")
		var row sqlitefile.Row
		if err := tab.Rows(context.Background(), func(r sqlitefile.Row) bool { row = r; return true }); err != nil {
			t.Fatal(err)
		}
		if !tab.Def().ParseOK {
			t.Fatal("the definition is not parsed")
		}
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		v := tab.Resolve(row)[1]
		runtime.ReadMemStats(&m1)
		if !v.Omitted || v.DefaultUndecided != sqlitefile.DefaultReasonInexactLiteral {
			t.Errorf("resolved %s (reason %v); want undecided", valStr(v), v.DefaultUndecided)
		}
		if d := m1.TotalAlloc - m0.TotalAlloc; d > 64<<10 {
			t.Errorf("Resolve allocated %d bytes for a %d byte default; want at most 64 KiB", d, len(digits))
		}
	})
	t.Run("over the cap: the definition is not parsed", func(t *testing.T) {
		digits := strings.Repeat("7", 4<<20)
		b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
		tb := b.CreateTable("t", "create table t(a, c integer default '"+digits+"')")
		tb.InsertRaw(1, []uint64{1}, []byte{5})
		tab, _ := openTable(t, b.Bytes(), "t")
		if d := tab.Def(); d.ParseOK {
			t.Errorf("a %d byte definition was parsed", len(digits))
		}
	})
}

// TestInexactLiteralDefaultFitsOnlyAtGuess (U22): a column whose default the
// reader cannot reproduce exactly is not "addable" for a strict fit, so a short
// record fits the table at BasisGuess, not BasisFit. An exact default stays a fit.
func TestInexactLiteralDefaultFitsOnlyAtGuess(t *testing.T) {
	I := sqlitefile.AffInteger
	lit := func(c sqlitefile.Column, v sqlitefile.Value) sqlitefile.Column {
		c.Default = sqlitefile.Default{Kind: sqlitefile.DefaultLiteral, Value: v}
		return c
	}
	inexactDDL := func(c sqlitefile.Column) sqlitefile.Column {
		c.Default = sqlitefile.Default{Kind: sqlitefile.DefaultInexactLiteral, Text: "1e23"}
		return c
	}
	cases := []struct {
		name  string
		col   sqlitefile.Column
		basis sqlitefile.TableBasis
	}{
		{"exact literal default, not null", lit(notNull(fcol("c", I)), vInt(0)), sqlitefile.BasisFit},
		{"exact text default, not null", lit(notNull(fcol("c", I)), vText("12")), sqlitefile.BasisFit},
		{"nullable, no default", fcol("c", I), sqlitefile.BasisFit},
		{"inexact text default, not null", lit(notNull(fcol("c", I)), vText("12345678901234567890")), sqlitefile.BasisGuess},
		{"inexact text default, nullable", lit(fcol("c", I), vText("1e23")), sqlitefile.BasisGuess},
		{"inexact DDL literal, nullable", inexactDDL(fcol("c", I)), sqlitefile.BasisGuess},
		{"inexact DDL literal, not null", inexactDDL(notNull(fcol("c", I))), sqlitefile.BasisGuess},
	}
	for _, k := range cases {
		s := schemaOf(tableObj("t", tableDef(false, -1, fcol("a", I), k.col)))
		tb, basis := s.FitPage([][]sqlitefile.Value{{vInt(1)}})
		if tb != "t" || basis != k.basis {
			t.Errorf("%s: fit %q %q; want t %q", k.name, tb, basis, k.basis)
		}
	}
}
