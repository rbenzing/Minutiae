package records_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func warnEntries(t *testing.T, c *evidence.Case) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range auditOf(t, c, evidence.ActionAnalysisWarning) {
		out = append(out, e.Details)
	}
	return out
}

// TestWarnClipsAndCleansText: path and reason are bounded (4096 and 1024 bytes,
// cut on a rune boundary, marked), and invalid UTF-8 and NUL are replaced.
func TestWarnClipsAndCleansText(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	huge := strings.Repeat("é", 3000) // 6000 bytes of two-byte runes
	if err := w.Warn(ctx, huge, huge); err != nil {
		t.Fatal(err)
	}
	if err := w.Warn(ctx, "a\x00b\xffc", "x\x00y\xc3"); err != nil {
		t.Fatal(err)
	}
	ws := warnEntries(t, c)
	if len(ws) != 2 {
		t.Fatalf("%d warnings", len(ws))
	}
	path, reason := ws[0]["path"].(string), ws[0]["reason"].(string)
	if len(path) > 4096+3 || len(path) < 4000 || !strings.HasSuffix(path, "...") || !utf8.ValidString(path) {
		t.Errorf("path: %d bytes, valid utf8 %v", len(path), utf8.ValidString(path))
	}
	if len(reason) > 1024+3 || len(reason) < 1000 || !strings.HasSuffix(reason, "...") || !utf8.ValidString(reason) {
		t.Errorf("reason: %d bytes, valid utf8 %v", len(reason), utf8.ValidString(reason))
	}
	p2, r2 := ws[1]["path"].(string), ws[1]["reason"].(string)
	if p2 != "a?b?c" || r2 != "x?y?" {
		t.Errorf("cleaned to %q and %q, want %q and %q", p2, r2, "a?b?c", "x?y?")
	}
}

// TestWarnIsCappedPerIngest: after the cap one "further warnings suppressed"
// entry is written and the rest are dropped (and counted).
func TestWarnIsCappedPerIngest(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	if records.MaxWarnings != 10000 {
		t.Fatalf("MaxWarnings = %d, want 10000", records.MaxWarnings)
	}
	w.SetMaxWarnings(5)
	for i := 0; i < 12; i++ {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatalf("warning %d: %v", i, err)
		}
	}
	ws := warnEntries(t, c)
	if len(ws) != 6 {
		t.Fatalf("%d analysis.warning entries, want 5 plus one suppression note", len(ws))
	}
	if r, _ := ws[5]["reason"].(string); !strings.Contains(r, "further warnings suppressed") {
		t.Errorf("last entry = %v, want the suppression note", ws[5])
	}
	for _, e := range ws[:5] {
		if e["reason"] != "r" {
			t.Errorf("entry %v", e)
		}
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warnings != 5 || res.WarningsSuppressed != 7 {
		t.Errorf("result warnings %d suppressed %d, want 5 and 7", res.Warnings, res.WarningsSuppressed)
	}
}

// TestEndWithCancelledContextStillConcludes: a context cancelled after the end
// entry was audited cannot leave the run unrecorded.
func TestEndWithCancelledContextStillConcludes(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 3, 1))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.SetHook(func(p string) error {
		if p == "after-end-audit" {
			cancel()
		}
		return nil
	})
	res, err := w.End(cctx)
	if err != nil || res.Outcome != "complete" {
		t.Fatalf("End = %+v, %v", res, err)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs WHERE ingest_id = ?`, w.IngestID()); o != "complete" {
		t.Errorf("run outcome %q", o)
	}
	if un, err := c.UnresolvedIngests(); err != nil || len(un) != 0 {
		t.Errorf("unresolved = %v, %v", un, err)
	}
}

// TestAbortStaysOpenWhenItsAuditFails: Abort closes the writer only after
// records.ingest.error is on disk, so a failed append can be retried.
func TestAbortStaysOpenWhenItsAuditFails(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1))
	boom := errors.New("disk full")
	w.SetHook(func(p string) error {
		if p == "before-abort-audit" {
			return boom
		}
		return nil
	})
	if _, err := w.Abort(ctx, errors.New("cause")); !errors.Is(err, boom) {
		t.Fatalf("Abort = %v, want the audit failure", err)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestError)); n != 0 {
		t.Fatalf("%d records.ingest.error entries after the failed Abort", n)
	}
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 2)[0]); errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("the writer was closed by a failed Abort: %v", err)
	}
	w.SetHook(nil)
	res, err := w.Abort(ctx, errors.New("cause"))
	if err != nil || res.Outcome != "incomplete" {
		t.Fatalf("retried Abort = %+v, %v", res, err)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs WHERE ingest_id = ?`, w.IngestID()); o != "incomplete" {
		t.Errorf("run outcome %q", o)
	}
}

// TestIngestIDIsSetOnlyByASuccessfulStart: a Start that fails leaves IngestID
// empty (an id that was never audited).
func TestIngestIDIsSetOnlyByASuccessfulStart(t *testing.T) {
	c, a := setup(t)
	first := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	if _, err := first.End(ctx); err != nil {
		t.Fatal(err)
	}
	w := newWriter(t, c, records.Parser{Name: testParser.Name, Version: testParser.Version, Hash: "another"}, records.WriterOptions{})
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); !errors.Is(err, records.ErrParserIdentityConflict) {
		t.Fatalf("Start = %v", err)
	}
	if id := w.IngestID(); id != "" {
		t.Errorf("IngestID after a failed Start = %q", id)
	}
}
