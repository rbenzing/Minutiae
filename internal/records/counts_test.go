package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// refusedRecord is a record that validation refuses (ErrUnknownArtifact).
func refusedRecord(artifactID string) records.Record {
	r := recordstest.Records(artifactID, 1, 1)[0]
	r.ArtifactID = "no-such-artifact"
	return r
}

func suppressionEntries(t *testing.T, c *evidence.Case) int {
	t.Helper()
	n := 0
	for _, e := range warnEntries(t, c) {
		if r, _ := e["reason"].(string); strings.Contains(r, "further warnings suppressed") {
			n++
		}
	}
	return n
}

func wantCounts(t *testing.T, w *records.Writer, warnings, suppressed, rejected int) {
	t.Helper()
	gw, gs, gr := w.Counts()
	if gw != warnings || gs != suppressed || gr != rejected {
		t.Errorf("counters warnings/suppressed/rejected = %d/%d/%d, want %d/%d/%d", gw, gs, gr, warnings, suppressed, rejected)
	}
}

func TestIngestEndCarriesWarningsAndRejected(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	const n, m, k = 3, 2, 4
	for range n {
		if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrInvalidRecord) {
			t.Fatalf("Add = %v, want a validation error", err)
		}
	}
	for range m {
		if err := w.Reject(ctx, "/p", "refused by the caller"); err != nil {
			t.Fatal(err)
		}
	}
	for range k {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	add(t, w, recordstest.Records(a.ID, 2, 1))
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != n+m || res.Warnings != k+m || res.WarningsSuppressed != 0 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want %d, %d, 0", res.Rejected, res.Warnings, res.WarningsSuppressed, n+m, k+m)
	}
	end := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
	if end.Rejected != n+m || end.Warnings != k+m || end.WarningsSuppressed != 0 {
		t.Errorf("end entry rejected %d warnings %d suppressed %d, want %d, %d, 0", end.Rejected, end.Warnings, end.WarningsSuppressed, n+m, k+m)
	}
	raw := auditOf(t, c, evidence.ActionIngestEnd)[0].Details
	for _, key := range []string{"warnings", "warnings_suppressed", "rejected"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the end entry has no %q key: %v", key, raw)
		}
	}
	if got := len(warnEntries(t, c)); got != k+m {
		t.Errorf("%d analysis.warning entries, want %d (the Rejects write one each, the refused Adds none)", got, k+m)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
	}
}

func TestSingleRejectedAddCountsOnce(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrInvalidRecord) {
		t.Fatalf("Add = %v", err)
	}
	if n := len(warnEntries(t, c)); n != 0 {
		t.Fatalf("the writer wrote %d warning entries for a refused Add itself", n)
	}
	wantCounts(t, w, 0, 0, 1)
	// the emitter's path: the caller warns about the refusal it just saw
	if err := w.Warn(ctx, "/p", "record refused"); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, w, 1, 0, 1)
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 1 || res.Warnings != 1 {
		t.Errorf("result rejected %d warnings %d, want 1 and 1", res.Rejected, res.Warnings)
	}
}

func TestIngestErrorEntryCarriesCounts(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(2)
	if err := w.Add(ctx, refusedRecord(a.ID)); err == nil {
		t.Fatal("Add accepted a refused record")
	}
	for range 4 {
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := w.Abort(ctx, errors.New("cause"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 5 || res.Warnings != 2 || res.WarningsSuppressed != 2 {
		t.Errorf("result = rejected %d warnings %d suppressed %d, want 5, 2, 2", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
	e := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestError)[0])
	if e.Rejected != 5 || e.Warnings != 2 || e.WarningsSuppressed != 2 || e.Outcome != "incomplete" {
		t.Errorf("error entry = %+v", e)
	}
}

func TestWarningsSuppressedCountedInEnd(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(3)
	for range 10 {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	e := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
	if e.Warnings != 3 || e.WarningsSuppressed != 7 || e.Rejected != 0 {
		t.Errorf("end entry warnings %d suppressed %d rejected %d, want 3, 7, 0", e.Warnings, e.WarningsSuppressed, e.Rejected)
	}
}

func TestRejectSharesWarnCapAndCountsWhenSuppressed(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(3)
	for i := range 10 {
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatalf("Reject %d: %v", i, err)
		}
	}
	wantCounts(t, w, 3, 7, 10)
	if n := suppressionEntries(t, c); n != 1 {
		t.Errorf("%d suppression entries, want exactly 1", n)
	}
	if n := len(warnEntries(t, c)); n != 4 {
		t.Errorf("%d analysis.warning entries, want 3 plus the suppression note", n)
	}
	// Warn and Reject draw on the same budget
	if err := w.Warn(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, w, 3, 8, 10)
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 10 || res.Warnings != 3 || res.WarningsSuppressed != 8 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want 10, 3, 8", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
}

func TestRejectAuditsBeforeCounting(t *testing.T) {
	t.Run("individual warning", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
		_ = c.Audit.Close() // every later append fails
		if err := w.Reject(ctx, "/p", "r"); err == nil {
			t.Fatal("Reject succeeded although its audit entry could not be written")
		}
		wantCounts(t, w, 1, 0, 1)
	})
	t.Run("suppression note", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		w.SetMaxWarnings(1)
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
		_ = c.Audit.Close()
		if err := w.Reject(ctx, "/p", "r"); err == nil {
			t.Fatal("Reject succeeded although the suppression entry could not be written")
		}
		wantCounts(t, w, 1, 0, 1)
	})
}

func TestFailedAbortReleasesLiveIngestSlot(t *testing.T) {
	boom := errors.New("disk full")
	for name, fail := range map[string]func(c *evidence.Case, w *records.Writer){
		"before-abort hook": func(_ *evidence.Case, w *records.Writer) {
			w.SetHook(func(p string) error {
				if p == "before-abort-audit" {
					return boom
				}
				return nil
			})
		},
		"audit append": func(c *evidence.Case, _ *records.Writer) { _ = c.Audit.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			c, a := setup(t)
			w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
			add(t, w, recordstest.Records(a.ID, 2, 1))
			if c.LiveIngest() == "" {
				t.Fatal("no live ingest before the Abort")
			}
			fail(c, w)
			if _, err := w.Abort(ctx, errors.New("cause")); err == nil {
				t.Fatal("Abort succeeded")
			} else if name == "before-abort hook" && !errors.Is(err, boom) {
				t.Fatalf("Abort = %v, want the hook failure", err)
			}
			if live := c.LiveIngest(); live != "" {
				t.Errorf("the failed Abort left the live-ingest slot taken by %q", live)
			}
			if err := w.Add(ctx, recordstest.Records(a.ID, 1, 2)[0]); !errors.Is(err, records.ErrWriterClosed) {
				t.Errorf("Add after a failed Abort = %v, want ErrWriterClosed", err)
			}
			if _, err := w.Abort(ctx, nil); !errors.Is(err, records.ErrWriterClosed) {
				t.Errorf("a second Abort = %v, want ErrWriterClosed", err)
			}
			if n := len(auditOf(t, c, evidence.ActionIngestError)); n != 0 {
				t.Errorf("%d records.ingest.error entries after the failed Abort", n)
			}
			if name != "before-abort hook" {
				return // the audit log is closed: nothing can Start
			}
			w2 := newWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{})
			if err := w2.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
				t.Fatalf("Start after a failed Abort = %v, want it to succeed (not ErrIngestActive)", err)
			}
			recs := auditOf(t, c, evidence.ActionIngestRecover)
			if len(recs) != 1 || details[evidence.IngestRecover](t, recs[0]).IngestID != w.IngestID() {
				t.Fatalf("recover entries = %v, want one for the dead ingest %s", recs, w.IngestID())
			}
		})
	}
}

func TestRejectedCountsOnlyInvalidRecordErrors(t *testing.T) {
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib) // in the manifest, never declared
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 1}, a.ID)

	if err := w.Add(ctx, recordstest.Records(b.ID, 1, 1)[0]); !errors.Is(err, records.ErrUndeclaredArtifact) {
		t.Fatalf("undeclared Add = %v", err)
	}
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrUnknownArtifact) {
		t.Fatalf("unknown Add = %v", err)
	}
	wantCounts(t, w, 0, 0, 2)

	// a batch failure poisons the writer: its errors are not rejections
	boom := errors.New("boom")
	w.SetHook(func(p string) error {
		if p == "after-batch-audit" {
			return boom
		}
		return nil
	})
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 2)[0]); !errors.Is(err, boom) {
		t.Fatalf("Add = %v, want the batch failure", err)
	}
	wantCounts(t, w, 0, 0, 2)
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 3)[0]); !errors.Is(err, boom) {
		t.Fatalf("Add on a poisoned writer = %v", err)
	}
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, boom) {
		t.Fatalf("refused Add on a poisoned writer = %v, want the poison", err)
	}
	wantCounts(t, w, 0, 0, 2)
	res, err := w.Abort(ctx, boom)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 2 {
		t.Errorf("result rejected %d, want 2", res.Rejected)
	}

	// a closed writer is not a rejection either
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrWriterClosed) {
		t.Fatalf("Add on a closed writer = %v", err)
	}
	wantCounts(t, w, 0, 0, 2)
}

func TestRejectRequiresStartedWriter(t *testing.T) {
	c, a := setup(t)
	w := newWriter(t, c, testParser, records.WriterOptions{})
	if err := w.Reject(ctx, "/p", "r"); !errors.Is(err, records.ErrWriterNotStarted) {
		t.Errorf("Reject before Start = %v", err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Reject(ctx, "/p", "r"); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Reject after End = %v", err)
	}
	wantCounts(t, w, 0, 0, 0)
	if n := len(warnEntries(t, c)); n != 0 {
		t.Errorf("%d warning entries written by refused Rejects", n)
	}
}

// countedIngestCrashedAfterEnd runs an ingest with 2 refused Adds, 1 Reject and
// 1 Warn that dies right after records.ingest.end was audited.
func countedIngestCrashedAfterEnd(t *testing.T, c *evidence.Case, a evidence.ManifestRecord) *records.Writer {
	t.Helper()
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1))
	for range 2 {
		_ = w.Add(ctx, refusedRecord(a.ID))
	}
	if err := w.Reject(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	if err := w.Warn(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	crashAt(w, "after-end-audit", 1)
	if !survive(t, func() { _, _ = w.End(ctx) }) {
		t.Fatal("the crash seam never fired")
	}
	return w
}

func TestRecoverCopiesCountsOfRunMissingIngest(t *testing.T) {
	c, a := setup(t)
	w := countedIngestCrashedAfterEnd(t, c, a)
	orig := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
	if orig.Rejected != 3 || orig.Warnings != 2 {
		t.Fatalf("end entry = %+v", orig)
	}
	startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	rcs := auditOf(t, c, evidence.ActionIngestRecover)
	if len(rcs) != 1 {
		t.Fatalf("%d recover entries", len(rcs))
	}
	rc := details[evidence.IngestRecover](t, rcs[0])
	if !rc.RunMissing || rc.IngestID != w.IngestID() || rc.Rejected != 3 || rc.Warnings != 2 || rc.WarningsSuppressed != 0 {
		t.Errorf("recover entry = %+v, want the end entry's counts (rejected 3, warnings 2)", rc)
	}
}

func TestVerifyRecoverMustMatchConclusionCounts(t *testing.T) {
	t.Run("a hand-edited count breaks the chain", func(t *testing.T) {
		c, a := setup(t)
		countedIngestCrashedAfterEnd(t, c, a)
		w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		if _, err := w2.End(ctx); err != nil {
			t.Fatal(err)
		}
		if rep := mustVerify(t, c); !rep.OK() {
			t.Fatalf("not clean before the edit: %q", rep.Problems)
		}
		rc := auditOf(t, c, evidence.ActionIngestRecover)[0]
		recordstest.ReplaceInAuditLine(t, c.Dir, int(rc.Seq), `"rejected":3`, `"rejected":0`)
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() || !strings.Contains(strings.Join(rep.Problems, "\n"), "hash mismatch") {
			t.Errorf("the edited recover entry was not caught: %q", rep.Problems)
		}
	})
	t.Run("a consistent forged copy is a lifecycle problem", func(t *testing.T) {
		c, a := setup(t)
		w := countedIngestCrashedAfterEnd(t, c, a)
		end := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
		forged := end
		forged.Rejected = 0
		rc := evidence.IngestRecover{
			IngestConclusion: forged, ByIngestID: "ing-forged", Reason: "forged for the test", RunMissing: true, BatchNos: []int{},
		}
		if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", rc.Details()); err != nil {
			t.Fatal(err)
		}
		rep := mustVerify(t, c)
		expectProblems(t, rep, []string{"differs from the conclusion it copies"}, "run row")
		if !strings.Contains(strings.Join(rep.Problems, "\n"), quoted(w.IngestID())) {
			t.Errorf("the problem does not name the ingest: %q", rep.Problems)
		}
	})
}

func TestOlderEndEntryWithoutCountsDecodesAndVerifies(t *testing.T) {
	c, a := setup(t)
	w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1) // one committed batch of 2, never concluded
	b := details[evidence.BatchCommit](t, auditOf(t, c, evidence.ActionBatch)[0])
	old := evidence.IngestConclusion{
		IngestID: w.IngestID(), Outcome: "complete", Batches: 1, Records: 2, FirstID: 1, LastID: 2,
		Rollup: evidence.IngestRollup([]string{b.Digest}), Types: b.Types,
	}.Details()
	for _, key := range []string{"warnings", "warnings_suppressed", "rejected"} {
		delete(old, key) // an entry written before these fields existed
	}
	if _, err := c.Audit.Append(evidence.ActionIngestEnd, "", old); err != nil {
		t.Fatal(err)
	}
	got := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
	if got.Warnings != 0 || got.WarningsSuppressed != 0 || got.Rejected != 0 {
		t.Errorf("absent keys decoded to %+v, want zeros", got)
	}
	// the next Start finishes the missing run row, copying the conclusion
	w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("verify: %q", rep.Problems)
	}
}
