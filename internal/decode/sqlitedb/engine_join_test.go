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

func engineOn(t testing.TB, data []byte) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

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
func TestJoinsAgreeWithTheEngineJoinMatrix(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		for _, sc := range matrixDecls {
			for _, tc := range matrixDecls {
				t.Run(fmt.Sprintf("%s/src-%s/tgt-%s", enc, sc.name, tc.name), func(t *testing.T) {
					checkJoinAgainstEngine(t, engineWritten(t, enc, sc.decl, tc.decl), sc, tc)
				})
			}
		}
	}
}

// engineWritten returns the bytes of a database the ENGINE wrote (so every
// value is stored as the engine's affinity rules leave it): tables s and g,
// each holding the matrix values in column k.
func engineWritten(t *testing.T, enc, sdecl, gdecl string) []byte {
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
	for i, v := range matrixValues {
		if _, err := db.Exec("insert into s(id, k) values(?, ?)", i+1, v); err != nil {
			t.Fatal(err)
		}
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

func checkJoinAgainstEngine(t *testing.T, data []byte, sc, tc matrixCase) {
	t.Helper()
	db := engineOn(t, data)
	rows, err := db.Query("select s.id, g.id from g join s on g.k = s.k")
	if err != nil {
		t.Fatal(err)
	}
	engine := map[[2]int64]bool{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		engine[[2]int64{a, b}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	d := openBytes(t, data, nil, nil, bigBudget())
	src, err := d.Table(t.Context(), "s", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tgt.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
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
	ours := map[[2]int64]bool{}
	lookups := 0
	if err := src.Scan(t.Context(), func(row sqlitedb.Row) error {
		sid, _ := row.Rowid()
		k, ok, kerr := sqlitedb.KeyOf(row, 1)
		if kerr != nil {
			t.Errorf("source row %d: KeyOf: %v (every matrix value is decidable)", sid, kerr)
			return nil
		}
		if !ok {
			return nil
		}
		ids, fl, err := ix.Rowids(k)
		if err != nil {
			if errors.Is(err, sqlitedb.ErrKeyUndecidable) {
				t.Errorf("source row %d: undecidable lookup %v (none expected in this matrix)", sid, err)
			} else {
				t.Errorf("source row %d: Rowids: %v", sid, err)
			}
			return nil
		}
		lookups++
		if fl != want {
			t.Errorf("source row %d: flags %b, want %b (src %s/%d, tgt %s/%d)", sid, fl, want, sc.collation, sc.class, tc.collation, tc.class)
		}
		for _, id := range ids {
			ours[[2]int64{sid, id}] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if lookups == 0 {
		t.Fatal("no lookup was made")
	}
	if sc.class != tc.class {
		return // the accepted affinity gap: flagged above, pairs may differ from the engine's
	}
	// Same class: the layer's pairs equal the engine's exactly, flagged or not
	// (a collation flag alone never changes the pairs, because the target's
	// collation decides on both sides).
	for p := range engine {
		if !ours[p] {
			t.Errorf("engine pair %v missed", p)
		}
	}
	for p := range ours {
		if !engine[p] {
			t.Errorf("extra pair %v", p)
		}
	}
}
