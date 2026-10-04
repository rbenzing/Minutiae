package records_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestNewWriterRequiresSchemaV2(t *testing.T) {
	c, err := evidence.Open(recordstest.NewV1Case(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := records.NewWriter(c, testParser, records.WriterOptions{}); !errors.Is(err, evidence.ErrNeedsUpgrade) {
		t.Fatalf("NewWriter on a v1 case = %v, want ErrNeedsUpgrade", err)
	}
}

func TestNewWriterValidatesParserAndOptions(t *testing.T) {
	c, _ := setup(t)
	for name, p := range map[string]records.Parser{
		"empty name":  {Name: "", Version: "1"},
		"bad name":    {Name: "a b", Version: "1"},
		"bad version": {Name: "a", Version: ""},
		"bad hash":    {Name: "a", Version: "1", Hash: "a b"},
	} {
		if _, err := records.NewWriter(c, p, records.WriterOptions{}); !errors.Is(err, records.ErrInvalidField) {
			t.Errorf("%s: NewWriter = %v, want ErrInvalidField", name, err)
		}
	}
	for name, o := range map[string]records.WriterOptions{
		"rows over max":  {BatchRows: 20001},
		"rows negative":  {BatchRows: -1},
		"bytes negative": {BatchBytes: -1},
	} {
		if _, err := records.NewWriter(c, testParser, o); err == nil {
			t.Errorf("%s: NewWriter accepted %+v", name, o)
		}
	}
	if _, err := records.NewWriter(nil, testParser, records.WriterOptions{}); err == nil {
		t.Error("NewWriter accepted a nil case")
	}
	for _, o := range []records.WriterOptions{{}, {BatchRows: 1}, {BatchRows: 20000, BatchBytes: 1}} {
		if _, err := records.NewWriter(c, testParser, o); err != nil {
			t.Errorf("NewWriter(%+v) = %v", o, err)
		}
	}
}

func TestStartRequiresDeclaredArtifactsInManifest(t *testing.T) {
	c, a := setup(t)
	w := newWriter(t, c, testParser, records.WriterOptions{})
	for name, arts := range map[string][]string{
		"none":      nil,
		"empty id":  {""},
		"unknown":   {"nope"},
		"one known": {a.ID, "nope"},
	} {
		err := w.Start(ctx, records.StartOptions{Artifacts: arts})
		if err == nil {
			t.Errorf("%s: Start accepted %v", name, arts)
		}
		if name != "none" && name != "empty id" && !errors.Is(err, evidence.ErrUnknownArtifact) {
			t.Errorf("%s: err = %v, want ErrUnknownArtifact", name, err)
		}
	}
	if n := len(auditOf(t, c, evidence.ActionIngestStart)); n != 0 {
		t.Fatalf("%d records.ingest.start entries after refused Starts", n)
	}
	// duplicates are folded and the list is sorted in the audit entry
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{b.ID, a.ID, b.ID}}); err != nil {
		t.Fatal(err)
	}
	st := details[evidence.IngestStart](t, auditOf(t, c, evidence.ActionIngestStart)[0])
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(st.Artifacts, want) {
		t.Errorf("audited artifacts = %v, want %v", st.Artifacts, want)
	}
	if st.Parser != testParser.Name || st.ParserVersion != testParser.Version || st.ParserHash != testParser.Hash || st.BatchRows != 5000 || st.Reingest {
		t.Errorf("start entry = %+v", st)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err == nil {
		t.Error("a second Start was accepted")
	}
}

func TestWriterStateErrors(t *testing.T) {
	c, a := setup(t)
	w := newWriter(t, c, testParser, records.WriterOptions{})
	rec := recordstest.Records(a.ID, 1, 1)[0]
	if err := w.Add(ctx, rec); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("Add before Start = %v", err)
	}
	if err := w.Flush(ctx); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("Flush before Start = %v", err)
	}
	if err := w.Warn(ctx, "p", "r"); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("Warn before Start = %v", err)
	}
	if _, err := w.End(ctx); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("End before Start = %v", err)
	}
	if _, err := w.Abort(ctx, errors.New("x")); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("Abort before Start = %v", err)
	}
	if w.IngestID() != "" {
		t.Errorf("IngestID before Start = %q", w.IngestID())
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if w.IngestID() == "" {
		t.Error("no IngestID after Start")
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(ctx, rec); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Add after End = %v", err)
	}
	if _, err := w.End(ctx); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("End after End = %v", err)
	}
	if _, err := w.Abort(ctx, errors.New("x")); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Abort after End = %v", err)
	}
}

func TestAddRejectsUnknownArtifact(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	before := len(auditOf(t, c, ""))
	rec := recordstest.Records("nope", 1, 1)[0]
	if err := w.Add(ctx, rec); !errors.Is(err, records.ErrUnknownArtifact) || errors.Is(err, records.ErrUndeclaredArtifact) {
		t.Errorf("Add of an artifact not in the manifest = %v, want ErrUnknownArtifact", err)
	}
	rec.ArtifactID = ""
	if err := w.Add(ctx, rec); !errors.Is(err, records.ErrInvalidRecord) {
		t.Errorf("Add with no artifact = %v", err)
	}
	if got := len(auditOf(t, c, "")); got != before {
		t.Errorf("%d audit entries added by rejected records", got-before)
	}
	res, err := w.End(ctx)
	if err != nil || res.Records != 0 {
		t.Errorf("End = %+v, %v; nothing was added", res, err)
	}
}

func TestAddRejectsUndeclaredArtifact(t *testing.T) {
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	before := len(auditOf(t, c, ""))
	err := w.Add(ctx, recordstest.Records(b.ID, 1, 1)[0])
	if !errors.Is(err, records.ErrUndeclaredArtifact) || !errors.Is(err, records.ErrInvalidRecord) {
		t.Fatalf("Add of an undeclared artifact = %v, want ErrUndeclaredArtifact", err)
	}
	if got := len(auditOf(t, c, "")); got != before {
		t.Errorf("%d audit entries added by a rejected record", got-before)
	}
	res, err := w.End(ctx)
	if err != nil || res.Records != 0 || res.Batches != 0 || count(t, c, "records") != 0 {
		t.Errorf("End = %+v, %v; nothing was buffered", res, err)
	}
}

// TestBatchAuditedBeforeRowsWritten: the batch is announced in the (fsynced)
// audit log before any row of it exists, and the hooks fire outside any
// transaction (they query the database).
func TestBatchAuditedBeforeRowsWritten(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 3}, a.ID)
	digestRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	var points []string
	w.SetHook(func(p string) error {
		points = append(points, p)
		batches := auditOf(t, c, evidence.ActionBatch)
		switch p {
		case "after-batch-audit", "before-insert":
			if len(batches) != 1 {
				t.Errorf("%s: %d records.batch audit entries, want 1", p, len(batches))
				return nil
			}
			b := details[evidence.BatchCommit](t, batches[0])
			if !digestRE.MatchString(b.Digest) || b.Count != 3 || b.FirstID != 1 || b.BatchNo != 1 || b.IngestID != w.IngestID() {
				t.Errorf("%s: audited batch = %+v", p, b)
			}
			if n := scalar[int](t, c, `SELECT count(*) FROM records`); n != 0 {
				t.Errorf("%s: %d records already written", p, n)
			}
			if n := scalar[int](t, c, `SELECT count(*) FROM record_batches`); n != 0 {
				t.Errorf("%s: %d record_batches rows already written", p, n)
			}
			if n := scalar[int](t, c, `SELECT count(*) FROM record_times`); n != 0 {
				t.Errorf("%s: record_times rows already written", p)
			}
		case "after-insert":
			if n := scalar[int](t, c, `SELECT count(*) FROM records`); n != 3 {
				t.Errorf("after-insert: %d records, want 3", n)
			}
			if n := scalar[int](t, c, `SELECT count(*) FROM record_batches`); n != 1 {
				t.Errorf("after-insert: %d record_batches rows, want 1", n)
			}
		}
		return nil
	})
	add(t, w, recordstest.Records(a.ID, 3, 7))
	want := []string{"after-batch-audit", "before-insert", "after-insert"}
	if !reflect.DeepEqual(points, want) {
		t.Fatalf("hook points = %v, want %v", points, want)
	}
}

func TestBatchAuditCarriesDigestAndArtifactHashes(t *testing.T) {
	c, a := setup(t)
	inc := recordstest.AddIncompleteArtifact(t, c, "cut.db", mib[:5000])
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID, inc.ID)
	recs := recordstest.Records(a.ID, 20, 3)
	for _, r := range recordstest.Records(inc.ID, 5, 4) {
		if r.Range != nil && r.Range.Offset+r.Range.Length > inc.Size {
			r.Range = nil
		}
		recs = append(recs, r)
	}
	add(t, w, recs)
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	batches := auditOf(t, c, evidence.ActionBatch)
	if len(batches) != 1 {
		t.Fatalf("%d batches audited", len(batches))
	}
	b := details[evidence.BatchCommit](t, batches[0])

	sha := map[string]string{a.ID: a.SHA256, inc.ID: inc.SHA256}
	bd := evidence.NewBatchDigest()
	types := map[string]int64{}
	for _, r := range loadRows(t, c) {
		bd.Add(evidence.RowDigest(r, sha[r.ArtifactID]))
		types[r.Type]++
	}
	if b.Digest != bd.Sum() {
		t.Errorf("audited digest %s != recomputed %s", b.Digest, bd.Sum())
	}
	if !reflect.DeepEqual(b.Artifacts, sha) {
		t.Errorf("audited artifact hashes = %v, want %v", b.Artifacts, sha)
	}
	if !reflect.DeepEqual(b.ArtifactIncomplete, []string{inc.ID}) {
		t.Errorf("artifact_incomplete = %v, want [%s]", b.ArtifactIncomplete, inc.ID)
	}
	if !reflect.DeepEqual(b.Types, types) {
		t.Errorf("audited types = %v, want %v", b.Types, types)
	}
	if b.Count != 25 || b.FirstID != 1 {
		t.Errorf("count/first = %d/%d", b.Count, b.FirstID)
	}
	stored := scalar[string](t, c, `SELECT digest FROM record_batches`)
	if stored != b.Digest {
		t.Errorf("record_batches.digest %s != audited %s", stored, b.Digest)
	}
}

func TestBatchSplitsAtRowAndByteThresholds(t *testing.T) {
	t.Run("rows", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 10}, a.ID)
		add(t, w, recordstest.Records(a.ID, 25, 1))
		if n := count(t, c, "records"); n != 20 {
			t.Errorf("%d records written before the final flush, want 20 (two full batches)", n)
		}
		res, err := w.End(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Batches != 3 || res.Records != 25 || res.FirstID != 1 || res.LastID != 25 {
			t.Errorf("result = %+v", res)
		}
		var got [][2]int64
		for _, e := range auditOf(t, c, evidence.ActionBatch) {
			b := details[evidence.BatchCommit](t, e)
			got = append(got, [2]int64{b.FirstID, int64(b.Count)})
		}
		if want := [][2]int64{{1, 10}, {11, 10}, {21, 5}}; !reflect.DeepEqual(got, want) {
			t.Errorf("audited (first,count) = %v, want %v", got, want)
		}
		if lo, hi := scalar[int](t, c, `SELECT min(id) FROM records`), scalar[int](t, c, `SELECT max(id) FROM records`); lo != 1 || hi != 25 {
			t.Errorf("ids %d..%d", lo, hi)
		}
		if n := scalar[int](t, c, `SELECT count(*) FROM records r JOIN record_batches b ON b.batch_id = r.batch_id WHERE r.id BETWEEN b.first_id AND b.first_id + b.count - 1`); n != 25 {
			t.Errorf("%d records inside their batch's range, want 25", n)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{BatchBytes: 1}, a.ID)
		add(t, w, recordstest.Records(a.ID, 5, 1))
		res, err := w.End(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Batches != 5 {
			t.Errorf("%d batches with a tiny BatchBytes, want one per record (5)", res.Batches)
		}
	})
	t.Run("bytes with large row threshold", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 20000, BatchBytes: 4000}, a.ID)
		add(t, w, recordstest.Records(a.ID, 50, 1))
		res, err := w.End(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Batches < 2 || res.Batches >= 50 {
			t.Errorf("%d batches with BatchBytes 4000, want a few", res.Batches)
		}
	})
}

func TestStoredTextIsOriginal(t *testing.T) {
	c, a := setup(t)
	nfd := "Cafe" + string(rune(0x301)) + " " + string(rune(0x202e)) + "evil" + string(rune(0x202c)) + " " + string(rune(0x1F600)) + " " + string(rune(0x200d)) + " trailing   "
	body := strings.Repeat("b", 4<<20-1)
	r := records.Record{
		Type: "note", ArtifactID: a.ID, Summary: nfd, SourcePath: "/p/with trailing space ", Body: body,
		Locator: "sqlite:t=" + string(rune(0x202e)) + ";r=1  ", Payload: map[string]any{"nfd": nfd, "k" + string(rune(0x301)): "v "},
	}
	// the same letter precomposed (NFC) is a different byte string and must stay one
	nfc := "Caf" + string(rune(0xe9))
	r2 := records.Record{Type: "note", ArtifactID: a.ID, Summary: nfc, Payload: map[string]any{"nfc": nfc}}
	recordstest.Ingest(t, c, testParser, []string{a.ID}, []records.Record{r, r2})
	rows := loadRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[1].Summary != nfc || !strings.Contains(rows[1].Payload, nfc) {
		t.Errorf("the NFC text came back changed: %q %q", rows[1].Summary, rows[1].Payload)
	}
	got := rows[0]
	if got.Summary != nfd || *got.SourcePath != r.SourcePath || *got.Body != body || *got.Locator != r.Locator {
		t.Error("text came back changed (normalized, trimmed or truncated)")
	}
	canon, err := records.CanonicalPayload(r.Payload)
	if err != nil || got.Payload != canon {
		t.Errorf("payload = %q, want %q (%v)", got.Payload, canon, err)
	}
}

func failBeforeInsert(w *records.Writer, err error) {
	w.SetHook(func(p string) error {
		if p == "before-insert" {
			return err
		}
		return nil
	})
}

func TestBatchFailurePoisonsWriter(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	boom := errors.New("disk on fire")
	failBeforeInsert(w, boom)
	recs := recordstest.Records(a.ID, 5, 1)
	if err := w.Add(ctx, recs[0]); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(ctx, recs[1]); !errors.Is(err, boom) {
		t.Fatalf("Add that flushed the failing batch = %v, want the hook error", err)
	}
	errs := auditOf(t, c, evidence.ActionBatchError)
	if len(errs) != 1 {
		t.Fatalf("%d records.batch.error entries", len(errs))
	}
	f := details[evidence.BatchFailure](t, errs[0])
	if f.IngestID != w.IngestID() || f.BatchNo != 1 || !strings.Contains(f.Error, "disk on fire") {
		t.Errorf("batch.error = %+v", f)
	}
	if n := count(t, c, "records"); n != 0 {
		t.Errorf("%d records written by a failed batch", n)
	}
	if n := count(t, c, "record_batches"); n != 0 {
		t.Errorf("%d record_batches rows written by a failed batch", n)
	}
	if err := w.Add(ctx, recs[2]); !errors.Is(err, boom) {
		t.Errorf("Add after the failure = %v, want the same error", err)
	}
	if err := w.Flush(ctx); !errors.Is(err, boom) {
		t.Errorf("Flush after the failure = %v", err)
	}
	if _, err := w.End(ctx); !errors.Is(err, boom) {
		t.Errorf("End after the failure = %v", err)
	}
	res, err := w.Abort(ctx, boom)
	if err != nil || res.Outcome != "incomplete" {
		t.Fatalf("Abort = %+v, %v", res, err)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs`); o != "incomplete" {
		t.Errorf("run outcome = %s", o)
	}
}

func TestFailedBatchIdsNotReused(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	failBeforeInsert(w, errors.New("fail"))
	add(t, w, recordstest.Records(a.ID, 1, 1))
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 2)[0]); err == nil {
		t.Fatal("the failing batch did not fail")
	}
	if _, err := w.Abort(ctx, errors.New("fail")); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, c, `SELECT value FROM records_meta WHERE key = 'next_id'`); v != "1" {
		t.Fatalf("next_id = %s after a failed batch, want it unchanged (1)", v)
	}
	p2 := records.Parser{Name: "other", Version: "1"}
	recordstest.Ingest(t, c, p2, []string{a.ID}, recordstest.Records(a.ID, 3, 3))
	if lo := scalar[int](t, c, `SELECT min(id) FROM records`); lo != 3 {
		t.Errorf("first id of the later ingest = %d, want 3 (above the failed batch's 1..2)", lo)
	}
}

func TestNextIDHighWaterTakesMaximum(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 5, 1)) // ids 1..5

	w := startWriter(t, c, records.Parser{Name: "failing", Version: "1"}, records.WriterOptions{BatchRows: 2}, a.ID)
	failBeforeInsert(w, errors.New("fail"))
	add(t, w, recordstest.Records(a.ID, 1, 1))
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 2)[0]); err == nil {
		t.Fatal("expected the batch to fail")
	}
	if _, err := w.Abort(ctx, errors.New("fail")); err != nil { // audited range 6..7, no rows
		t.Fatal(err)
	}

	recordstest.Ingest(t, c, records.Parser{Name: "third", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 9))
	if lo := scalar[int](t, c, `SELECT min(id) FROM records WHERE id > 5`); lo != 8 {
		t.Errorf("first id after a failed batch = %d, want 8 (above max(id)=5 and the audited range 6..7)", lo)
	}
	if got := scalar[string](t, c, `SELECT value FROM records_meta WHERE key = 'next_id'`); got != "10" {
		t.Errorf("next_id = %s, want 10", got)
	}
}

func TestEndWritesRunCoverageAndRollup(t *testing.T) {
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	w := newWriter(t, c, testParser, records.WriterOptions{BatchRows: 10})
	if err := w.Start(ctx, records.StartOptions{AnalysisID: "an-77", Artifacts: []string{b.ID, a.ID}}); err != nil {
		t.Fatal(err)
	}
	recs := append(recordstest.Records(a.ID, 17, 1), recordstest.Records(b.ID, 13, 2)...)
	add(t, w, recs)
	if err := w.Warn(ctx, "x/y", "odd"); err != nil {
		t.Fatal(err)
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.IngestID != w.IngestID() || res.Outcome != "complete" || res.Batches != 3 || res.Records != 30 ||
		res.FirstID != 1 || res.LastID != 30 || res.Warnings != 1 {
		t.Errorf("result = %+v", res)
	}
	var digests []string
	for _, e := range auditOf(t, c, evidence.ActionBatch) {
		digests = append(digests, details[evidence.BatchCommit](t, e).Digest)
	}
	if want := evidence.IngestRollup(digests); res.Rollup != want {
		t.Errorf("result rollup %s, want %s", res.Rollup, want)
	}
	ends := auditOf(t, c, evidence.ActionIngestEnd)
	if len(ends) != 1 {
		t.Fatalf("%d end entries", len(ends))
	}
	concl := details[evidence.IngestConclusion](t, ends[0])
	if concl.Outcome != "complete" || concl.Batches != 3 || concl.Records != 30 || concl.FirstID != 1 || concl.LastID != 30 || concl.Rollup != res.Rollup || concl.IngestID != w.IngestID() {
		t.Errorf("audited conclusion = %+v", concl)
	}
	if !reflect.DeepEqual(concl.Types, res.Types) || len(res.Types) == 0 {
		t.Errorf("types %v vs %v", concl.Types, res.Types)
	}
	var sum int64
	for _, n := range res.Types {
		sum += n
	}
	if sum != 30 {
		t.Errorf("types sum to %d", sum)
	}

	type run struct {
		EndSeq                      int64
		Ingest, Name, Version, Hash string
		Analysis, Outcome           string
		Batches, Records            int
		First, Last                 int64
		Rollup, Ended               string
	}
	var r run
	err = c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		return h.QueryRow(`SELECT r.end_seq, r.ingest_id, p.name, p.version, COALESCE(p.hash, ''), COALESCE(r.analysis_id, ''), r.outcome,
			r.batches, r.records, r.first_id, r.last_id, r.rollup, r.ended
			FROM record_runs r JOIN parsers p ON p.id = r.parser_id`).Scan(&r.EndSeq, &r.Ingest, &r.Name, &r.Version, &r.Hash, &r.Analysis, &r.Outcome,
			&r.Batches, &r.Records, &r.First, &r.Last, &r.Rollup, &r.Ended)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := run{
		EndSeq: ends[0].Seq, Ingest: w.IngestID(), Name: testParser.Name, Version: testParser.Version, Hash: testParser.Hash,
		Analysis: "an-77", Outcome: "complete", Batches: 3, Records: 30, First: 1, Last: 30, Rollup: res.Rollup, Ended: ends[0].Time,
	}
	if r != want {
		t.Errorf("run row = %+v\nwant     %+v", r, want)
	}
	cov := scalar[string](t, c, `SELECT group_concat(artifact_id, ',') FROM (SELECT artifact_id FROM record_run_artifacts ORDER BY artifact_id)`)
	ids := []string{a.ID, b.ID}
	slices.Sort(ids)
	if wantCov := strings.Join(ids, ","); cov != wantCov {
		t.Errorf("coverage = %s, want %s", cov, wantCov)
	}
	if n := count(t, c, "parsers"); n != 1 {
		t.Errorf("%d parsers rows", n)
	}
}

func TestAbortKeepsCommittedBatchesFlagged(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 10}, a.ID)
	add(t, w, recordstest.Records(a.ID, 25, 1)) // 2 flushed batches, 5 buffered
	cause := errors.New("parser gave up")
	res, err := w.Abort(ctx, cause)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "incomplete" || res.Batches != 2 || res.Records != 20 || res.FirstID != 1 || res.LastID != 20 {
		t.Errorf("result = %+v", res)
	}
	if n := count(t, c, "records"); n != 20 {
		t.Errorf("%d records remain, want the 20 of the flushed batches", n)
	}
	es := auditOf(t, c, evidence.ActionIngestError)
	if len(es) != 1 || len(auditOf(t, c, evidence.ActionIngestEnd)) != 0 {
		t.Fatalf("%d error entries, %d end entries", len(es), len(auditOf(t, c, evidence.ActionIngestEnd)))
	}
	cn := details[evidence.IngestConclusion](t, es[0])
	if cn.Outcome != "incomplete" || cn.Batches != 2 || cn.Records != 20 || cn.FirstID != 1 || cn.LastID != 20 || !strings.Contains(cn.Error, "parser gave up") || cn.Rollup != res.Rollup {
		t.Errorf("error entry = %+v", cn)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs`); o != "incomplete" {
		t.Errorf("run outcome = %s", o)
	}
	if seq := scalar[int64](t, c, `SELECT end_seq FROM record_runs`); seq != es[0].Seq {
		t.Errorf("end_seq = %d, want the error entry's %d", seq, es[0].Seq)
	}
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 1)[0]); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Add after Abort = %v", err)
	}
}

func TestAbortWithCancelledContextStillRecordsRun(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 5}, a.ID)
	add(t, w, recordstest.Records(a.ID, 7, 1))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res, err := w.Abort(cctx, cctx.Err())
	if err != nil || res.Outcome != "incomplete" {
		t.Fatalf("Abort with a cancelled context = %+v, %v", res, err)
	}
	if n := count(t, c, "record_runs"); n != 1 {
		t.Errorf("%d run rows", n)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestError)); n != 1 {
		t.Errorf("%d records.ingest.error entries", n)
	}
}

func TestParserIdentityConflict(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	starts := len(auditOf(t, c, evidence.ActionIngestStart))
	for name, p := range map[string]records.Parser{
		"other hash": {Name: testParser.Name, Version: testParser.Version, Hash: "different"},
		"no hash":    {Name: testParser.Name, Version: testParser.Version},
	} {
		w := newWriter(t, c, p, records.WriterOptions{})
		err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}, AllowReingest: true})
		if !errors.Is(err, records.ErrParserIdentityConflict) {
			t.Errorf("%s: Start = %v, want ErrParserIdentityConflict", name, err)
		}
		if got := len(auditOf(t, c, evidence.ActionIngestStart)); got != starts {
			t.Errorf("%s: %d records.ingest.start entries, want %d (none for the refused ingest)", name, got, starts)
		}
	}
	if n := count(t, c, "parsers"); n != 1 {
		t.Errorf("%d parsers rows", n)
	}
	// a NULL hash vs a given one is the same conflict the other way round
	p := records.Parser{Name: "nohash", Version: "1"}
	recordstest.Ingest(t, c, p, []string{a.ID}, nil)
	p.Hash = "now-has-one"
	if err := newWriter(t, c, p, records.WriterOptions{}).Start(ctx, records.StartOptions{Artifacts: []string{a.ID}, AllowReingest: true}); !errors.Is(err, records.ErrParserIdentityConflict) {
		t.Errorf("NULL vs given hash: %v", err)
	}
}

func TestAlreadyIngestedRefusedUnlessAllowed(t *testing.T) {
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 2, 1))

	if ok, err := records.AlreadyIngested(ctx, c, a.ID, testParser); err != nil || !ok {
		t.Fatalf("AlreadyIngested = %v, %v", ok, err)
	}
	if ok, _ := records.AlreadyIngested(ctx, c, b.ID, testParser); ok {
		t.Error("AlreadyIngested for an artifact never ingested")
	}
	newer := records.Parser{Name: testParser.Name, Version: "2.0", Hash: "zzz"}
	if ok, _ := records.AlreadyIngested(ctx, c, a.ID, newer); ok {
		t.Error("AlreadyIngested for a newer parser version")
	}

	w := newWriter(t, c, testParser, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID, b.ID}}); !errors.Is(err, records.ErrAlreadyIngested) {
		t.Fatalf("Start of the same parser+version = %v, want ErrAlreadyIngested", err)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestStart)); n != 1 {
		t.Errorf("%d start entries; the refused one must not be audited", n)
	}
	recordstest.Ingest(t, c, newer, []string{a.ID}, recordstest.Records(a.ID, 1, 2)) // newer version: fine

	w = newWriter(t, c, testParser, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}, AllowReingest: true}); err != nil {
		t.Fatalf("Start with AllowReingest = %v", err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	starts := auditOf(t, c, evidence.ActionIngestStart)
	if last := details[evidence.IngestStart](t, starts[len(starts)-1]); !last.Reingest {
		t.Error("the reingest start entry does not say reingest:true")
	}
	if first := details[evidence.IngestStart](t, starts[0]); first.Reingest {
		t.Error("the first ingest says reingest:true")
	}

	// an incomplete run does not count as ingested
	p3 := records.Parser{Name: "third", Version: "1"}
	w = startWriter(t, c, p3, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1))
	if _, err := w.Abort(ctx, errors.New("x")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := records.AlreadyIngested(ctx, c, a.ID, p3); ok {
		t.Error("an incomplete run counted as ingested")
	}
	w = newWriter(t, c, p3, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Errorf("Start after an incomplete run = %v", err)
	}
}

func TestSupersedeNewerCompleteRunHidesOlder(t *testing.T) {
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	old := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID, b.ID}, recordstest.Records(a.ID, 2, 1))
	if s := superseded(t, c); len(s) != 0 {
		t.Fatalf("superseded after the first run = %v", s)
	}
	newer := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "2"}, []string{a.ID}, recordstest.Records(a.ID, 2, 2))
	want := map[pair]bool{{old.IngestID, a.ID}: true}
	if got := superseded(t, c); !reflect.DeepEqual(got, want) {
		t.Errorf("superseded = %v, want %v ((old,B) stays visible: the second run covers only A)", got, want)
	}
	// a third run over both hides what is left of the first and the second
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "3"}, []string{a.ID, b.ID}, nil)
	want = map[pair]bool{{old.IngestID, a.ID}: true, {old.IngestID, b.ID}: true, {newer.IngestID, a.ID}: true}
	if got := superseded(t, c); !reflect.DeepEqual(got, want) {
		t.Errorf("superseded = %v, want %v", got, want)
	}
}

func TestIncompleteRunDoesNotSupersede(t *testing.T) {
	c, a := setup(t)
	old := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	w := startWriter(t, c, records.Parser{Name: "p", Version: "2"}, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 2))
	if _, err := w.Abort(ctx, errors.New("cut")); err != nil {
		t.Fatal(err)
	}
	if got := superseded(t, c); len(got) != 0 {
		t.Errorf("an incomplete run superseded %v", got)
	}
	// ... but a later complete run supersedes both the complete and the incomplete one
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "3"}, []string{a.ID}, nil)
	if n := len(superseded(t, c)); n != 2 {
		t.Errorf("%d superseded pairs after a complete run, want 2 (old %s and the incomplete one)", n, old.IngestID)
	}
}

func TestDifferentParserNameDoesNotSupersede(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	recordstest.Ingest(t, c, records.Parser{Name: "q", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 2))
	if got := superseded(t, c); len(got) != 0 {
		t.Errorf("a different parser name superseded %v", got)
	}
}

func TestWarnAppendsAnalysisWarning(t *testing.T) {
	c, a := setup(t)
	w := newWriter(t, c, testParser, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{AnalysisID: "an-1", Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Warn(ctx, "/data/x.db", "table t is damaged"); err != nil {
		t.Fatal(err)
	}
	ws := auditOf(t, c, evidence.ActionAnalysisWarning)
	if len(ws) != 1 {
		t.Fatalf("%d analysis.warning entries", len(ws))
	}
	// the examine shape plus the writer's own ingest_id (no source field, no rejected/suppression marker on a Warn)
	want := map[string]any{"analysis_id": "an-1", "path": "/data/x.db", "reason": "table t is damaged", "ingest_id": w.IngestID()}
	if !reflect.DeepEqual(ws[0].Details, want) {
		t.Errorf("details = %v, want exactly %v", ws[0].Details, want)
	}
	res, err := w.End(ctx)
	if err != nil || res.Warnings != 1 {
		t.Errorf("End = %+v, %v", res, err)
	}
}

func TestAddConcurrent(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	const goroutines, each = 8, 1000
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, r := range recordstest.Records(a.ID, each, int64(g)) {
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
	if res.Records != goroutines*each || res.FirstID != 1 || res.LastID != goroutines*each {
		t.Errorf("result = %+v", res)
	}
	if n, lo, hi := scalar[int](t, c, `SELECT count(*) FROM records`), scalar[int](t, c, `SELECT min(id) FROM records`), scalar[int](t, c, `SELECT max(id) FROM records`); n != goroutines*each || lo != 1 || hi != goroutines*each {
		t.Errorf("count %d ids %d..%d", n, lo, hi)
	}
	if n := scalar[int](t, c, `SELECT count(*) FROM records r JOIN record_batches b ON b.batch_id = r.batch_id WHERE r.id BETWEEN b.first_id AND b.first_id + b.count - 1`); n != goroutines*each {
		t.Errorf("%d records inside their batch's range", n)
	}
}

// TestStartAuditedBeforeParserRow: records.ingest.start is audited (and fsynced)
// before the parser row exists, and before anything else of the ingest.
func TestStartAuditedBeforeParserRow(t *testing.T) {
	c, a := setup(t)
	w := newWriter(t, c, testParser, records.WriterOptions{})
	called := false
	w.SetHook(func(point string) error {
		if point != "after-start-audit" {
			return nil
		}
		called = true
		if n := len(auditOf(t, c, evidence.ActionIngestStart)); n != 1 {
			t.Errorf("%d records.ingest.start entries at the hook, want 1", n)
		}
		if n := count(t, c, "parsers"); n != 0 {
			t.Errorf("the parsers table holds %d rows before the start entry's parser row is written", n)
		}
		return nil
	})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("the after-start-audit hook never ran")
	}
	if n := count(t, c, "parsers"); n != 1 {
		t.Errorf("%d parser rows after Start, want 1", n)
	}
}
