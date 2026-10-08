package sqlitefile_test

// Helpers of the engine oracle matrix (plan 3I, Task 13): the engine's dump of a
// database as typed rows, the library's dump of a view in the same shape, a
// diff that names the first difference, and the generator that builds the
// scenario databases through the engine.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// engineRow is a row as the engine (or the library) reads it. Vals hold
// nil|int64|float64|string|[]byte, or omittedVal for a value the library flags
// as not materialized.
type engineRow struct {
	Rowid    int64
	HasRowid bool
	Vals     []any
}

// omittedVal marks a library value with Omitted set (a virtual generated column,
// a non-literal default): flagged, never a wrong value.
type omittedVal struct{}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// engineTableNames lists the engine's tables (sqlite_sequence included, virtual
// tables not), sorted, lower case.
func engineTableNames(t testing.TB, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("select name from sqlite_schema where type = 'table' and sql not like 'create virtual%' and rootpage > 0")
	if err != nil {
		t.Fatalf("engine table list: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

type xinfoCol struct {
	cid        int
	name, typ  string
	notnull    bool
	dflt       sql.NullString
	pk, hidden int
}

// engineXinfo returns pragma table_xinfo of table n.
func engineXinfo(t testing.TB, db *sql.DB, n string) []xinfoCol {
	t.Helper()
	rows, err := db.Query("pragma table_xinfo(" + quoteIdent(n) + ")")
	if err != nil {
		t.Fatalf("table_xinfo %s: %v", n, err)
	}
	defer func() { _ = rows.Close() }()
	var out []xinfoCol
	for rows.Next() {
		var c xinfoCol
		var nn int
		if err := rows.Scan(&c.cid, &c.name, &c.typ, &nn, &c.dflt, &c.pk, &c.hidden); err != nil {
			t.Fatal(err)
		}
		c.notnull = nn != 0
		out = append(out, c)
	}
	return out
}

// engineWithoutRowid reports whether table n is a WITHOUT ROWID table.
func engineWithoutRowid(t testing.TB, db *sql.DB, n string) bool {
	t.Helper()
	var wr int
	if err := db.QueryRow("select wr from pragma_table_list where name = ? and schema = 'main'", n).Scan(&wr); err != nil {
		t.Fatalf("table_list %s: %v", n, err)
	}
	return wr != 0
}

// engineDumpRows returns every table as the engine reads it: rowid tables as
// (rowid, columns) in rowid order, WITHOUT ROWID tables in key order. Virtual
// generated columns are not selected (they are not stored); the position of
// each selected column among the declared ones is returned by visibleCols.
func engineDumpRows(t testing.TB, db *sql.DB) map[string][]engineRow {
	t.Helper()
	out := map[string][]engineRow{}
	for _, n := range engineTableNames(t, db) {
		out[strings.ToLower(n)] = engineTableRows(t, db, n)
	}
	return out
}

func engineTableRows(t testing.TB, db *sql.DB, n string) []engineRow {
	t.Helper()
	var cols []string
	for _, c := range engineXinfo(t, db, n) {
		if c.hidden == 0 || c.hidden == 3 {
			cols = append(cols, quoteIdent(c.name))
		}
	}
	wr := engineWithoutRowid(t, db, n)
	sel, tail := "", ""
	if !wr {
		sel, tail = "rowid, ", " order by rowid"
	}
	if len(cols) == 0 {
		return nil
	}
	q := "select " + sel + strings.Join(cols, ", ") + " from " + quoteIdent(n) + " not indexed" + tail //nolint:gosec // every identifier is quoted by quoteIdent and comes from the test database
	r, err := db.Query(q)
	if err != nil {
		t.Fatalf("engine dump %s: %v", n, err)
	}
	defer func() { _ = r.Close() }()
	var out []engineRow
	for r.Next() {
		vals := make([]any, len(cols)+map[bool]int{true: 0, false: 1}[wr])
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			t.Fatalf("engine dump %s: %v", n, err)
		}
		row := engineRow{}
		if !wr {
			row.HasRowid = true
			row.Rowid = vals[0].(int64)
			vals = vals[1:]
		}
		for i, v := range vals {
			if s, ok := v.([]byte); ok {
				vals[i] = bytes.Clone(s)
			}
		}
		row.Vals = vals
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("engine dump %s: %v", n, err)
	}
	return out
}

// normValue turns a library value into the oracle's value space.
func normValue(v sqlitefile.Value) any {
	if v.Omitted {
		return omittedVal{}
	}
	switch v.Kind {
	case sqlitefile.KindInt:
		return v.Int
	case sqlitefile.KindFloat:
		return v.Float
	case sqlitefile.KindText:
		s, ok := v.Text()
		if !ok {
			return fmt.Sprintf("<invalid text %x>", v.Bytes)
		}
		return s
	case sqlitefile.KindBlob:
		return bytes.Clone(v.Bytes)
	}
	return nil
}

// liveDumpRows reads every table of the view through Table.Rows and Resolve, in
// the shape of engineDumpRows (virtual generated columns dropped).
func liveDumpRows(t testing.TB, v *sqlitefile.View) map[string][]engineRow {
	t.Helper()
	ctx := context.Background()
	sch, err := v.Schema(ctx)
	if err != nil {
		t.Fatalf("live schema: %v", err)
	}
	out := map[string][]engineRow{}
	for _, o := range sch.Objects {
		if o.Type != "table" || o.Virtual || o.RootPage == 0 {
			continue
		}
		tb, err := v.Table(ctx, o.Name)
		if err != nil {
			t.Fatalf("live table %s: %v", o.Name, err)
		}
		def := tb.Def()
		var rows []engineRow
		err = tb.Rows(ctx, func(r sqlitefile.Row) bool {
			vals := tb.Resolve(r)
			er := engineRow{Rowid: r.Rowid, HasRowid: r.HasRowid}
			for i, val := range vals {
				if i < len(def.Columns) && def.Columns[i].RecordIndex < 0 {
					continue // virtual generated: not stored
				}
				er.Vals = append(er.Vals, normValue(val))
			}
			rows = append(rows, er)
			return true
		})
		if err != nil {
			t.Fatalf("live rows %s: %v", o.Name, err)
		}
		out[strings.ToLower(o.Name)] = rows
	}
	return out
}

func sameValue(a, b any) bool {
	if _, ok := a.(omittedVal); ok {
		return true // flagged by the library: never compared, never a wrong value
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return false
}

func showValue(v any) string {
	switch x := v.(type) {
	case []byte:
		if len(x) > 16 {
			return fmt.Sprintf("blob(%d)%x...", len(x), x[:16])
		}
		return fmt.Sprintf("blob%x", x)
	case string:
		if len(x) > 40 {
			return fmt.Sprintf("text(%d)%q...", len(x), x[:40])
		}
		return fmt.Sprintf("%q", x)
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// diffDumps returns the first difference between got (the library) and want
// (the engine): table, rowid, column. Empty when they agree. omitted counts
// the flagged values skipped.
func diffDumps(got, want map[string][]engineRow) (diff string, omitted int) {
	var tables []string
	for n := range want {
		tables = append(tables, n)
	}
	sort.Strings(tables)
	for _, n := range tables {
		g, ok := got[n]
		if !ok {
			return fmt.Sprintf("table %s: not read by the library", n), omitted
		}
		w := want[n]
		for i := 0; i < len(w) || i < len(g); i++ {
			if i >= len(g) {
				return fmt.Sprintf("table %s: library lacks row %d (engine rowid %d)", n, i, w[i].Rowid), omitted
			}
			if i >= len(w) {
				return fmt.Sprintf("table %s: library has an extra row %d (rowid %d)", n, i, g[i].Rowid), omitted
			}
			if g[i].HasRowid != w[i].HasRowid || g[i].Rowid != w[i].Rowid {
				return fmt.Sprintf("table %s row %d: rowid %d (has %v), engine %d (has %v)", n, i, g[i].Rowid, g[i].HasRowid, w[i].Rowid, w[i].HasRowid), omitted
			}
			if len(g[i].Vals) != len(w[i].Vals) {
				return fmt.Sprintf("table %s rowid %d: %d columns, engine %d", n, w[i].Rowid, len(g[i].Vals), len(w[i].Vals)), omitted
			}
			for c := range w[i].Vals {
				if _, o := g[i].Vals[c].(omittedVal); o {
					omitted++
					continue
				}
				if !sameValue(g[i].Vals[c], w[i].Vals[c]) {
					return fmt.Sprintf("table %s rowid %d column %d: library %s, engine %s", n, w[i].Rowid, c, showValue(g[i].Vals[c]), showValue(w[i].Vals[c])), omitted
				}
			}
		}
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			return fmt.Sprintf("table %s: the library reads a table the engine does not have", n), omitted
		}
	}
	return "", omitted
}

// readBytes reads a whole file (retrying a sharing violation like copyFiles).
func readFileRetry(t testing.TB, p string) []byte {
	t.Helper()
	d := t.TempDir()
	copyFiles(t, d, p)
	b, err := os.ReadFile(filepath.Join(d, filepath.Base(p)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openLibrary opens db bytes (and optional companions) in the library.
func openLibrary(t testing.TB, db, wal, journal []byte) *sqlitefile.DB {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatalf("library open: %v", err)
	}
	if wal != nil {
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatalf("attach wal: %v", err)
		}
	}
	if journal != nil {
		if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
			t.Fatalf("attach journal: %v", err)
		}
	}
	return d
}

// engineCopyDump copies the files to a fresh directory, opens the copy in the
// engine and returns its dump. The engine may checkpoint, roll back or delete
// files: only the copy is touched.
func engineCopyDump(t testing.TB, name string, db, wal, journal []byte) (map[string][]engineRow, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, db, 0o600); err != nil {
		t.Fatal(err)
	}
	if wal != nil {
		if err := os.WriteFile(p+"-wal", wal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if journal != nil {
		if err := os.WriteFile(p+"-journal", journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := sql.Open("sqlite", p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = e.Close() }()
	e.SetMaxOpenConns(1)
	var n int
	if err := e.QueryRow("select count(*) from sqlite_schema").Scan(&n); err != nil {
		return nil, err
	}
	return engineDumpRowsSafe(t, e)
}

// engineDumpRowsSafe is engineDumpRows that reports a read error instead of
// failing the test (a damaged copy may make the engine fail half way).
func engineDumpRowsSafe(t testing.TB, e *sql.DB) (out map[string][]engineRow, err error) {
	t.Helper()
	ft := &failCatcher{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(engineFailure); !ok {
					panic(r)
				}
				err = fmt.Errorf("%s", ft.msg)
			}
		}()
		out = engineDumpRows(ft, e)
	}()
	return out, err
}

type engineFailure struct{}

// failCatcher turns the Fatalf of the dump helpers into a panic the caller
// recovers, so a damaged database the engine cannot read is an outcome.
type failCatcher struct {
	testing.TB
	msg string
}

func (f *failCatcher) Fatalf(format string, a ...any) {
	f.msg = fmt.Sprintf(format, a...)
	panic(engineFailure{})
}

func (f *failCatcher) Fatal(a ...any) {
	f.msg = fmt.Sprint(a...)
	panic(engineFailure{})
}
