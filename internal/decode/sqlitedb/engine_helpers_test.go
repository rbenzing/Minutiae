package sqlitedb_test

// Shared helpers of the engine oracles (engine_test.go, engine_join_test.go,
// oracle_fixture_test.go). The modernc driver is imported in test files only
// and only ever opens COPIES the tests write to their own temporary directory.

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
)

// copyFiles writes the given files (name -> bytes; nil is skipped) into dir.
// A sharing violation (Windows, a scanner holding the new file) is retried and
// the retry is logged.
func copyFiles(t testing.TB, dir string, files map[string][]byte) {
	t.Helper()
	for name, b := range files {
		if b == nil {
			continue
		}
		var err error
		for try := range 5 {
			if err = os.WriteFile(filepath.Join(dir, name), b, 0o600); err == nil {
				break
			}
			t.Logf("copy of %s: %v (retry %d)", name, err, try+1)
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// openEngine opens the database file at path with the engine, on one
// connection, and closes it when the test ends.
func openEngine(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// engineOnCopy opens the engine on a copy of the given files in a fresh
// temporary directory; the copy is named x.db / x.db-wal / x.db-journal.
func engineOnCopy(t testing.TB, db, wal, journal []byte) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	copyFiles(t, dir, map[string][]byte{"x.db": db, "x.db-wal": wal, "x.db-journal": journal})
	p := filepath.Join(dir, "x.db")
	return openEngine(t, p), p
}

// engineOn is engineOnCopy for a database file alone.
func engineOn(t testing.TB, data []byte) *sql.DB {
	t.Helper()
	e, _ := engineOnCopy(t, data, nil, nil)
	return e
}

// ev is one value as either side reports it, comparable with ==.
type ev struct {
	kind string // null, int, float, text, blob, omitted
	i    int64
	f    uint64 // IEEE bits
	b    string
}

func (v ev) String() string {
	switch v.kind {
	case "int":
		return "int " + strconv.FormatInt(v.i, 10)
	case "float":
		return fmt.Sprintf("float %v", math.Float64frombits(v.f))
	case "text":
		return fmt.Sprintf("text %q", v.b)
	case "blob":
		return fmt.Sprintf("blob %x", v.b)
	case "omitted":
		return "omitted(" + v.b + ")"
	}
	return v.kind
}

// engVal converts a driver value. A float NaN is NULL, as the engine reads it.
func engVal(x any) (ev, error) {
	switch v := x.(type) {
	case nil:
		return ev{kind: "null"}, nil
	case int64:
		return ev{kind: "int", i: v}, nil
	case float64:
		if math.IsNaN(v) {
			return ev{kind: "null"}, nil
		}
		return ev{kind: "float", f: math.Float64bits(v)}, nil
	case string:
		return ev{kind: "text", b: v}, nil
	case []byte:
		return ev{kind: "blob", b: string(v)}, nil
	}
	return ev{}, fmt.Errorf("driver value of type %T", x)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// engineStoredCols is the engine's own list of the columns of a table that its
// select returns: the stored ones, from pragma table_xinfo (hidden 2 marks a
// virtual generated column, which is not stored). It asserts the layer describes
// the same columns (names, order, Virtual flags), so a column the layer dropped,
// renamed or mis-numbered fails here instead of silently leaving the comparison;
// the select list of every engine comparison comes from this list, never from
// the layer (B84).
func engineStoredCols(t testing.TB, eng *sql.DB, tb *sqlitedb.Table) []string {
	t.Helper()
	rs, err := eng.Query("select name, hidden from pragma_table_xinfo(?) order by cid", tb.Name())
	if err != nil {
		t.Fatalf("table_xinfo(%s): %v", tb.Name(), err)
	}
	defer func() { _ = rs.Close() }()
	var all, stored []string
	var virtual []bool
	for rs.Next() {
		var name string
		var hidden int
		if err := rs.Scan(&name, &hidden); err != nil {
			t.Fatal(err)
		}
		if hidden == 1 {
			continue // a virtual table's hidden column: not a column of an ordinary table
		}
		all = append(all, name)
		virtual = append(virtual, hidden == 2)
		if hidden != 2 {
			stored = append(stored, name)
		}
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	cols := tb.Cols()
	if len(cols) != len(all) {
		t.Errorf("table %s: the engine has %d columns %q, the layer %d", tb.Name(), len(all), all, len(cols))
		return stored
	}
	for i, c := range cols {
		if c.Name != all[i] || c.Virtual != virtual[i] {
			t.Errorf("table %s column %d: the engine has %q (virtual %v), the layer %q (virtual %v)", tb.Name(), i, all[i], virtual[i], c.Name, c.Virtual)
		}
	}
	return stored
}

// visibleCols are the columns the layer says the engine's select can return: the
// stored ones. Only the layer-side positions are taken from it; the engine's
// select list comes from engineStoredCols.
func visibleCols(tb *sqlitedb.Table) (idx []int) {
	for i, c := range tb.Cols() {
		if !c.Virtual {
			idx = append(idx, i)
		}
	}
	return idx
}

// engineRows returns the engine's rows of the table: for a rowid table
// [rowid, visible columns...] ordered by rowid, for a WITHOUT ROWID table the
// visible columns in key order.
func engineRows(db *sql.DB, table string, cols []string, withoutRowid bool) ([][]ev, error) {
	q := make([]string, 0, len(cols)+1)
	if !withoutRowid {
		q = append(q, "rowid")
	}
	for _, c := range cols {
		q = append(q, quoteIdent(c))
	}
	stmt := "select " + strings.Join(q, ", ") + " from " + quoteIdent(table) //nolint:gosec // identifiers are quoted; the database is a test copy
	if !withoutRowid {
		stmt += " not indexed order by rowid"
	}
	rs, err := db.Query(stmt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out [][]ev
	for rs.Next() {
		dst := make([]any, len(q))
		ptr := make([]any, len(q))
		for i := range dst {
			ptr[i] = &dst[i]
		}
		if err := rs.Scan(ptr...); err != nil {
			return nil, err
		}
		row := make([]ev, len(q))
		for i, x := range dst {
			if row[i], err = engVal(x); err != nil {
				return nil, fmt.Errorf("%s column %s: %w", table, q[i], err)
			}
		}
		out = append(out, row)
	}
	return out, rs.Err()
}

// layerRows returns the layer's rows in the engineRows shape. A column whose
// state is not Known is the "omitted" marker carrying the state name.
func layerRows(t testing.TB, tb *sqlitedb.Table) [][]ev {
	t.Helper()
	idx := visibleCols(tb)
	var out [][]ev
	err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		var row []ev
		if !tb.WithoutRowid() {
			id, ok := r.Rowid()
			if !ok {
				t.Errorf("%s: a live row without a rowid", tb.Name())
			}
			row = append(row, ev{kind: "int", i: id})
		}
		for _, c := range idx {
			row = append(row, layerVal(r, c))
		}
		out = append(out, row)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan %s: %v", tb.Name(), err)
	}
	return out
}

func layerVal(r sqlitedb.Row, c int) ev {
	st := r.State(c)
	if !st.Known() {
		return ev{kind: "omitted", b: st.String()}
	}
	if r.IsNull(c) {
		return ev{kind: "null"}
	}
	if v, ok := r.Int(c); ok {
		return ev{kind: "int", i: v}
	}
	if f, ok := r.Float(c); ok {
		if math.IsNaN(f) {
			return ev{kind: "null"}
		}
		return ev{kind: "float", f: math.Float64bits(f)}
	}
	if b, ok := r.Text(c); ok {
		return ev{kind: "text", b: string(b)}
	}
	if b, ok := r.Blob(c); ok {
		return ev{kind: "blob", b: string(b)}
	}
	return ev{kind: "omitted", b: "unreadable"}
}

// diffRows names the first difference between two row lists, or "".
func diffRows(want, got [][]ev) string {
	for i := range max(len(want), len(got)) {
		if i >= len(want) {
			return fmt.Sprintf("row %d: the layer has an extra row %v", i, got[i])
		}
		if i >= len(got) {
			return fmt.Sprintf("row %d: the layer lacks the row %v", i, want[i])
		}
		if len(want[i]) != len(got[i]) {
			return fmt.Sprintf("row %d: %d values, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if want[i][j] != got[i][j] {
				return fmt.Sprintf("row %d (first value %v), value %d: layer %v, want %v", i, want[i][0], j, got[i][j], want[i][j])
			}
		}
	}
	return ""
}

// oracleRows converts the fixture oracle's live rows of a table.
func oracleRows(lt fxLiveTable) ([][]ev, error) {
	out := make([][]ev, 0, len(lt.Rows))
	for _, r := range lt.Rows {
		row := make([]ev, len(r))
		for i, v := range r {
			var e ev
			switch v.T {
			case "null":
				e = ev{kind: "null"}
			case "int":
				n, err := strconv.ParseInt(v.V, 10, 64)
				if err != nil {
					return nil, err
				}
				e = ev{kind: "int", i: n}
			case "float":
				bits, err := strconv.ParseUint(v.V, 16, 64)
				if err != nil {
					return nil, err
				}
				if math.IsNaN(math.Float64frombits(bits)) {
					e = ev{kind: "null"}
				} else {
					e = ev{kind: "float", f: bits}
				}
			case "text":
				e = ev{kind: "text", b: v.V}
			case "blob":
				b, err := base64.StdEncoding.DecodeString(v.V)
				if err != nil {
					return nil, err
				}
				e = ev{kind: "blob", b: string(b)}
			default:
				return nil, fmt.Errorf("oracle value type %q", v.T)
			}
			row[i] = e
		}
		out = append(out, row)
	}
	return out, nil
}

// engineTables lists the engine's ordinary tables (no sqlite_ internals).
func engineTables(db *sql.DB) (map[string]bool, error) {
	rs, err := db.Query("select name from sqlite_master where type = 'table' and substr(name, 1, 7) <> 'sqlite_' and sql not like 'create virtual%'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := map[string]bool{}
	for rs.Next() {
		var n string
		if err := rs.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rs.Err()
}

// layerDump scans every table of d and returns the rows by table name; a table
// the layer cannot resolve is reported through skip.
func layerDump(t testing.TB, d *sqlitedb.DB, skip func(name string, err error)) map[string][][]ev {
	t.Helper()
	names, err := d.Tables(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][][]ev{}
	for _, n := range names {
		tb, err := d.Table(t.Context(), n, nil, nil)
		if err != nil {
			skip(n, err)
			continue
		}
		out[n] = layerRows(t, tb)
	}
	return out
}
