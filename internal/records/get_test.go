package records_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestGetReturnsPayloadTimesAndProvenance(t *testing.T) {
	c, art := setup(t)
	payload := map[string]any{"b": []any{"x", int64(2), nil}, "a": map[string]any{"z": true, "é": "ü"}, "n": int64(-7)}
	start := time.Date(2024, 3, 5, 10, 30, 0, 123_456_000, time.UTC)
	rec := func(i int) records.Record {
		return records.Record{
			Type: "message", ArtifactID: art.ID, SourcePath: "/data/sms.db", Locator: "sqlite:table=sms;row=4",
			Range:   &records.Range{Offset: 10, Length: 20},
			Time:    &records.Time{T: start, Basis: records.BasisLocalOffset, OffsetMin: 90},
			TimeEnd: &records.Time{T: start.Add(time.Minute), Basis: records.BasisLocalOffset, OffsetMin: 90},
			Times: []records.NamedTime{
				{Kind: "sent", Time: records.Time{T: start.Add(-time.Hour), Basis: records.BasisLocalUnknown}},
				{Kind: "created", Time: records.Time{T: start.Add(-2 * time.Hour)}},
			},
			Deleted: true, Recovery: "carve", Confidence: ptr(77), Summary: "hello " + string(rune('a'+i)),
			Body: "line1\nline2 é", Payload: payload,
		}
	}
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, art.ID)
	for i := 0; i < 5; i++ {
		if err := w.Add(ctx, rec(i)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 3 {
		t.Fatalf("%d batches, want 3 (2+2+1 rows)", res.Batches)
	}
	r := newReader(t, c)
	id := res.FirstID + 4 // the only row of batch 3
	full, err := r.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wantPayload, err := records.CanonicalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(full.Payload) != wantPayload {
		t.Fatalf("payload %s\nwant    %s", full.Payload, wantPayload)
	}
	if full.ID != id || full.Type != "message" || full.Summary != "hello e" || full.Body != "line1\nline2 é" ||
		full.SourcePath != "/data/sms.db" || full.Locator != "sqlite:table=sms;row=4" ||
		full.Range == nil || *full.Range != (records.Range{Offset: 10, Length: 20}) ||
		!full.Deleted || !full.Recovered || full.Method != "carve" || full.Confidence == nil || *full.Confidence != 77 ||
		full.Parser != (records.ParserInfo{Name: "sms-parser", Version: "1.0", Hash: "abc123"}) || full.Superseded || full.SupersededBy != "" {
		t.Fatalf("full = %+v", full.Row)
	}
	if full.TS == nil || !full.TS.T.Equal(start) || full.TS.Basis != records.BasisLocalOffset || full.TS.OffsetMin != 90 ||
		full.TSEnd == nil || !full.TSEnd.T.Equal(start.Add(time.Minute)) || full.TSEnd.OffsetMin != 90 {
		t.Fatalf("ts %+v end %+v", full.TS, full.TSEnd)
	}
	// secondary times, by kind
	if len(full.Times) != 2 || full.Times[0].Kind != "created" || full.Times[1].Kind != "sent" ||
		full.Times[0].TS != start.Add(-2*time.Hour).UnixMicro() || full.Times[0].Basis != "utc" || full.Times[0].TZOffsetMin != nil ||
		full.Times[1].TS != start.Add(-time.Hour).UnixMicro() || full.Times[1].Basis != "local-unknown" {
		t.Fatalf("times = %+v", full.Times)
	}
	// the artifact, from the manifest
	if full.Artifact.ID != art.ID || full.Artifact.SHA256 != art.SHA256 || full.Artifact.Size != art.Size || full.ArtifactIncomplete {
		t.Fatalf("artifact %+v incomplete=%v", full.Artifact, full.ArtifactIncomplete)
	}
	// the batch and its audit entry
	var batchEntry evidence.AuditEntry
	for _, e := range auditOf(t, c, "records.batch") {
		if d := details[evidence.BatchCommit](t, e); d.IngestID == res.IngestID && d.BatchNo == 3 {
			batchEntry = e
		}
	}
	if batchEntry.Seq == 0 {
		t.Fatal("no audit entry for batch 3")
	}
	bd := details[evidence.BatchCommit](t, batchEntry)
	if full.Batch.IngestID != res.IngestID || full.Batch.BatchNo != 3 || full.Batch.Count != 1 || full.Batch.FirstID != id ||
		full.Batch.Digest != bd.Digest || full.Batch.AuditSeq != batchEntry.Seq || full.Batch.Created != bd.Created {
		t.Fatalf("batch %+v, audit %+v seq %d", full.Batch, bd, batchEntry.Seq)
	}
	// the run and its concluding audit entry
	ends := auditOf(t, c, "records.ingest.end")
	if len(ends) != 1 || full.Run == nil {
		t.Fatalf("%d end entries, run %+v", len(ends), full.Run)
	}
	run := *full.Run
	if run.IngestID != res.IngestID || run.Outcome != "complete" || run.AuditSeq != ends[0].Seq || run.ID != ends[0].Seq ||
		run.Parser != "sms-parser" || run.ParserVersion != "1.0" || run.ParserHash != "abc123" ||
		run.Batches != 3 || run.Records != 5 || run.FirstID != res.FirstID || run.LastID != res.LastID {
		t.Fatalf("run %+v, end entry seq %d", run, ends[0].Seq)
	}
	// the first row sits in batch 1
	first, err := r.Get(ctx, res.FirstID)
	if err != nil || first.Batch.BatchNo != 1 || first.Batch.Count != 2 {
		t.Fatalf("first: %+v, %v", first.Batch, err)
	}
	// a record without optional parts
	recordstest.Ingest(t, c, records.Parser{Name: "bare", Version: "1"}, []string{art.ID}, []records.Record{{Type: "note", ArtifactID: art.ID, Payload: map[string]any{}}})
	bareID := res.LastID + 1
	bare, err := r.Get(ctx, bareID)
	if err != nil {
		t.Fatal(err)
	}
	if string(bare.Payload) != "{}" || bare.Body != "" || len(bare.Times) != 0 || bare.TS != nil || bare.Range != nil || bare.Confidence != nil ||
		bare.Parser.Hash != "" || bare.SourcePath != "" || bare.Locator != "" {
		t.Fatalf("bare = %+v payload %s", bare, bare.Payload)
	}
}

func TestGetUnknownIDIsErrNotFound(t *testing.T) {
	_, r := bigDataset(t, 10)
	for _, id := range []int64{0, -1, 11 + 1000, math.MaxInt64, math.MinInt64} {
		if _, err := r.Get(ctx, id); !errors.Is(err, records.ErrNotFound) {
			t.Errorf("Get(%d) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestGetFlagsIncompleteArtifact(t *testing.T) {
	c := recordstest.NewCase(t)
	good := recordstest.AddArtifact(t, c, "good.db", mib)
	bad := recordstest.AddIncompleteArtifact(t, c, "bad.db", mib)
	res := recordstest.Ingest(t, c, testParser, []string{good.ID, bad.ID}, []records.Record{
		{Type: "note", ArtifactID: good.ID, Payload: map[string]any{}},
		{Type: "note", ArtifactID: bad.ID, Payload: map[string]any{}},
	})
	r := newReader(t, c)
	for i, want := range []bool{false, true} {
		full, err := r.Get(ctx, res.FirstID+int64(i))
		if err != nil || full.ArtifactIncomplete != want || full.Artifact.Incomplete != want {
			t.Fatalf("record %d: incomplete=%v artifact.incomplete=%v, %v; want %v", i, full.ArtifactIncomplete, full.Artifact.Incomplete, err, want)
		}
	}
}

func TestGetShowsSupersededBy(t *testing.T) {
	c, art := setup(t)
	parser := func(v string) records.Parser { return records.Parser{Name: "sms", Version: v} }
	one := func(label string) []records.Record {
		return []records.Record{{Type: "note", ArtifactID: art.ID, Summary: label, Payload: map[string]any{}}}
	}
	run1 := recordstest.Ingest(t, c, parser("1"), []string{art.ID}, one("run1"))
	w := startWriter(t, c, parser("2"), records.WriterOptions{}, art.ID)
	add(t, w, one("run2"))
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	run2, err := w.Abort(ctx, errors.New("crashed")) // incomplete: never supersedes
	if err != nil {
		t.Fatal(err)
	}
	run3 := recordstest.Ingest(t, c, parser("3"), []string{art.ID}, one("run3"))
	run4 := recordstest.Ingest(t, c, parser("4"), []string{art.ID}, one("run4"))
	// another parser name on the same artifact supersedes nothing
	other := recordstest.Ingest(t, c, records.Parser{Name: "other", Version: "9"}, []string{art.ID}, one("other"))
	r := newReader(t, c)
	for _, tc := range []struct {
		name       string
		run        records.IngestResult
		by         string
		superseded bool
	}{
		{"first", run1, run3.IngestID, true}, // the lowest later complete run, not the incomplete run2
		{"incomplete", run2, run3.IngestID, true},
		{"third", run3, run4.IngestID, true},
		{"newest", run4, "", false},
		{"other parser", other, "", false},
	} {
		full, err := r.Get(ctx, tc.run.FirstID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if full.SupersededBy != tc.by || full.Superseded != tc.superseded {
			t.Errorf("%s: SupersededBy %q superseded=%v, want %q %v", tc.name, full.SupersededBy, full.Superseded, tc.by, tc.superseded)
		}
	}
	// a coverage that does not overlap is not superseded
	art2 := recordstest.AddArtifact(t, c, "b.db", mib)
	lone := recordstest.Ingest(t, c, parser("0"), []string{art2.ID}, []records.Record{{Type: "note", ArtifactID: art2.ID, Payload: map[string]any{}}})
	full, err := r.Get(ctx, lone.FirstID)
	if err != nil || full.SupersededBy != "" || full.Superseded {
		t.Fatalf("lone: %+v, %v", full.SupersededBy, err)
	}
}
