package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// genuineFTS builds both FTS tables in a scratch file database with the real DDL and returns it.
func genuineFTS(t testing.TB) *sql.DB {
	t.Helper()
	return newGenuineFTS(t, filepath.Join(t.TempDir(), "g.db"))
}

func newGenuineFTS(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range FTSTables() {
		ddl, ok := FTSTableDDL(table)
		if !ok {
			t.Fatalf("no DDL for %s", table)
		}
		probeExec(t, db, ddl)
	}
	return db
}

// fillText is the text of document i: a summary and a body of words tokens.
func fillText(i, words int) (summary, body string) {
	var b strings.Builder
	for w := 0; w < words; w++ {
		fmt.Fprintf(&b, "w%d_%d ", (i*7+w)%997, i%13)
	}
	return fmt.Sprintf("s%d", i), b.String()
}

// genuineImage caches the file of a workload's database for the whole test binary.
type genuineImage struct {
	once sync.Once
	data []byte
}

var genuineBuilt sync.Map // workload name -> *genuineImage

// genuineFTSWorkload returns a private copy of the database that the named workload builds. The workload
// runs once per test binary, because the merge workloads take many seconds and several tests read them
// (they never write to the copy they get, and the copy is deleted with the test).
func genuineFTSWorkload(t testing.TB, name string, load func(testing.TB, *sql.DB)) *sql.DB {
	t.Helper()
	v, _ := genuineBuilt.LoadOrStore(name, &genuineImage{})
	img := v.(*genuineImage)
	img.once.Do(func() {
		path := filepath.Join(t.TempDir(), "w.db")
		db := newGenuineFTS(t, path)
		load(t, db)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		img.data = data
	})
	if img.data == nil {
		t.Fatalf("workload %q did not build", name)
	}
	path := filepath.Join(t.TempDir(), "g.db")
	if err := os.WriteFile(path, img.data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fill(t testing.TB, db *sql.DB, from, n, words int) {
	t.Helper()
	for _, table := range FTSTables() {
		for i := from; i < from+n; i++ {
			summary, body := fillText(i, words)
			probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (?, ?, ?)", i, summary, body)
		}
	}
}

// accounted requires the decoded segment set to equal the segment ids of the data rows exactly, and
// the idx rows to name only decoded segments.
func accounted(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var structure []byte
	if err := db.QueryRow("SELECT block FROM "+table+"_data WHERE id = ?", ftsStructureRowID).Scan(&structure); err != nil {
		t.Fatal(err)
	}
	segs, err := decodeFTSStructure(structure)
	if err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	rows, err := db.Query("SELECT id FROM "+table+"_data WHERE id NOT IN (?, ?)", ftsAveragesRowID, ftsStructureRowID) //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id < 1<<ftsSegidShift {
			t.Fatalf("%s: genuine data row %d is below the segment range", table, id)
		}
		seen[id>>ftsSegidShift] = struct{}{}
	}
	_ = rows.Close()
	if len(seen) != len(segs) {
		t.Fatalf("%s: data rows hold %d segments, the structure names %d", table, len(seen), len(segs))
	}
	for s := range seen {
		if _, ok := segs[s]; !ok {
			t.Fatalf("%s: data row segment %d is not in the structure", table, s)
		}
	}
	ir, err := db.Query("SELECT DISTINCT segid FROM " + table + "_idx") //nolint:gosec // a constant table name in a test
	if err != nil {
		t.Fatal(err)
	}
	for ir.Next() {
		var s int64
		if err := ir.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if _, ok := segs[s]; !ok {
			t.Fatalf("%s: idx segment %d is not in the structure", table, s)
		}
	}
	_ = ir.Close()
}

// genuineFTSWorkloads are the five shapes of genuine index the structure, leaf and index tests share: each loads
// both FTS tables of a fresh genuineFTS database. They take a testing.TB so that fuzz targets can seed from them.
func genuineFTSWorkloads() map[string]func(testing.TB, *sql.DB) {
	return map[string]func(testing.TB, *sql.DB){
		"one document": func(t testing.TB, db *sql.DB) { fill(t, db, 1, 1, 5) },
		"many small inserts, merges": func(t testing.TB, db *sql.DB) {
			for i := 0; i < 60; i++ {
				fill(t, db, 1+i*50, 50, 40)
			}
		},
		"after optimize": func(t testing.TB, db *sql.DB) {
			for i := 0; i < 20; i++ {
				fill(t, db, 1+i*50, 50, 40)
			}
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+") VALUES('optimize')")
			}
		},
		"one huge body": func(t testing.TB, db *sql.DB) {
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"(rowid, summary, body) VALUES (1, 's', ?)", strings.Repeat("alpha beta gamma delta ", 9000))
			}
		},
		"after delete-all and refill": func(t testing.TB, db *sql.DB) {
			fill(t, db, 1, 300, 20)
			for _, table := range FTSTables() {
				probeExec(t, db, "INSERT INTO "+table+"("+table+") VALUES('delete-all')")
			}
			fill(t, db, 1000, 100, 20)
		},
	}
}

func TestFTSStructureAccountsForEveryGenuineRow(t *testing.T) {
	for name, load := range genuineFTSWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			for _, table := range FTSTables() {
				accounted(t, db, table)
			}
		})
	}
}

func TestFTSStructureRejectsHostileRecords(t *testing.T) {
	db := genuineFTS(t)
	fill(t, db, 1, 200, 20)
	var good []byte
	if err := db.QueryRow("SELECT block FROM records_fts_data WHERE id = ?", ftsStructureRowID).Scan(&good); err != nil {
		t.Fatal(err)
	}
	huge := append(append([]byte{0, 0, 0, 0}, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f), 0x01, 0x00)
	for name, b := range map[string][]byte{
		"empty":                      nil,
		"three bytes":                {1, 2, 3},
		"cookie only":                {0, 0, 0, 0},
		"huge level count":           huge,
		"truncated":                  good[:len(good)-1],
		"trailing bytes":             append(append([]byte{}, good...), 0x01),
		"more segments than fit":     append([]byte{0, 0, 0, 0, 0x01, 0x7f, 0x00}, 0x01),
		"segment total differs":      append([]byte{0, 0, 0, 0, 0x01, 0x02, 0x00, 0x00, 0x01, 0x05, 0x01, 0x01}, 0),
		"last page before the first": {0, 0, 0, 0, 0x01, 0x01, 0x00, 0x00, 0x01, 0x01, 0x05, 0x02},
		"merging more than present":  {0, 0, 0, 0, 0x01, 0x01, 0x00, 0x02, 0x01, 0x01, 0x01, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			if segs, err := decodeFTSStructure(b); err == nil {
				t.Fatalf("accepted a malformed record: %v", segs)
			}
		})
	}
	if _, err := decodeFTSStructure(good); err != nil {
		t.Fatalf("the genuine record is refused: %v", err)
	}
}

// A hostile record cannot make the decoder allocate more than it has bytes: the map starts small.
func TestFTSStructureDoesNotAllocateFromADeclaredCount(t *testing.T) {
	b := append([]byte{0, 0, 0, 0}, 0x01, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x00) // 1 level, 2^35-1 segments
	allocs := testing.AllocsPerRun(20, func() { _, _ = decodeFTSStructure(b) })
	if allocs > 20 {
		t.Fatalf("%v allocations for a 7-byte hostile record", allocs)
	}
}

// checkUnreferenced itself, on a cancelled context, adds no problem of its own: the cancellation is what the
// verify reports (TestVerifyCancelledDuringP11, through VerifyWith), and a "could not be checked" line beside it
// is noise. A read error on a live context
// is still reported.
func TestFTSStructureCheckIsQuietWhenCancelled(t *testing.T) {
	c := indexedTextCase(t, 50)
	run := func(ctx context.Context) []string {
		rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
		r := &ftsVerify{c: c, ctx: ctx, ps: &problemSet{rep: rep, counts: map[string]int{}}, rep: rep, listed: map[string]map[int64]bool{}}
		r.checkUnreferenced(FTSWordTable)
		return rep.Problems
	}
	if p := run(context.Background()); len(p) != 0 {
		t.Fatalf("a live check of a clean index reports %q", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p := run(ctx); len(p) != 0 {
		t.Fatalf("a cancelled check reports %q", p)
	}
}

// A read error on a live context goes through r.fail and is reported (D112).
func TestFTSStructureReadErrorOnALiveContextIsReported(t *testing.T) {
	c := indexedTextCase(t, 50)
	// the structure record is gone: the read finds no row
	if _, err := c.store.db.Exec("DELETE FROM "+FTSWordTable+"_data WHERE id = ?", ftsStructureRowID); err != nil { //nolint:gosec // a constant table name in a test
		t.Fatal(err)
	}
	rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
	r := &ftsVerify{c: c, ctx: context.Background(), ps: &problemSet{rep: rep, counts: map[string]int{}}, rep: rep, listed: map[string]map[int64]bool{}}
	r.checkUnreferenced(FTSWordTable)
	found := false
	for _, p := range rep.Problems {
		found = found || strings.Contains(p, FTSWordTable+": the index cannot be read")
	}
	if !found {
		t.Fatalf("a failed structure read on a live context was not reported: %q", rep.Problems)
	}
}

// A verify cancelled while P11 reads the index pages (5D-14k: the index pass, there is no whole-index rebuild any
// more) stops there, is never OK and says why; a problem
// found before the cancel is still in the report (integrity is never masked by a cancel).
func TestVerifyCancelledDuringP11(t *testing.T) {
	c := indexedTextCase(t, 12000) // three record chunks of 5000, 5000, 2000; the index pages are read after them
	// an integrity problem found before P11 starts: the artifact's bytes no longer match the manifest
	recs, err := c.Manifest()
	if err != nil || len(recs) == 0 {
		t.Fatalf("manifest: %v %d", err, len(recs))
	}
	path := filepath.Join(c.Dir, filepath.FromSlash(recs[0].Path))
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil { //nolint:gosec // a test edits its own temp case
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := 0
	start := time.Now()
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "fts-data" {
			chunks++
			cancel() // the first chunk of index pages is read; the rest must not be
		}
	}, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if chunks != 1 {
		t.Fatalf("%d chunks of index pages were read after the cancel, want 1 (the one in flight)", chunks)
	}
	if time.Since(start) > 2*time.Minute {
		t.Fatalf("a cancelled verify took %v", time.Since(start))
	}
	if rep.OK() {
		t.Fatal("a cancelled verify must not be OK")
	}
	if !slices.Contains(rep.Problems, verifyCancelled) {
		t.Fatalf("no %q in %q", verifyCancelled, rep.Problems)
	}
	if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "hash mismatch") }) {
		t.Fatalf("the integrity problem found before the cancel is missing: %q", rep.Problems)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "context canceled") || strings.Contains(p, "unreadable") || strings.Contains(p, "could not") {
			t.Fatalf("cancel noise in the report: %q", p)
		}
	}
	if rep.FTSDocsChecked != 0 {
		t.Fatalf("FTSDocsChecked = %d: a cut comparison counts no document", rep.FTSDocsChecked)
	}
}

// The accounting of shadow rows is linear in the unreferenced rows, not in the table (5D-15a): a clean index
// costs a bounded number of range probes however many data rows it holds. Counted operations, not time.
func TestCheckUnreferencedIsLinear(t *testing.T) {
	ops := func(n int) (int, int) {
		c := indexedTextCase(t, n)
		rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
		r := &ftsVerify{c: c, ctx: context.Background(), ps: &problemSet{rep: rep, counts: map[string]int{}}, rep: rep, listed: map[string]map[int64]bool{}}
		r.checkUnreferenced(FTSSubTable)
		if len(rep.Problems) != 0 {
			t.Fatalf("n=%d: %q", n, rep.Problems)
		}
		var rows int
		if err := c.store.db.QueryRow("SELECT count(*) FROM " + FTSSubTable + "_data").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return r.unrefOps, rows
	}
	small, smallRows := ops(5000)
	large, largeRows := ops(20000)
	if largeRows < 3*smallRows {
		t.Fatalf("the workloads do not scale: %d and %d data rows", smallRows, largeRows)
	}
	if large > 3*small || large > largeRows/4 {
		t.Fatalf("operations grew from %d to %d for %d to %d data rows", small, large, smallRows, largeRows)
	}
}
