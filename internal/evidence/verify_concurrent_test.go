package evidence_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// verifyMode verifies c in the serial order (concurrent false) or with the concurrent phases and returns the report,
// with the audit counter (one entry per verify) taken out, and its JSON.
func verifyMode(t *testing.T, c *evidence.Case, concurrent bool) (evidence.VerifyReport, []byte) {
	t.Helper()
	c.SetVerifyConcurrency(concurrent)
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	rep.AuditEntries = 0
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return rep, b
}

// requireSameConcurrentReport verifies c serially, then concurrently twice, and requires the same report every time.
func requireSameConcurrentReport(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	serial, want := verifyMode(t, c, false)
	for range 2 {
		got, b := verifyMode(t, c, true)
		if string(b) != string(want) || !reflect.DeepEqual(got, serial) {
			t.Fatalf("the concurrent phases changed the report:\nserial     %s\nconcurrent %s", want, b)
		}
	}
	return serial
}

// TestVerifyConcurrentPhasesEqualSerial (5D-15c): P10 on its own read-only connection beside the record stream and
// the index pass, and the two full-text tables decoded together, produce byte for byte the report of the serial
// order: the P10 problems first, then the rest as before.
func TestVerifyConcurrentPhasesEqualSerial(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		c, _ := parallelCase(t)
		rep := requireSameConcurrentReport(t, c)
		if !rep.OK() || rep.RecordsChecked != 1400 || rep.FTSDocsChecked == 0 {
			t.Fatalf("the clean case must verify clean: %+v", rep)
		}
	})
	// damage that every concurrent part sees: P10 (an index that disagrees with its table), the record stream (a
	// record altered, deleted, repointed), and the two full-text tables (a record hidden from each, text invented)
	t.Run("damaged", func(t *testing.T) {
		c, ids := parallelCase(t)
		recordstest.SetRecordSummary(t, c.Dir, 77, "altered in place")
		recordstest.DeleteRecord(t, c.Dir, 900)
		recordstest.RepointRecord(t, c.Dir, 1100, ids[1])
		recordstest.HideFromIndex(t, c.Dir, "records_fts", 17)
		recordstest.HideFromIndex(t, c.Dir, "records_fts_sub", 1150)
		recordstest.InventIndexText(t, c.Dir, "records_fts_sub", 5, "zzzz", "yyyy")
		recordstest.InventIndexText(t, c.Dir, "records_fts", 1380, "qqqq", "pppp")
		recordstest.CorruptDocsize(t, c.Dir, "records_fts", 300)
		recordstest.DesyncIndex(t, c.Dir, "records_ts", `CREATE INDEX records_ts ON records (id)`)
		rep := requireSameConcurrentReport(t, c)
		requireProblem(t, rep, "integrity_check failed")
		requireProblem(t, rep, "digest mismatch")
		requireProblem(t, rep, "records_fts:")
		requireProblem(t, rep, "records_fts_sub:")
		// the order of the serial run: the first problems are P10's
		first := -1
		for i, p := range rep.Problems {
			if strings.HasPrefix(p, "integrity_check failed") {
				first = i
				break
			}
		}
		if first < 0 || slices.IndexFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "digest mismatch") }) < first {
			t.Fatalf("the integrity_check problems must come before the record problems: %q", rep.Problems)
		}
	})
}

// cancelAt returns a context that a phase hook ends at the end of the named phase.
func cancelAt(c *evidence.Case, phase string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	c.SetPhaseHook(func(name string, _ time.Duration) {
		if name == phase {
			cancel()
		}
	})
	return ctx, func() { cancel(); c.SetPhaseHook(nil) }
}

// TestVerifyConcurrentCancelKeepsFindings (D111 to D120, D162 with concurrent phases): a cancel ends every goroutine
// and the report is never a bare cancel when any phase had found something: the finding of P10, of the record
// stream or of one full-text table is kept, and the run is a cancelled one (never a pass).
func TestVerifyConcurrentCancelKeepsFindings(t *testing.T) {
	check := func(t *testing.T, c *evidence.Case, phase string, want ...string) {
		t.Helper()
		ctx, done := cancelAt(c, phase)
		defer done()
		before := runtime.NumGoroutine()
		rep, err := c.VerifyWith(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.HasSuffix(p, "verification cancelled") }) {
			t.Fatalf("a cancel at the end of %s is not reported: %q", phase, rep.Problems)
		}
		if rep.CancelledOnly() {
			t.Fatalf("a finding was masked by the cancel at the end of %s: %q", phase, rep.Problems)
		}
		for _, w := range want {
			requireProblem(t, rep, w)
		}
		for _, p := range rep.Problems {
			if strings.Contains(p, "context canceled") || strings.Contains(p, "unreadable") {
				t.Errorf("cancel noise in the report: %q", p)
			}
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !os.IsNotExist(err) {
			t.Errorf("the scratch area is still there: %v", err)
		}
		waitGoroutines(t, before)
	}
	t.Run("p10", func(t *testing.T) {
		c, _ := parallelCase(t)
		recordstest.DesyncIndex(t, c.Dir, "records_ts", `CREATE INDEX records_ts ON records (id)`)
		check(t, c, "records.integrity_check", "integrity_check failed")
	})
	t.Run("stream", func(t *testing.T) {
		c, _ := parallelCase(t)
		recordstest.SetRecordSummary(t, c.Dir, 77, "altered in place")
		check(t, c, "records.lifecycle", "digest mismatch")
	})
	t.Run("tables", func(t *testing.T) {
		c, _ := parallelCase(t)
		recordstest.HideFromIndex(t, c.Dir, "records_fts", 17)
		recordstest.HideFromIndex(t, c.Dir, "records_fts_sub", 1150)
		check(t, c, "fts.index/records_fts/decode", "records_fts: 1 batches whose index postings differ from their records were not localized", "segment 3 holds a delete marker for record 17")
	})
}

// TestVerifyIntegrityCheckConnectionIsReadOnly (5D-15c): the second connection never writes. Every connection of
// the pool has query_only on (SQLite refuses a write), artifacts.db is byte for byte as it was after the verify,
// and the pool is closed when verify returns, on a clean run and on a cancelled one. (It is not opened mode=ro:
// integrity_check on such a connection skips the CHECK constraints, see verify_readers.go; the CHECK finding of
// TestVerifyIntegrityCheckFlagged pins that.)
func TestVerifyIntegrityCheckConnectionIsReadOnly(t *testing.T) {
	hashDB := func(c *evidence.Case) [32]byte {
		b, err := os.ReadFile(filepath.Join(c.Dir, "artifacts.db"))
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	run := func(t *testing.T, cancelAtEnd string) {
		c, _ := parallelCase(t)
		var opened *sql.DB
		var writeErrs []error
		c.SetVerifyReadersHook(func(db *sql.DB) {
			opened = db
			// three connections pinned at once, each probed on its own goroutine
			var wg sync.WaitGroup
			var mu sync.Mutex
			var conns []*sql.Conn
			for range 3 {
				conn, err := db.Conn(context.Background())
				if err != nil {
					t.Errorf("pinning a connection: %v", err)
					return
				}
				conns = append(conns, conn)
			}
			for _, conn := range conns {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var errs []error
					for _, stmt := range []string{
						`CREATE TABLE zz_probe (x)`,
						`INSERT INTO parsers (name, version, hash) VALUES ('zz', '1', NULL)`,
						`UPDATE records SET summary = 'zz'`,
						`DELETE FROM records`,
					} {
						_, err := conn.ExecContext(context.Background(), stmt)
						errs = append(errs, err)
					}
					var queryOnly int
					if err := conn.QueryRowContext(context.Background(), `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
						t.Errorf("a connection of the pool has query_only = %d (%v), want 1", queryOnly, err)
					}
					mu.Lock()
					writeErrs = append(writeErrs, errs...)
					mu.Unlock()
				}()
			}
			wg.Wait()
			for _, conn := range conns {
				_ = conn.Close()
			}
		})
		ctx := context.Background()
		if cancelAtEnd != "" {
			var done func()
			ctx, done = cancelAt(c, cancelAtEnd)
			defer done()
		}
		// the verify itself appends to the audit log only; artifacts.db must be as it was
		before := hashDB(c)
		filesBefore := listDir(t, c.Dir)
		rep, err := c.VerifyWith(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cancelAtEnd == "" && !rep.OK() {
			t.Fatalf("the clean case must verify clean: %q", rep.Problems)
		}
		if opened == nil {
			t.Fatal("verify did not open a read-only connection: P10 does not run beside the stream")
		}
		if len(writeErrs) != 12 {
			t.Fatalf("%d write probes ran, want 12 (4 on each of 3 connections)", len(writeErrs))
		}
		for i, err := range writeErrs {
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") && !strings.Contains(strings.ToLower(err.Error()), "read-only") && !strings.Contains(strings.ToLower(err.Error()), "read only") {
				t.Errorf("write probe %d: err = %v, want a read-only refusal", i, err)
			}
		}
		filesAfter := listDir(t, c.Dir)
		for _, f := range append(slices.Clone(filesBefore), filesAfter...) {
			if strings.HasSuffix(f, "-wal") || strings.HasSuffix(f, "-shm") || strings.HasSuffix(f, "-journal") {
				t.Errorf("a SQLite side file %q exists around the verify", f)
			}
		}
		if got := hashDB(c); got != before {
			t.Error("artifacts.db changed during verify")
		}
		if err := opened.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
			t.Errorf("the read-only pool is still open after verify returned: ping err = %v", err)
		}
	}
	t.Run("clean", func(t *testing.T) { run(t, "") })
	t.Run("cancelled", func(t *testing.T) { run(t, "records.stream") })
	t.Run("serial opens none", func(t *testing.T) {
		c, _ := parallelCase(t)
		c.SetVerifyConcurrency(false)
		c.SetVerifyReadersHook(func(*sql.DB) { t.Error("the serial order opened a second connection") })
		if rep, err := c.Verify(); err != nil || !rep.OK() {
			t.Fatalf("serial verify: %v %q", err, rep.Problems)
		}
	})
}

// waitGoroutines requires that the goroutines started by a verify are gone: the count returns to at most before
// (it may take a moment for the last ones to be scheduled out).
func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	var now int
	for range 200 {
		if now = runtime.NumGoroutine(); now <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d goroutines before the verify, %d two seconds after: one outlived it", before, now)
}

// TestVerifyPhaseTimesSumToWallTime (D183, 5D-15c): the phase times of a concurrent verify overlap, so a sum counts
// only top-level wall time: the top-level phases outside the overlap span plus the span itself, which cannot exceed
// the time the verify took, and no phase inside the span lasts longer than the span.
func TestVerifyPhaseTimesSumToWallTime(t *testing.T) {
	c := ftsCase(t, 300)
	var mu sync.Mutex
	phases := map[string]time.Duration{}
	spans := 0
	c.SetPhaseHook(func(name string, d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		phases[name] += d
		if name == evidence.VerifyOverlapPhase {
			spans++
		}
	})
	start := time.Now()
	if _, err := c.Verify(); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	if spans != 1 {
		t.Fatalf("the overlap span was reported %d times, want once", spans)
	}
	var sum time.Duration
	for name, d := range phases {
		if strings.Contains(name, "/") || evidence.VerifyPhaseOverlapped(name) {
			if evidence.VerifyPhaseOverlapped(name) && d > phases[evidence.VerifyOverlapPhase] {
				t.Errorf("%s took %s, longer than the span that contains it (%s)", name, d, phases[evidence.VerifyOverlapPhase])
			}
			continue
		}
		sum += d
	}
	if sum > wall {
		t.Fatalf("the top-level phases (with the span) sum to %s, more than the %s the verify took: %v", sum, wall, phases)
	}
}

// D184 and D185: when the pool cannot be opened (here: query_only cannot be set) verify runs the serial order on the
// case connection, reports the same findings byte for byte, says so to the phase hook only, and opens no pool.
func TestVerifyFallsBackToTheSerialOrderWhenThePoolCannotBeOpened(t *testing.T) {
	c := ftsCase(t, 300)
	recordstest.SetRecordSummary(t, c.Dir, 7, "altered in place")
	_, want := verifyMode(t, c, false)
	defer evidence.SetReaderPragmas([]string{`PRAGMA temp_store = MEMORY`})()
	var events []string
	c.SetPhaseHook(func(name string, _ time.Duration) {
		if name == evidence.VerifyReadersUnavailable {
			events = append(events, name)
		}
	})
	defer c.SetPhaseHook(nil)
	c.SetVerifyReadersHook(func(*sql.DB) { t.Error("a pool was opened") })
	rep, got := verifyMode(t, c, true)
	if string(got) != string(want) {
		t.Fatalf("the fallback changed the report:\nserial   %s\nfallback %s", want, got)
	}
	if len(events) != 1 {
		t.Fatalf("the hook was told %d times that the pool is unavailable, want once", len(events))
	}
	if len(rep.Notices) != 0 && !slices.Equal(rep.Notices, []string{}) {
		for _, n := range rep.Notices {
			if strings.Contains(n, "pool") || strings.Contains(n, "read-only") {
				t.Errorf("the fallback reached the report: %q", n)
			}
		}
	}
}

// listDir returns the names of the entries of dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
