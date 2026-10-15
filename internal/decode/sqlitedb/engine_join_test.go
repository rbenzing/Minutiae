package sqlitedb_test

// Engine oracles for the join layer. The modernc driver is used only here, in
// test code, on files the tests write to their own temporary directory.

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// B45: the engine reads a stored NaN REAL as NULL, so a NaN equals nothing.
func TestEngineReadsAStoredNaNAsNull(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, a)")
		tt.Insert(1, 1, math.NaN())
		tt.Insert(2, 2, 1.5)
	})
	db := engineOn(t, data)
	var isNull int
	var typeOf string
	if err := db.QueryRow("select a is null, typeof(a) from t where id = 1").Scan(&isNull, &typeOf); err != nil {
		t.Fatal(err)
	}
	if isNull != 1 || typeOf != "null" {
		t.Fatalf("engine reads a stored NaN as is-null=%d typeof=%s; the layer must follow it", isNull, typeOf)
	}
	var n int
	if err := db.QueryRow("select count(*) from t x join t y on x.a = y.a where x.id = 1").Scan(&n); err != nil || n != 0 {
		t.Fatalf("NaN self-join pairs = %d, %v", n, err)
	}
}

// matrixCase is one column declaration with the collation and affinity class the
// ENGINE gives it (written down here, not read from the layer).
type matrixCase struct {
	name, decl, collation string
	class                 parse.JoinClass
}

var matrixDecls = []matrixCase{
	{"none", "", "binary", parse.ClassBlob},
	{"integer", "integer", "binary", parse.ClassNumeric},
	{"text", "text", "binary", parse.ClassText},
	{"nocase", "text collate nocase", "nocase", parse.ClassText},
	{"rtrim", "text collate rtrim", "rtrim", parse.ClassText},
	{"real", "real", "binary", parse.ClassNumeric},
	{"numeric", "numeric", "binary", parse.ClassNumeric},
	{"blob-nocase", "blob collate nocase", "nocase", parse.ClassBlob},
}

var matrixValues = []any{
	1, 1.0, "1", "abc", "ABC", "abc  ", []byte("abc"), []byte("ABC"), nil, 1.5, "Abc", 2,
	r(0xe9), r(0xc9), r(0xff), r(0x178), "caf" + r(0xe9), "CAF" + r(0xc9), r(0xe9) + "  ",
}

// r is a non-ASCII letter (Latin-1 or beyond) kept out of the source text; NOCASE
// folds ASCII only, so r(0xe9) and r(0xc9) must NOT meet under it.
func r(c rune) string { return string(c) }

// B46/B50: the engine JOIN (target as the left operand) against KeyOf + Rowids.
//
// One database per (encoding, source declaration) holds the source table s and
// one target table per target declaration (B89), so the engine writes 16
// databases instead of 128; the 256 subtests and their assertions are unchanged.
func TestJoinsAgreeWithTheEngineJoinMatrix(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		t.Run(enc, func(t *testing.T) {
			for _, sc := range matrixDecls {
				t.Run("src-"+sc.name, func(t *testing.T) {
					data := engineMatrixDB(t, enc, sc.decl)
					eng := engineOn(t, data)
					d := openBytes(t, data, nil, nil, bigBudget())
					for _, tc := range matrixDecls {
						t.Run("tgt-"+tc.name, func(t *testing.T) {
							checkJoinAgainstEngine(t, eng, d, sc, tc, "g_"+tc.name)
						})
					}
				})
			}
		})
	}
}

// engineMatrixDB returns the bytes of a database the ENGINE wrote (so every
// value is stored as the engine's affinity rules leave it): the source table s
// (column k declared sdecl) and, for every declaration of the matrix, the
// target table g_<name> (column k declared so); each holds the matrix values,
// written in one transaction.
func engineMatrixDB(t *testing.T, enc, sdecl string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`pragma encoding = "` + enc + `"`)
	type table struct{ name, decl string }
	tables := []table{{"s", sdecl}}
	for _, tc := range matrixDecls {
		tables = append(tables, table{"g_" + tc.name, tc.decl})
	}
	exec("begin")
	for _, tb := range tables {
		// The rows go into a fixed-name table that is renamed afterwards, so no SQL
		// text here names a table dynamically (the single-writer scan in archtest).
		exec("create table w(id integer primary key, k " + tb.decl + ")")
		for i, v := range matrixValues {
			exec("insert into w(id, k) values(?, ?)", i+1, v)
		}
		exec("alter table w rename to " + quoteIdent(tb.name))
	}
	exec("commit")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// engineTwoTables is the database the ENGINE writes with tables s(id, k sdecl)
// and g(id, k gdecl) holding the given values (row i has id i+1).
func engineTwoTables(t *testing.T, enc, sdecl, gdecl string, svals, gvals []any) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`pragma encoding = "` + enc + `"`,
		"create table s(id integer primary key, k " + sdecl + ")",
		"create table g(id integer primary key, k " + gdecl + ")",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, v := range svals {
		if _, err := db.Exec("insert into s(id, k) values(?, ?)", i+1, v); err != nil {
			t.Fatal(err)
		}
	}
	for i, v := range gvals {
		if _, err := db.Exec("insert into g(id, k) values(?, ?)", i+1, v); err != nil {
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

// enginePairs is the engine's own join of the tables s and g of data, with the
// TARGET g as the left operand so that its column's collation decides:
// (source rowid, target rowid) pairs.
func enginePairs(t testing.TB, data []byte) map[[2]int64]bool {
	t.Helper()
	return enginePairsOn(t, engineOn(t, data), "g")
}

// enginePairsOn is enginePairs over an open engine and the target table gname.
func enginePairsOn(t testing.TB, db *sql.DB, gname string) map[[2]int64]bool {
	t.Helper()
	rows, err := db.Query("select s.id, g.id from " + quoteIdent(gname) + " g join s on g.k = s.k") //nolint:gosec // gname is a fixed test table name
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[[2]int64]bool{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		out[[2]int64{a, b}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// layerLookup is one source row's lookup through KeyOf and Index.Rowids.
type layerLookup struct {
	source int64
	ids    []int64
	flags  sqlitedb.JoinFlags
	err    error
}

// layerLookups runs every non-NULL source key of s.k against the index of g.k.
// A NULL or NaN source value makes no lookup, as the engine matches it with
// nothing.
func layerLookups(t testing.TB, data []byte) []layerLookup {
	t.Helper()
	return layerLookupsOn(t, openBytes(t, data, nil, nil, bigBudget()), "g")
}

// layerLookupsOn is layerLookups over an open database and the target table gname.
func layerLookupsOn(t testing.TB, d *sqlitedb.DB, gname string) []layerLookup {
	t.Helper()
	src, err := d.Table(t.Context(), "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := d.Table(t.Context(), gname, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tgt.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []layerLookup
	if err := src.Scan(t.Context(), func(row sqlitedb.Row) error {
		sid, _ := row.Rowid()
		k, ok, kerr := sqlitedb.KeyOf(row, 1)
		if kerr != nil {
			out = append(out, layerLookup{source: sid, err: kerr})
			return nil
		}
		if !ok {
			return nil
		}
		ids, fl, err := ix.Rowids(k)
		out = append(out, layerLookup{source: sid, ids: ids, flags: fl, err: err})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func pairsOf(ls []layerLookup) map[[2]int64]bool {
	out := map[[2]int64]bool{}
	for _, l := range ls {
		for _, id := range l.ids {
			out[[2]int64{l.source, id}] = true
		}
	}
	return out
}

func samePairs(t testing.TB, engine, ours map[[2]int64]bool) {
	t.Helper()
	for p := range engine {
		if !ours[p] {
			t.Errorf("engine pair %v missed by the layer", p)
		}
	}
	for p := range ours {
		if !engine[p] {
			t.Errorf("pair %v returned by the layer but not by the engine", p)
		}
	}
}

func checkJoinAgainstEngine(t *testing.T, eng *sql.DB, d *sqlitedb.DB, sc, tc matrixCase, gname string) {
	t.Helper()
	engine := enginePairsOn(t, eng, gname)
	lookups := layerLookupsOn(t, d, gname)

	// B50: the flags are exact in both directions. The expectation is written
	// down from the declarations (the engine's collation and affinity class of
	// each column), not read from the layer: a lookup is flagged exactly when
	// they differ.
	var want sqlitedb.JoinFlags
	if sc.collation != tc.collation {
		want |= sqlitedb.JoinCollationDiffers
	}
	if sc.class != tc.class {
		want |= sqlitedb.JoinAffinityDiffers
	}
	if len(lookups) == 0 {
		t.Fatal("no lookup was made")
	}
	for _, l := range lookups {
		if l.err != nil {
			t.Errorf("source row %d: %v (every matrix value is decidable)", l.source, l.err)
			continue
		}
		if l.flags != want {
			t.Errorf("source row %d: flags %b, want %b (src %s/%d, tgt %s/%d)", l.source, l.flags, want, sc.collation, sc.class, tc.collation, tc.class)
		}
	}
	if sc.class != tc.class {
		// The accepted affinity gap is only a MISS: flagged above. A pair the layer
		// returns that the engine does not (a false positive) always fails.
		ours := pairsOf(lookups)
		for p := range ours {
			if !engine[p] {
				t.Errorf("cross-class pair %v returned by the layer but not by the engine", p)
			}
		}
		return
	}
	// Same class: the layer's pairs equal the engine's exactly, flagged or not
	// (a collation flag alone never changes the pairs, because the target's
	// collation decides on both sides).
	samePairs(t, engine, pairsOf(lookups))
}

// TestJoinMatchesEngineJoin (B12): for each pair of declarations the engine's
// own join and the layer's KeyOf + Rowids return the same pairs, and every
// lookup carries exactly the expected flags.
func TestJoinMatchesEngineJoin(t *testing.T) {
	const big = int64(1)<<53 + 1
	cases := []joinCase{
		{
			name: "numeric", enc: "UTF-8", sdecl: "real", tdecl: "integer",
			svals: []any{1, 1.0, -3, -3.0, big, 2.5, nil}, tvals: []any{1, -3, big, big - 1, 2, 3, nil},
			minPairs: 4,
		},
		{
			name: "nocase", enc: "UTF-8", sdecl: "text", tdecl: "text collate nocase",
			svals: []any{"abc", "ABC", "Abc", "x", r(0xe9), r(0xc9), nil}, tvals: []any{"ABC", "abc", "x", r(0xe9), "y", nil},
			flags: sqlitedb.JoinCollationDiffers, minPairs: 6,
		},
		{
			name: "rtrim", enc: "UTF-8", sdecl: "text", tdecl: "text collate rtrim",
			svals: []any{"a", "a   ", " a", "b", ""}, tvals: []any{"a", "a  ", "b  ", " a", "  "},
			flags: sqlitedb.JoinCollationDiffers, minPairs: 5,
		},
		{
			name: "binary", enc: "UTF-8", sdecl: "text", tdecl: "text",
			svals: []any{"abc", "ABC", r(0xe9), r(0xc9)}, tvals: []any{"abc", "Abc", r(0xe9), "x"},
			minPairs: 2,
		},
		{
			name: "utf16", enc: "UTF-16le", sdecl: "text", tdecl: "text collate nocase",
			svals: []any{"abc", "ABC", r(0xe9), r(0xc9), "z"}, tvals: []any{"Abc", "ABC", r(0xe9), "q", r(0x4e2d)},
			flags: sqlitedb.JoinCollationDiffers, minPairs: 3,
		},
		{
			name: "blob", enc: "UTF-8", sdecl: "text", tdecl: "blob",
			svals: []any{"abc", "ABC", []byte("abc")}, tvals: []any{[]byte("abc"), "abc", []byte("ABC"), []byte{0, 1}},
			flags: sqlitedb.JoinAffinityDiffers, minPairs: 1,
		},
		{
			name: "null", enc: "UTF-8", sdecl: "integer", tdecl: "integer",
			svals: []any{nil, nil, 1}, tvals: []any{nil, nil, 1},
			minPairs: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := engineTwoTables(t, c.enc, c.sdecl, c.tdecl, c.svals, c.tvals)
			engine := enginePairs(t, data)
			lookups := layerLookups(t, data)
			if len(engine) < c.minPairs {
				t.Fatalf("the engine joins %d pairs, the case is meant to hold at least %d", len(engine), c.minPairs)
			}
			for _, l := range lookups {
				if l.err != nil {
					t.Errorf("source row %d: %v", l.source, l.err)
				}
				if l.flags != c.flags {
					t.Errorf("source row %d: flags %b, want %b", l.source, l.flags, c.flags)
				}
			}
			samePairs(t, engine, pairsOf(lookups))
		})
	}
}

// TestJoinAffinityDifferenceIsFlaggedAndKnownToDiffer documents the accepted
// gap: the engine applies the numeric affinity of an INTEGER target to a TEXT
// '7' and matches it; the layer does not convert, finds nothing, and flags
// every such lookup.
func TestJoinAffinityDifferenceIsFlaggedAndKnownToDiffer(t *testing.T) {
	data := engineTwoTables(t, "UTF-8", "text", "integer", []any{"7", "8", "x"}, []any{7, 8, 9})
	engine := enginePairs(t, data)
	if len(engine) != 2 {
		t.Fatalf("the engine matches %d pairs, want 2 ('7' with 7, '8' with 8)", len(engine))
	}
	lookups := layerLookups(t, data)
	if got := pairsOf(lookups); len(got) != 0 {
		t.Errorf("the layer returned %v; it does not apply affinity", got)
	}
	flagged := 0
	missed := map[int64]bool{}
	for p := range engine {
		missed[p[0]] = true
	}
	for _, l := range lookups {
		if l.err != nil {
			t.Fatalf("lookup %d: %v", l.source, l.err)
		}
		if l.flags&sqlitedb.JoinAffinityDiffers != 0 {
			flagged++
		} else if missed[l.source] {
			t.Errorf("source row %d: the engine matched it, the layer did not, and it is not flagged", l.source)
		}
	}
	if flagged != len(lookups) || len(lookups) != 3 {
		t.Errorf("%d of %d lookups flagged, want all 3 (one per source key)", flagged, len(lookups))
	}

	// Through the Context the count reaches the report.
	d := openBytes(t, data, nil, nil, bigBudget())
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	src, err := d.Table(t.Context(), "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Scan(t.Context(), func(row sqlitedb.Row) error {
		k, ok, kerr := sqlitedb.KeyOf(row, 1)
		if kerr != nil || !ok {
			t.Fatalf("KeyOf: %v %v", ok, kerr)
		}
		_, err := c.Match("g", "k", k)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if j := c.Joins(); len(j) != 1 || j[0].AffinityDiffers != 3 || j[0].Lookups != 3 || j[0].CollationDiffers != 0 {
		t.Errorf("Joins = %+v, want 3 lookups all flagged for affinity", j)
	}
}

// TestUnsupportedCollationJoinMatchesEngineRefusal: a target column with a
// collation the engine does not know. The engine refuses the join; the layer
// refuses the index (never BINARY) with its typed error.
func TestUnsupportedCollationJoinMatchesEngineRefusal(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		s := b.CreateTable("s", "create table s(id integer primary key, k text)")
		g := b.CreateTable("g", "create table g(id integer primary key, k text collate foo)")
		s.Insert(1, "a")
		g.Insert(1, "a")
	})
	eng := engineOn(t, data)
	rows, err := eng.Query("select s.id, g.id from g join s on g.k = s.k")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		_ = rows.Close()
	}
	if err == nil {
		t.Fatal("the engine did not refuse a join on an unknown collation")
	}
	d := openBytes(t, data, nil, nil, bigBudget())
	tgt, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uc *sqlitedb.UnsupportedCollationError
	if ix, err := tgt.Index(t.Context(), "k", 0); !errors.As(err, &uc) || ix != nil {
		t.Errorf("Index = %v, %v; want *UnsupportedCollationError", ix, err)
	}
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	if _, err := c.Match("g", "k", sqlitedb.LiteralTextKey([]byte("a"))); !errors.As(err, &uc) {
		t.Errorf("Match = %v; want *UnsupportedCollationError", err)
	}
}

// TestMatchInsideScanMatchesEngine: Context.Match called from inside a scan of
// the source table reproduces the engine's join rows, end to end through the
// parse.MapContext a mapping receives.
func TestMatchInsideScanMatchesEngine(t *testing.T) {
	for _, c := range []struct {
		name, sdecl, tdecl string
		svals, tvals       []any
	}{
		{"nocase", "text", "text collate nocase", []any{"abc", "ABC", "x", r(0xe9), nil}, []any{"Abc", "abc", "x", r(0xc9), "q"}},
		{"numeric", "real", "integer", []any{1, 1.0, 2.5, -4, nil}, []any{1, -4, 7, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := engineTwoTables(t, "UTF-8", c.sdecl, c.tdecl, c.svals, c.tvals)
			engine := enginePairs(t, data)
			d := openBytes(t, data, nil, nil, bigBudget())
			ctx := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
			t.Cleanup(ctx.Close)
			var mc parse.MapContext = ctx
			col := mc.Column("g", "k")
			if col < 0 {
				t.Fatal("no column g.k")
			}
			src, err := d.Table(t.Context(), "s", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ours := map[[2]int64]bool{}
			if err := src.Scan(t.Context(), func(row sqlitedb.Row) error {
				sid, _ := row.Rowid()
				k, ok, kerr := sqlitedb.KeyOf(row, 1)
				if kerr != nil {
					return kerr
				}
				if !ok {
					return nil
				}
				rows, err := mc.Match("g", "k", k)
				if err != nil {
					return err
				}
				for _, m := range rows {
					id, _ := m.Rowid()
					ours[[2]int64{sid, id}] = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(engine) == 0 {
				t.Fatal("the engine joined nothing")
			}
			samePairs(t, engine, ours)
		})
	}
}

// joinCase is one declaration pair of TestJoinMatchesEngineJoin.
type joinCase struct {
	name, enc, sdecl, tdecl string
	svals, tvals            []any
	flags                   sqlitedb.JoinFlags
	minPairs                int
}

// T8 M2 (B88), pinned from limitations.md: the engine applies the column's
// affinity to a constant (int_col = '7' matches the integer 7), the layer does
// not, so LiteralTextKey("7") against an INTEGER or REAL target misses with no
// flag. If the layer ever learns this, the limitation must be closed with it.
func TestLiteralTextKeyAgainstANumericColumnMissesWhereTheEngineMatches(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(id integer primary key, i integer, r real)")
		tt.Insert(1, nil, 7, 7.0)
		tt.Insert(2, nil, 8, 8.0)
	})
	eng := engineOn(t, data)
	tb := tableFrom(t, data, "t")
	for _, col := range []string{"i", "r"} {
		var n int
		if err := eng.QueryRow(fmt.Sprintf("select count(*) from t where %s = '7'", col)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("engine: %s = '7' matched %d rows, %v; want 1", col, n, err)
		}
		ix, err := tb.Index(t.Context(), col, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids, flags, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("7")))
		if err != nil || len(ids) != 0 || flags != 0 {
			t.Errorf("%s: layer Rowids(LiteralTextKey 7) = %v, flags %v, %v; the documented gap is an unflagged miss", col, ids, flags, err)
		}
		if ids, _, err := ix.Rowids(sqlitedb.LiteralIntKey(7)); err != nil || len(ids) != 1 {
			t.Errorf("%s: the numeric literal must match: %v, %v", col, ids, err)
		}
	}
}
