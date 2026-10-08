package sqlitefile_test

// Engine helpers. The modernc driver is used ONLY here, in test code, as an
// independent oracle: the library under test links no engine.

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the oracle engine (tests only)
)

// openEngine opens the database file at path with one connection (so pragmas
// such as cache_size stick) and closes it when the test ends. Callers pass a
// COPY in t.TempDir(): the engine writes -shm, recovers and rolls back.
func openEngine(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("engine open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// copy modes reported by copyFiles.
const (
	copyDirect = "direct" // every file copied at the first attempt
	copyRetry  = "retry"  // a sharing violation was met and a retry succeeded
)

// copyFiles copies each src file into the directory dst (same base name) and
// returns the mode it needed. A Windows sharing violation (a file held open
// by the engine) is retried with a short back-off; a copy that never
// succeeds fails the test.
func copyFiles(t testing.TB, dst string, src ...string) string {
	t.Helper()
	mode := copyDirect
	for _, s := range src {
		var data []byte
		var err error
		for attempt := range 50 {
			data, err = os.ReadFile(s)
			if err == nil || errors.Is(err, fs.ErrNotExist) { // only a sharing violation is worth another try
				break
			}
			mode = copyRetry
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("copy %s: still refused after retries: %v", s, err)
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(s)), data, 0o600); err != nil { //nolint:gosec // copies into the test's own temp dir
			t.Fatal(err)
		}
	}
	if mode != copyDirect {
		t.Logf("copyFiles: needed mode %q", mode)
	}
	return mode
}

// engineDump returns a canonical text of every table of the database the
// engine sees: object names sorted, every row in rowid order, values typed.
func engineDump(t testing.TB, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query("select name from sqlite_schema where type = 'table' and name not like 'sqlite_%' order by name")
	if err != nil {
		t.Fatalf("engine dump: list tables: %v", err)
	}
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
	_ = rows.Close()
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "table %q\n", n)
		q := "select * from \"" + strings.ReplaceAll(n, `"`, `""`) + "\" order by rowid" //nolint:gosec // the name comes from the test's own database and is quoted
		r, err := db.Query(q)
		if err != nil {
			t.Fatalf("engine dump %q: %v", n, err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				fmt.Fprintf(&b, "  %s=%T:%v", cols[i], v, v)
			}
			b.WriteByte('\n')
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
	}
	return b.String()
}

// pragmaString runs a single-value pragma and returns its text.
func pragmaString(t testing.TB, db *sql.DB, pragma string) string {
	t.Helper()
	var v any
	if err := db.QueryRow("pragma " + pragma).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", pragma, err)
	}
	return fmt.Sprint(v)
}
