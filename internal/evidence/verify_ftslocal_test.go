package evidence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestStreamRecordsBetweenReadsOnlyTheRange (5D-14k): on 12,000 records, the range [5001, 5100] yields exactly
// 100 rows, ids 5001..5100, in order, with their times, whatever the chunk size; the times of the records just
// outside the range are not read, and a range is not a whole-table scan (no table count is compared).
func TestStreamRecordsBetweenReadsOnlyTheRange(t *testing.T) {
	c := indexedTextCase(t, 12000)
	for _, id := range []int64{5000, 5001, 5050, 5100, 5101, 11999, 12000} {
		if _, err := c.store.db.Exec(`INSERT INTO record_times (record_id, kind, ts, ts_basis) VALUES (?, 'read', ?, 'utc')`, id, id*10); err != nil {
			t.Fatal(err)
		}
	}
	type got struct {
		ids   []int64
		times map[int64]int64
	}
	run := func(lo, hi int64, chunk int) (got, []string) {
		g := got{times: map[int64]int64{}}
		ps := &problemSet{rep: &VerifyReport{Problems: []string{}, Notices: []string{}}, counts: map[string]int{}}
		chunks := 0
		ok := c.streamRecordsBetween(t.Context(), ps, lo, hi, chunk, func(string, int) { chunks++ }, func(row RecordRow, _ recordRowMeta) {
			g.ids = append(g.ids, row.ID)
			for _, tm := range row.Times {
				g.times[row.ID] = tm.TS
			}
		})
		if !ok {
			t.Fatalf("[%d, %d] chunk %d: the stream failed: %q", lo, hi, chunk, ps.rep.Problems)
		}
		if want := max(1, (len(g.ids)+chunk-1)/chunk); chunks != want {
			t.Errorf("[%d, %d] chunk %d: %d chunk reads for %d rows, want %d", lo, hi, chunk, chunks, len(g.ids), want)
		}
		return g, ps.rep.Problems
	}
	for _, chunk := range []int{5000, 30, 100, 1} {
		t.Run(fmt.Sprint("chunk", chunk), func(t *testing.T) {
			g, problems := run(5001, 5100, chunk)
			if len(problems) != 0 {
				t.Fatalf("problems for a range: %q", problems)
			}
			if len(g.ids) != 100 {
				t.Fatalf("%d rows, want 100", len(g.ids))
			}
			for i, id := range g.ids {
				if id != int64(5001+i) {
					t.Fatalf("row %d is record %d, want %d", i, id, 5001+i)
				}
			}
			if len(g.times) != 3 || g.times[5001] != 50010 || g.times[5050] != 50500 || g.times[5100] != 51000 {
				t.Fatalf("times %v, want only those of records 5001, 5050 and 5100 (5000 and 5101 are outside the range)", g.times)
			}
		})
	}
	t.Run("edges", func(t *testing.T) {
		if g, _ := run(12000, 12000, 5000); len(g.ids) != 1 || g.ids[0] != 12000 || g.times[12000] != 120000 {
			t.Errorf("single record: %v %v", g.ids, g.times)
		}
		if g, _ := run(11990, math.MaxInt64, 5000); len(g.ids) != 11 || len(g.times) != 2 {
			t.Errorf("the tail: %d rows, times %v", len(g.ids), g.times)
		}
		if g, _ := run(math.MinInt64, 3, 5000); len(g.ids) != 3 {
			t.Errorf("the head: %d rows", len(g.ids))
		}
		if g, _ := run(20000, 30000, 5000); len(g.ids) != 0 {
			t.Errorf("an empty range holds %d rows", len(g.ids))
		}
		// the whole table is still compared with its counts
		g, _ := run(math.MinInt64, math.MaxInt64, 5000)
		if len(g.ids) != 12000 {
			t.Errorf("the whole range: %d rows", len(g.ids))
		}
	})
}

// insertTextRecordsAt inserts unaudited records at the given ids, with text(id), in one record_batches row.
func insertTextRecordsAt(t *testing.T, c *Case, artifactID string, ids []int64, text func(i int) (string, string)) {
	t.Helper()
	db := c.store.db
	if _, err := db.Exec(`INSERT OR IGNORE INTO parsers (name, version, hash) VALUES ('raw', '1', NULL)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO record_batches (ingest_id, batch_no, first_id, count, digest, created) VALUES ('ing-raw', 1, ?, ?, ?, 'x')`, ids[0], len(ids), strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	var last int64
	for _, id := range ids {
		s, b := text(int(id))
		var body any
		if b != "" {
			body = b
		}
		if _, err := tx.Exec(`INSERT INTO records (id, batch_id, type, payload_v, artifact_id, parser_id, summary, body, payload)
			VALUES (?, 1, 'event', 1, ?, (SELECT id FROM parsers WHERE name = 'raw'), ?, ?, '{}')`, id, artifactID, s, body); err != nil {
			t.Fatal(err)
		}
		last = max(last, id)
	}
	if _, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = 'next_id'`, last+1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyLocalizationStopsWhenEveryKindIsFull (CC1, the second stop): when the six per-item kinds (hidden,
// invented and order, for each of the two tables) already hold their 50 listed items, the buckets that are left
// are not rebuilt, and each is still named with that reason.
func TestVerifyLocalizationStopsWhenEveryKindIsFull(t *testing.T) {
	defer func(old int) { ftsLocalizeRoundPostings = old }(ftsLocalizeRoundPostings)
	ftsLocalizeRoundPostings = 1 // one bucket per round
	c, rec := caseWithArtifact(t)
	var ids []int64
	for b := range int64(3) { // three blocks of 2^16 ids: three outside buckets
		for i := range int64(150) {
			ids = append(ids, b<<16+i+1)
		}
	}
	insertTextRecordsAt(t, c, rec.ID, ids, ftsText)
	if _, err := c.ReindexText(t.Context(), ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	db := c.store.db
	for _, table := range FTSTables() {
		norm := func(id int) (string, string) { s, b := ftsText(id); return NormalizeText(s), NormalizeText(b) }
		for id := 1; id <= 50; id++ { // hidden
			s, b := norm(id)
			if _, err := db.Exec(`INSERT INTO `+table+`(`+table+`, rowid, summary, body) VALUES ('delete', ?, ?, ?)`, id, s, b); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
		}
		for id := 51; id <= 100; id++ { // indexed twice
			s, b := norm(id)
			if _, err := db.Exec(`INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, id, s, b); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
		}
		for id := 101; id <= 150; id++ { // text the record does not hold
			if _, err := db.Exec(`INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, id, fmt.Sprintf("zzinvented%d", id), "zzinventedbody"); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
		}
		for _, id := range []int{1<<16 + 1, 2<<16 + 1} { // the other two blocks fail too
			s, b := norm(id)
			if _, err := db.Exec(`INSERT INTO `+table+`(`+table+`, rowid, summary, body) VALUES ('delete', ?, ?, ?)`, id, s, b); err != nil { //nolint:gosec // a constant table name in a test
				t.Fatal(err)
			}
		}
	}
	var rebuilt int
	c.verifyFTSDone = func(r *ftsVerify) {
		if r.lc != nil {
			rebuilt = r.lc.rebuilt
		}
	}
	rep := mustVerify(t, c)
	if rebuilt != 1 {
		t.Fatalf("%d buckets rebuilt, want 1: the six kinds hold their 50 items after the first", rebuilt)
	}
	for _, table := range FTSTables() {
		want := table + ": 2 batches whose index postings differ from their records were not localized (every kind of difference already lists its 50 items): "
		if !containsSubstr(rep.Problems, want) {
			t.Errorf("no %q in %q", want, ftsProblems(rep))
		}
		for _, kind := range []string{"the index lacks", "the index holds", "twice"} {
			n := 0
			for _, p := range rep.Problems {
				if strings.HasPrefix(p, table+": ") && strings.Contains(p, kind) {
					n++
				}
			}
			if n < verifyMaxPerKind {
				t.Errorf("%s: %d lines with %q, want at least %d", table, n, kind, verifyMaxPerKind)
			}
		}
	}
}

// TestVerifyFTSOversizeSegmentChecksNoDocument (D153, D158): a segment that cannot be read is a problem even when
// it contributes no posting to any bucket; no document is then counted as checked.
func TestVerifyFTSOversizeSegmentChecksNoDocument(t *testing.T) {
	c := indexedTextCase(t, 300)
	st := ftsStructureWithExtraSegment(t, c, FTSWordTable, 1<<30, 1, 1)
	if _, err := c.store.db.Exec(`UPDATE records_fts_data SET block = ? WHERE id = ?`, st, ftsStructureRowID); err != nil {
		t.Fatal(err)
	}
	var failing int
	c.verifyFTSInspect = func(r *ftsVerify) { failing = len(r.failing[0]) + len(r.failing[1]) }
	rep := mustVerify(t, c)
	if failing != 0 {
		t.Fatalf("%d failing buckets: the test must show a segment that fails with every bucket equal", failing)
	}
	if !containsSubstr(rep.Problems, "records_fts: segment 1073741824 cannot have data rows") {
		t.Fatalf("the oversize segment is not reported: %q", ftsProblems(rep))
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d with a segment that could not be read", rep.FTSDocsChecked)
	}
}

// TestVerifyLocalizationDeleteMarkerWithoutAnEffectIsNotAReplicaMismatch: a delete marker for terms the record never
// had fails its bucket (a genuine index holds no marker), changes no posting, and is explained by its own problem
// line: the bucket is not also accused of a tokenizer mismatch.
func TestVerifyLocalizationDeleteMarkerWithoutAnEffectIsNotAReplicaMismatch(t *testing.T) {
	c := indexedTextCase(t, 50)
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 5, 'zzabsent', 'zzabsentbody')`); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !containsSubstr(rep.Problems, "holds a delete marker for record 5") {
		t.Fatalf("the delete marker is not reported: %q", ftsProblems(rep))
	}
	if containsSubstr(rep.Problems, "own rebuild there") || containsSubstr(rep.Problems, "were not localized") {
		t.Errorf("the explained bucket is accused or silent: %q", ftsProblems(rep))
	}
}

// TestVerifyCancelDuringTheRebuildStreamOfALocalizationRound: a cancel that lands while the records of a failing
// bucket are read is a cut, with the findings of the index pass kept, the bucket named as not localized, no
// cancel noise, and no scratch area left.
func TestVerifyCancelDuringTheRebuildStreamOfALocalizationRound(t *testing.T) {
	c := indexedTextCase(t, 12000) // three chunks of 5000, 5000, 2000 rows in the rebuild stream
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', 5, ?, ?)`, normalizeFor(5)...); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := 0
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "fts-localize" {
			chunks++
			cancel()
		}
	}, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if chunks != 1 {
		t.Fatalf("%d chunks of the rebuild stream were read after the cancel, want 1 (the one in flight)", chunks)
	}
	if !slices.Contains(rep.Problems, verifyCancelled) {
		t.Fatalf("no %q in %q", verifyCancelled, rep.Problems)
	}
	if !containsSubstr(rep.Problems, "holds a delete marker for record 5") || !containsSubstr(rep.Problems, "were not localized") {
		t.Errorf("the findings or the unlocalized bucket are missing: %q", ftsProblems(rep))
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "context canceled") || strings.Contains(p, "unreadable") || strings.Contains(p, "could not") {
			t.Errorf("cancel noise in the report: %q", p)
		}
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scratch area is still there: %v", err)
	}
}

// TestVerifyCancelDuringTheIndexPassKeepsTheKnownFailingBuckets (D162): table 0 is tampered without a delete marker,
// so its failing bucket is known only from the digests. A cancel at the first chunk of table 1's pages ends the pass;
// the bucket is still named as not localized, so the report is not a bare cancel (the CLI exits 4, not 1).
func TestVerifyCancelDuringTheIndexPassKeepsTheKnownFailingBuckets(t *testing.T) {
	c := indexedTextCase(t, 300)
	c.verifySerial = true // the cancel lands in table 1's pass while table 0's is over: an order only the serial pass has
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(rowid, summary, body) VALUES (7, 'zzinvented', 'zzinventedbody')`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ended []string
	c.verifyPhaseHook = func(name string, _ time.Duration) { ended = append(ended, name) }
	chunks, atCancel := 0, []string(nil)
	rep, err := c.verifyRun(ctx, func(table string, _ int) {
		if table == "fts-data" {
			if chunks++; chunks == 2 { // the first chunk is table 0's last, the second is table 1's first
				atCancel = slices.Clone(ended)
				cancel()
			}
		}
	}, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// D169: the cancel must land in table 1's pass, not merely be the second chunk of anything: table 0's decode
	// phase has ended and table 1's has not.
	if chunks != 2 || !slices.Contains(atCancel, "fts.index/records_fts/decode") ||
		slices.Contains(atCancel, "fts.index/records_fts_sub/decode") {
		t.Fatalf("the cancel did not land in table 1's pass: %d chunks, phases ended by then %q", chunks, atCancel)
	}
	if !slices.Contains(rep.Problems, verifyCancelled) {
		t.Fatalf("no cancelled line in %q", rep.Problems)
	}
	if !containsSubstr(rep.Problems, "records_fts: 1 batches whose index postings differ from their records were not localized (the verification ended before they were rebuilt)") {
		t.Fatalf("the known failing bucket is not named: %q", ftsProblems(rep))
	}
	if rep.CancelledOnly() {
		t.Fatal("a finding was masked by the cancel: the report is only a cancel")
	}
}

// TestVerifyFTSIncompleteRecordStreamChecksNoDocument (D163): when the expected side did not see the whole record
// stream (without the verify being dead), no document is counted as checked.
func TestVerifyFTSIncompleteRecordStreamChecksNoDocument(t *testing.T) {
	c := indexedTextCase(t, 50)
	c.verifyFTSInspect = func(r *ftsVerify) { r.ex.complete = false }
	rep := mustVerify(t, c)
	if rep.FTSDocsChecked != 0 {
		t.Fatalf("FTSDocsChecked = %d for an incomplete record stream", rep.FTSDocsChecked)
	}
}

// TestVerifyLocalizationNamesAtMost50BatchesAndCountsTheRest (D165): the closing line lists 50 names and counts all.
func TestVerifyLocalizationNamesAtMost50BatchesAndCountsTheRest(t *testing.T) {
	c, rec := caseWithArtifact(t)
	var ids []int64
	for b := range int64(60) {
		ids = append(ids, b<<16+1)
	}
	insertTextRecordsAt(t, c, rec.ID, ids, ftsText)
	if _, err := c.ReindexText(t.Context(), ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rank) VALUES ('automerge', 0)`); err != nil {
		t.Fatal(err)
	}
	defer func(old int) { ftsLocalizeMaxBuckets = old }(ftsLocalizeMaxBuckets)
	ftsLocalizeMaxBuckets = 1
	for _, id := range ids {
		s, b := ftsText(int(id))
		if _, err := c.store.db.Exec(`INSERT INTO records_fts(records_fts, rowid, summary, body) VALUES ('delete', ?, ?, ?)`, id, NormalizeText(s), NormalizeText(b)); err != nil {
			t.Fatal(err)
		}
	}
	rep := mustVerify(t, c)
	var line string
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, "records_fts: 59 batches whose index postings differ from their records were not localized") {
			line = p
		}
	}
	if line == "" {
		t.Fatalf("no line for 59 unlocalized batches in %q", ftsProblems(rep))
	}
	if n := strings.Count(line, "outside every audited batch"); n != 50 || !strings.Contains(line, "and 9 more") {
		t.Fatalf("%d names, want 50 and \"and 9 more\": %s", n, line)
	}
}
