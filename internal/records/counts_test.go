package records_test

import (
	"errors"
	"fmt"
	"strconv"
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

// suppressionNotes returns the suppression notes among the analysis.warning entries.
func suppressionNotes(t *testing.T, c *evidence.Case) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range warnEntries(t, c) {
		if b, _ := e[evidence.WarnKeySuppression].(bool); b {
			out = append(out, e)
		}
	}
	return out
}

// noteCount reads one of the numbers a suppression note carries (-1 when the key is absent).
func noteCount(e map[string]any, key string) int {
	v, ok := e[key]
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(fmt.Sprint(v))
	if err != nil {
		return -2
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
	if res.Rejected != n+m || res.Warnings != n+m+k || res.WarningsSuppressed != 0 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want %d, %d, 0", res.Rejected, res.Warnings, res.WarningsSuppressed, n+m, n+m+k)
	}
	end := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestEnd)[0])
	if end.Rejected != n+m || end.Warnings != n+m+k || end.WarningsSuppressed != 0 {
		t.Errorf("end entry rejected %d warnings %d suppressed %d, want %d, %d, 0", end.Rejected, end.Warnings, end.WarningsSuppressed, n+m, n+m+k)
	}
	raw := auditOf(t, c, evidence.ActionIngestEnd)[0].Details
	for _, key := range []string{"warnings", "warnings_suppressed", "rejected"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the end entry has no %q key: %v", key, raw)
		}
	}
	marked := 0
	for _, e := range warnEntries(t, c) {
		if b, _ := e[evidence.WarnKeyRejected].(bool); b {
			marked++
		}
	}
	if got := len(warnEntries(t, c)); got != n+m+k || marked != n+m {
		t.Errorf("%d analysis.warning entries (%d marked rejected), want %d (%d marked): every refused Add and every Reject writes one", got, marked, n+m+k, n+m)
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
	// the refusal is audited by Add itself, once, and marked as a rejection
	ws := warnEntries(t, c)
	if len(ws) != 1 {
		t.Fatalf("the writer wrote %d warning entries for one refused Add, want 1", len(ws))
	}
	if b, _ := ws[0][evidence.WarnKeyRejected].(bool); !b || ws[0][evidence.WarnKeyIngest] != w.IngestID() {
		t.Errorf("entry %v is not a rejection of this ingest", ws[0])
	}
	if r, _ := ws[0]["reason"].(string); !strings.HasPrefix(r, "record refused: invalid record") {
		t.Errorf("reason = %q, want the refusal and its cause", r)
	}
	wantCounts(t, w, 1, 0, 1)
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 1 || res.Warnings != 1 || res.WarningsSuppressed != 0 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want 1, 1, 0", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
	}
}

// TestRefusedAddAtTheCapIsCountedAndNoted: a refused Add past the shared cap
// writes no entry of its own, is counted, and shows up in the one note.
func TestRefusedAddAtTheCapIsCountedAndNoted(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(1)
	for range 3 {
		if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrInvalidRecord) {
			t.Fatalf("Add = %v", err)
		}
	}
	if n := len(warnEntries(t, c)); n != 1 {
		t.Errorf("%d entries before End, want 1 (the cap)", n)
	}
	wantCounts(t, w, 1, 2, 3)
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 3 || res.Warnings != 1 || res.WarningsSuppressed != 2 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want 3, 1, 2", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
	notes := suppressionNotes(t, c)
	if len(notes) != 1 || noteCount(notes[0], evidence.WarnKeySuppressedWarnings) != 0 || noteCount(notes[0], evidence.WarnKeySuppressedRejects) != 2 {
		t.Fatalf("notes = %v, want one with 0 suppressed warnings and 2 suppressed rejections", notes)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
	}
}

// TestRefusedAddAuditFailureIsNotARejection: when the audit entry of a refusal
// cannot be written the Add fails with that error (not as a refusal the caller
// could skip) and nothing is counted.
func TestRefusedAddAuditFailureIsNotARejection(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	_ = c.Audit.Close() // every later append fails
	err := w.Add(ctx, refusedRecord(a.ID))
	if err == nil || errors.Is(err, records.ErrInvalidRecord) {
		t.Fatalf("Add = %v, want the audit failure and not a validation error", err)
	}
	wantCounts(t, w, 0, 0, 0)
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
	if res.Rejected != 5 || res.Warnings != 2 || res.WarningsSuppressed != 3 {
		t.Errorf("result = rejected %d warnings %d suppressed %d, want 5, 2, 3", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
	e := details[evidence.IngestConclusion](t, auditOf(t, c, evidence.ActionIngestError)[0])
	if e.Rejected != 5 || e.Warnings != 2 || e.WarningsSuppressed != 3 || e.Outcome != "incomplete" {
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
	// Warn and Reject draw on the same budget
	if err := w.Warn(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	wantCounts(t, w, 3, 8, 10)
	if n := len(warnEntries(t, c)); n != 3 {
		t.Errorf("%d analysis.warning entries before End, want the 3 under the cap (the note is written at the end)", n)
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 10 || res.Warnings != 3 || res.WarningsSuppressed != 8 {
		t.Errorf("result rejected %d warnings %d suppressed %d, want 10, 3, 8", res.Rejected, res.Warnings, res.WarningsSuppressed)
	}
	if n := suppressionEntries(t, c); n != 1 {
		t.Errorf("%d suppression entries, want exactly 1", n)
	}
	notes := suppressionNotes(t, c)
	if len(notes) != 1 || noteCount(notes[0], evidence.WarnKeySuppressedWarnings) != 1 || noteCount(notes[0], evidence.WarnKeySuppressedRejects) != 7 {
		t.Fatalf("notes = %v, want one carrying 1 suppressed warning and 7 suppressed rejections", notes)
	}
	// the note sits right before the conclusion that counts it
	all := auditOf(t, c, "")
	endSeq := auditOf(t, c, evidence.ActionIngestEnd)[0].Seq
	var noteSeq int64
	for _, e := range auditOf(t, c, evidence.ActionAnalysisWarning) {
		if b, _ := e.Details[evidence.WarnKeySuppression].(bool); b {
			noteSeq = e.Seq
		}
	}
	if noteSeq == 0 || noteSeq >= endSeq || all[len(all)-1].Seq != endSeq {
		t.Errorf("note seq %d, end seq %d: the note must come before records.ingest.end", noteSeq, endSeq)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
	}
}

// TestNoNoteWithoutSuppression: an ingest that stays within the cap writes no note.
func TestNoNoteWithoutSuppression(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(3)
	for range 3 {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(suppressionNotes(t, c)); n != 0 {
		t.Errorf("%d suppression notes, want none", n)
	}
}

// TestNoWarnOrRejectAfterTheNote: once the end-of-ingest note was written (by an
// End whose conclusion could not be audited), a later Warn, Reject or refused Add
// could not be counted by it, so each is refused and nothing is counted.
func TestNoWarnOrRejectAfterTheNote(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(1)
	for range 3 {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("stop")
	w.SetHook(func(p string) error {
		if p == "after-suppression-note" {
			return boom
		}
		return nil
	})
	if _, err := w.End(ctx); !errors.Is(err, boom) {
		t.Fatalf("End = %v, want the hook failure", err)
	}
	w.SetHook(nil)
	if err := w.Warn(ctx, "/p", "r"); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Warn after the note = %v, want ErrWriterClosed", err)
	}
	if err := w.Reject(ctx, "/p", "r"); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("Reject after the note = %v, want ErrWriterClosed", err)
	}
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrWriterClosed) {
		t.Errorf("refused Add after the note = %v, want ErrWriterClosed", err)
	}
	wantCounts(t, w, 1, 2, 0)
	// the conclusion (here an Abort) states exactly what the one note counted
	res, err := w.Abort(ctx, errors.New("cause"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Warnings != 1 || res.WarningsSuppressed != 2 || res.Rejected != 0 || len(suppressionNotes(t, c)) != 1 {
		t.Errorf("result %+v, notes %d: want warnings 1, suppressed 2, rejected 0 and the one note", res, len(suppressionNotes(t, c)))
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
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
		// past the cap a Reject writes nothing and is only counted; the note that
		// carries the count is written by End, and its failure fails the End
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		w.SetMaxWarnings(1)
		for range 2 {
			if err := w.Reject(ctx, "/p", "r"); err != nil {
				t.Fatal(err)
			}
		}
		wantCounts(t, w, 1, 1, 2)
		_ = c.Audit.Close()
		if _, err := w.End(ctx); err == nil {
			t.Fatal("End succeeded although the suppression note could not be written")
		}
		if n := len(auditOf(t, c, evidence.ActionIngestEnd)); n != 0 {
			t.Errorf("%d records.ingest.end entries after the failed note", n)
		}
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
			if _, err := w2.End(ctx); err != nil {
				t.Fatal(err)
			}
			if rep := mustVerify(t, c); !rep.OK() {
				t.Errorf("verify after the recovery: %q", rep.Problems)
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
	wantCounts(t, w, 2, 0, 2)

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
	wantCounts(t, w, 2, 0, 2)
	if err := w.Add(ctx, recordstest.Records(a.ID, 1, 3)[0]); !errors.Is(err, boom) {
		t.Fatalf("Add on a poisoned writer = %v", err)
	}
	if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, boom) {
		t.Fatalf("refused Add on a poisoned writer = %v, want the poison", err)
	}
	wantCounts(t, w, 2, 0, 2)
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
	wantCounts(t, w, 2, 0, 2)
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
	if orig.Rejected != 3 || orig.Warnings != 4 {
		t.Fatalf("end entry = %+v", orig)
	}
	startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	rcs := auditOf(t, c, evidence.ActionIngestRecover)
	if len(rcs) != 1 {
		t.Fatalf("%d recover entries", len(rcs))
	}
	rc := details[evidence.IngestRecover](t, rcs[0])
	if !rc.RunMissing || rc.IngestID != w.IngestID() || rc.Rejected != 3 || rc.Warnings != 4 || rc.WarningsSuppressed != 0 {
		t.Errorf("recover entry = %+v, want the end entry's counts (rejected 3, warnings 4)", rc)
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

// auditedWarnings runs an ingest that dies after one committed batch: nWarn Warn
// calls, nReject Reject calls and nRefused refused Adds (warnCap, when above 0, is
// the shared cap) and 2 records. With note the process dies right after the
// end-of-ingest suppression note was audited (the ingest must have suppressed
// something); without it the process dies before End. It returns the case, the
// artifact, the dead writer and the conclusion its counters give.
func auditedWarnings(t *testing.T, nWarn, nReject, nRefused, warnCap int, note bool) (*evidence.Case, evidence.ManifestRecord, *records.Writer, evidence.IngestConclusion) {
	t.Helper()
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	if warnCap > 0 {
		w.SetMaxWarnings(warnCap)
	}
	for range nWarn {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	for range nReject {
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	for range nRefused {
		if err := w.Add(ctx, refusedRecord(a.ID)); !errors.Is(err, records.ErrInvalidRecord) {
			t.Fatalf("Add = %v, want a validation error", err)
		}
	}
	add(t, w, recordstest.Records(a.ID, 2, 1)) // one batch of 2, committed
	if note {
		crashAt(w, "after-suppression-note", 1)
		if !survive(t, func() { _, _ = w.End(ctx) }) {
			t.Fatal("the crash seam after the suppression note never fired")
		}
	} else {
		w.Die()
	}
	warnings, suppressed, rejected := w.Counts()
	b := details[evidence.BatchCommit](t, auditOf(t, c, evidence.ActionBatch)[0])
	return c, a, w, evidence.IngestConclusion{
		IngestID: w.IngestID(), Outcome: "complete", Batches: 1, Records: 2, FirstID: 1, LastID: 2,
		Rollup: evidence.IngestRollup([]string{b.Digest}), Types: b.Types,
		Warnings: warnings, WarningsSuppressed: suppressed, Rejected: rejected,
	}
}

// TestVerifyIngestCountsMustMatchAuditLog: warnings, warnings_suppressed and
// rejected of records.ingest.end are proven exactly from the analysis.warning
// entries the writer put in the hash-chained log for that ingest: warnings equals
// the entries, warnings_suppressed the suppressed warnings plus suppressed
// rejections the one note carries, rejected the rejection entries plus the
// suppressed rejections. Each case re-chains an end entry by hand (the audit-log
// anchor limit) and the recover that follows copies it.
func TestVerifyIngestCountsMustMatchAuditLog(t *testing.T) {
	cases := []struct {
		name                          string
		nWarn, nReject, nRefused, cap int
		note                          bool
		mutate                        func(c *evidence.IngestConclusion)
		want                          string // "" means verify must be clean
	}{
		{"honest counts", 3, 2, 1, 0, false, func(*evidence.IngestConclusion) {}, ""},
		{"honest counts at the cap", 5, 0, 0, 2, true, func(*evidence.IngestConclusion) {}, ""},
		{"honest mixed counts at the cap", 2, 3, 2, 3, true, func(*evidence.IngestConclusion) {}, ""},
		{"warnings forged to zero", 3, 2, 1, 0, false, func(c *evidence.IngestConclusion) { c.Warnings = 0 }, "warnings is 0, the audit log holds 6"},
		{"warnings inflated", 3, 2, 1, 0, false, func(c *evidence.IngestConclusion) { c.Warnings = 9 }, "warnings is 9, the audit log holds 6"},
		{"rejected forged to zero", 3, 2, 1, 0, false, func(c *evidence.IngestConclusion) { c.Rejected = 0 }, "rejected is 0, the audit log proves 3 (3 rejection entries plus 0 suppressed rejections)"},
		{"rejected inflated", 3, 2, 1, 0, false, func(c *evidence.IngestConclusion) { c.Rejected = 7 }, "rejected is 7, the audit log proves 3 (3 rejection entries plus 0 suppressed rejections)"},
		{"rejected without the suppressed ones", 2, 3, 2, 3, true, func(c *evidence.IngestConclusion) { c.Rejected = 3 }, "rejected is 3, the audit log proves 5 (1 rejection entries plus 4 suppressed rejections)"},
		{"suppression invented", 3, 0, 0, 0, false, func(c *evidence.IngestConclusion) { c.WarningsSuppressed = 4 }, "warnings_suppressed is 4, the audit log proves 0 (0 suppressed warnings plus 0 suppressed rejections)"},
		{"suppression erased", 5, 0, 0, 2, true, func(c *evidence.IngestConclusion) { c.WarningsSuppressed = 0 }, "warnings_suppressed is 0, the audit log proves 3 (3 suppressed warnings plus 0 suppressed rejections)"},
		{"suppression understated by one", 5, 0, 0, 2, true, func(c *evidence.IngestConclusion) { c.WarningsSuppressed = 2 }, "warnings_suppressed is 2, the audit log proves 3 (3 suppressed warnings plus 0 suppressed rejections)"},
		{"warnings counted with the note", 5, 0, 0, 2, true, func(c *evidence.IngestConclusion) { c.Warnings = 3 }, "warnings is 3, the audit log holds 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, a, w, concl := auditedWarnings(t, tc.nWarn, tc.nReject, tc.nRefused, tc.cap, tc.note)
			tc.mutate(&concl)
			if _, err := c.Audit.Append(evidence.ActionIngestEnd, "", concl.Details()); err != nil {
				t.Fatal(err)
			}
			w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID) // recovers the run row
			if _, err := w2.End(ctx); err != nil {
				t.Fatal(err)
			}
			rep := mustVerify(t, c)
			if tc.want == "" {
				if !rep.OK() {
					t.Fatalf("honest counts: %q", rep.Problems)
				}
				return
			}
			expectProblems(t, rep, []string{tc.want, quoted(w.IngestID())})
		})
	}
}

// TestVerifyMalformedSuppressionNotes: a note must carry non-negative integer
// counts that are not both zero, and an ingest has at most one.
func TestVerifyMalformedSuppressionNotes(t *testing.T) {
	note := func(w *records.Writer, extra map[string]any) map[string]any {
		d := map[string]any{
			"analysis_id": "", "path": "", "reason": "forged note",
			evidence.WarnKeyIngest: w.IngestID(), evidence.WarnKeySuppression: true,
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	cases := []struct {
		name  string
		notes func(w *records.Writer) []map[string]any
		want  string
	}{
		{
			"counts missing", func(w *records.Writer) []map[string]any { return []map[string]any{note(w, nil)} },
			"a suppression note with missing, negative or non-integer counts",
		},
		{"count negative", func(w *records.Writer) []map[string]any {
			return []map[string]any{note(w, map[string]any{evidence.WarnKeySuppressedWarnings: -1, evidence.WarnKeySuppressedRejects: 3})}
		}, "a suppression note with missing, negative or non-integer counts"},
		{"count fractional", func(w *records.Writer) []map[string]any {
			return []map[string]any{note(w, map[string]any{evidence.WarnKeySuppressedWarnings: 1.5, evidence.WarnKeySuppressedRejects: 3})}
		}, "a suppression note with missing, negative or non-integer counts"},
		{"counts both zero", func(w *records.Writer) []map[string]any {
			return []map[string]any{note(w, map[string]any{evidence.WarnKeySuppressedWarnings: 0, evidence.WarnKeySuppressedRejects: 0})}
		}, "a suppression note that counts no suppressed entry"},
		{"two notes", func(w *records.Writer) []map[string]any {
			n := map[string]any{evidence.WarnKeySuppressedWarnings: 1, evidence.WarnKeySuppressedRejects: 0}
			return []map[string]any{note(w, n), note(w, n)}
		}, "the audit log holds 2 suppression notes for it, at most one is possible"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, a := setup(t)
			w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
			for _, d := range tc.notes(w) {
				if _, err := c.Audit.Append(evidence.ActionAnalysisWarning, "", d); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.End(ctx); err != nil {
				t.Fatal(err)
			}
			rep := mustVerify(t, c)
			expectProblems(t, rep, []string{tc.want, quoted(w.IngestID())}, "warnings is 0, the audit log holds", "warnings_suppressed is 0, the audit log proves")
		})
	}
}

// TestVerifyForgedUnfinishedRecoverCounts: a recover of an unfinished ingest
// (no end entry) carries counts that the audit log must confirm too.
func TestVerifyForgedUnfinishedRecoverCounts(t *testing.T) {
	c, _, w, concl := auditedWarnings(t, 3, 1, 0, 0, false)
	forged := concl
	forged.Outcome, forged.Warnings, forged.Rejected = "interrupted", 99, 0
	rc := evidence.IngestRecover{IngestConclusion: forged, ByIngestID: "ing-forged", Reason: "forged for the test", BatchNos: []int{}}
	if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", rc.Details()); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"warnings is 99, the audit log holds 4", "rejected is 0, the audit log proves 1 (1 rejection entries plus 0 suppressed rejections)", quoted(w.IngestID())}, "run row")
}

// TestRecoveryDerivesCountsFromAuditLog: recovering an ingest that never
// concluded (or whose Abort failed) states the warnings, suppressed entries and
// rejections the log proves, never zeros. The suppressed numbers come from the
// end-of-ingest note, so an ingest that died before writing it loses them.
func TestRecoveryDerivesCountsFromAuditLog(t *testing.T) {
	check := func(t *testing.T, c *evidence.Case, a evidence.ManifestRecord, id string, warnings, suppressed, rejected int) {
		t.Helper()
		w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		rcs := auditOf(t, c, evidence.ActionIngestRecover)
		if len(rcs) != 1 {
			t.Fatalf("%d recover entries", len(rcs))
		}
		rc := details[evidence.IngestRecover](t, rcs[0])
		if rc.IngestID != id || rc.Outcome != "interrupted" || rc.Warnings != warnings || rc.WarningsSuppressed != suppressed || rc.Rejected != rejected {
			t.Errorf("recover entry warnings %d suppressed %d rejected %d (%s), want %d, %d, %d", rc.Warnings, rc.WarningsSuppressed, rc.Rejected, rc.Outcome, warnings, suppressed, rejected)
		}
		if _, err := w2.End(ctx); err != nil {
			t.Fatal(err)
		}
		if rep := mustVerify(t, c); !rep.OK() {
			t.Errorf("verify: %q", rep.Problems)
		}
	}
	t.Run("process died", func(t *testing.T) {
		c, a, w, _ := auditedWarnings(t, 3, 2, 1, 0, false)
		check(t, c, a, w.IngestID(), 6, 0, 3)
	})
	t.Run("process died after the note", func(t *testing.T) {
		// 4 warns, 3 rejects, 2 refused Adds under a cap of 2: the entries are the first
		// two warns; the note carries 2 suppressed warnings and 5 suppressed rejections
		c, a, w, _ := auditedWarnings(t, 4, 3, 2, 2, true)
		check(t, c, a, w.IngestID(), 2, 7, 5)
	})
	t.Run("process died at the cap before the note", func(t *testing.T) {
		c, a, w, _ := auditedWarnings(t, 4, 3, 2, 2, false)
		check(t, c, a, w.IngestID(), 2, 0, 0) // nothing past the cap is provable without the note
	})
	t.Run("abort failed", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		for range 2 {
			if err := w.Warn(ctx, "/p", "r"); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Reject(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
		w.SetHook(func(p string) error {
			if p == "before-abort-audit" {
				return errors.New("disk full")
			}
			return nil
		})
		if _, err := w.Abort(ctx, errors.New("cause")); err == nil {
			t.Fatal("Abort succeeded")
		}
		check(t, c, a, w.IngestID(), 3, 0, 1)
	})
}

// TestWriterWarningEntriesAreMarked: the entries verify counts carry keys only
// the writer sets; a parser's text cannot pose as a suppression note or a
// rejection.
func TestWriterWarningEntriesAreMarked(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(3)
	if err := w.Warn(ctx, "", "further warnings suppressed (the cap is 1 per ingest)"); err != nil { // mimics the note
		t.Fatal(err)
	}
	if err := w.Reject(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = w.Warn(ctx, "/p", "r")
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	ws := warnEntries(t, c)
	if len(ws) != 4 { // warn, reject, warn, then the note written by End
		t.Fatalf("%d warning entries", len(ws))
	}
	for i, e := range ws {
		if e[evidence.WarnKeyIngest] != w.IngestID() {
			t.Errorf("entry %d does not name its ingest: %v", i, e)
		}
		_, rejected := e[evidence.WarnKeyRejected]
		_, note := e[evidence.WarnKeySuppression]
		if rejected != (i == 1) || note != (i == 3) {
			t.Errorf("entry %d markers rejected=%v suppression=%v: %v", i, rejected, note, e)
		}
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Errorf("verify: %q", rep.Problems)
	}
}
