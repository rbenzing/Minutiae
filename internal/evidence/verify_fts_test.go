package evidence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// P11 tests that need no writer: the records are inserted straight into the tables (their audit
// batches do not exist, so verify also reports the record problems, which these tests ignore) and
// the index is built with ReindexText. Tamper tests with a real corpus are in internal/records.

// insertTextRecords inserts n unaudited records whose text is text(i) (summary, body), ids from
// firstID, and returns nothing: the index is built by the caller.
func insertTextRecords(t *testing.T, c *Case, artifactID string, n int, text func(i int) (summary, body string)) {
	t.Helper()
	db := c.store.db
	if _, err := db.Exec(`INSERT OR IGNORE INTO parsers (name, version, hash) VALUES ('raw', '1', NULL)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES ('ing-raw', 1, 1, ?, ?, 'x')`, n, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		s, b := text(i)
		var body any
		if b != "" {
			body = b
		}
		if _, err := tx.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, summary, body, payload)
			VALUES (?, 1, 'event', 1, ?, (SELECT id FROM parsers WHERE name = 'raw'), ?, ?, '{}')`, i, artifactID, s, body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, n+1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// ftsText is a deterministic text with unique tokens per record.
func ftsText(i int) (string, string) {
	return fmt.Sprintf("alpha%d beta%d gamma%d", i, i*7, i*13), fmt.Sprintf("body of record %d with delta%d and epsilon%d lorem ipsum dolor sit amet", i, i*3, i*11)
}

// indexedTextCase is a case holding n text records with a current index.
func indexedTextCase(t *testing.T, n int) *Case {
	t.Helper()
	c, rec := caseWithArtifact(t)
	insertTextRecords(t, c, rec.ID, n, ftsText)
	if _, err := c.ReindexText(context.Background(), ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	return c
}

// ftsProblems returns the problems and notices that concern the full-text index.
func ftsProblems(r VerifyReport) []string {
	var out []string
	for _, p := range r.Problems {
		if strings.Contains(p, "records_fts") || strings.Contains(p, "temporary") {
			out = append(out, p)
		}
	}
	return out
}

func TestVerifyFTSCleanIndexHasNoFTSProblems(t *testing.T) {
	c := indexedTextCase(t, 50)
	r := mustVerify(t, c)
	if p := ftsProblems(r); len(p) != 0 {
		t.Fatalf("a freshly built index has full-text problems: %q", p)
	}
	if r.FTSDocsChecked != 50 {
		t.Fatalf("FTSDocsChecked = %d, want 50", r.FTSDocsChecked)
	}
	for _, n := range r.Notices {
		if strings.Contains(n, "records_fts") {
			t.Errorf("a clean index raised a notice: %q", n)
		}
	}
	// the count is audited
	es, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	last := es[len(es)-1]
	if last.Action != "verify.run" || fmt.Sprint(last.Details["fts_docs_checked"]) != "50" {
		t.Fatalf("verify.run details = %v, want fts_docs_checked 50", last.Details)
	}
	// nothing is left behind: the case has no tmp directory
	if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp after verify: %v, want it removed", err)
	}
}

// TestVerifyFTSStreamsInChunks: the rebuild reads the record rows in keyset chunks of at most 5000
// rows, never by one query: ceil(12000/5000) chunk reads, reported by table "fts".
func TestVerifyFTSStreamsInChunks(t *testing.T) {
	c := indexedTextCase(t, 12000)
	var fts, recs []int
	start := time.Now()
	c.verifyFTSHook = func(point string, _ *sql.DB) {
		t.Logf("P11 %-22s at +%v", point, time.Since(start).Round(time.Millisecond))
	}
	r, err := c.verify(func(table string, rows int) {
		switch table {
		case "fts":
			fts = append(fts, rows)
		case "records":
			recs = append(recs, rows)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("P11 + verify of 12000 records (two indexes): %v", time.Since(start))
	want := []int{5000, 5000, 2000}
	if !slices.Equal(fts, want) {
		t.Fatalf("full-text rebuild chunk reads = %v, want %v", fts, want)
	}
	if !slices.Equal(recs, want) {
		t.Fatalf("record scan chunk reads = %v, want %v", recs, want)
	}
	if p := ftsProblems(r); len(p) != 0 {
		t.Fatalf("full-text problems: %q", p)
	}
	if r.FTSDocsChecked != 12000 {
		t.Fatalf("FTSDocsChecked = %d, want 12000", r.FTSDocsChecked)
	}
}

// onlyRunDir returns the one verify-* directory under <case>/tmp while a verification runs.
func onlyRunDir(t *testing.T, c *Case) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(c.Dir, "tmp"))
	if err != nil {
		t.Fatalf("tmp does not exist during the run: %v", err)
	}
	if len(ents) != 1 || !ents[0].IsDir() || !strings.HasPrefix(ents[0].Name(), "verify-") {
		t.Fatalf("tmp holds %v, want exactly one verify-<runid> directory", ents)
	}
	return filepath.Join(c.Dir, "tmp", ents[0].Name())
}

// redirectOSTemp points every environment variable that can name a temporary directory at fresh
// directories and returns them: nothing of the evidence may appear there.
func redirectOSTemp(t *testing.T) []string {
	t.Helper()
	var dirs []string
	for _, k := range []string{"TMP", "TEMP", "TMPDIR", "SQLITE_TMPDIR"} {
		d := t.TempDir()
		t.Setenv(k, d)
		dirs = append(dirs, d)
	}
	return dirs
}

func assertNoSpillIn(t *testing.T, dirs []string) {
	t.Helper()
	for _, d := range dirs {
		var left []string
		for _, f := range spillFiles(t, d) {
			if !strings.HasPrefix(f, "Test") { // the test's own temporary directories live here too (TMP is redirected)
				left = append(left, f)
			}
		}
		if len(left) != 0 {
			t.Errorf("temporary files reached %s: %v", d, left)
		}
	}
}

// spillWorks proves the process-wide SQLite temp directory is back to its default: a private
// database that spills must not fail ("unable to open database file" when it still pointed at a
// removed directory).
func spillWorks(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	probeForceSpill(t, db)
	probeExec(t, db, `ROLLBACK`)
}

// TestVerifyFTSLeavesNoTemporaryFiles: the expected index and every SQLite spill file live under
// <case>/tmp/verify-<runid>/ (asserted positively: a spill file existed there during the run),
// never in the operating system's temporary directories, and nothing is left afterwards: not after a
// clean verify, not after a failing one, not after a panic, and SQLite's process-wide temp
// directory is back to its default.
func TestVerifyFTSLeavesNoTemporaryFiles(t *testing.T) {
	osTemp := redirectOSTemp(t)
	run := func(t *testing.T, c *Case, mustSpill bool) VerifyReport {
		t.Helper()
		c.verifyFTSCacheKiB = 256
		spilled := false
		c.verifyFTSHook = func(point string, _ *sql.DB) {
			if point != "after-rebuild" {
				return
			}
			dir := onlyRunDir(t, c)
			files := spillFiles(t, dir)
			t.Logf("spill files in %s while verifying: %v", dir, files)
			spilled = len(files) > 0
			if mi, err := os.Stat(dir); err != nil {
				t.Error(err)
			} else if runtime.GOOS != "windows" && mi.Mode().Perm() != 0o700 {
				t.Errorf("run directory mode %v, want 0700", mi.Mode().Perm())
			}
		}
		r := mustVerify(t, c)
		if mustSpill && !spilled {
			t.Fatal("no spill file existed in the run directory during the run: the test does not show where spill files go")
		}
		return r
	}
	t.Run("clean", func(t *testing.T) {
		c := indexedTextCase(t, 6000)
		r := run(t, c, true)
		if p := ftsProblems(r); len(p) != 0 {
			t.Fatalf("full-text problems: %q", p)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tmp after a clean verify: %v", err)
		}
		assertNoSpillIn(t, osTemp)
		spillWorks(t)
	})
	t.Run("failing", func(t *testing.T) {
		c := indexedTextCase(t, 6000)
		if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 5, ?, ?)`, normalizeFor(5)...); err != nil {
			t.Fatal(err)
		}
		r := run(t, c, true)
		if !containsSubstr(r.Problems, "a search hit is hidden") {
			t.Fatalf("the tampered index was not reported: %q", ftsProblems(r))
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tmp after a failing verify: %v", err)
		}
		assertNoSpillIn(t, osTemp)
		spillWorks(t)
	})
	t.Run("panic", func(t *testing.T) {
		c := indexedTextCase(t, 6000)
		c.verifyFTSCacheKiB = 256
		c.verifyFTSHook = func(point string, _ *sql.DB) {
			if point == "after-rebuild" {
				panic("simulated crash inside P11")
			}
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the simulated panic did not propagate")
				}
			}()
			_, _ = c.Verify()
		}()
		// the deferred cleanup ran: the run directory is gone and the temp directory is reset
		if ents, err := os.ReadDir(filepath.Join(c.Dir, "tmp")); err == nil && len(ents) != 0 {
			t.Fatalf("tmp holds %v after a panic", ents)
		}
		assertNoSpillIn(t, osTemp)
		spillWorks(t)
	})
}

func normalizeFor(i int) []any {
	s, b := ftsText(i)
	return []any{NormalizeText(s), NormalizeText(b)}
}

// TestVerifyFTSExpectedIndexLostIsAProblem (R4): the rebuilt index lives in ONE pinned connection of
// a private database. If that connection is lost, a reconnect would hand out a new EMPTY database;
// that must be a hard problem ("expected index lost"), never an empty expected set that makes
// every hit look invented, never a clean result.
func TestVerifyFTSExpectedIndexLostIsAProblem(t *testing.T) {
	c := indexedTextCase(t, 40)
	c.verifyFTSHook = func(point string, expected *sql.DB) {
		if point != "after-rebuild" {
			return
		}
		conn, err := expected.Conn(context.Background())
		if err != nil {
			t.Errorf("pinned connection: %v", err)
			return
		}
		// the driver reports the connection as bad: database/sql discards it and would dial again
		if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
			t.Errorf("Raw = %v", err)
		}
		_ = conn.Close()
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "expected index lost") {
		t.Fatalf("verify did not report the lost expected index: problems %q", r.Problems)
	}
	// no comparison ran against an empty rebuild
	if containsSubstr(r.Problems, "a search hit is invented") || containsSubstr(r.Problems, "a search hit is hidden") {
		t.Fatalf("hits were reported against a lost rebuild: %q", ftsProblems(r))
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp after the failure: %v", err)
	}
}

// TestExpectedConnectorDialsOnce: the connector of the expected index refuses a second connection.
func TestExpectedConnectorDialsOnce(t *testing.T) {
	cn := &expectedConnector{}
	conn, err := cn.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := cn.Connect(context.Background()); !errors.Is(err, errExpectedIndexLost) {
		t.Fatalf("second Connect = %v, want errExpectedIndexLost", err)
	}
}

// TestVerifyFTSFlagsLeftoverTmp: a leftover <case>/tmp is a problem on the next run, like leftover
// staging; verify does not delete it (an examiner decides) and leaves its own run directory clean.
func TestVerifyFTSFlagsLeftoverTmp(t *testing.T) {
	c := indexedTextCase(t, 20)
	left := filepath.Join(c.Dir, "tmp", "verify-deadbeef")
	if err := os.MkdirAll(left, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "expected.db"), []byte("text of a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	const want = "leftover temporary directory tmp/verify-deadbeef"
	if r.OK() || !containsSubstr(r.Problems, want) {
		t.Fatalf("problems %q, want one containing %q", r.Problems, want)
	}
	if p := ftsProblems(r); len(p) != 1 {
		t.Errorf("the leftover is the only full-text/temporary problem, got %q", p)
	}
	ents, err := os.ReadDir(filepath.Join(c.Dir, "tmp"))
	if err != nil || len(ents) != 1 || ents[0].Name() != "verify-deadbeef" {
		t.Fatalf("tmp = %v, %v; want only the leftover (verify must remove just its own directory)", ents, err)
	}
	if r.FTSDocsChecked != 20 {
		t.Errorf("P11 did not run beside a leftover: FTSDocsChecked = %d", r.FTSDocsChecked)
	}
	// once the examiner removes it, the next run is clean
	if err := os.RemoveAll(filepath.Join(c.Dir, "tmp")); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); containsSubstr(r.Problems, "leftover temporary") {
		t.Fatalf("still flagged: %q", r.Problems)
	}
}
