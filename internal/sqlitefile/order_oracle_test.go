package sqlitefile_test

// Engine oracle for the key comparator (LX-4 Task 7, L105). The modernc driver
// is used only here, in test code, on databases the test creates.

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// orderText is a text Value stored in enc.
func orderText(enc sqlitefile.Encoding, s string) sqlitefile.Value {
	var b []byte
	switch enc {
	case sqlitefile.EncUTF16LE, sqlitefile.EncUTF16BE:
		for _, u := range utf16.Encode([]rune(s)) {
			if enc == sqlitefile.EncUTF16LE {
				b = append(b, byte(u), byte(u>>8))
			} else {
				b = append(b, byte(u>>8), byte(u))
			}
		}
	default:
		b = []byte(s)
	}
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: enc}
}

// orderEntry is one value of the matrix: the engine's argument and the library's Value.
type orderEntry struct {
	arg any
	val sqlitefile.Value
}

func orderMatrix(enc sqlitefile.Encoding) []orderEntry {
	var out []orderEntry
	out = append(out, orderEntry{nil, sqlitefile.Value{}})
	for _, n := range []int64{0, 1, -1, 7, math.MinInt64, math.MaxInt64, 1 << 53, 1<<53 + 1} {
		out = append(out, orderEntry{n, evInt(n)})
	}
	for _, f := range []float64{math.Copysign(0, -1), 0.5, -2.5, 1e300, 1 << 53} {
		out = append(out, orderEntry{f, evFloat(f)})
	}
	for _, s := range []string{
		"", "a", "A", "a ", "a  ", "b", "B", "ab", "aB", "é", "É", "中", "\U0001f600", "a\x00b", "a\x00", "z", "éa",
	} {
		out = append(out, orderEntry{s, orderText(enc, s)})
	}
	for _, s := range []string{"", "a", "A", "a ", "ab", "\x00", "\xff"} {
		out = append(out, orderEntry{[]byte(s), evBlob(s)})
	}
	return out
}

func engineOrderDB(t *testing.T, encPragma string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if encPragma != "" {
		if _, err := db.Exec("pragma encoding='" + encPragma + "'"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func engineIDs(t *testing.T, db *sql.DB, q string) []int {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Engine-written tables for every collation, direction and encoding: the engine's
// ORDER BY sequence is monotone under OrderValues and the engine's equal pairs
// are exactly the pairs OrderValues calls equal.
func TestOrderValuesMatchesEngineOrderBy(t *testing.T) {
	encs := []struct {
		pragma string
		enc    sqlitefile.Encoding
	}{{"", sqlitefile.EncUTF8}, {"UTF-16le", sqlitefile.EncUTF16LE}, {"UTF-16be", sqlitefile.EncUTF16BE}}
	for _, e := range encs {
		for _, coll := range []string{"BINARY", "NOCASE", "RTRIM"} {
			t.Run(fmt.Sprintf("%v/%s", e.enc, coll), func(t *testing.T) {
				db := engineOrderDB(t, e.pragma)
				if _, err := db.Exec("create table t(id integer primary key, v)"); err != nil {
					t.Fatal(err)
				}
				m := orderMatrix(e.enc)
				for i, en := range m {
					if _, err := db.Exec("insert into t values(?, ?)", i, en.arg); err != nil {
						t.Fatalf("insert %d: %v", i, err)
					}
				}
				// The equal pairs, from the engine.
				eq := map[[2]int]bool{}
				rows, err := db.Query("select a.id, b.id from t a, t b where a.v is b.v collate " + coll) //nolint:gosec // coll is one of three constants
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var x, y int
					if err := rows.Scan(&x, &y); err != nil {
						t.Fatal(err)
					}
					eq[[2]int{x, y}] = true
				}
				_ = rows.Close()
				for i := range m {
					for j := range m {
						c, st := sqlitefile.OrderValues(m[i].val, m[j].val, coll)
						if st != sqlitefile.OrderOK {
							t.Fatalf("%d (%v) vs %d (%v): status %v, want a decided order", i, m[i].arg, j, m[j].arg, st)
						}
						if (c == 0) != eq[[2]int{i, j}] {
							t.Errorf("%v vs %v under %s: OrderValues %d, the engine says equal=%v", m[i].arg, m[j].arg, coll, c, eq[[2]int{i, j}])
						}
					}
				}
				for _, dir := range []string{"", " desc"} {
					seq := engineIDs(t, db, "select id from t order by v collate "+coll+dir)
					if len(seq) != len(m) {
						t.Fatalf("engine returned %d rows, want %d", len(seq), len(m))
					}
					sign := 1
					if dir != "" {
						sign = -1
					}
					for k := 1; k < len(seq); k++ {
						a, b := m[seq[k-1]], m[seq[k]]
						c, _ := sqlitefile.OrderValues(a.val, b.val, coll)
						if sign*c > 0 {
							t.Errorf("order by v collate %s%s puts %v before %v, but OrderValues says %d", coll, dir, a.arg, b.arg, c)
						}
					}
				}
			})
		}
	}
}

// Integers and reals compare exactly (no float64 rounding of an int64), as the
// engine's < and = do on bound parameters.
func TestOrderValuesIntFloatIsExact(t *testing.T) {
	db := engineOrderDB(t, "")
	ints := []int64{0, 1, -1, math.MaxInt64, math.MaxInt64 - 1, math.MinInt64, math.MinInt64 + 1, 1 << 53, 1<<53 + 1, 1<<53 - 1, -(1 << 53), -(1<<53 + 1)}
	floats := []float64{0, math.Copysign(0, -1), 0.5, -0.5, 1 << 53, float64(math.MaxInt64), -float64(math.MaxInt64), float64(math.MinInt64), math.Inf(1), math.Inf(-1), 9223372036854774784, 1e300, -1e300, 1<<53 + 2}
	check := func(a, b any, va, vb sqlitefile.Value) {
		t.Helper()
		var lt, eq int
		if err := db.QueryRow("select ?1 < ?2, ?1 = ?2", a, b).Scan(&lt, &eq); err != nil {
			t.Fatal(err)
		}
		c, st := sqlitefile.OrderValues(va, vb, "")
		if st != sqlitefile.OrderOK {
			t.Fatalf("%v vs %v: undecided", a, b)
		}
		if (c < 0) != (lt == 1) || (c == 0) != (eq == 1) {
			t.Errorf("%v vs %v: OrderValues %d, the engine says lt=%d eq=%d", a, b, c, lt, eq)
		}
	}
	for _, i := range ints {
		for _, f := range floats {
			check(i, f, evInt(i), evFloat(f))
			check(f, i, evFloat(f), evInt(i))
		}
		for _, j := range ints {
			check(i, j, evInt(i), evInt(j))
		}
	}
	for _, f := range floats {
		for _, g := range floats {
			check(f, g, evFloat(f), evFloat(g))
		}
	}
}

// The engine cannot store a NaN; the builder can. The engine reads it as NULL, so
// its ORDER BY puts the NaN rows with the NULL, before every number, and
// OrderValues does the same.
func TestOrderValuesNaNMatchesEngine(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(id integer primary key, v)")
	vals := []any{math.NaN(), nil, int64(5), 2.5, "a", []byte("a"), math.NaN(), int64(-3)}
	for i, v := range vals {
		tb.Insert(int64(i+1), int64(i+1), v)
	}
	path := filepath.Join(t.TempDir(), "nan.db")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	db := openEngine(t, path)
	seq := engineIDs(t, db, "select id from t order by v, id")
	nulls := map[int]bool{1: true, 2: true, 7: true}
	if len(seq) != len(vals) || !nulls[seq[0]] || !nulls[seq[1]] || !nulls[seq[2]] {
		t.Fatalf("the engine's order %v does not put the NaN rows with NULL first", seq)
	}
	toVal := func(v any) sqlitefile.Value {
		switch x := v.(type) {
		case nil:
			return sqlitefile.Value{}
		case int64:
			return evInt(x)
		case float64:
			return evFloat(x)
		case string:
			return evText(x)
		}
		return evBlob(string(v.([]byte)))
	}
	byID := func(id int) sqlitefile.Value { return toVal(vals[id-1]) }
	for k := 1; k < len(seq); k++ {
		c, st := sqlitefile.OrderValues(byID(seq[k-1]), byID(seq[k]), "")
		if st != sqlitefile.OrderOK || c > 0 {
			t.Errorf("rows %d, %d: OrderValues %d (%v)", seq[k-1], seq[k], c, st)
		}
	}
	for _, id := range []int{1, 7} {
		if c, st := sqlitefile.OrderValues(byID(id), sqlitefile.Value{}, ""); c != 0 || st != sqlitefile.OrderOK {
			t.Errorf("NaN row %d against NULL: %d, %v; want 0, OrderOK", id, c, st)
		}
		if c, _ := sqlitefile.OrderValues(byID(id), evInt(-3), ""); c != -1 {
			t.Errorf("NaN row %d against -3: %d, want -1", id, c)
		}
	}
}
