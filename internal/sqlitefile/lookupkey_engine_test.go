package sqlitefile_test

// GetByKey against the oracle engine (LX-4 Task 9, L105): engine-written WITHOUT ROWID tables in
// UTF-8 and UTF-16le. Every probe is also put to the engine as `select count(*) ... where k = ?`;
// the library's answer must be the engine's, except where it says it cannot decide (never a
// guess, never "not found").

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

type keySpec struct {
	name string
	ddl  string
	cols int      // columns of the table
	keys []string // the key columns, for the engine query
	aff  []sqlitefile.Affinity
	row  func(i int, r *rand.Rand) []any
}

func keySpecs() []keySpec {
	perm := func(n int) func(int) int {
		p := rand.New(rand.NewPCG(11, 13)).Perm(n) //nolint:gosec // a fixed seed makes the fixtures reproducible; no secret is involved
		return func(i int) int { return p[i] }
	}
	p := perm(3000)
	return []keySpec{
		{
			name: "int", ddl: "create table t(k integer primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffInteger},
			row: func(i int, _ *rand.Rand) []any { return []any{int64(p(i)*3 - 1000), "v"} },
		},
		{
			name: "int_desc", ddl: "create table t(k integer, v, primary key(k desc)) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffInteger},
			row: func(i int, _ *rand.Rand) []any { return []any{int64(p(i) * 2), "v"} },
		},
		{
			name: "composite_desc_nocase", ddl: "create table t(a text collate nocase, b integer, v, primary key(a, b desc)) without rowid", cols: 3,
			keys: []string{"a", "b"}, aff: []sqlitefile.Affinity{sqlitefile.AffText, sqlitefile.AffInteger},
			row: func(i int, r *rand.Rand) []any {
				return []any{randCase(r, fmt.Sprintf("grp%02d", i%47)), int64(i / 47), "v"}
			},
		},
		{
			name: "nocase", ddl: "create table t(k text collate nocase primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffText},
			row: func(i int, r *rand.Rand) []any { return []any{randCase(r, fmt.Sprintf("key%05d", p(i))), "v"} },
		},
		{
			name: "rtrim", ddl: "create table t(k text collate rtrim primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffText},
			row: func(i int, _ *rand.Rand) []any { return []any{fmt.Sprintf("k%05d", p(i)) + spaces(i%3), "v"} },
		},
		{
			name: "text_binary_numeric_looking", ddl: "create table t(k text primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffText},
			row: func(i int, _ *rand.Rand) []any {
				if i%5 == 0 {
					return []any{"0" + strconv.Itoa(p(i)*7), "v"}
				}
				return []any{strconv.Itoa(p(i) * 7), "v"}
			},
		},
		{
			name: "real_holding_integers", ddl: "create table t(k real primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffReal},
			row: func(i int, _ *rand.Rand) []any {
				if i%2 == 0 {
					return []any{int64(p(i) * 2), "v"}
				}
				return []any{float64(p(i)*2) + 0.5, "v"}
			},
		},
		{
			name: "numeric", ddl: "create table t(k numeric primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffNumeric},
			row: func(i int, _ *rand.Rand) []any { return []any{int64(p(i) + 100), "v"} },
		},
		{
			name: "blob", ddl: "create table t(k blob primary key, v) without rowid", cols: 2,
			keys: []string{"k"}, aff: []sqlitefile.Affinity{sqlitefile.AffBlob},
			row: func(i int, _ *rand.Rand) []any { return []any{[]byte(fmt.Sprintf("b%05d", p(i))), "v"} },
		},
		{
			name: "composite_nocase_blob", ddl: "create table t(a text collate nocase, b blob, v, primary key(a, b)) without rowid", cols: 3,
			keys: []string{"a", "b"}, aff: []sqlitefile.Affinity{sqlitefile.AffText, sqlitefile.AffBlob},
			row: func(i int, r *rand.Rand) []any {
				return []any{randCase(r, fmt.Sprintf("g%02d", i%31)), []byte{byte(i / 31), byte(i % 7)}, "v"}
			},
		},
	}
}

type keyFile struct {
	spec keySpec
	enc  string
	data []byte
	rows [][]any
	path string
	db   *sql.DB
}

func buildKeyFile(t *testing.T, s keySpec, enc string) *keyFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "k.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, a ...any) {
		if _, err := db.Exec(q, a...); err != nil {
			_ = db.Close()
			t.Fatalf("%s: %s: %v", s.name, q, err)
		}
	}
	exec("pragma page_size=512")
	if enc != "" {
		exec("pragma encoding='" + enc + "'")
	}
	exec(s.ddl)
	ins := "insert into t values(" + strings.TrimSuffix(strings.Repeat("?,", s.cols), ",") + ")"
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // a fixed seed makes the fixtures reproducible; no secret is involved
	f := &keyFile{spec: s, enc: enc}
	exec("begin")
	for i := range 3000 {
		row := s.row(i, r)
		f.rows = append(f.rows, row)
		exec(ins, row...)
	}
	exec("commit")
	f.db = db
	f.path = path
	t.Cleanup(func() { _ = db.Close() })
	// the bytes: a checkpointed copy
	exec("pragma wal_checkpoint(truncate)")
	f.data = readFileRetry(t, path)
	return f
}

// toValue turns a Go probe into a library value (text in UTF-8: GetByKey re-encodes it).
func toValue(x any) sqlitefile.Value {
	switch v := x.(type) {
	case int64:
		return intKey(v)
	case float64:
		return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: v}
	case string:
		return txtKey(v)
	case []byte:
		return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: v, Len: int64(len(v))}
	}
	panic(fmt.Sprintf("probe type %T", x))
}

func toValues(k []any) []sqlitefile.Value {
	out := make([]sqlitefile.Value, len(k))
	for i, x := range k {
		out[i] = toValue(x)
	}
	return out
}

// mutations returns the probes derived from one component: the same value in another case, with
// trailing spaces, another class, a neighbour.
func mutations(x any) []any {
	switch v := x.(type) {
	case int64:
		return []any{v + 1, v - 1, int64(0), -v, v * 1000003, strconv.FormatInt(v, 10), " " + strconv.FormatInt(v, 10) + " ", "0" + strconv.FormatInt(v, 10), float64(v), float64(v) + 0.5, []byte(strconv.FormatInt(v, 10))}
	case float64:
		return []any{v + 0.5, v - 0.5, int64(v), int64(v) + 1, strconv.FormatFloat(v, 'g', -1, 64)}
	case string:
		out := []any{strings.ToUpper(v), strings.ToLower(v), v + " ", v + "  ", v + "x", "", v + "\x00"}
		if len(v) > 1 {
			out = append(out, v[:len(v)-1])
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out = append(out, n, float64(n))
		}
		return out
	case []byte:
		out := []any{append(append([]byte{}, v...), 0), []byte{}, string(v)}
		if len(v) > 1 {
			out = append(out, append([]byte{}, v[:len(v)-1]...))
		}
		return out
	}
	return nil
}

func (f *keyFile) engineCount(t *testing.T, key []any) int {
	t.Helper()
	conds := make([]string, len(key))
	for i := range key {
		conds[i] = f.spec.keys[i] + " = ?"
	}
	var n int
	if err := f.db.QueryRow("select count(*) from t where "+strings.Join(conds, " and "), key...).Scan(&n); err != nil {
		t.Fatalf("%s: engine count %v: %v", f.spec.name, key, err)
	}
	return n
}

// expectUndecided: a real against a TEXT key column needs the engine's spelling of the number.
func (f *keyFile) expectUndecided(key []any) bool {
	for i, x := range key {
		if _, ok := x.(float64); ok && f.spec.aff[i] == sqlitefile.AffText {
			return true
		}
	}
	return false
}

func (f *keyFile) name() string { return f.spec.name + "/" + f.enc }

func TestGetByKeyFindsEveryRowOfEngineWithoutRowidTables(t *testing.T) {
	var total, found, absent, undecided int
	for _, s := range keySpecs() {
		for _, enc := range []string{"", "UTF-16le"} {
			f := buildKeyFile(t, s, enc)
			_, v := openLive(t, f.data, sqlitefile.Options{})
			tbl, err := v.Table(context.Background(), "t")
			if err != nil {
				t.Fatal(err)
			}
			scan := scanTableRows(t, v, "t")
			if len(scan.rows) != 3000 || len(flaggedIdx(scan.rows)) != 0 || scan.loss.Any() {
				t.Fatalf("%s: the scan delivers %d rows, flagged %v, loss %+v", f.name(), len(scan.rows), flaggedIdx(scan.rows), scan.loss)
			}
			nkey := len(s.keys)
			for i, want := range scan.rows { // every row is found by its own key and equals the scan's row
				got := getKey(t, tbl, want.Values[:nkey]...)
				if got.what != "row" {
					t.Fatalf("%s: row %d: %s %v", f.name(), i, got.what, got.err)
				}
				if !agree(got.row.Values, want.Values) || got.row.Loc.Page != want.Loc.Page || got.row.Loc.Cell != want.Loc.Cell {
					t.Fatalf("%s: row %d differs from the scan's", f.name(), i)
				}
				total++
				found++
			}
			// the probes: the engine decides
			for i := 0; i < len(f.rows); i += 15 {
				base := f.rows[i][:nkey]
				var probes [][]any
				for c := range nkey {
					for _, m := range mutations(base[c]) {
						k := append([]any{}, base...)
						k[c] = m
						probes = append(probes, k)
					}
				}
				fresh := make([]any, nkey)
				for c := range fresh {
					fresh[c] = mutations(base[c])[0]
				}
				probes = append(probes, fresh)
				for _, k := range probes {
					want := f.engineCount(t, k)
					got := getKey(t, tbl, toValues(k)...)
					total++
					switch {
					case got.what == "undecided" && f.expectUndecided(k):
						undecided++
					case got.what == "row" && want == 1:
						found++
					case got.what == "absent" && want == 0:
						absent++
					default:
						t.Fatalf("%s: key %#v: library %s %v, engine count %d", f.name(), k, got.what, got.err, want)
					}
				}
			}
			v.Release()
		}
	}
	t.Logf("%d lookups: %d found, %d absent, %d undecided (a real against a TEXT key column)", total, found, absent, undecided)
	if absent < 2000 || undecided == 0 {
		t.Errorf("the probes cover too little: %d absent, %d undecided", absent, undecided)
	}
}

func simpleKeyFile(t *testing.T, ddl string, rows ...[]any) (*keyFile, *sqlitefile.Table, *sqlitefile.View) {
	t.Helper()
	s := keySpec{name: "simple", ddl: ddl, cols: len(rows[0])}
	s.row = func(i int, _ *rand.Rand) []any { return rows[i] }
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec("insert into t values("+strings.TrimSuffix(strings.Repeat("?,", len(r)), ",")+")", r...); err != nil { //nolint:gosec // only "?" placeholders are concatenated
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("pragma wal_checkpoint(truncate)"); err != nil {
		t.Fatal(err)
	}
	f := &keyFile{spec: s, path: path, db: db, data: readFileRetry(t, path)}
	_, v := openLive(t, f.data, sqlitefile.Options{})
	tbl, err := v.Table(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	return f, tbl, v
}

func TestGetByKeyAppliesTheKeyAffinity(t *testing.T) {
	ints := [][]any{{int64(7), "i7"}, {int64(8), "i8"}, {int64(100), "i100"}}
	f, tbl, v := simpleKeyFile(t, "create table t(k integer primary key, v) without rowid", ints...)
	defer v.Release()
	f.spec.keys = []string{"k"}
	if got := getKey(t, tbl, txtKey("7")); got.what != "row" || f.engineCount(t, []any{"7"}) != 1 {
		t.Errorf("'7' against an INTEGER key column: %s %v (engine %d)", got.what, got.err, f.engineCount(t, []any{"7"}))
	}
	if got := getKey(t, tbl, txtKey(" 7 ")); got.what != "row" {
		t.Errorf("' 7 ' against an INTEGER key column: %s %v", got.what, got.err)
	}
	if got := getKey(t, tbl, txtKey("7x")); got.what != "absent" || f.engineCount(t, []any{"7x"}) != 0 {
		t.Errorf("'7x' against an INTEGER key column: %s", got.what)
	}
	if got := getKey(t, tbl, sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: 8}); got.what != "row" {
		t.Errorf("8.0 against an INTEGER key column: %s %v", got.what, got.err)
	}

	texts := [][]any{{"7", "t7"}, {"07", "t07"}, {"abc", "tabc"}}
	f, tbl, v2 := simpleKeyFile(t, "create table t(k text primary key, v) without rowid", texts...)
	defer v2.Release()
	f.spec.keys = []string{"k"}
	if got := getKey(t, tbl, intKey(7)); got.what != "row" || f.engineCount(t, []any{int64(7)}) != 1 || string(got.row.Values[0].Bytes) != "7" {
		t.Errorf("the integer 7 against a TEXT key column: %s %v", got.what, got.err)
	}
	if got := getKey(t, tbl, intKey(8)); got.what != "absent" {
		t.Errorf("the integer 8 against a TEXT key column: %s", got.what)
	}
	for _, x := range []float64{7.5, 7} {
		got := getKey(t, tbl, sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: x})
		if got.what != "undecided" {
			t.Errorf("%v against a TEXT key column: %s %v, want undecided", x, got.what, got.err)
		}
		if got.err != nil && !errors.Is(got.err, sqlitefile.ErrKeyUndecided) {
			t.Errorf("not ErrKeyUndecided: %v", got.err)
		}
	}
}

func TestGetByKeyCollationDecidesEquality(t *testing.T) {
	for _, c := range []struct {
		coll string
		key  string
		want string
	}{
		{"nocase", "abc", "row"},
		{"nocase", "ABC", "row"},
		{"nocase", "abd", "absent"},
		{"binary", "abc", "absent"},
		{"binary", "ABC", "row"},
		{"rtrim", "ABC   ", "row"},
		{"rtrim", "ABC x", "absent"},
		{"rtrim", " ABC", "absent"},
	} {
		f, tbl, v := simpleKeyFile(t, "create table t(k text collate "+c.coll+" primary key, v) without rowid", []any{"ABC", "v"}, []any{"XYZ", "w"})
		f.spec.keys = []string{"k"}
		got := getKey(t, tbl, txtKey(c.key))
		if got.what != c.want {
			t.Errorf("%s: %q: %s %v, want %s", c.coll, c.key, got.what, got.err, c.want)
		}
		if n := f.engineCount(t, []any{c.key}); (n == 1) != (c.want == "row") {
			t.Errorf("%s: %q: the engine counts %d, the test expects %s", c.coll, c.key, n, c.want)
		}
		v.Release()
	}
}
