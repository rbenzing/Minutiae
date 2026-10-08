package sqlitedb_test

// Engine oracles for the join layer. The modernc driver is used only here, in
// test code, on files the tests write to their own temporary directory.

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
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

type matrixCase struct {
	name, decl string
}

var matrixDecls = []matrixCase{
	{"none", ""},
	{"integer", "integer"},
	{"text", "text"},
	{"nocase", "text collate nocase"},
	{"rtrim", "text collate rtrim"},
	{"real", "real"},
	{"numeric", "numeric"},
	{"blob-nocase", "blob collate nocase"},
}

var matrixValues = []any{1, 1.0, "1", "abc", "ABC", "abc  ", []byte("abc"), []byte("ABC"), nil, 1.5, "Abc", 2}

// B46: every pair of the engine's own JOIN is found through KeyOf + Rowids or
// its source row is flagged, and no pair the engine does not return is
// returned unflagged.
func TestJoinsAgreeWithTheEngineJoinMatrix(t *testing.T) {
	for _, enc := range []string{"UTF-8", "UTF-16le"} {
		for _, sc := range matrixDecls {
			for _, tc := range matrixDecls {
				t.Run(fmt.Sprintf("%s/src-%s/tgt-%s", enc, sc.name, tc.name), func(t *testing.T) {
					checkJoinAgainstEngine(t, engineWritten(t, enc, sc.decl, tc.decl))
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

func checkJoinAgainstEngine(t *testing.T, data []byte) {
	t.Helper()
	db := engineOn(t, data)
	rows, err := db.Query("select s.id, g.id from s join g on s.k = g.k")
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
	ours := map[[2]int64]bool{}
	flagged := map[int64]bool{}
	if err := src.Scan(t.Context(), func(r sqlitedb.Row) error {
		sid, _ := r.Rowid()
		k, ok, kerr := sqlitedb.KeyOf(r, 1)
		if kerr != nil {
			flagged[sid] = true
			return nil
		}
		if !ok {
			return nil
		}
		ids, fl, err := ix.Rowids(k)
		if err != nil {
			flagged[sid] = true
			return nil
		}
		if fl != 0 {
			flagged[sid] = true
		}
		for _, id := range ids {
			ours[[2]int64{sid, id}] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for p := range engine {
		if !ours[p] && !flagged[p[0]] {
			t.Errorf("engine pair %v missed with source row %d unflagged", p, p[0])
		}
	}
	for p := range ours {
		if !engine[p] && !flagged[p[0]] {
			t.Errorf("extra unflagged pair %v", p)
		}
	}
}
