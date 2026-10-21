package sqlitefile_test

// Engine oracle for ConvertForComparison. The modernc driver is used only here,
// in test code, on an in-memory database the test creates: the library under
// test links no engine. Every table name in the SQL below is a constant (the
// single-writer scan in archtest).

import (
	"database/sql"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// oracleProbes are the text probes of the matrix: the strings of
// TestConvertForComparisonTable plus the decimals, the NUL cases and the inf/nan spellings (which the engine leaves text).
var oracleProbes = []string{
	"7", " 7 ", "\t7\n", "+7", "-0", "007", "7.0", "7.", ".5", "5e0", "1e3", "-3",
	"9223372036854775807", "-9223372036854775808", "7.5", "19.99", "0.1", "\v\f\r7\r\f\v",
	"abc", "", "  ", "7x", "x7", "7 7", "0x10", "1e", "1e+", ".", "+", "--7", "7..0", "1_0", "inf", "-Infinity", "NaN",
	"\xc2\xa07", "\xd9\xa7", "\x007", "ab\x007", "1e\x00", "7\x00", "7\x00abc", "7 \x00", "nokia", "e5", "7e5e", "1.2.3", "7-", "+-7", "1000", "0",
	"9223372036854775808", "-9223372036854775809", "12345678901234567890", "3.141592653589793238", "1e23",
	"1e400", "1.5e-30", "0.1234567890123456", strings.Repeat("1", 65), "1" + strings.Repeat("0", 64),
	"19.990", "0.10", "1000.0", "0.7e1", "70e-1", ".7e1", "-nan", "Inf", "INF", "+Infinity", "NaN", "inf", "nan", "infinity", "-inf",
	"1e22", "1e-22", "123456789012345", "123456789012345.6", "0.000000000000001", "1E3", "7e+0", "-7e-0", "1e+2",
}

var (
	intRE = regexp.MustCompile(`^[+-]?[0-9]+$`)
	decRE = regexp.MustCompile(`^[+-]?([0-9]*)\.?([0-9]*)(?:[eE]([+-]?[0-9]+))?$`)
)

// inUndecidedFamily says whether s belongs to one of the documented undecided
// families (D2), written independently of the classifier.
func inUndecidedFamily(s string) bool {
	tr := strings.Trim(s, " \t\n\v\f\r")
	if len(tr) > 64 {
		return true
	}
	if intRE.MatchString(tr) {
		d := strings.TrimLeft(strings.TrimLeft(tr, "+-"), "0")
		maxMag := "9223372036854775807"
		if tr[0] == '-' {
			maxMag = "9223372036854775808"
		}
		return len(d) > 19 || (len(d) == 19 && d > maxMag)
	}
	if m := decRE.FindStringSubmatch(tr); m != nil && (m[1] != "" || m[2] != "") {
		digits := strings.TrimLeft(m[1]+m[2], "0")
		exp := 0
		if m[3] != "" {
			if len(strings.TrimLeft(strings.TrimLeft(m[3], "+-"), "0")) > 4 {
				return true
			}
			for _, c := range strings.TrimLeft(m[3], "+-") {
				exp = exp*10 + int(c-'0')
			}
			if m[3][0] == '-' {
				exp = -exp
			}
		}
		shift := exp - len(m[2])
		return len(digits) > 15 || shift > 22 || shift < -22
	}
	return false
}

type oracleTable struct {
	name   string
	aff    sqlitefile.Affinity
	insert string
	count  string
	sel    string
}

var oracleTables = []oracleTable{
	{"integer", sqlitefile.AffInteger, "insert into ti(c) values(?)", "select count(*) from ti where c = ?", "select typeof(c), c from ti"},
	{"real", sqlitefile.AffReal, "insert into tr(c) values(?)", "select count(*) from tr where c = ?", "select typeof(c), c from tr"},
	{"numeric", sqlitefile.AffNumeric, "insert into tn(c) values(?)", "select count(*) from tn where c = ?", "select typeof(c), c from tn"},
	{"text", sqlitefile.AffText, "insert into tx(c) values(?)", "select count(*) from tx where c = ?", "select typeof(c), c from tx"},
}

func oracleDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		"create table ti(c integer)", "create table tr(c real)", "create table tn(c numeric)", "create table tx(c text)",
		"create table tp(c numeric)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	numeric := []any{
		int64(7), 7.0, 7.5, int64(-3), int64(0), int64(1000), int64(math.MaxInt64), int64(math.MinInt64), 1e300, 0.1, 19.99, "abc", "",
		int64(5), 1000000.0, -0.5,
	}
	text := []any{"7", "07", "7.0", " 7", "abc", "7.5", int64(7), int64(-3), int64(0), int64(math.MaxInt64), int64(math.MinInt64)}
	for _, tb := range oracleTables {
		vals := numeric
		if tb.aff == sqlitefile.AffText {
			vals = text
		}
		for _, v := range vals {
			if _, err := db.Exec(tb.insert, v); err != nil {
				t.Fatalf("%s %v: %v", tb.insert, v, err)
			}
		}
	}
	return db
}

// storedValues reads back what the engine stored in one table, as Values.
func storedValues(t *testing.T, db *sql.DB, q string) []sqlitefile.Value {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []sqlitefile.Value
	for rows.Next() {
		var typ string
		var v any
		if err := rows.Scan(&typ, &v); err != nil {
			t.Fatal(err)
		}
		switch typ {
		case "integer":
			out = append(out, evInt(v.(int64)))
		case "real":
			out = append(out, evFloat(v.(float64)))
		case "text":
			switch x := v.(type) {
			case string:
				out = append(out, evText(x))
			case []byte:
				out = append(out, evText(string(x)))
			}
		case "null":
		default:
			t.Fatalf("stored type %s", typ)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func keyCount(stored []sqlitefile.Value, probe sqlitefile.Value) int {
	pk, pst, _ := sqlitefile.EqualityKey(probe, "BINARY")
	if pst != sqlitefile.KeyOK {
		return 0
	}
	n := 0
	for _, s := range stored {
		if k, st, _ := sqlitefile.EqualityKey(s, "BINARY"); st == sqlitefile.KeyOK && k == pk {
			n++
		}
	}
	return n
}

// The converted probe finds exactly the rows the engine finds for `c = ?`, with
// the bound parameter having no affinity of its own.
func TestConvertForComparisonMatchesEngine(t *testing.T) {
	db := oracleDB(t)
	decided, total := 0, 0
	for _, tb := range oracleTables {
		stored := storedValues(t, db, tb.sel)
		var probes []sqlitefile.Value
		var args []any
		if tb.aff == sqlitefile.AffText {
			for _, n := range []int64{7, -3, 0, math.MaxInt64, math.MinInt64, 8} {
				probes, args = append(probes, evInt(n)), append(args, n)
			}
			for _, f := range []float64{7, 7.5, -3, 0, 1e300} {
				probes, args = append(probes, evFloat(f)), append(args, f)
			}
		}
		for _, s := range oracleProbes {
			probes, args = append(probes, evText(s)), append(args, s)
		}
		for i, p := range probes {
			var engine int
			if err := db.QueryRow(tb.count, args[i]).Scan(&engine); err != nil {
				t.Fatalf("%s %q: %v", tb.name, args[i], err)
			}
			conv, st := sqlitefile.ConvertForComparison(p, sqlitefile.AffBlob, tb.aff)
			total++
			if st == sqlitefile.ConvUndecided {
				if p.Kind == sqlitefile.KindFloat {
					t.Logf("%s: float %v is undecided against TEXT; the engine finds %d rows", tb.name, args[i], engine)
					continue
				}
				if p.Kind == sqlitefile.KindText && !inUndecidedFamily(args[i].(string)) {
					t.Errorf("%s %q: undecided, but it is in no documented undecided family (engine finds %d)", tb.name, args[i], engine)
				}
				continue
			}
			decided++
			if ours := keyCount(stored, conv); ours != engine {
				t.Errorf("%s %q: converted probe finds %d stored values, the engine finds %d", tb.name, args[i], ours, engine)
			}
		}
	}
	if decided*100 < total*80 {
		t.Errorf("only %d of %d probes are decided; the classifier is vacuous", decided, total)
	}
	t.Logf("%d of %d probes decided", decided, total)
}

// The converted value is the very value the engine stores under NUMERIC
// affinity (bit for bit), and text that the engine leaves as text is never
// reported as a number or as undecided.
func TestConvertForComparisonMatchesEngineStoredValue(t *testing.T) {
	db := oracleDB(t)
	for _, s := range oracleProbes {
		if _, err := db.Exec("delete from tp"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("insert into tp(c) values(?)", s); err != nil {
			t.Fatal(err)
		}
		stored := storedValues(t, db, "select typeof(c), c from tp")
		if len(stored) != 1 {
			t.Fatalf("%q: %d stored values", s, len(stored))
		}
		eng := stored[0]
		got, st := sqlitefile.ConvertForComparison(evText(s), sqlitefile.AffBlob, sqlitefile.AffNumeric)
		switch {
		case eng.Kind == sqlitefile.KindText:
			if st != sqlitefile.ConvOK || got.Kind != sqlitefile.KindText || string(got.Bytes) != s {
				t.Errorf("%q: the engine leaves it text, got kind %d status %d", s, got.Kind, st)
			}
		case st == sqlitefile.ConvUndecided:
			if !inUndecidedFamily(s) {
				t.Errorf("%q: the engine converts it (%+v) and it is undecided outside the documented families", s, eng)
			}
		default:
			if got.Kind != eng.Kind || got.Int != eng.Int || math.Float64bits(got.Float) != math.Float64bits(eng.Float) {
				t.Errorf("%q: converted to %+v; the engine stores %+v", s, got, eng)
			}
		}
	}
}
