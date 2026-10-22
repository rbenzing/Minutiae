package evidence

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// indexableFTSTables is the pair of tables as the tests loop over them.
var indexableFTSTables = [...]string{FTSWordTable, FTSSubTable}

// TestVerifyFTSBucketsEqualOnCleanCaseInternal: a clean case compares its buckets and none fails. The case
// has no audit batches, so every record falls in the "outside" blocks (2^16 ids each).
func TestVerifyFTSBucketsEqualOnCleanCaseInternal(t *testing.T) {
	c := indexedTextCase(t, 12000)
	var got []int
	c.verifyFTSInspect = func(r *ftsVerify) {
		got = []int{r.bucketsCompared, len(r.failing[0]), len(r.failing[1])}
	}
	rep := mustVerify(t, c)
	if p := ftsProblems(rep); len(p) != 0 {
		t.Fatalf("full-text problems: %q", p)
	}
	// 12000 ids all fall in block 0 of the outside range: one bucket per table
	if !slices.Equal(got, []int{2, 0, 0}) {
		t.Fatalf("compared, failing word, failing sub = %v, want [2 0 0]", got)
	}
}

// TestVerifyFTSDocsizeOutsideEveryChunkIsReported (V10): a size entry for a record outside the ids of every
// window is found, in the first window (below every record) and in the last (above every record).
func TestVerifyFTSDocsizeOutsideEveryChunkIsReported(t *testing.T) {
	c := indexedTextCase(t, 6000) // windows of 5000 indexable records: [MinInt64, 5000], [5001, MaxInt64]
	for _, id := range []int64{-5, math.MaxInt64} {
		if _, err := c.store.db.Exec(`INSERT INTO records_fts_docsize (id, sz) VALUES (?, x'0101')`, id); err != nil {
			t.Fatal(err)
		}
	}
	rep := mustVerify(t, c)
	for _, want := range []string{
		"records_fts_docsize: the index holds a size entry for record -5, which does not exist or has no indexable text",
		"records_fts_docsize: the index holds a size entry for record 9223372036854775807, which does not exist or has no indexable text",
	} {
		if !containsSubstr(rep.Problems, want) {
			t.Errorf("no problem %q in %q", want, ftsProblems(rep))
		}
	}
	// only the tables with the stray row are named; the sub index has none
	if containsSubstr(rep.Problems, "records_fts_sub_docsize: the index holds a size entry") {
		t.Errorf("the sub index has no stray size entry: %q", ftsProblems(rep))
	}
}

// TestVerifyFTSDocsizeDifferenceIsReportedInTheFirstWindow: a changed size entry and a missing one, in the first
// window (before 5000 records were seen), are found by the comparison that runs inside the stream.
func TestVerifyFTSDocsizeDifferenceIsReportedInTheFirstWindow(t *testing.T) {
	c := indexedTextCase(t, 6000)
	if _, err := c.store.db.Exec(`UPDATE records_fts_docsize SET sz = x'0102' WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.db.Exec(`DELETE FROM records_fts_docsize WHERE id = 5500`); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	for _, want := range []string{
		"records_fts_docsize: record 7: the size entry differs from a rebuild",
		"records_fts_docsize: record 5500: the size entry is missing",
	} {
		if !containsSubstr(rep.Problems, want) {
			t.Errorf("no problem %q in %q", want, ftsProblems(rep))
		}
	}
}

// TestVerifyFTSTotalsDifferenceIsReported: the totals record is recomputed from the records.
func TestVerifyFTSTotalsDifferenceIsReported(t *testing.T) {
	c := indexedTextCase(t, 100)
	if _, err := c.store.db.Exec(`UPDATE records_fts_data SET block = x'640101' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !containsSubstr(rep.Problems, "records_fts: the index totals differ from a rebuild (ranking is unreliable)") {
		t.Fatalf("totals tamper not reported: %q", rep.Problems)
	}
	if containsSubstr(rep.Problems, "records_fts_sub: the index totals differ") {
		t.Errorf("the untouched sub index reports totals: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 100 {
		t.Errorf("FTSDocsChecked = %d, want 100: the content comparison ran to its end", rep.FTSDocsChecked)
	}
}

// TestVerifyFTSConfigDifferenceIsReported: the _config rows are compared with the fresh rows of this SQLite.
func TestVerifyFTSConfigDifferenceIsReported(t *testing.T) {
	c := indexedTextCase(t, 20)
	if _, err := c.store.db.Exec(`INSERT OR REPLACE INTO records_fts_config (k, v) VALUES ('pgsz', 64)`); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !containsSubstr(rep.Problems, "records_fts_config: key \"pgsz\"") {
		t.Fatalf("config tamper not reported: %q", rep.Problems)
	}
}

// TestVerifyFTSSelfTestFailureIsAProblem (V9): a tokenizer replica that does not match this SQLite is exactly one
// PROBLEM; the postings, sizes and totals are not compared, so no line says the index lacks or invents a
// posting, and the verify is not OK.
func TestVerifyFTSSelfTestFailureIsAProblem(t *testing.T) {
	c := indexedTextCase(t, 50)
	old := ftsReplicaOverride
	t.Cleanup(func() { ftsReplicaOverride = old })
	ftsReplicaOverride = func(table string) func(string, func([]byte, int)) {
		if table != FTSWordTable {
			return nil
		}
		return func(text string, emit func([]byte, int)) {
			tokenizeUnicode61(text, func(term []byte, pos int) { emit(append([]byte("x"), term...), pos) })
		}
	}
	rep := mustVerify(t, c)
	if rep.OK() {
		t.Fatal("a verify with a failing self-test is OK")
	}
	if n := countContaining(rep.Problems, "tokenizer replica does not match this SQLite"); n != 1 {
		t.Fatalf("%d tokenizer replica lines in %q, want 1", n, rep.Problems)
	}
	for _, bad := range []string{"the index lacks", "is invented", "totals differ", "size entry"} {
		if containsSubstr(rep.Problems, bad) {
			t.Errorf("a %q line although nothing was compared: %q", bad, rep.Problems)
		}
	}
	if !containsSubstr(rep.Problems, "the postings, sizes and totals of the index are not verified") {
		t.Errorf("the line does not say what is not verified: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d: the content was not compared", rep.FTSDocsChecked)
	}
}

// TestVerifyFTSReplicaBlindSpotIsAProblem (V15): a replica that differs from SQLite where the self-test corpus
// does not look makes a bucket fail while the index equals SQLite's own rebuild. That is a PROBLEM that names
// the batch, never a pass and never silent.
func TestVerifyFTSReplicaBlindSpotIsAProblem(t *testing.T) {
	c := indexedTextCase(t, 50)
	old := ftsReplicaOverride
	t.Cleanup(func() { ftsReplicaOverride = old })
	ftsReplicaOverride = func(table string) func(string, func([]byte, int)) {
		if table != FTSWordTable {
			return nil
		}
		return func(text string, emit func([]byte, int)) {
			tokenizeUnicode61(text, func(term []byte, pos int) {
				if string(term) == "alpha7" { // a token no probe holds
					term = []byte("alpha7x")
				}
				emit(term, pos)
			})
		}
	}
	rep := mustVerify(t, c)
	if rep.OK() {
		t.Fatal("a replica that disagrees with SQLite passed")
	}
	if containsSubstr(rep.Problems, "tokenizer replica does not match this SQLite") {
		t.Fatalf("the self-test was meant not to see the blind spot: %q", rep.Problems)
	}
	if n := countContaining(rep.Problems, "though a search finds what a rebuild by SQLite holds"); n != 1 {
		t.Fatalf("%d bucket lines in %q, want 1", n, ftsProblems(rep))
	}
	if containsSubstr(rep.Problems, "the index lacks") || containsSubstr(rep.Problems, "is invented") {
		t.Errorf("the index equals SQLite's rebuild, yet an entry is reported: %q", ftsProblems(rep))
	}
	if rep.FTSDocsChecked != 50 {
		t.Errorf("FTSDocsChecked = %d", rep.FTSDocsChecked)
	}
}

// TestVerifyFTSDeleteMarkerIsAProblem: a 'delete' of an indexed record writes a delete marker per term (a
// genuine index of this case never holds one). Each marker is a PROBLEM, and so are the postings the markers
// hide: the vocab comparison runs for the table (it is the failing bucket's localization).
func TestVerifyFTSDeleteMarkerIsAProblem(t *testing.T) {
	c := indexedTextCase(t, 50)
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rank) VALUES ('automerge', 0)`); err != nil {
		t.Fatal(err)
	}
	var terms int
	if err := c.store.db.QueryRow(`SELECT count(DISTINCT term) FROM ` + ftsVocabName(FTSWordTable) + ` WHERE doc = 5`).Scan(&terms); err != nil || terms < 5 {
		t.Fatalf("record 5 has %d terms: %v", terms, err)
	}
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 5, ?, ?)`, normalizeFor(5)...); err != nil {
		t.Fatal(err)
	}
	var seen []string
	c.SetPhaseHook(func(name string, _ time.Duration) { seen = append(seen, name) })
	rep := mustVerify(t, c)
	if rep.OK() {
		t.Fatal("a delete marker passed")
	}
	if n := countContaining(rep.Problems, "records_fts: segment "); n < terms {
		t.Errorf("%d delete marker lines for %d terms of record 5: %q", n, terms, ftsProblems(rep))
	}
	if n := countContaining(rep.Problems, "holds a delete marker for record 5 (a genuine index of this case never holds one)"); n != min(terms, verifyMaxPerKind) {
		t.Errorf("%d marker lines for record 5, want %d (one per distinct term)", n, min(terms, verifyMaxPerKind))
	}
	if !containsSubstr(rep.Problems, "records_fts: record 5: the index lacks") {
		t.Errorf("the hidden postings of record 5 are not reported: %q", ftsProblems(rep))
	}
	if containsSubstr(rep.Problems, "though a search finds what a rebuild by SQLite holds") {
		t.Errorf("the rebuild explained the failing bucket, yet the unexplained-bucket line is there: %q", ftsProblems(rep))
	}
	if countOf(seen, "fts.compare/records_fts/vocab") != 1 {
		t.Errorf("the vocab comparison did not run for the table with a failing bucket: %v", seen)
	}
}

func countOf(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}

// TestVerifyFTSHostilePageIsAProblemNotAPanic: a leaf page overwritten with random bytes is a problem, never a
// panic or a hang.
func TestVerifyFTSHostilePageIsAProblemNotAPanic(t *testing.T) {
	for _, table := range indexableFTSTables {
		t.Run(table, func(t *testing.T) {
			c := indexedTextCase(t, 300)
			junk := make([]byte, 64)
			_, _ = rand.New(rand.NewSource(1)).Read(junk)                                                                                                                   //nolint:gosec // a seeded generator: the test must be reproducible
			res, err := c.store.db.Exec(`UPDATE `+table+`_data SET block = ? WHERE id = (SELECT min(id) FROM `+table+`_data WHERE id >= ?)`, junk, int64(1)<<ftsSegidShift) //nolint:gosec // a constant table name in a test
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("%d rows overwritten", n)
			}
			done := make(chan VerifyReport, 1)
			go func() { done <- mustVerify(t, c) }()
			select {
			case rep := <-done:
				if rep.OK() {
					t.Fatal("a random leaf passed")
				}
				if !containsSubstr(rep.Problems, table+": segment ") || !containsSubstr(rep.Problems, "is not a valid leaf") {
					t.Fatalf("no invalid-leaf line for %s: %q", table, ftsProblems(rep))
				}
			case <-time.After(5 * time.Minute):
				t.Fatal("verify did not finish")
			}
		})
	}
}

// TestVerifyFTSIndexPassCountsPages (14d, V3): the page counter of the index pass moves in the unit "pages" and
// ends at the total, and "fts-data" chunks are observed for every leaf page.
func TestVerifyFTSIndexPassCountsPages(t *testing.T) {
	c := indexedTextCase(t, 12000)
	var pages int
	var leafRows int
	for _, table := range indexableFTSTables {
		var n int
		if err := c.store.db.QueryRow(`SELECT count(*) FROM `+table+`_data WHERE id >= ? AND (id & ?) < ?`, int64(1)<<ftsSegidShift, ftsPageMask, ftsLeafLimit).Scan(&n); err != nil {
			t.Fatal(err)
		}
		leafRows += n
	}
	if leafRows < 10 {
		t.Fatalf("only %d leaf pages: the test shows nothing", leafRows)
	}
	_, err := c.verify(func(table string, rows int) {
		if table == "fts-data" {
			pages += rows
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages != leafRows {
		t.Fatalf("fts-data chunks add up to %d pages, the tables hold %d leaf pages", pages, leafRows)
	}
}

// TestVerifyFTSSelfTestFailureStillComparesConfig: the checks that need no tokenizer still run when the
// replica does not match, and the fresh _config rows are true whatever the replica does.
func TestVerifyFTSSelfTestFailureStillComparesConfig(t *testing.T) {
	c := indexedTextCase(t, 20)
	if _, err := c.store.db.Exec(`INSERT OR REPLACE INTO records_fts_config (k, v) VALUES ('pgsz', 64)`); err != nil {
		t.Fatal(err)
	}
	old := ftsReplicaOverride
	t.Cleanup(func() { ftsReplicaOverride = old })
	ftsReplicaOverride = func(string) func(string, func([]byte, int)) {
		return func(string, func([]byte, int)) {} // a replica that finds no token
	}
	rep := mustVerify(t, c)
	if !containsSubstr(rep.Problems, "tokenizer replica does not match this SQLite") || !containsSubstr(rep.Problems, "records_fts_config: key \"pgsz\"") {
		t.Fatalf("want the replica line and the config line: %q", rep.Problems)
	}
}

// the highest segment of a table, its last leaf page and the id of that page
func lastLeaf(t *testing.T, c *Case, table string) (seg, id int64) {
	t.Helper()
	if err := c.store.db.QueryRow(`SELECT id >> ?, id FROM `+table+`_data WHERE id >= ? AND (id & ?) < ? ORDER BY id DESC LIMIT 1`,
		ftsSegidShift, int64(1)<<ftsSegidShift, ftsPageMask, ftsLeafLimit).Scan(&seg, &id); err != nil {
		t.Fatal(err)
	}
	return seg, id
}

// TestVerifyFTSFailedSegmentContributesNothing (D147): a segment that fails after all its postings were
// decoded still contributes none of them, so its buckets fail and the rebuild is asked (the failing bucket is
// never explained away by postings of a segment that is not valid).
func TestVerifyFTSFailedSegmentContributesNothing(t *testing.T) {
	c := indexedTextCase(t, 300)
	seg, id := lastLeaf(t, c, FTSWordTable)
	// a page past the last page of the segment: every real page decodes, the segment is not valid
	if _, err := c.store.db.Exec(`INSERT INTO records_fts_data (id, block) VALUES (?, x'0000000400')`, id+1); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	want := fmt.Sprintf("records_fts: segment %d page %d is not a valid leaf: page outside the segment", seg, id&ftsPageMask+1)
	if !containsSubstr(rep.Problems, want) {
		t.Fatalf("no %q in %q", want, ftsProblems(rep))
	}
	if !containsSubstr(rep.Problems, "though a search finds what a rebuild by SQLite holds") {
		t.Errorf("the postings of the failed segment were used: its buckets did not fail: %q", ftsProblems(rep))
	}
}

// TestVerifyFTSMissingPagesAreAProblem: a segment the structure lists whose leaf rows are gone is a problem,
// found at the end of the pass for a segment no row of which was seen.
func TestVerifyFTSMissingPagesAreAProblem(t *testing.T) {
	c := indexedTextCase(t, 300)
	seg, _ := lastLeaf(t, c, FTSWordTable)
	if _, err := c.store.db.Exec(`DELETE FROM records_fts_data WHERE id >= ? AND id < ?`, seg<<ftsSegidShift, seg<<ftsSegidShift+ftsLeafLimit); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !containsSubstr(rep.Problems, fmt.Sprintf("records_fts: segment %d page ", seg)) || !containsSubstr(rep.Problems, "is not a valid leaf: missing page") {
		t.Fatalf("no missing-page line for segment %d: %q", seg, ftsProblems(rep))
	}
}

// TestVerifyFTSIndexPassSkipsEmptiedSegments (D143, D149): in the middle of an incremental merge the structure
// lists a segment with pages 0 to 0. Nothing is read for it, whatever rows of it exist, and it is not a problem.
func TestVerifyFTSIndexPassSkipsEmptiedSegments(t *testing.T) {
	c := indexedTextCase(t, 1)
	db := c.store.db
	for _, table := range FTSTables() { // replace the one-record index by a merge workload
		vocab := ftsVocabName(table)
		for _, stmt := range []string{"DROP TABLE " + vocab, "DROP TABLE " + table} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		ddl, _ := FTSTableDDL(table)
		vddl, _ := FTSVocabDDL(table)
		for _, stmt := range []string{ddl, vddl} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	probeExec(t, db, "INSERT INTO records_fts(records_fts, rank) VALUES('automerge', 0)")
	fill(t, db, 1, 120, 20)
	run := func() []string {
		rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
		r := c.newFTSVerify(context.Background(), &problemSet{rep: rep, counts: map[string]int{}}, nil, nil)
		if err := r.indexPass(FTSWordTable); err != nil {
			t.Fatal(err)
		}
		return rep.Problems
	}
	emptied := int64(-1)
	for step := 0; step < 400 && emptied < 0; step++ {
		probeExec(t, db, "INSERT INTO records_fts(records_fts, rank) VALUES('merge', 1)")
		for _, seg := range ftsStructureOf(t, db, FTSWordTable) {
			if seg.Empty() {
				emptied = seg.ID
			}
		}
		if p := run(); len(p) != 0 {
			t.Fatalf("step %d: a genuine index in the middle of a merge has problems: %q", step, p)
		}
	}
	if emptied < 0 {
		t.Fatal("the merge workload never produced an emptied segment, so it proves nothing")
	}
	// rows left behind for the emptied segment are not read
	probeExec(t, db, "INSERT OR REPLACE INTO records_fts_data (id, block) VALUES (?, x'ffffffff')", emptied<<ftsSegidShift+1)
	if p := run(); len(p) != 0 {
		t.Fatalf("a row of an emptied segment was read: %q", p)
	}
}

// TestVerifyFTSExpectedSideStopsWhenCancelled: once the verify context has ended the consumer of the record
// stream tokenizes nothing more (a chunk already read is not folded), and the cut is never a pass.
func TestVerifyFTSExpectedSideStopsWhenCancelled(t *testing.T) {
	c := indexedTextCase(t, 6000) // two chunks of 5000 and 1000 rows
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	docs, complete := int64(-1), true
	c.verifyFTSInspect = func(r *ftsVerify) { docs, complete = r.ex.docs, r.ex.complete }
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "records" {
			cancel() // the first chunk is read; its rows must not be folded
		}
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if docs != 0 || complete {
		t.Fatalf("after a cancel the expected side folded %d documents (complete %v), want none", docs, complete)
	}
	if !slices.Contains(rep.Problems, verifyCancelled) || containsSubstr(rep.Problems, "records_fts") {
		t.Fatalf("a cancelled verify: want the cancelled line and no full-text problem, got %q", ftsProblems(rep))
	}
}
