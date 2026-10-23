package evidence

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The pool opens the file at the path it is given, whatever characters the path holds (a space, '#', and '%41',
// which a careless URI would read as an 'A'), reads it, and refuses a write.
func TestOpenVerifyReadersReachesTheSameFile(t *testing.T) {
	for _, name := range []string{"plain.db", "a b#c%41.db"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (41)`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := openVerifyReaders(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer rd.close()
			var x int
			if err := rd.readTx(t.Context(), func(h ReadHandle) error {
				return h.QueryRowContext(t.Context(), `SELECT x FROM t`).Scan(&x)
			}); err != nil || x != 41 {
				t.Fatalf("read %d, %v", x, err)
			}
			if _, err := rd.db.Exec(`INSERT INTO t VALUES (1)`); err == nil || !strings.Contains(err.Error(), "readonly") {
				t.Fatalf("a write through the pool: %v, want a read-only refusal", err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatal("the file changed")
			}
		})
	}
	// a file that does not exist is never created
	missing := filepath.Join(t.TempDir(), "missing.db")
	if rd, err := openVerifyReaders(t.Context(), missing); err == nil {
		rd.close()
		t.Fatal("opened a database that does not exist")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the pool created the missing database")
	}
}

// D184: a connection that does not report query_only = 1 is refused, whatever the reason it is not.
func TestOpenVerifyReadersRefusesAConnectionWithoutQueryOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	old := readerPragmas
	defer func() { readerPragmas = old }()
	readerPragmas = []string{`PRAGMA temp_store = MEMORY`}
	if rd, err := openVerifyReaders(t.Context(), path); err == nil {
		rd.close()
		t.Fatal("a pool without query_only was opened")
	} else if !strings.Contains(err.Error(), "query_only") {
		t.Fatalf("err = %v", err)
	}
	readerPragmas = []string{`PRAGMA no_such_pragma_statement(`} // a statement that fails is refused too
	if rd, err := openVerifyReaders(t.Context(), path); err == nil {
		rd.close()
		t.Fatal("a pool whose pragma failed was opened")
	}
}
