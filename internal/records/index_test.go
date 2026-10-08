package records_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// The corpus has 16 records; `empty` (summary "", no body) has nothing to index, so the other 15 do.
const (
	corpusRecords = 16
	corpusIndexed = 15
)

func docsizeCount(t *testing.T, c *evidence.Case, table string) int {
	t.Helper()
	return count(t, c, table+"_docsize")
}

func idRange(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// TestWriterIndexesInTheBatchTransaction: before the batch rows are inserted neither the records nor
// the index rows exist; after the insert both exist, and they hold exactly the records that have text.
func TestWriterIndexesInTheBatchTransaction(t *testing.T) {
	c, art := setup(t)
	recs, byName := recordstest.TextRecords(art.ID)
	w := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
	seen := map[string]bool{}
	w.SetHook(func(point string) error {
		seen[point] = true
		want, wantRecords := 0, 0
		switch point {
		case "after-batch-audit", "before-insert":
		case "after-insert":
			want, wantRecords = corpusIndexed, corpusRecords
		default:
			return nil
		}
		for _, table := range evidence.FTSTables() {
			if got := docsizeCount(t, c, table); got != want {
				t.Errorf("at %s: %s holds %d documents, want %d", point, table, got, want)
			}
		}
		if got := count(t, c, "records"); got != wantRecords {
			t.Errorf("at %s: %d records, want %d", point, got, wantRecords)
		}
		return nil
	})
	add(t, w, recs)
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"after-batch-audit", "before-insert", "after-insert"} {
		if !seen[p] {
			t.Errorf("the hook was never called at %s", p)
		}
	}
	for _, table := range evidence.FTSTables() {
		got := recordstest.IndexedIDs(t, c, table)
		if len(got) != corpusIndexed || slices.Contains(got, int64(byName["empty"]+1)) {
			t.Errorf("%s holds ids %v, want %d ids without the empty record", table, got, corpusIndexed)
		}
	}
}

// TestBatchFailureLeavesNoIndexRows: a fault inside the transaction (the structure record of the
// SUB index is wrecked, so the word index has already been written when the sub index fails) rolls
// the whole batch back: no record, no time, no batch row, no index row in either index, and the word
// index's own storage is byte for byte what it was. The batch is audited and its failure too.
func TestBatchFailureLeavesNoIndexRows(t *testing.T) {
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	w := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
	wordData := func() string {
		return fmt.Sprint(scalar[int](t, c, `SELECT count(*) FROM records_fts_data`), "/",
			scalar[string](t, c, `SELECT hex(group_concat(block)) FROM (SELECT block FROM records_fts_data ORDER BY id)`))
	}
	before := wordData()
	w.SetHook(func(point string) error {
		if point == "before-insert" {
			recordstest.WreckFTSStructure(t, c.Dir, evidence.FTSSubTable)
		}
		return nil
	})
	add(t, w, recs)
	err := w.Flush(ctx)
	if err == nil {
		t.Fatal("the batch succeeded although the index structure is wrecked")
	}
	if errors.Is(err, evidence.ErrIntegrity) || errors.Is(err, records.ErrIndexNotCurrent) {
		t.Fatalf("the fault is inside the transaction, not a schema or state refusal: %v", err)
	}
	if n := len(auditOf(t, c, evidence.ActionBatch)); n != 1 {
		t.Errorf("%d records.batch entries, want 1 (the batch was announced)", n)
	}
	if n := len(auditOf(t, c, evidence.ActionBatchError)); n != 1 {
		t.Errorf("%d records.batch.error entries, want 1", n)
	}
	for _, table := range []string{"records", "record_times", "record_batches"} {
		if n := count(t, c, table); n != 0 {
			t.Errorf("%s holds %d rows after the failed batch, want 0", table, n)
		}
	}
	for _, table := range evidence.FTSTables() {
		if n := docsizeCount(t, c, table); n != 0 {
			t.Errorf("%s holds %d documents after the failed batch, want 0", table, n)
		}
	}
	if after := wordData(); after != before {
		t.Errorf("the word index storage changed although the batch failed")
	}
	if err2 := w.Add(ctx, recs[0]); err2 == nil || err2.Error() != err.Error() {
		t.Errorf("writer after the failure: Add = %v, want the poison %v", err2, err)
	}
	res, err := w.Abort(ctx, err)
	if err != nil || res.Outcome != "incomplete" || res.Records != 0 {
		t.Fatalf("Abort = %+v, %v", res, err)
	}
}

// TestBatchRefusedWhenIndexSchemaIsTampered: a dropped FTS shadow table is a schema difference, so
// the writer refuses before it audits the batch: there is no records.batch entry, and nothing is
// written.
func TestBatchRefusedWhenIndexSchemaIsTampered(t *testing.T) {
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	w := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
	recordstest.DropTable(t, c.Dir, "records_fts_sub_data")
	add(t, w, recs)
	err := w.Flush(ctx)
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Flush = %v, want ErrIntegrity", err)
	}
	if n := len(auditOf(t, c, evidence.ActionBatch)); n != 0 {
		t.Errorf("%d records.batch entries, want none: the refusal comes before the audit", n)
	}
	if n := len(auditOf(t, c, evidence.ActionBatchError)); n != 0 {
		t.Errorf("%d records.batch.error entries, want none (no batch was announced)", n)
	}
	if n := count(t, c, "records"); n != 0 {
		t.Errorf("%d records stored, want 0", n)
	}
	if err2 := w.Add(ctx, recs[0]); !errors.Is(err2, evidence.ErrIntegrity) {
		t.Errorf("writer after the refusal: Add = %v, want the poison (ErrIntegrity)", err2)
	}
	res, err := w.Abort(ctx, err)
	if err != nil || res.Outcome != "incomplete" {
		t.Fatalf("Abort = %+v, %v", res, err)
	}
}

// TestRecordsWithoutTextAreNotIndexed: a record with no summary and no body has no document in
// either index; one with only a body or only a summary has one in both.
func TestRecordsWithoutTextAreNotIndexed(t *testing.T) {
	c, art := setup(t)
	recs, byName := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	id := func(name string) int64 { return int64(byName[name] + 1) }
	for _, table := range evidence.FTSTables() {
		got := recordstest.IndexedIDs(t, c, table)
		if slices.Contains(got, id("empty")) {
			t.Errorf("%s holds a document for the empty record", table)
		}
		for _, name := range []string{"bodyonly", "summaryonly"} {
			if !slices.Contains(got, id(name)) {
				t.Errorf("%s has no document for %s", table, name)
			}
		}
		if len(got) != corpusIndexed {
			t.Errorf("%s holds %d documents, want %d", table, len(got), corpusIndexed)
		}
	}
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, "bodyneedle"); !slices.Equal(got, []int64{id("bodyonly")}) {
		t.Errorf("MATCH bodyneedle = %v, want only bodyonly", got)
	}
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, "summary : summaryneedle"); !slices.Equal(got, []int64{id("summaryonly")}) {
		t.Errorf("MATCH summaryneedle in summary = %v, want only summaryonly", got)
	}
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, "body : "+recordstest.LongNeedle); !slices.Equal(got, []int64{id("long")}) {
		t.Errorf("MATCH the needle at the end of the 1 MiB body = %v, want only long", got)
	}
}

// TestIndexTextIsNormalized: the index holds the normalized text, so a query in the normalized
// spelling finds every spelling of it, in both indexes; the stored text stays the original.
func TestIndexTextIsNormalized(t *testing.T) {
	c, art := setup(t)
	recs, byName := recordstest.TextRecords(art.ID)
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	id := func(name string) int64 { return int64(byName[name] + 1) }

	norm := func(s string) string { return `"` + evidence.NormalizeText(s) + `"` }
	cafeNFD := recs[byName["cafe_nfd"]].Summary
	cafeNFC := recs[byName["cafe_nfc"]].Summary
	if cafeNFD == cafeNFC {
		t.Fatal("the corpus must hold two different spellings of the same word")
	}
	for _, c1 := range []struct {
		table, match string
		want         []int64
	}{
		{evidence.FTSWordTable, norm(cafeNFD), []int64{id("cafe_nfc"), id("cafe_nfd")}},
		{evidence.FTSWordTable, norm(cafeNFC), []int64{id("cafe_nfc"), id("cafe_nfd")}},
		{evidence.FTSSubTable, norm(cafeNFD), []int64{id("cafe_nfc"), id("cafe_nfd")}},
		{evidence.FTSWordTable, `"strasse"`, []int64{id("strasse")}},
		{evidence.FTSWordTable, `"abc123"`, []int64{id("fullwidth")}},
		{evidence.FTSWordTable, `"istanbul"`, []int64{id("turkish")}},
		{evidence.FTSWordTable, `"ispirta"`, []int64{id("turkish")}},
		{evidence.FTSWordTable, `"password"`, []int64{id("bidi")}},       // the bidi override is dropped
		{evidence.FTSWordTable, `"pass word"`, []int64{id("zerowidth")}}, // a zero width space separates words
		{evidence.FTSSubTable, `"example.com/path"`, []int64{id("url")}},
	} {
		if got := recordstest.FTSMatch(t, c, c1.table, c1.match); !slices.Equal(got, c1.want) {
			t.Errorf("%s MATCH %s = %v, want %v", c1.table, c1.match, got, c1.want)
		}
	}
	// the stored text is untouched
	rows := loadRows(t, c)
	for _, name := range []string{"cafe_nfc", "cafe_nfd", "strasse", "bidi", "zerowidth", "turkish"} {
		if got := rows[byName[name]].Summary; got != recs[byName[name]].Summary {
			t.Errorf("%s: the stored summary %q is not the original %q", name, got, recs[byName[name]].Summary)
		}
	}
}

// TestWriterRefusesIndexThatIsNotCurrent: an index that is not current is refused by Start before
// anything is audited or recovered, and a state flipped after Start is refused before the batch is
// audited (or, when it flips after the audit, fails the batch inside the transaction).
func TestWriterRefusesIndexThatIsNotCurrent(t *testing.T) {
	other := records.Parser{Name: "other-parser", Version: "9.9"}
	for _, state := range []struct{ name, value string }{
		{"unbuilt", ""}, {"building", "building"}, {"stale", "fts1/unicode-9.0.0/sqlite-3.0.0"}, {"garbage", "not a version!"},
	} {
		t.Run(state.name, func(t *testing.T) {
			c, art := setup(t)
			recs, _ := recordstest.TextRecords(art.ID)
			recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
			recordstest.SetFTSNormVersion(t, c.Dir, state.value)
			entries := len(auditOf(t, c, ""))

			w := newWriter(t, c, other, records.WriterOptions{})
			err := w.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}})
			if !errors.Is(err, records.ErrIndexNotCurrent) {
				t.Fatalf("Start = %v, want ErrIndexNotCurrent", err)
			}
			if !strings.Contains(err.Error(), "records reindex --case "+c.Dir) {
				t.Errorf("the refusal %q does not name the remedy", err)
			}
			if got := len(auditOf(t, c, "")); got != entries {
				t.Errorf("Start appended %d audit entries, want none", got-entries)
			}
			if err := w.Add(ctx, recs[0]); !errors.Is(err, records.ErrWriterNotStarted) {
				t.Errorf("Add on a writer whose Start was refused = %v, want ErrWriterNotStarted", err)
			}
		})
	}

	for _, point := range []string{"", "after-batch-audit"} {
		name := "flipped after Start"
		if point != "" {
			name = "flipped after the batch audit"
		}
		t.Run(name, func(t *testing.T) {
			c, art := setup(t)
			recs, _ := recordstest.TextRecords(art.ID)
			w := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
			if point == "" {
				recordstest.SetFTSNormVersion(t, c.Dir, "building")
			} else {
				w.SetHook(func(p string) error {
					if p == point {
						recordstest.SetFTSNormVersion(t, c.Dir, "building")
					}
					return nil
				})
			}
			add(t, w, recs)
			err := w.Flush(ctx)
			if !errors.Is(err, records.ErrIndexNotCurrent) {
				t.Fatalf("Flush = %v, want ErrIndexNotCurrent", err)
			}
			wantBatch, wantErr := 0, 0
			if point != "" {
				wantBatch, wantErr = 1, 1 // announced, then failed inside the transaction
			}
			if n := len(auditOf(t, c, evidence.ActionBatch)); n != wantBatch {
				t.Errorf("%d records.batch entries, want %d", n, wantBatch)
			}
			if n := len(auditOf(t, c, evidence.ActionBatchError)); n != wantErr {
				t.Errorf("%d records.batch.error entries, want %d", n, wantErr)
			}
			if n := count(t, c, "records"); n != 0 {
				t.Errorf("%d records stored, want 0", n)
			}
			if n := docsizeCount(t, c, evidence.FTSWordTable); n != 0 {
				t.Errorf("%d documents indexed, want 0", n)
			}
			if res, err := w.Abort(ctx, err); err != nil || res.Outcome != "incomplete" {
				t.Fatalf("Abort = %+v, %v", res, err)
			}
		})
	}
}

// TestStartRefusesNonCurrentIndexBeforeRecovery: a dead ingest is not recovered by a Start that is
// refused for the index: the refusal comes before recovery writes anything.
func TestStartRefusesNonCurrentIndexBeforeRecovery(t *testing.T) {
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	dead := startWriter(t, c, testParser, records.WriterOptions{}, art.ID)
	crashAt(dead, "after-batch-audit", 1)
	add(t, dead, recs[:3])
	if !survive(t, func() { _ = dead.Flush(ctx) }) {
		t.Fatal("the writer did not crash")
	}
	recordstest.SetFTSNormVersion(t, c.Dir, "building")
	entries := len(auditOf(t, c, ""))
	w := newWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}}); !errors.Is(err, records.ErrIndexNotCurrent) {
		t.Fatalf("Start = %v, want ErrIndexNotCurrent", err)
	}
	if got := len(auditOf(t, c, "")); got != entries {
		t.Errorf("the refused Start appended %d audit entries (recovery must not run), want none", got-entries)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)); n != 0 {
		t.Errorf("%d records.ingest.recover entries, want none", n)
	}
	if n := count(t, c, "record_runs"); n != 0 {
		t.Errorf("%d run rows, want none", n)
	}
}

// TestIndexMaintenanceKeepsEveryRecordVerifiable: the index adds nothing to what is audited or
// digested (the batch entry does not mention it), and a case that holds the corpus verifies.
func TestIndexMaintenanceKeepsEveryRecordVerifiable(t *testing.T) {
	c, art := setup(t)
	recs, _ := recordstest.TextRecords(art.ID)
	res := recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	if res.Records != corpusRecords {
		t.Fatalf("ingested %d records, want %d", res.Records, corpusRecords)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.RecordsChecked != corpusRecords || len(rep.Notices) != 0 {
		t.Fatalf("verify: ok=%v records=%d problems=%v notices=%v", rep.OK(), rep.RecordsChecked, rep.Problems, rep.Notices)
	}
	batches := auditOf(t, c, evidence.ActionBatch)
	if len(batches) != 1 {
		t.Fatalf("%d records.batch entries, want 1", len(batches))
	}
	for k, v := range batches[0].Details {
		if s := strings.ToLower(k + fmt.Sprint(v)); strings.Contains(s, "fts") || strings.Contains(s, "index") {
			t.Errorf("the records.batch entry mentions the index: %q = %v", k, v)
		}
	}
}

// TestAddConcurrentWithIndex: eight goroutines add 500 records each; every record is indexed in both
// tables, with contiguous ids.
func TestAddConcurrentWithIndex(t *testing.T) {
	const goroutines, each = 8, 500
	c, art := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 700}, art.ID)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, r := range recordstest.Records(art.ID, each, int64(g)) {
				if err := w.Add(ctx, r); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != goroutines*each || res.Batches < 2 {
		t.Fatalf("End = %+v, want %d records in several batches", res, goroutines*each)
	}
	want := idRange(1, goroutines*each)
	for _, table := range evidence.FTSTables() {
		if got := recordstest.IndexedIDs(t, c, table); !slices.Equal(got, want) {
			t.Errorf("%s holds %d documents (first %v), want ids 1..%d", table, len(got), got[:min(3, len(got))], goroutines*each)
		}
	}
	if got := recordstest.FTSMatch(t, c, evidence.FTSWordTable, "summary : seed"); !slices.Equal(got, want) {
		t.Errorf("MATCH seed finds %d records, want %d", len(got), goroutines*each)
	}
}
