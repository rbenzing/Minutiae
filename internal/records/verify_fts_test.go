package records_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// Verify P11: the full-text index equals a rebuild from the records. Every tamper test makes the
// tamper real first (a search really loses the hit; PRAGMA integrity_check is still "ok"), then
// asserts the specific problem text, and shows that nothing else is reported.

// integrityCheck returns the messages of PRAGMA integrity_check (P10's own query).
func integrityCheck(t *testing.T, c *evidence.Case) []string {
	t.Helper()
	var msgs []string
	err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		rows, err := h.QueryContext(ctx, `SELECT integrity_check FROM pragma_integrity_check`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				return err
			}
			msgs = append(msgs, m)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func requireIntegrityOK(t *testing.T, c *evidence.Case) {
	t.Helper()
	if got := integrityCheck(t, c); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("PRAGMA integrity_check = %q, want ok: the tamper is not the silent kind", got)
	}
}

// problemsWith returns the problems that contain sub.
func problemsWith(rep evidence.VerifyReport, sub string) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.Contains(p, sub) {
			out = append(out, p)
		}
	}
	return out
}

func requireProblem(t *testing.T, rep evidence.VerifyReport, subs ...string) {
	t.Helper()
	for _, p := range rep.Problems {
		ok := true
		for _, s := range subs {
			if !strings.Contains(p, s) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Errorf("no single problem contains all of %q; problems: %q", subs, rep.Problems)
}

// onlyFTS requires that every problem of the report concerns the full-text index (names one of
// the tables), so a tamper test shows the index problem and nothing else.
func onlyFTS(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	for _, p := range rep.Problems {
		if !strings.Contains(p, "records_fts") && !strings.Contains(p, "further fts-") {
			t.Errorf("a problem outside the full-text index: %s", p)
		}
	}
	if rep.OK() {
		t.Error("verify reported OK")
	}
}

// ftsBaseRecords is the number of records of recordstest.TextRecords ftsBase ingests (every one but the
// last, `long`): 15 records, 14 with text.
const (
	ftsBaseRecords = 15
	ftsBaseIndexed = 14
)

// ftsBase is the text corpus of ftsBaseRecords records (ids 1..15), ingested into a case that is closed
// again, to be cloned per tamper.
func ftsBase(t *testing.T) string {
	t.Helper()
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs[:ftsBaseRecords]) // without the 1 MiB record: a vocab of a million entries slows every verify
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return c.Dir
}

// clone returns an open copy of the closed base case.
func clone(t *testing.T, base string) *evidence.Case {
	t.Helper()
	return openClone(t, cloneCase(t, base))
}

// bulkRecords returns n records with three unique tokens in the summary (so the index has many
// terms and, for n in the thousands, segments of many leaf pages) and a body.
func bulkRecords(artifactID string, n int) []records.Record {
	out := make([]records.Record, n)
	for i := range out {
		out[i] = records.Record{
			Type: "note", ArtifactID: artifactID,
			Summary: fmt.Sprintf("alpha%d beta%d gamma%d", i, i*7, i*13),
			Body:    fmt.Sprintf("body of record %d with delta%d and epsilon%d lorem ipsum", i, i*3, i*11),
			Payload: map[string]any{"i": i},
		}
	}
	return out
}

func bulkBase(t *testing.T, n int) string {
	t.Helper()
	c, art := setup(t)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, bulkRecords(art.ID, n))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return c.Dir
}

func TestVerifyFTSCleanAfterIngest(t *testing.T) {
	c, _ := corpusIndexedCase(t)
	start := time.Now()
	rep := mustVerify(t, c)
	t.Logf("verify of the 16-record corpus (one record has a 1 MiB body): %v", time.Since(start))
	if !rep.OK() {
		t.Fatalf("a freshly ingested case must verify: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != corpusIndexed || rep.RecordsChecked != corpusRecords {
		t.Fatalf("FTSDocsChecked = %d (want %d), RecordsChecked = %d (want %d)", rep.FTSDocsChecked, corpusIndexed, rep.RecordsChecked, corpusRecords)
	}
	if len(rep.Notices) != 0 {
		t.Fatalf("notices: %q", rep.Notices)
	}
}

func TestVerifyFTSEmptyCaseIsClean(t *testing.T) {
	c := recordstest.NewCase(t)
	rep := mustVerify(t, c)
	if !rep.OK() || rep.FTSDocsChecked != 0 || len(rep.Notices) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

// TestVerifyDetectsHiddenHit: a record whose postings were removed with FTS5's own 'delete' command
// is still in `records`, but a search no longer finds it. PRAGMA integrity_check is silent; P11 names
// the record and the missing term.
func TestVerifyDetectsHiddenHit(t *testing.T) {
	base := ftsBase(t)
	c := clone(t, base)
	recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, 1)
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, `"cafe"`); !slices.Equal(got, []int64{2}) {
		t.Fatalf("MATCH cafe = %v: record 1 is not hidden", got)
	}
	requireIntegrityOK(t, c)
	rep := mustVerify(t, c)
	requireProblem(t, rep, `records_fts: record 1: the index lacks "`, `which the text of the record holds (a search hit is hidden)`)
	if len(problemsWith(rep, "integrity_check")) != 0 {
		t.Errorf("integrity_check reported it: P11 is not what found it: %q", rep.Problems)
	}
	// the substring index and every other record are untouched
	for _, p := range rep.Problems {
		if strings.Contains(p, "records_fts_sub") || strings.Contains(p, "record 2:") {
			t.Errorf("a problem outside the tampered record and table: %s", p)
		}
	}
	onlyFTS(t, rep)
}

// TestVerifyDetectsInventedHit: extra postings under the rowid of an existing record, of a record that
// does not exist and of a record with nothing to index make a search find a record for words its text
// does not hold.
func TestVerifyDetectsInventedHit(t *testing.T) {
	base := ftsBase(t)
	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"existing record", 2, `records_fts: record 2: the index holds "forgedword" (`},
		{"record that does not exist", 9999, `records_fts: the index holds "forgedword" (summary, offset 0) for record 9999, which does not exist or has no indexable text (a search hit is invented)`},
		{"record without indexable text", 13, `records_fts: the index holds "forgedword" (summary, offset 0) for record 13, which does not exist or has no indexable text (a search hit is invented)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clone(t, base)
			recordstest.InventIndexText(t, c.Dir, evidence.FTSWordTable, tc.id, "forgedword", "")
			if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, `"forgedword"`); !slices.Equal(got, []int64{tc.id}) {
				t.Fatalf("MATCH forgedword = %v: the hit is not invented", got)
			}
			requireIntegrityOK(t, c)
			rep := mustVerify(t, c)
			requireProblem(t, rep, tc.want)
			if tc.id == 2 {
				requireProblem(t, rep, `which the text of the record does not hold (a search hit is invented)`)
			}
			if len(problemsWith(rep, "integrity_check")) != 0 {
				t.Errorf("integrity_check reported it: %q", rep.Problems)
			}
			onlyFTS(t, rep)
		})
	}
}

// TestVerifyDetectsHiddenSubstringHit: the same for the trigram (substring) index.
func TestVerifyDetectsHiddenSubstringHit(t *testing.T) {
	c := clone(t, ftsBase(t))
	recordstest.HideFromIndex(t, c.Dir, evidence.FTSSubTable, 14)
	if got := recordstest.FTSMatch(t, c, evidence.FTSSubTable, `"dyneedl"`); len(got) != 0 {
		t.Fatalf("MATCH dyneedl = %v: record 14 is not hidden", got)
	}
	requireIntegrityOK(t, c)
	rep := mustVerify(t, c)
	requireProblem(t, rep, `records_fts_sub: record 14: the index lacks "`, `(a search hit is hidden)`)
	for _, p := range rep.Problems {
		if !strings.Contains(p, "records_fts_sub") {
			t.Errorf("the word index is untouched but a problem names it: %s", p)
		}
	}
	onlyFTS(t, rep)
}

// TestVerifyDetectsEmptiedIndex: meta says the index is current but it holds nothing (delete-all):
// every record is reported, at most 50 of them listed, then one "further" line.
func TestVerifyDetectsEmptiedIndex(t *testing.T) {
	c := clone(t, bulkBase(t, 120))
	recordstest.EmptyFTSIndex(t, c.Dir)
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, `"alpha5"`); len(got) != 0 {
		t.Fatalf("the index still answers: %v", got)
	}
	requireIntegrityOK(t, c)
	rep := mustVerify(t, c)
	listed := map[string]bool{}
	for _, p := range problemsWith(rep, "records_fts: record ") {
		if !strings.Contains(p, "the index lacks") {
			continue
		}
		rest := strings.TrimPrefix(p, "records_fts: record ")
		listed[rest[:strings.Index(rest, ":")]] = true
	}
	if len(listed) != 50 {
		t.Errorf("%d distinct records listed as hidden in the word index, want 50 (the cap per kind)", len(listed))
	}
	requireProblem(t, rep, "further fts-hidden problems are not listed")
	requireProblem(t, rep, "records_fts: the index totals differ from a rebuild")
	onlyFTS(t, rep)
}

// TestVerifyDetectsDocsizeTamper: the per-document sizes feed bm25: an altered, deleted or extra
// row is reported (and a value of another storage class: only a typeof check sees it).
func TestVerifyDetectsDocsizeTamper(t *testing.T) {
	base := ftsBase(t)
	for _, tc := range []struct {
		name   string
		tamper func(dir string)
		want   string
	}{
		{
			"altered", func(dir string) { recordstest.CorruptDocsize(t, dir, evidence.FTSWordTable, 3) },
			`records_fts_docsize: record 3: the size entry differs from a rebuild`,
		},
		{
			"deleted", func(dir string) { recordstest.DeleteDocsize(t, dir, evidence.FTSWordTable, 3) },
			`records_fts_docsize: record 3: the size entry is missing`,
		},
		{
			"extra", func(dir string) { recordstest.AddDocsize(t, dir, evidence.FTSWordTable, 9999) },
			`records_fts_docsize: the index holds a size entry for record 9999, which does not exist or has no indexable text`,
		},
		{
			"TEXT instead of a blob", func(dir string) { recordstest.SetDocsize(t, dir, evidence.FTSWordTable, 3, "0101") },
			`records_fts_docsize: record 3: the size entry is of storage class text, a rebuild holds blob`,
		},
		{
			"INTEGER instead of a blob", func(dir string) { recordstest.SetDocsize(t, dir, evidence.FTSWordTable, 3, int64(7)) },
			`records_fts_docsize: record 3: the size entry is of storage class integer, a rebuild holds blob`,
		},
		{
			"sub index altered", func(dir string) { recordstest.CorruptDocsize(t, dir, evidence.FTSSubTable, 4) },
			`records_fts_sub_docsize: record 4: the size entry differs from a rebuild`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clone(t, base)
			tc.tamper(c.Dir)
			requireIntegrityOK(t, c)
			rep := mustVerify(t, c)
			requireProblem(t, rep, tc.want)
			if len(rep.Problems) != 1 {
				t.Errorf("want exactly the one problem, got %q", rep.Problems)
			}
			onlyFTS(t, rep)
		})
	}
}

// TestVerifyDetectsTotalsTamper: the totals record (document and token counts) feeds bm25.
func TestVerifyDetectsTotalsTamper(t *testing.T) {
	base := ftsBase(t)
	for _, table := range evidence.FTSTables() {
		t.Run(table, func(t *testing.T) {
			c := clone(t, base)
			b := recordstest.FTSTotals(t, c.Dir, table)
			if len(b) == 0 || b[0] >= 0x7f {
				t.Fatalf("unexpected totals record %x", b)
			}
			b[0]++
			recordstest.SetFTSTotals(t, c.Dir, table, b)
			requireIntegrityOK(t, c)
			rep := mustVerify(t, c)
			requireProblem(t, rep, table+`: the index totals differ from a rebuild (ranking is unreliable)`)
			if len(rep.Problems) != 1 {
				t.Errorf("want exactly the one problem, got %q", rep.Problems)
			}
			onlyFTS(t, rep)
		})
	}
}

// TestVerifyDetectsConfigTamper: the FTS5 configuration rows (version, page size, ...) are compared;
// a value of another storage class and an extra key too.
func TestVerifyDetectsConfigTamper(t *testing.T) {
	base := ftsBase(t)
	for _, tc := range []struct {
		name   string
		tamper func(dir string)
		want   string
	}{
		{
			"value changed", func(dir string) { recordstest.SetFTSConfigInt(t, dir, evidence.FTSWordTable, "version", 5) },
			`records_fts_config: key "version": the index holds integer 5, a rebuild holds integer 4`,
		},
		{
			"TEXT instead of an integer", func(dir string) { recordstest.SetFTSConfig(t, dir, evidence.FTSWordTable, "version", "4") },
			`records_fts_config: key "version": the index holds text "4", a rebuild holds integer 4`,
		},
		{
			"extra key", func(dir string) { recordstest.SetFTSConfigInt(t, dir, evidence.FTSSubTable, "forged", 1) },
			`records_fts_sub_config: key "forged" is not part of a rebuild`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clone(t, base)
			tc.tamper(c.Dir)
			requireIntegrityOK(t, c)
			rep := mustVerify(t, c)
			requireProblem(t, rep, tc.want)
			if len(rep.Problems) != 1 {
				t.Errorf("want exactly the one problem, got %q", rep.Problems)
			}
			onlyFTS(t, rep)
		})
	}
}

// TestVerifyDetectsFTSStructureCorruption: a wrecked structure record makes the index unreadable
// inside SQLite; verify completes, names it, and does not panic.
func TestVerifyDetectsFTSStructureCorruption(t *testing.T) {
	base := ftsBase(t)
	for _, table := range evidence.FTSTables() {
		t.Run(table, func(t *testing.T) {
			c := clone(t, base)
			recordstest.WreckFTSStructure(t, c.Dir, table)
			rep := mustVerify(t, c)
			requireProblem(t, rep, table+`: the index cannot be read`)
			if rep.OK() {
				t.Fatal("verify reported OK")
			}
			// the audit still records the run
			all := auditOf(t, c, "verify.run")
			if len(all) == 0 {
				t.Fatal("verify.run was not audited")
			}
			// a rebuild repairs it
			if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
				t.Fatal(err)
			}
			if rep := mustVerify(t, c); !rep.OK() {
				t.Fatalf("after reindex: %q", rep.Problems)
			}
		})
	}
}

// TestVerifyDetectsIndexOfTamperedRecordText: P4 (the digest) and P11 see the same alteration: the
// record text no longer equals what the index was built from.
func TestVerifyDetectsIndexOfTamperedRecordText(t *testing.T) {
	c := clone(t, ftsBase(t))
	recordstest.SetRecordSummary(t, c.Dir, 4, "completely different text")
	rep := mustVerify(t, c)
	requireProblem(t, rep, "digest mismatch")
	requireProblem(t, rep, `records_fts: record 4: the index holds "abc123"`, `a search hit is invented`)
	requireProblem(t, rep, `records_fts: record 4: the index lacks "completely"`, `a search hit is hidden`)
	if rep.OK() {
		t.Fatal("verify reported OK")
	}
}

// TestVerifyDetectsIndexEntryOfDeletedRecord: a record row removed while its index entry stays.
func TestVerifyDetectsIndexEntryOfDeletedRecord(t *testing.T) {
	c := clone(t, ftsBase(t))
	recordstest.DeleteRecord(t, c.Dir, 6)
	rep := mustVerify(t, c)
	requireProblem(t, rep, "for record 6, which does not exist or has no indexable text (a search hit is invented)")
	requireProblem(t, rep, "records_fts_docsize: the index holds a size entry for record 6, which does not exist or has no indexable text")
	if rep.OK() {
		t.Fatal("verify reported OK")
	}
}

// TestVerifyDetectsDuplicateDocument: a document indexed a second time under the same rowid (what a
// writer that did not check for an existing rowid would leave) is not accepted: the index of a rebuild
// holds each record once.
func TestVerifyDetectsDuplicateDocument(t *testing.T) {
	base := ftsBase(t)
	for _, table := range evidence.FTSTables() {
		t.Run(table, func(t *testing.T) {
			c := clone(t, base)
			recordstest.DuplicateIndexRow(t, c.Dir, table, 3)
			rep := mustVerify(t, c)
			requireProblem(t, rep, table+`: the index totals differ from a rebuild (ranking is unreliable)`)
			onlyFTS(t, rep)
		})
	}
}

// TestVerifyFTSIndexTamperWithMetaDodgingStillSafe: an attacker who hides a hit and also sets the
// state to "not built" gets a clean verify with a NOTICE that says the content is not verified, but
// neither a search nor the writer will use the index: the dodge costs availability, never a wrong hit.
func TestVerifyFTSIndexTamperWithMetaDodgingStillSafe(t *testing.T) {
	c := clone(t, ftsBase(t))
	recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, 1)
	recordstest.SetFTSNormVersion(t, c.Dir, "")
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("problems: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d: the content was not compared", rep.FTSDocsChecked)
	}
	if !containsNotice(rep, "the index content is not verified") {
		t.Fatalf("notices %q: the dodge is silent", rep.Notices)
	}
	// what search relies on (Reader.Search and TermHits call it first) refuses
	if err := c.RequireIndexCurrent(ctx); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Fatalf("RequireIndexCurrent = %v, want ErrIndexNotCurrent", err)
	}
	// R38: a real search and term lookup refuse too (the hidden hit is never answered around)
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	q, err := records.CompileQuery("cafe", records.TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Search(ctx, records.Filter{Text: q}, records.Page{}, records.SearchOptions{}); !errors.Is(err, evidence.ErrIndexNotCurrent) || !strings.Contains(err.Error(), "records reindex") {
		t.Fatalf("Search = %v, want ErrIndexNotCurrent naming records reindex", err)
	}
	if _, err := r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "cafe"}}, 5); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Fatalf("TermHits = %v, want ErrIndexNotCurrent", err)
	}
	// and so does the writer
	w, err := records.NewWriter(c, testParser, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	man, err := c.Manifest()
	if err != nil || len(man) == 0 {
		t.Fatalf("manifest: %v, %v", man, err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{man[0].ID}}); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Fatalf("Start = %v, want ErrIndexNotCurrent", err)
	}
	// reindex repairs it and the content is verified again
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	rep = mustVerify(t, c)
	if !rep.OK() || rep.FTSDocsChecked != ftsBaseIndexed || len(rep.Notices) != 0 {
		t.Fatalf("after reindex: %+v", rep)
	}
}

func containsNotice(rep evidence.VerifyReport, sub string) bool {
	for _, n := range rep.Notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestVerifyFTSNoticesForNonCurrentStates: every state in which the index may not answer is a NOTICE
// that says the content is not verified, never a problem and never silence.
func TestVerifyFTSNoticesForNonCurrentStates(t *testing.T) {
	base := ftsBase(t)
	for _, tc := range []struct{ name, value, want string }{
		{"unbuilt", "", "is not built"},
		{"building", "building", "was interrupted"},
		{"stale version", "fts0/unicode-1.0.0/sqlite-3.0.0", "was built by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clone(t, base)
			recordstest.SetFTSNormVersion(t, c.Dir, tc.value)
			rep := mustVerify(t, c)
			if !rep.OK() {
				t.Fatalf("problems: %q", rep.Problems)
			}
			if rep.FTSDocsChecked != 0 {
				t.Errorf("FTSDocsChecked = %d", rep.FTSDocsChecked)
			}
			found := false
			for _, n := range rep.Notices {
				if strings.Contains(n, tc.want) && strings.Contains(n, "the index content is not verified") && strings.Contains(n, "records reindex") {
					found = true
				}
			}
			if !found {
				t.Errorf("no notice with %q that says the content is not verified and names the remedy: %q", tc.want, rep.Notices)
			}
		})
	}
}

// TestVerifyFTSInvalidMetaIsProblem: a state that is not a version of the index at all is a problem
// (the case cannot say what its index is).
func TestVerifyFTSInvalidMetaIsProblem(t *testing.T) {
	base := ftsBase(t)
	t.Run("garbage", func(t *testing.T) {
		c := clone(t, base)
		recordstest.SetFTSNormVersion(t, c.Dir, "garbage!")
		rep := mustVerify(t, c)
		requireProblem(t, rep, `records_meta fts_norm_version holds "garbage!", which is not a version of the full-text index`)
		if rep.FTSDocsChecked != 0 {
			t.Errorf("FTSDocsChecked = %d", rep.FTSDocsChecked)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		c := clone(t, base)
		recordstest.DeleteFTSNormVersion(t, c.Dir)
		rep := mustVerify(t, c)
		requireProblem(t, rep, `records_meta fts_norm_version is missing`)
	})
}

// TestVerifyFTSAfterReindexIsClean: a tampered index is repaired by `records reindex`.
func TestVerifyFTSAfterReindexIsClean(t *testing.T) {
	c := clone(t, ftsBase(t))
	recordstest.HideFromIndex(t, c.Dir, evidence.FTSWordTable, 1)
	recordstest.InventIndexText(t, c.Dir, evidence.FTSSubTable, 2, "forgedword", "")
	recordstest.CorruptDocsize(t, c.Dir, evidence.FTSWordTable, 5)
	if rep := mustVerify(t, c); rep.OK() {
		t.Fatal("the tampered case verified")
	}
	if _, err := c.ReindexText(ctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !rep.OK() || rep.FTSDocsChecked != ftsBaseIndexed {
		t.Fatalf("after reindex: problems %q, FTSDocsChecked %d", rep.Problems, rep.FTSDocsChecked)
	}
}

// TestVerifyFTSOnV2CaseIsSkipped: a schema v2 case has no index: no P11, no count, no full-text finding.
func TestVerifyFTSOnV2CaseIsSkipped(t *testing.T) {
	c := openClone(t, recordstest.CopyV2Case(t))
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("problems: %q", rep.Problems)
	}
	if rep.FTSDocsChecked != 0 {
		t.Errorf("FTSDocsChecked = %d", rep.FTSDocsChecked)
	}
	for _, p := range append(append([]string{}, rep.Problems...), rep.Notices...) {
		if strings.Contains(p, "records_fts") {
			t.Errorf("a v2 case reports on the full-text index: %s", p)
		}
	}
}

// idxCorpusRecords is large enough that every segment of both indexes spans many leaf pages (so
// <table>_idx has many rows to tamper with).
const idxCorpusRecords = 3000

// TestVerifyDetectsIdxTamper (R5, R29): deleting rows of <table>_idx (the b-tree index over the leaf
// pages of a segment) makes MATCH lose hits while the leaf pages, the vocab, PRAGMA integrity_check
// and FTS5's own integrity-check all stay silent. P11 therefore also proves MATCH reachability: for
// every term of the rebuilt vocabulary, MATCH on the case index returns exactly the rebuilt document
// set. Repointed or altered rows are caught by integrity_check (P10).
func TestVerifyDetectsIdxTamper(t *testing.T) {
	base := bulkBase(t, idxCorpusRecords)
	lostSearches := func(t *testing.T, c *evidence.Case, table string) int {
		t.Helper()
		lost := 0
		for i := 0; i < idxCorpusRecords; i += 3 {
			tok := "alpha" + strconv.Itoa(i)
			if table == evidence.FTSSubTable {
				tok = "lpha" + strconv.Itoa(i)
			}
			got := recordstest.FTSMatch(t, c, table, `"`+tok+`"`)
			if !slices.Contains(got, int64(i+1)) {
				lost++
			}
		}
		return lost
	}
	for _, table := range evidence.FTSTables() {
		t.Run(table+"/deleted rows are silent to integrity_check but found by MATCH reachability", func(t *testing.T) {
			c := clone(t, base)
			if lostSearches(t, c, table) != 0 {
				t.Fatal("a healthy index loses hits")
			}
			if rep := mustVerify(t, c); !rep.OK() {
				t.Fatalf("the healthy corpus does not verify: %q", rep.Problems)
			}
			n := recordstest.DeleteFTSIdxRows(t, c.Dir, table)
			if n < 10 {
				t.Fatalf("only %d _idx rows were deleted: the corpus is too small for a multi-page segment", n)
			}
			if lostSearches(t, c, table) == 0 {
				t.Fatal("no search lost a hit: the tamper is not real")
			}
			requireIntegrityOK(t, c)
			rep := mustVerify(t, c)
			requireProblem(t, rep, table+`: a search for "`, `does not find record`, `(a search hit is lost: the index structure no longer reaches it)`)
			if len(problemsWith(rep, "integrity_check")) != 0 {
				t.Errorf("integrity_check reported it: %q", rep.Problems)
			}
			// the logical content equals a rebuild: only the reachability check sees it
			for _, p := range rep.Problems {
				if strings.Contains(p, "the index lacks") || strings.Contains(p, "is invented") || strings.Contains(p, "totals differ") || strings.Contains(p, "size entry") {
					t.Errorf("a content problem beside the reachability one: %s", p)
				}
			}
			onlyFTS(t, rep)
			if got := len(problemsWith(rep, "a search hit is lost")); got > 51 {
				t.Errorf("%d reachability problems listed, want at most 50 and a stop notice", got)
			}
		})
		t.Run(table+"/repointed or altered rows are caught by integrity_check", func(t *testing.T) {
			for name, tamper := range map[string]func(t testing.TB, dir, table string){
				"repointed": recordstest.RepointFTSIdxRow,
				"term":      recordstest.AlterFTSIdxTerm,
			} {
				c := clone(t, base)
				tamper(t, c.Dir, table)
				if got := integrityCheck(t, c); slices.Equal(got, []string{"ok"}) {
					t.Fatalf("%s: integrity_check did not notice the tamper", name)
				}
				rep := mustVerify(t, c)
				if len(problemsWith(rep, "integrity_check failed")) == 0 {
					t.Errorf("%s: verify did not report integrity_check: %q", name, rep.Problems)
				}
				if rep.OK() {
					t.Errorf("%s: verify reported OK", name)
				}
			}
		})
	}
}
