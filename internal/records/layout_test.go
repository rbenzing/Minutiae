package records_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// appendFuture appends a well-chained records entry whose details claim layout 2 (a later build wrote it).
func appendFuture(t *testing.T, c *evidence.Case, action string, d map[string]any) evidence.AuditEntry {
	t.Helper()
	d["layout"] = json.Number("2")
	e, err := c.Audit.Append(action, "", d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func futureStart(id string, artifacts ...string) map[string]any {
	return evidence.IngestStart{IngestID: id, Parser: "future", ParserVersion: "9", AnalysisID: "an", Artifacts: artifacts, BatchRows: 10}.Details()
}

func futureBatch(id string) map[string]any {
	return evidence.BatchCommit{
		IngestID: id, BatchNo: 1, FirstID: 1000, Count: 1, Digest: "d", Created: "2026-01-01T00:00:00Z",
		Artifacts: map[string]string{}, ArtifactIncomplete: []string{}, Types: map[string]int64{},
	}.Details()
}

func futureRecover(id string) map[string]any {
	return evidence.IngestRecover{
		IngestConclusion: evidence.IngestConclusion{IngestID: id, Outcome: "interrupted", Types: map[string]int64{}},
		ByIngestID:       "ing-other", Reason: "x", BatchNos: []int{1},
	}.Details()
}

func requireNoGrowth(t *testing.T, c *evidence.Case, before int) {
	t.Helper()
	if n := len(auditOf(t, c, "")); n != before {
		t.Errorf("the audit log grew from %d to %d entries", before, n)
	}
}

// TestWriterStartRefusesANewerRecordsLayout: an unresolved ingest whose start entry a later build wrote
// is never recovered or written past: Start fails closed and appends nothing.
func TestWriterStartRefusesANewerRecordsLayout(t *testing.T) {
	c, a := setup(t)
	appendFuture(t, c, evidence.ActionIngestStart, futureStart("ing-future", a.ID))
	before := len(auditOf(t, c, ""))
	w := newWriter(t, c, testParser, records.WriterOptions{})
	err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
	if !errors.Is(err, evidence.ErrAuditLayoutNewer) {
		t.Fatalf("Start = %v, want ErrAuditLayoutNewer", err)
	}
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("Start = %v does not keep the integrity class (exit 4)", err)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)) + len(auditOf(t, c, evidence.ActionIngestStart)); n != 1 {
		t.Errorf("%d recover+start entries, want only the planted start", n)
	}
	requireNoGrowth(t, c, before)
}

// TestNewerLayoutKeepsItsClassThroughEveryCaller (K30): each decoding path of the records.* entries
// returns the typed, integrity-class error for a layout-2 entry; none turns it into not-found, empty or a
// plain error.
func TestNewerLayoutKeepsItsClassThroughEveryCaller(t *testing.T) {
	requireTyped := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, evidence.ErrAuditLayoutNewer) {
			t.Fatalf("err = %v, want ErrAuditLayoutNewer", err)
		}
		if !errors.Is(err, evidence.ErrIntegrity) {
			t.Errorf("err = %v lost the integrity class", err)
		}
		if errors.Is(err, records.ErrNotFound) {
			t.Errorf("err = %v became not-found", err)
		}
	}
	t.Run("UnresolvedIngests (start)", func(t *testing.T) {
		c, a := setup(t)
		appendFuture(t, c, evidence.ActionIngestStart, futureStart("ing-future", a.ID))
		un, err := c.UnresolvedIngests()
		requireTyped(t, err)
		if len(un) != 0 {
			t.Errorf("listed %d unresolved ingests next to an error", len(un))
		}
	})
	t.Run("UnresolvedIngests (batch, failure, conclusion, recover)", func(t *testing.T) {
		for name, mk := range map[string]struct {
			action string
			d      map[string]any
		}{
			"batch":      {evidence.ActionBatch, futureBatch("ing-future")},
			"failure":    {evidence.ActionBatchError, evidence.BatchFailure{IngestID: "ing-future", BatchNo: 1, Error: "x"}.Details()},
			"conclusion": {evidence.ActionIngestEnd, evidence.IngestConclusion{IngestID: "ing-future", Outcome: "complete", Types: map[string]int64{}}.Details()},
			"recover":    {evidence.ActionIngestRecover, futureRecover("ing-future")},
		} {
			t.Run(name, func(t *testing.T) {
				c, a := setup(t)
				w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
				_ = w
				appendFuture(t, c, mk.action, mk.d)
				_, err := c.UnresolvedIngests()
				requireTyped(t, err)
			})
		}
	})
	t.Run("IngestGaps (recover lookup)", func(t *testing.T) {
		c, a := setup(t)
		appendFuture(t, c, evidence.ActionIngestRecover, futureRecover("ing-future"))
		_ = a
		_, err := c.IngestGaps()
		requireTyped(t, err)
	})
	t.Run("Writer.Start", func(t *testing.T) {
		c, a := setup(t)
		appendFuture(t, c, evidence.ActionIngestStart, futureStart("ing-future", a.ID))
		err := newWriter(t, c, testParser, records.WriterOptions{}).Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
		requireTyped(t, err)
	})
	t.Run("ingest preflight (batch entry)", func(t *testing.T) {
		c, a := setup(t)
		appendFuture(t, c, evidence.ActionBatch, futureBatch("ing-future"))
		before := len(auditOf(t, c, ""))
		err := newWriter(t, c, testParser, records.WriterOptions{}).Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
		requireTyped(t, err)
		requireNoGrowth(t, c, before)
	})
	t.Run("records show", func(t *testing.T) {
		c, a := setup(t)
		recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
		appendFuture(t, c, evidence.ActionBatch, futureBatch("ing-future"))
		r, err := records.NewReader(c)
		if err != nil {
			t.Fatal(err)
		}
		full, err := r.Get(ctx, 1)
		requireTyped(t, err)
		if full.ID != 0 {
			t.Errorf("a row came back with the error: %+v", full)
		}
	})
}
