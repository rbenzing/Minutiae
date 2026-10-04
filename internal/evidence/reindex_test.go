package evidence_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// These tests drive ReindexText through its test seam (Case.SetReindexHook, export_test.go), so they
// live in the external test package, which may import the records writer that builds the corpus.

var (
	rctx      = context.Background()
	textParse = records.Parser{Name: "text-parser", Version: "1.0", Hash: "abc123"}
	corpusN   = len(recordstest.TextNames) // 16 records; `empty` has nothing to index
	corpusIdx = corpusN - 1
)

// corpusCase returns a case holding the 16-record text corpus, indexed by the writer.
func corpusCase(t *testing.T) (*evidence.Case, evidence.ManifestRecord) {
	t.Helper()
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, 256))
	recs, _ := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, textParse, []string{art.ID}, recs)
	return c, art
}

func auditActions(t *testing.T, c *evidence.Case, prefix string) []evidence.AuditEntry {
	t.Helper()
	all, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		if strings.HasPrefix(e.Action, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func decode[T any](t *testing.T, e evidence.AuditEntry) T {
	t.Helper()
	d, err := evidence.DecodeDetails[T](e.Details)
	if err != nil {
		t.Fatalf("audit seq %d (%s): %v", e.Seq, e.Action, err)
	}
	return d
}

func scalarInt(t *testing.T, c *evidence.Case, q string) int64 {
	t.Helper()
	var n int64
	if err := c.ReadTx(rctx, func(h evidence.ReadHandle) error { return h.QueryRow(q).Scan(&n) }); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func scalarString(t *testing.T, c *evidence.Case, q string) string {
	t.Helper()
	var s string
	if err := c.ReadTx(rctx, func(h evidence.ReadHandle) error { return h.QueryRow(q).Scan(&s) }); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func metaValue(t *testing.T, c *evidence.Case) string {
	t.Helper()
	return scalarString(t, c, `SELECT value FROM records_meta WHERE key = 'fts_norm_version'`)
}

// indexBytes is the whole storage of both full-text tables (data and docsize rows), in a form that
// changes when any index row changes.
func indexBytes(t *testing.T, c *evidence.Case) string {
	t.Helper()
	var parts []string
	for _, table := range evidence.FTSTables() {
		parts = append(parts,
			scalarString(t, c, `SELECT coalesce(group_concat(id || ':' || hex(block), ';'), '') FROM (SELECT id, block FROM `+table+`_data ORDER BY id)`),
			scalarString(t, c, `SELECT coalesce(group_concat(id || ':' || sz, ';'), '') FROM (SELECT id, sz FROM `+table+`_docsize ORDER BY id)`))
	}
	return strings.Join(parts, "|")
}

func docsizeCount(t *testing.T, c *evidence.Case, table string) int64 {
	t.Helper()
	return scalarInt(t, c, `SELECT count(*) FROM `+table+`_docsize`)
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestReindexAuditsBeforeChangingIndex: when records.reindex is on the audit log the index and the
// state are exactly as they were (a stale version, the old documents); after the reset the tables are
// empty and the state is "building". The audit comes first, so no index change is unaudited.
func TestReindexAuditsBeforeChangingIndex(t *testing.T) {
	c, _ := corpusCase(t)
	recordstest.SetFTSNormVersion(t, c.Dir, "fts0/old")
	before := indexBytes(t, c)
	seen := map[string]int{}
	c.SetReindexHook(func(point string) error {
		seen[point]++
		switch point {
		case "after-start-audit":
			es := auditActions(t, c, "records.reindex")
			if len(es) != 1 || es[0].Action != evidence.ActionReindex {
				t.Errorf("at after-start-audit the reindex audit entries are %+v, want one records.reindex", es)
				return nil
			}
			all, _ := c.ReadAudit()
			if last := all[len(all)-1]; last.Action != evidence.ActionReindex {
				t.Errorf("the last audit entry is %s, want records.reindex", last.Action)
			}
			d := decode[evidence.ReindexStart](t, es[0])
			if d.FromNormVersion != "fts0/old" || d.NormVersion != evidence.FTSNormVersion() || d.Records != int64(corpusN) || !slices.Equal(d.Tables, evidence.FTSTables()) {
				t.Errorf("records.reindex details = %+v", d)
			}
			if got := indexBytes(t, c); got != before {
				t.Error("the index changed before records.reindex was audited")
			}
			if got := metaValue(t, c); got != "fts0/old" {
				t.Errorf("the state at after-start-audit = %q, want the old value", got)
			}
		case "after-reset":
			for _, table := range evidence.FTSTables() {
				if n := docsizeCount(t, c, table); n != 0 {
					t.Errorf("at after-reset %s holds %d documents, want 0", table, n)
				}
			}
			if got := metaValue(t, c); got != "building" {
				t.Errorf("the state at after-reset = %q, want building", got)
			}
			if got := len(auditActions(t, c, "records.reindex")); got != 1 {
				t.Errorf("%d reindex entries at after-reset, want only the start", got)
			}
		}
		return nil
	})
	res, err := c.ReindexText(rctx, evidence.ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"after-start-audit", "after-reset", "after-chunk", "before-meta", "after-meta"} {
		if seen[p] == 0 {
			t.Errorf("the hook was never called at %s", p)
		}
	}
	if res.Indexed != int64(corpusIdx) || metaValue(t, c) != evidence.FTSNormVersion() {
		t.Errorf("result %+v, state %q", res, metaValue(t, c))
	}
}

// TestReindexInterruptedStaysUnsearchable: a fault after the first chunk is audited as an error, the
// state stays "building" so neither a search nor the writer may use the half-built index, and a second
// reindex completes the job.
func TestReindexInterruptedStaysUnsearchable(t *testing.T) {
	c, art := corpusCase(t)
	boom := errors.New("boom: the disk caught fire")
	chunks := 0
	c.SetReindexHook(func(point string) error {
		if point == "after-chunk" {
			if chunks++; chunks == 1 {
				return boom
			}
		}
		return nil
	})
	res, err := c.ReindexText(rctx, evidence.ReindexOptions{ChunkRows: 4})
	if !errors.Is(err, boom) {
		t.Fatalf("ReindexText = %+v, %v; want the injected fault", res, err)
	}
	es := auditActions(t, c, "records.reindex")
	if len(es) != 2 || es[0].Action != evidence.ActionReindex || es[1].Action != evidence.ActionReindexError {
		t.Fatalf("reindex audit entries = %+v, want records.reindex then records.reindex.error", es)
	}
	start, fail := decode[evidence.ReindexStart](t, es[0]), decode[evidence.ReindexFailure](t, es[1])
	if fail.ReindexID != start.ReindexID || !strings.Contains(fail.Error, "boom") || fail.RecordsIndexed != 4 {
		t.Errorf("records.reindex.error = %+v (start %+v), want the same id, the fault and 4 records indexed", fail, start)
	}
	st, err := c.IndexState(rctx)
	if err != nil || st.Kind != evidence.IndexBuilding {
		t.Fatalf("index state = %+v, %v; want building", st, err)
	}
	if err := c.RequireIndexCurrent(rctx); !errors.Is(err, evidence.ErrIndexNotCurrent) {
		t.Errorf("RequireIndexCurrent = %v, want ErrIndexNotCurrent", err)
	}
	w, err := records.NewWriter(c, records.Parser{Name: "other", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(rctx, records.StartOptions{Artifacts: []string{art.ID}}); !errors.Is(err, records.ErrIndexNotCurrent) {
		t.Errorf("Writer.Start on a building index = %v, want ErrIndexNotCurrent", err)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() || !containsSub(rep.Notices, "interrupted") {
		t.Errorf("verify of an interrupted reindex: %+v, %v; want OK with a notice naming the interruption", rep, err)
	} else if containsSub(rep.Notices, "never concluded") {
		t.Errorf("a reindex concluded by its error entry is reported as dangling: %v", rep.Notices)
	}

	c.SetReindexHook(nil)
	res, err = c.ReindexText(rctx, evidence.ReindexOptions{ChunkRows: 4})
	if err != nil || res.Indexed != int64(corpusIdx) {
		t.Fatalf("second ReindexText = %+v, %v", res, err)
	}
	if err := c.RequireIndexCurrent(rctx); err != nil {
		t.Errorf("RequireIndexCurrent after the second reindex: %v", err)
	}
	for _, table := range evidence.FTSTables() {
		if n := docsizeCount(t, c, table); n != int64(corpusIdx) {
			t.Errorf("%s holds %d documents, want %d", table, n, corpusIdx)
		}
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Errorf("verify after the second reindex: %+v, %v", rep, err)
	}
}

// TestReindexCancelledContextAuditsError: a context cancelled between two chunks stops the reindex
// with the cancellation, an audited error entry, and the index state "building".
func TestReindexCancelledContextAuditsError(t *testing.T) {
	c, _ := corpusCase(t)
	cctx, cancel := context.WithCancel(rctx)
	defer cancel()
	c.SetReindexHook(func(point string) error {
		if point == "after-chunk" {
			cancel()
		}
		return nil
	})
	_, err := c.ReindexText(cctx, evidence.ReindexOptions{ChunkRows: 4})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReindexText = %v, want context.Canceled", err)
	}
	es := auditActions(t, c, "records.reindex")
	if len(es) != 2 || es[1].Action != evidence.ActionReindexError {
		t.Fatalf("reindex audit entries = %+v, want the start and an error", es)
	}
	if fail := decode[evidence.ReindexFailure](t, es[1]); !strings.Contains(fail.Error, "cancel") || fail.RecordsIndexed != 4 {
		t.Errorf("records.reindex.error = %+v", fail)
	}
	if got := metaValue(t, c); got != "building" {
		t.Errorf("state = %q, want building", got)
	}
}

// TestReindexChunksAreBounded: 12,000 records with ChunkRows 5000 are written in three transactions
// (5000, 5000, 2000 documents), and a ChunkRows outside 0..20000 is refused.
func TestReindexChunksAreBounded(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	recs := recordstest.Records(art.ID, 12000, 7)
	for i := range recs {
		recs[i].Summary = "record number " + strings.Repeat("x", i%7) + " alpha"
	}
	recordstest.Ingest(t, c, textParse, []string{art.ID}, recs)

	var sizes []int64
	var last int64
	c.SetReindexHook(func(point string) error {
		if point == "after-chunk" {
			n := docsizeCount(t, c, evidence.FTSWordTable)
			sizes = append(sizes, n-last)
			last = n
		}
		return nil
	})
	res, err := c.ReindexText(rctx, evidence.ReindexOptions{ChunkRows: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sizes, []int64{5000, 5000, 2000}) {
		t.Errorf("chunk transactions added %v documents, want [5000 5000 2000]", sizes)
	}
	if res.Records != 12000 || res.Indexed != 12000 {
		t.Errorf("result = %+v", res)
	}
	n := len(auditActions(t, c, "records.reindex"))
	for _, rows := range []int{-1, 20001} {
		if _, err := c.ReindexText(rctx, evidence.ReindexOptions{ChunkRows: rows}); err == nil {
			t.Errorf("ChunkRows %d was accepted", rows)
		}
	}
	if got := len(auditActions(t, c, "records.reindex")); got != n {
		t.Errorf("a refused ChunkRows audited %d entries", got-n)
	}
}

// TestStartRefusedWhileReindexing: a records writer cannot start while a reindex holds the case's
// live-ingest slot (ErrIngestActive, nothing audited by the writer); once the index is being rebuilt
// the index state refuses it too; afterwards the slot is free and the writer starts.
func TestStartRefusedWhileReindexing(t *testing.T) {
	c, art := corpusCase(t)
	start := func() error {
		w, err := records.NewWriter(c, records.Parser{Name: "late", Version: "1"}, records.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Start(rctx, records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
			return err
		}
		if _, err := w.Abort(rctx, errors.New("test over")); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	var atStart, atChunk error
	c.SetReindexHook(func(point string) error {
		switch point {
		case "after-start-audit":
			atStart = start()
		case "after-chunk":
			atChunk = start()
		}
		return nil
	})
	if _, err := c.ReindexText(rctx, evidence.ReindexOptions{ChunkRows: 8}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(atStart, records.ErrIngestActive) || !errors.Is(atStart, evidence.ErrIngestActive) {
		t.Errorf("Start during the reindex = %v, want ErrIngestActive", atStart)
	}
	if !errors.Is(atChunk, records.ErrIndexNotCurrent) && !errors.Is(atChunk, records.ErrIngestActive) {
		t.Errorf("Start in the middle of the rebuild = %v, want a refusal", atChunk)
	}
	if n := len(auditActions(t, c, "records.ingest.start")); n != 1 {
		t.Errorf("%d records.ingest.start entries, want only the corpus ingest's", n)
	}
	c.SetReindexHook(nil)
	if err := start(); err != nil {
		t.Errorf("Start after the reindex = %v", err)
	}
	if live := c.LiveIngest(); live != "" {
		t.Errorf("the live-ingest slot still holds %q", live)
	}
}

// TestReindexCrashAfterMetaIsAVerifyNotice (ruling R18): the state is set to current BEFORE
// records.reindex.done is audited. A process that dies in between leaves a current index and an
// announced reindex that never concluded: verify says so in a notice (not a problem), and the next
// reindex concludes it.
func TestReindexCrashAfterMetaIsAVerifyNotice(t *testing.T) {
	c, _ := corpusCase(t)
	crash := errors.New("power cut")
	c.SetReindexHook(func(point string) error {
		if point == "after-meta" {
			return crash
		}
		return nil
	})
	if _, err := c.ReindexText(rctx, evidence.ReindexOptions{}); !errors.Is(err, crash) {
		t.Fatalf("ReindexText = %v, want the injected fault", err)
	}
	es := auditActions(t, c, "records.reindex")
	if len(es) != 1 || es[0].Action != evidence.ActionReindex {
		t.Fatalf("audit = %+v, want the announcement only (the index is current, so no error is claimed)", es)
	}
	if err := c.RequireIndexCurrent(rctx); err != nil {
		t.Fatalf("the index is current after the crash window: %v", err)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() || !containsSub(rep.Notices, "never concluded") || !containsSub(rep.Notices, "records reindex --case "+c.Dir) {
		t.Fatalf("verify = %+v, %v; want OK and a notice naming the unconcluded reindex and the remedy", rep, err)
	}
	c.SetReindexHook(nil)
	if _, err := c.ReindexText(rctx, evidence.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if rep, err := c.Verify(); err != nil || !rep.OK() || len(rep.Notices) != 0 {
		t.Fatalf("verify after the next reindex = %+v, %v; want no notice", rep, err)
	}
	if got := auditActions(t, c, "records.reindex"); len(got) != 3 {
		t.Errorf("reindex audit entries = %d, want start, start, done", len(got))
	}
}
