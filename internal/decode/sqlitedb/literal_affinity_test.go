package sqlitedb_test

// A literal key has no affinity, so the engine applies the target column's
// affinity to it (L124). These tests hold the layer to the engine on databases
// the engine wrote itself.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// engineOneTable returns a database the ENGINE wrote: the table t(id, c decl)
// with the given values in column c (row i has id i+1).
func engineOneTable(t testing.TB, enc, decl string, values []any) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`pragma encoding = "` + enc + `"`,
		"create table t(id integer primary key, c " + decl + ")",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, v := range values {
		if _, err := db.Exec("insert into t(id, c) values(?, ?)", i+1, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// engineIDs is the engine's answer to `c = ?` with a bound parameter (which has
// no affinity), as rowids in ascending order.
func engineIDs(t testing.TB, eng *sql.DB, arg any) []int64 {
	t.Helper()
	rows, err := eng.Query("select id from t where c = ? order by id", arg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
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

func indexOf(t testing.TB, data []byte, col string) *sqlitedb.Index {
	t.Helper()
	ix, err := tableFrom(t, data, "t").Index(t.Context(), col, 0)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// checkLiteral holds the layer's answer for key to the engine's for arg.
func checkLiteral(t *testing.T, label string, eng *sql.DB, ix *sqlitedb.Index, key parse.JoinKey, arg any) []int64 {
	t.Helper()
	want := engineIDs(t, eng, arg)
	got, flags, err := ix.Rowids(key)
	if err != nil {
		t.Errorf("%s: Rowids = %v; the engine finds %v", label, err, want)
		return want
	}
	if flags != 0 {
		t.Errorf("%s: a literal key is never flagged, got %b", label, flags)
	}
	if !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
		t.Errorf("%s: layer rowids %v, engine rowids %v", label, got, want)
	}
	return want
}

func TestLiteralTextKeyAgainstANumericColumnMatchesTheEngine(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		for _, decl := range []string{"integer", "real", "numeric"} {
			data := engineOneTable(t, enc, decl, []any{7, 7.0, 8, "abc", 7.5, int64(-3)})
			eng := engineOn(t, data)
			ix := indexOf(t, data, "c")
			found := 0
			for _, s := range []string{"7", "7.0", " 7", "+7", "07", "abc", "7.5", "-3", "8", "9", "", "7x", "0x7", "1e0", "70e-1"} {
				found += len(checkLiteral(t, fmt.Sprintf("%s/%s %q", enc, decl, s), eng, ix, sqlitedb.LiteralTextKey([]byte(s)), s))
			}
			if found < 10 {
				t.Errorf("%s/%s: the probes matched %d rows in all; the test is vacuous", enc, decl, found)
			}
			// The two numeric literals of the old test still match.
			if ids, _, err := ix.Rowids(sqlitedb.LiteralIntKey(7)); err != nil || len(ids) != 2 {
				t.Errorf("%s/%s: LiteralIntKey(7) = %v, %v", enc, decl, ids, err)
			}
			if k, ok := sqlitedb.LiteralFloatKey(7.5); !ok {
				t.Fatal("no key for 7.5")
			} else if ids, _, err := ix.Rowids(k); err != nil || len(ids) != 1 {
				t.Errorf("%s/%s: LiteralFloatKey(7.5) = %v, %v", enc, decl, ids, err)
			}
		}
	}
}

func TestLiteralIntKeyAgainstATextColumnMatchesTheEngine(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
		for _, decl := range []string{"text", "varchar(20)", "text collate nocase", "clob"} {
			data := engineOneTable(t, enc, decl, []any{"7", "07", "7.0", "x", 7, "-1", -1, " 7"})
			eng := engineOn(t, data)
			ix := indexOf(t, data, "c")
			found := 0
			for _, n := range []int64{7, 0, -1, 8, 70} {
				found += len(checkLiteral(t, fmt.Sprintf("%s/%s %d", enc, decl, n), eng, ix, sqlitedb.LiteralIntKey(n), n))
			}
			if found != 4 {
				t.Errorf("%s/%s: the integer probes matched %d rows, want 4 (7 twice, -1 twice)", enc, decl, found)
			}
		}
	}
}

func TestLiteralFloatKeyAgainstATextColumnIsUndecidable(t *testing.T) {
	data := engineOneTable(t, "UTF-8", "text", []any{"7.5", "x"})
	ix := indexOf(t, data, "c")
	k, ok := sqlitedb.LiteralFloatKey(7.5)
	if !ok {
		t.Fatal("no key for 7.5")
	}
	ids, _, err := ix.Rowids(k)
	if !errors.Is(err, sqlitedb.ErrKeyUndecidable) || len(ids) != 0 {
		t.Errorf("a real against a TEXT column = %v, %v; want ErrKeyUndecidable (the engine finds the row '7.5')", ids, err)
	}
	// Against an INTEGER and a no-affinity column nothing changes and the engine agrees.
	for _, decl := range []string{"integer", ""} {
		data := engineOneTable(t, "UTF-8", decl, []any{7.5, 7, "7.5", 8})
		eng := engineOn(t, data)
		checkLiteral(t, "real against "+decl, eng, indexOf(t, data, "c"), k, 7.5)
	}
}

func TestLiteralTextKeyThatLooksNumericButIsInexactIsUndecidable(t *testing.T) {
	for _, decl := range []string{"integer", "real"} {
		data := engineOneTable(t, "UTF-8", decl, []any{1, 2})
		d := openBytes(t, data, nil, nil, bigBudget())
		notes := &fakeNotes{}
		c := newCtx(t, d, notes)
		for _, s := range []string{"12345678901234567890", "3.141592653589793238"} {
			rows, err := c.Match("t", "c", sqlitedb.LiteralTextKey([]byte(s)))
			if !errors.Is(err, sqlitedb.ErrKeyUndecidable) || len(rows) != 0 {
				t.Errorf("%s %q: Match = %d rows, %v; want ErrKeyUndecidable", decl, s, len(rows), err)
			}
		}
		j := c.Joins()
		if len(j) != 1 || j[0].Undecidable != 2 || j[0].Lookups != 2 {
			t.Errorf("%s: Joins = %+v; want 2 undecidable lookups", decl, j)
		}
		c.Close()
		if v := notes.m["join.t.c"]; !strings.Contains(v, "undecidable:2") {
			t.Errorf("%s: the Close note is %q; want undecidable:2", decl, v)
		}
	}
}

func TestLiteralTextKeyOfFourMiBDigitsIsUndecidableAndCheap(t *testing.T) {
	data := engineOneTable(t, "UTF-8", "integer", []any{1, 2})
	ix := indexOf(t, data, "c")
	key := sqlitedb.LiteralTextKey([]byte(strings.Repeat("7", 4<<20)))
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, _, err := ix.Rowids(key)
	runtime.ReadMemStats(&m1)
	if !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Fatalf("Rowids = %v; want ErrKeyUndecidable", err)
	}
	if d, limit := m1.TotalAlloc-m0.TotalAlloc, uint64(2*len(key.S)+64<<10); d > limit {
		t.Errorf("allocated %d bytes for a %d byte key; want at most %d", d, len(key.S), limit)
	}
}

// PASSES ON ARRIVAL (the layer never converted a blob). Mutation: treat a blob
// like text in ConvertForComparison.
func TestLiteralBlobKeyIsNeverConverted(t *testing.T) {
	for _, decl := range []string{"text", "integer", "numeric"} {
		data := engineOneTable(t, "UTF-8", decl, []any{"7", 7})
		eng := engineOn(t, data)
		ix := indexOf(t, data, "c")
		if got := checkLiteral(t, "blob against "+decl, eng, ix, sqlitedb.LiteralBlobKey([]byte{0x37}), []byte{0x37}); len(got) != 0 {
			t.Errorf("%s: the engine matched a blob with %v", decl, got)
		}
	}
}

// PASSES ON ARRIVAL (a numeric literal already matched a numeric column).
// Mutation: render the integer as text for a numeric target.
func TestLiteralIntKeyAgainstANumericColumnStillMatches(t *testing.T) {
	for _, decl := range []string{"integer", "real", "numeric"} {
		data := engineOneTable(t, "UTF-8", decl, []any{7, 7.0, 8, "7"})
		eng := engineOn(t, data)
		ix := indexOf(t, data, "c")
		if got := checkLiteral(t, "int against "+decl, eng, ix, sqlitedb.LiteralIntKey(7), int64(7)); len(got) != 3 {
			t.Errorf("%s: the engine matched %v; want three rows (7, 7.0 and the text '7' stored as 7)", decl, got)
		}
	}
}

// PASSES ON ARRIVAL (an unknown kind was always undecidable). Mutation: make the
// default branch of the kind switch in Rowids continue with a NULL probe, which
// is KeyNever and turns the lookup into an empty answer.
func TestUnknownKeyKindStaysUndecidable(t *testing.T) {
	data := engineOneTable(t, "UTF-8", "integer", []any{1})
	ix := indexOf(t, data, "c")
	ids, _, err := ix.Rowids(parse.JoinKey{Kind: parse.JoinKeyKind(200)})
	if !errors.Is(err, sqlitedb.ErrKeyUndecidable) || len(ids) != 0 {
		t.Errorf("unknown kind = %v, %v; want ErrKeyUndecidable", ids, err)
	}
}

// PASSES ON ARRIVAL (no conversion existed). Mutation: apply the TEXT conversion
// for a no-affinity target.
func TestLiteralKeyAgainstAnUnknownAffinityColumnIsUnchanged(t *testing.T) {
	data := engineOneTable(t, "UTF-8", "", []any{7, "7", 7.5, "x", "07"})
	eng := engineOn(t, data)
	ix := indexOf(t, data, "c")
	if got := checkLiteral(t, "text 7", eng, ix, sqlitedb.LiteralTextKey([]byte("7")), "7"); !slices.Equal(got, []int64{2}) {
		t.Errorf("engine: text '7' matched %v, want only the text row", got)
	}
	if got := checkLiteral(t, "int 7", eng, ix, sqlitedb.LiteralIntKey(7), int64(7)); !slices.Equal(got, []int64{1}) {
		t.Errorf("engine: integer 7 matched %v, want only the integer row", got)
	}
	k, _ := sqlitedb.LiteralFloatKey(7.5)
	checkLiteral(t, "real 7.5", eng, ix, k, 7.5)
}
