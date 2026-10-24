package evidence_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// unfinishedIngest builds a real unfinished ingest of three stored batches (one record
// each): the writer is neither ended nor aborted, the case is closed and reopened.
func unfinishedIngest(t *testing.T) (*evidence.Case, string, evidence.ManifestRecord) {
	t.Helper()
	ctx := context.Background()
	c := recordstest.NewCase(t)
	dir := c.Dir
	art := recordstest.AddArtifact(t, c, "a.db", make([]byte, recordstest.ArtifactSize))
	w, err := records.NewWriter(c, records.Parser{Name: "xcheck", Version: "1"}, records.WriterOptions{BatchRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recordstest.Records(art.ID, 3, 7) {
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	id := w.IngestID()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	return c2, id, art
}

func appendAudit(t *testing.T, c *evidence.Case, action string, d map[string]any) evidence.AuditEntry {
	t.Helper()
	e, err := c.Audit.Append(action, "", d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func announce(t *testing.T, c *evidence.Case, art evidence.ManifestRecord, ingest string, no int) evidence.AuditEntry {
	t.Helper()
	return appendAudit(t, c, evidence.ActionBatch, evidence.BatchCommit{
		IngestID: ingest, BatchNo: no, FirstID: 1000 + int64(no), Count: 1, Digest: "d", Created: "2026-01-01T00:00:00Z",
		Artifacts: map[string]string{art.ID: art.SHA256}, ArtifactIncomplete: []string{}, Types: map[string]int64{"note": 1},
	}.Details())
}

func craftRecover(t *testing.T, c *evidence.Case, id, by string, nos []int) evidence.AuditEntry {
	t.Helper()
	return appendAudit(t, c, evidence.ActionIngestRecover, evidence.IngestRecover{
		IngestConclusion: evidence.IngestConclusion{IngestID: id, Outcome: "interrupted", Batches: 3, Records: 3, FirstID: 1, LastID: 3, Types: map[string]int64{}},
		ByIngestID:       by, Reason: "crafted", BatchNos: nos,
	}.Details())
}

func TestVerifyRecoverCrossCheck(t *testing.T) {
	rows := []struct {
		name string
		run  func(t *testing.T, c *evidence.Case, id string, art evidence.ManifestRecord) string
	}{
		{"twice", func(t *testing.T, c *evidence.Case, id string, art evidence.ManifestRecord) string {
			announce(t, c, art, id, 4)
			e := craftRecover(t, c, id, "other", []int{4, 4})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names batch 4 more than once", e.Seq)
		}},
		{"never announced", func(t *testing.T, c *evidence.Case, id string, _ evidence.ManifestRecord) string {
			e := craftRecover(t, c, id, "other", []int{9})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names batch 9, which was never announced (no records.batch entry)", e.Seq)
		}},
		{"row stored", func(t *testing.T, c *evidence.Case, id string, _ evidence.ManifestRecord) string {
			e := craftRecover(t, c, id, "other", []int{2})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names batch 2 as never written, but its row is stored", e.Seq)
		}},
		{"errored", func(t *testing.T, c *evidence.Case, id string, art evidence.ManifestRecord) string {
			announce(t, c, art, id, 4)
			appendAudit(t, c, evidence.ActionBatchError, evidence.BatchFailure{IngestID: id, BatchNo: 4, Error: "disk"}.Details())
			e := craftRecover(t, c, id, "other", []int{4})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names batch 4, which records.batch.error already explains", e.Seq)
		}},
		{"announced after", func(t *testing.T, c *evidence.Case, id string, art evidence.ManifestRecord) string {
			e := craftRecover(t, c, id, "other", []int{4})
			b := announce(t, c, art, id, 4)
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names batch 4, announced at audit seq %d, after the recover entry", e.Seq, b.Seq)
		}},
		{"empty by", func(t *testing.T, c *evidence.Case, id string, _ evidence.ManifestRecord) string {
			e := craftRecover(t, c, id, "", []int{})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names no recovering ingest (by_ingest_id is empty)", e.Seq)
		}},
		{"by itself", func(t *testing.T, c *evidence.Case, id string, _ evidence.ManifestRecord) string {
			e := craftRecover(t, c, id, id, []int{})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names the recovered ingest itself as the recovering one", e.Seq)
		}},
		{"by started before", func(t *testing.T, c *evidence.Case, id string, art evidence.ManifestRecord) string {
			s := appendAudit(t, c, evidence.ActionIngestStart, evidence.IngestStart{
				IngestID: "early", Parser: "p", ParserVersion: "1", Artifacts: []string{art.ID}, BatchRows: 1,
			}.Details())
			e := craftRecover(t, c, id, "early", []int{})
			return fmt.Sprintf("records.ingest.recover (audit seq %d) names recovering ingest \"early\", whose records.ingest.start (audit seq %d) comes before it", e.Seq, s.Seq)
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c, id, art := unfinishedIngest(t)
			want := r.run(t, c, id, art)
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			evidencetest.RequireProblem(t, rep, fmt.Sprintf("ingest %q: ", id)+want)
		})
	}
}

// A real recovery whose recovering Start is then refused (the same parser and version already
// covers the artifact, no reingest allowed) leaves a recover entry naming an ingest that never
// started: the notice, and nothing else.
func TestVerifyRecoverByUnstartedIngestIsANotice(t *testing.T) {
	ctx := context.Background()
	c, _, art := unfinishedIngest(t)
	// a complete run of parser next 1 (it also recovers the first unfinished ingest), then a second
	// unfinished ingest of the same parser: the next Start recovers it and is refused (already ingested)
	w1, err := records.NewWriter(c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w1.End(ctx); err != nil {
		t.Fatal(err)
	}
	w2, err := records.NewWriter(c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{BatchRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}, AllowReingest: true}); err != nil {
		t.Fatal(err)
	}
	id := w2.IngestID()
	for _, r := range recordstest.Records(art.ID, 1, 9) {
		if err := w2.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w, err := records.NewWriter(c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}}); err == nil {
		t.Fatal("the reingest was not refused")
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %q", rep.Problems)
	}
	evidencetest.RequireNotice(t, rep, fmt.Sprintf("ingest %q was recovered by ingest ", id))
	evidencetest.RequireNotice(t, rep, ", which has no records.ingest.start (its start failed after it recovered, or this entry was not written by the writer)")
}

// PASSES ON ARRIVAL: the rules are new but a real recovery satisfies all of them.
func TestVerifyRealRecoveryIsClean(t *testing.T) {
	ctx := context.Background()
	c, id, art := unfinishedIngest(t)
	announce(t, c, art, id, 4) // announced, never written: the writer names it
	w, err := records.NewWriter(c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %q", rep.Problems)
	}
}
