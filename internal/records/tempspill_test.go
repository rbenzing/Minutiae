package records_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Evidence text must never reach the OS temp directory through SQLite's own temporary files (a
// sorter, a statement journal, a temp table). Reads run with temp_store = MEMORY (ReadTx); this pins
// the statements that run outside ReadTx: the ingest batches and the run conclusion (supersede),
// the full-text reindex. See docs/invariants.md and docs/limitations.md (full-text temporary files).

// openTempFiles lists what is open in the redirected OS temp directory. A temp file SQLite opens is
// created there by name on Windows (delete on close) and unlinked at once on Linux, where an open
// descriptor is the only trace.
func openTempFiles(dir string) []string {
	var out []string
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			out = append(out, e.Name())
		}
	}
	if runtime.GOOS == "linux" {
		if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
			for _, fd := range fds {
				if l, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(l, dir) {
					out = append(out, l)
				}
			}
		}
	}
	return out
}

// watchTemp polls dir without pause until stop is called, and returns the sorted distinct names it saw.
func watchTemp(dir string) (stop func() []string) {
	seen := map[string]bool{}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			for _, n := range openTempFiles(dir) {
				seen[n] = true
			}
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	var once sync.Once
	return func() []string {
		once.Do(func() { close(done); <-finished })
		var out []string
		for n := range seen {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
}

// redirectOSTemp points every OS temp variable at a fresh directory outside any case and returns it.
func redirectOSTemp(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ostemp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"TMPDIR", "TMP", "TEMP", "SQLITE_TMPDIR"} {
		t.Setenv(v, dir)
	}
	return dir
}

// TestTempSpillWatcherSeesASpill is the control of the test below: a connection that keeps the
// default temp store and sorts more than its cache holds MUST show a file in the redirected
// directory, or the redirect or the watcher is blind on this platform and the test below proves nothing.
func TestTempSpillWatcherSeesASpill(t *testing.T) {
	db0 := filepath.Join(t.TempDir(), "control.db")
	tmp := redirectOSTemp(t)
	db, err := sql.Open("sqlite", db0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, s := range []string{
		`PRAGMA cache_size = 10`,
		`CREATE TABLE t (x TEXT)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000) INSERT INTO t SELECT hex(randomblob(64)) FROM n`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := db.Query(`SELECT x FROM t ORDER BY x`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	if got := openTempFiles(tmp); len(got) == 0 {
		t.Fatal("a sort over a 10 page cache left no file in the redirected temp directory: the control is blind")
	}
}

func bigRecords(artifactID string, n int) []records.Record {
	recs := recordstest.Records(artifactID, n, 7)
	filler := strings.Repeat("evidence text that must stay inside the case ", 50) // about 2.2 KB
	for i := range recs {
		recs[i].Body = filler + strconv.Itoa(i)
	}
	return recs
}

func TestIngestAndReindexLeaveNoSQLiteTempFileOutsideTheCase(t *testing.T) {
	c, a := setup(t) // the case exists before the redirect, so it does not live in the redirected directory
	tmp := redirectOSTemp(t)
	stop := watchTemp(tmp)
	defer stop()
	// The case setup ran a ReadTx, which leaves temp_store = MEMORY on the one pooled connection, so
	// without this reset the pragma the setup left behind would hide a missing one (ruling D53). The
	// pooled connection goes back to the default with a tiny page cache, so any sorter or temporary
	// table the ingest, the supersede or the reindex needs would overflow into a file. Limit, stated:
	// every later ReadTx sets MEMORY again and leaves it, so this test fails when ReadTx, StoreTx and
	// the connector pragmas are all gone (checked by mutation), while the write-path pragmas are pinned
	// on their own by TestStoreTxUsesMemoryForTemporaryStorage (internal/evidence).
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`PRAGMA temp_store = DEFAULT`); err != nil {
			return err
		}
		_, err := tx.Exec(`PRAGMA cache_size = 10`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2000}, a.ID)
	add(t, w, bigRecords(a.ID, 3000))
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	// a second parser over the same artifact: its conclusion runs the supersede statement
	recordstest.Ingest(t, c, records.Parser{Name: "other-parser", Version: "2"}, []string{a.ID}, bigRecords(a.ID, 500))
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if seen := stop(); len(seen) != 0 {
		t.Fatalf("SQLite temp files appeared outside the case (the evidence text can spill there): %q", seen)
	}
}

// TestUpgradeAndReindexOfAV2CaseLeaveNoSQLiteTempFileOutsideTheCase runs the migration path (case
// upgrade v2 to v3, which builds the full-text tables) and the reindex under the watcher. Limits,
// stated plainly: the watcher is a poll, so a pass is a sampling statement and not a proof; and the
// v2 fixture holds only 12 records, so the migration's index builds are far too small to overflow a
// sorter. This pins that the migration path is covered by temp_store = MEMORY (a regression that
// removed the connection pragma would show on a larger fixture, not necessarily on this one); the
// size-sensitive proof is the ingest test above.
func TestUpgradeAndReindexOfAV2CaseLeaveNoSQLiteTempFileOutsideTheCase(t *testing.T) {
	dir := recordstest.CopyV2Case(t) // the case exists before the redirect
	tmp := redirectOSTemp(t)
	stop := watchTemp(tmp)
	defer stop()
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Upgrade(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if seen := stop(); len(seen) != 0 {
		t.Fatalf("SQLite temp files appeared outside the case during upgrade and reindex: %q", seen)
	}
}
