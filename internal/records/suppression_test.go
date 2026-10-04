package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// beganNotes returns the "suppression began" notes among the analysis.warning entries.
func beganNotes(t *testing.T, c *evidence.Case) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range warnEntries(t, c) {
		if b, _ := e[evidence.WarnKeySuppressionBegan].(bool); b {
			out = append(out, e)
		}
	}
	return out
}

// TestSuppressionBeganNoteWrittenWhenCapFirstHit: the moment the shared cap is first
// hit (by Warn, Reject or a refused Add) the writer appends one marker note, before
// the first entry is suppressed. It carries no numbers, is written once, and counts
// as neither a warning nor a rejection; the conclusion note still carries the totals.
func TestSuppressionBeganNoteWrittenWhenCapFirstHit(t *testing.T) {
	hitters := map[string]func(w *records.Writer, artifactID string) error{
		"warn":   func(w *records.Writer, _ string) error { return w.Warn(ctx, "/p", "r") },
		"reject": func(w *records.Writer, _ string) error { return w.Reject(ctx, "/p", "r") },
		"refused add": func(w *records.Writer, artifactID string) error {
			if err := w.Add(ctx, refusedRecord(artifactID)); !errors.Is(err, records.ErrInvalidRecord) {
				return errors.New("Add = " + err.Error())
			}
			return nil
		},
	}
	for name, hit := range hitters {
		t.Run(name, func(t *testing.T) {
			c, a := setup(t)
			w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
			w.SetMaxWarnings(2)
			for range 2 {
				if err := w.Warn(ctx, "/p", "r"); err != nil {
					t.Fatal(err)
				}
			}
			if n := len(beganNotes(t, c)); n != 0 {
				t.Fatalf("%d began notes at the cap (nothing suppressed yet), want 0", n)
			}
			if err := hit(w, a.ID); err != nil {
				t.Fatal(err)
			}
			ws := warnEntries(t, c)
			if len(ws) != 3 {
				t.Fatalf("%d entries after the first suppressed one, want 2 + the began note", len(ws))
			}
			note := ws[2]
			if b, _ := note[evidence.WarnKeySuppressionBegan].(bool); !b || note[evidence.WarnKeyIngest] != w.IngestID() {
				t.Fatalf("entry %v is not the began note of this ingest", note)
			}
			for _, k := range []string{evidence.WarnKeySuppression, evidence.WarnKeyRejected, evidence.WarnKeySuppressedWarnings, evidence.WarnKeySuppressedRejects} {
				if _, ok := note[k]; ok {
					t.Errorf("the began note carries %q: it must be a marker only: %v", k, note)
				}
			}
			for range 4 {
				_ = hit(w, a.ID)
			}
			if n := len(beganNotes(t, c)); n != 1 {
				t.Errorf("%d began notes after more suppression, want exactly 1", n)
			}
			wantCounts(t, w, 2, 5, 0+rejectsOf(name, 5))
			res, err := w.End(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.Warnings != 2 || res.WarningsSuppressed != 5 {
				t.Errorf("result warnings %d suppressed %d, want 2 and 5 (the began note is not a warning)", res.Warnings, res.WarningsSuppressed)
			}
			if n := len(suppressionNotes(t, c)); n != 1 {
				t.Errorf("%d conclusion notes, want 1: the totals are still written at conclusion", n)
			}
			if rep := mustVerify(t, c); !rep.OK() {
				t.Errorf("verify: %q", rep.Problems)
			}
		})
	}
}

// rejectsOf is how many of n suppressed calls of the named hitter are rejections.
func rejectsOf(name string, n int) int {
	if name == "warn" {
		return 0
	}
	return n
}

// TestSuppressionBeganAuditFailureCountsNothing: when the began note cannot be
// audited the call fails with that error and nothing is counted (audit before
// count), so the next call tries the note again.
func TestSuppressionBeganAuditFailureCountsNothing(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(1)
	if err := w.Warn(ctx, "/p", "r"); err != nil {
		t.Fatal(err)
	}
	_ = c.Audit.Close() // every later append fails
	if err := w.Warn(ctx, "/p", "r"); err == nil {
		t.Fatal("Warn at the cap succeeded although the began note could not be audited")
	}
	if err := w.Reject(ctx, "/p", "r"); err == nil {
		t.Fatal("Reject at the cap succeeded although the began note could not be audited")
	}
	wantCounts(t, w, 1, 0, 0)
}

// TestRecoveryOfUnconcludedSuppressionIsUnknown: an ingest that began suppressing
// and died before its conclusion note has suppressed counts nobody can prove; the
// recovery says so with an explicit flag (never a silent 0), and verify accepts it.
// An ingest that never reached the cap, or one whose conclusion note exists, is not
// flagged: its numbers are exact.
func TestRecoveryOfUnconcludedSuppressionIsUnknown(t *testing.T) {
	recoverAndCheck := func(t *testing.T, nWarn, nReject, nRefused, warnCap int, note bool, wantUnknown bool, warnings, suppressed, rejected int) {
		t.Helper()
		c, a, w, _ := auditedWarnings(t, nWarn, nReject, nRefused, warnCap, note)
		w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		rcs := auditOf(t, c, evidence.ActionIngestRecover)
		if len(rcs) != 1 {
			t.Fatalf("%d recover entries", len(rcs))
		}
		rc := details[evidence.IngestRecover](t, rcs[0])
		if rc.IngestID != w.IngestID() || rc.SuppressionUnknown != wantUnknown ||
			rc.Warnings != warnings || rc.WarningsSuppressed != suppressed || rc.Rejected != rejected {
			t.Errorf("recover entry unknown=%v warnings %d suppressed %d rejected %d, want %v %d %d %d",
				rc.SuppressionUnknown, rc.Warnings, rc.WarningsSuppressed, rc.Rejected, wantUnknown, warnings, suppressed, rejected)
		}
		if _, err := w2.End(ctx); err != nil {
			t.Fatal(err)
		}
		if rep := mustVerify(t, c); !rep.OK() {
			t.Errorf("verify: %q", rep.Problems)
		}
	}
	t.Run("died after the cap, before the conclusion note", func(t *testing.T) {
		// 4 warns, 3 rejects, 2 refused Adds under a cap of 2: two entries, the began note, then suppression
		recoverAndCheck(t, 4, 3, 2, 2, false, true, 2, 0, 0)
	})
	t.Run("died with a rejection entry and suppression", func(t *testing.T) {
		recoverAndCheck(t, 1, 3, 0, 2, false, true, 2, 0, 1) // the rejection entry that fit is proven, the rest unknown
	})
	t.Run("died after the conclusion note", func(t *testing.T) {
		recoverAndCheck(t, 4, 3, 2, 2, true, false, 2, 7, 5)
	})
	t.Run("never reached the cap", func(t *testing.T) {
		recoverAndCheck(t, 3, 2, 1, 0, false, false, 6, 0, 3)
	})
	t.Run("died right after the began note was audited", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		w.SetMaxWarnings(2)
		for range 2 {
			if err := w.Warn(ctx, "/p", "r"); err != nil {
				t.Fatal(err)
			}
		}
		crashAt(w, "after-suppression-began", 1)
		if !survive(t, func() { _ = w.Warn(ctx, "/p", "r") }) {
			t.Fatal("the crash seam after the began note never fired")
		}
		w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		rc := details[evidence.IngestRecover](t, auditOf(t, c, evidence.ActionIngestRecover)[0])
		if !rc.SuppressionUnknown || rc.Warnings != 2 || rc.WarningsSuppressed != 0 || rc.Rejected != 0 {
			t.Errorf("recover entry = %+v, want unknown with 2 warnings", rc)
		}
		if _, err := w2.End(ctx); err != nil {
			t.Fatal(err)
		}
		if rep := mustVerify(t, c); !rep.OK() {
			t.Errorf("verify: %q", rep.Problems)
		}
	})
	t.Run("an aborted ingest whose abort failed", func(t *testing.T) {
		c, a := setup(t)
		w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
		w.SetMaxWarnings(1)
		for range 3 {
			if err := w.Warn(ctx, "/p", "r"); err != nil {
				t.Fatal(err)
			}
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
		_ = startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		rc := details[evidence.IngestRecover](t, auditOf(t, c, evidence.ActionIngestRecover)[0])
		if !rc.SuppressionUnknown || rc.Warnings != 1 || rc.WarningsSuppressed != 0 {
			t.Errorf("recover entry = %+v, want unknown", rc)
		}
	})
}

// TestVerifyUnknownSuppressionOnlyForRecoveredIngests: suppression_unknown is the
// recovery's statement about an ingest that began suppressing and never concluded.
// Verify refuses it on an end entry, on a recover with no began note, and on a
// recover whose conclusion note proves the numbers; and it refuses a recover of an
// ingest that began suppressing but states the numbers as known.
func TestVerifyUnknownSuppressionOnlyForRecoveredIngests(t *testing.T) {
	type tc struct {
		name                          string
		nWarn, nReject, nRefused, cap int
		note                          bool
		end                           bool // forge a conclusion entry (and let the next Start copy it) instead of a recover
		mutate                        func(c *evidence.IngestConclusion)
		want                          string // "" means verify must be clean
	}
	// flag marks the conclusion as unknown; honest also zeroes the placeholders the way recovery writes them
	flag := func(c *evidence.IngestConclusion) { c.SuppressionUnknown = true }
	honest := func(rejectEntries int) func(c *evidence.IngestConclusion) {
		return func(c *evidence.IngestConclusion) {
			c.SuppressionUnknown, c.WarningsSuppressed, c.Rejected = true, 0, rejectEntries
		}
	}
	const (
		onlyRecovery = "suppression_unknown is set, but only the recovery of an ingest that never concluded may state it"
		noBegan      = "suppression_unknown is set, but the audit log holds no suppression began note for it"
		hasNote      = "suppression_unknown is set, but the audit log holds the suppression note that proves the numbers"
		mustBeSet    = "the audit log holds a suppression began note but no conclusion note, so suppression_unknown must be set"
	)
	cases := []tc{
		{"honest unknown recover", 4, 0, 0, 2, false, false, honest(0), ""},
		{"honest unknown recover with a rejection entry", 1, 3, 0, 2, false, false, honest(1), ""},
		{"unknown on an end entry that proves its numbers", 4, 0, 0, 2, true, true, flag, onlyRecovery},
		{"unknown on an end entry without suppression", 2, 0, 0, 0, false, true, flag, onlyRecovery},
		{"unknown recover without a began note", 3, 1, 0, 0, false, false, flag, noBegan},
		{"unknown recover although the conclusion note proves the numbers", 4, 0, 0, 2, true, false, flag, hasNote},
		{"began but stated as known", 4, 0, 0, 2, false, false, func(*evidence.IngestConclusion) {}, mustBeSet},
		{
			"unknown with a suppressed count invented", 4, 0, 0, 2, false, false, func(c *evidence.IngestConclusion) { honest(0)(c); c.WarningsSuppressed = 9 },
			"warnings_suppressed is 9, a recovered ingest of unknown suppression states 0",
		},
		{
			"unknown with rejected above the entries", 1, 3, 0, 2, false, false, func(c *evidence.IngestConclusion) { honest(1)(c); c.Rejected = 9 },
			"rejected is 9, a recovered ingest of unknown suppression states the 1 rejection entries",
		},
		{
			"unknown with warnings forged", 4, 0, 0, 2, false, false, func(c *evidence.IngestConclusion) { honest(0)(c); c.Warnings = 7 },
			"warnings is 7, the audit log holds 2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, a, w, concl := auditedWarnings(t, tc.nWarn, tc.nReject, tc.nRefused, tc.cap, tc.note)
			if tc.end {
				tc.mutate(&concl)
				if _, err := c.Audit.Append(evidence.ActionIngestEnd, "", concl.Details()); err != nil {
					t.Fatal(err)
				}
				w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
				if _, err := w2.End(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				// the honest recovery, then verify the recover entry; the forged variants replace it by a hand-chained one
				// written before any real recovery can happen
				concl.Outcome = "interrupted"
				concl.SuppressionUnknown = false
				tc.mutate(&concl)
				concl.IngestID = w.IngestID()
				rc := evidence.IngestRecover{IngestConclusion: concl, ByIngestID: "ing-forged", Reason: "forged for the test", BatchNos: []int{}}
				if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", rc.Details()); err != nil {
					t.Fatal(err)
				}
			}
			rep := mustVerify(t, c)
			if tc.want == "" {
				if !rep.OK() {
					for _, p := range rep.Problems {
						if !strings.Contains(p, "run row missing") { // the forged recover is hand-chained: no real recovery wrote its run row
							t.Errorf("honest unknown recover: %s", p)
						}
					}
				}
				return
			}
			expectProblems(t, rep, []string{tc.want, quoted(w.IngestID())}, "run row")
		})
	}
}

// TestVerifyFlagsASecondBeganNote: an ingest begins suppressing once.
func TestVerifyFlagsASecondBeganNote(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	w.SetMaxWarnings(1)
	for range 3 {
		if err := w.Warn(ctx, "/p", "r"); err != nil {
			t.Fatal(err)
		}
	}
	forged := map[string]any{
		"analysis_id": "", "path": "", "reason": "forged", evidence.WarnKeyIngest: w.IngestID(), evidence.WarnKeySuppressionBegan: true,
	}
	if _, err := c.Audit.Append(evidence.ActionAnalysisWarning, "", forged); err != nil {
		t.Fatal(err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"the audit log holds 2 suppression began notes for it, at most one is possible", quoted(w.IngestID())}, "the audit log holds a suppression note but no suppression began note")
}

// TestVerifyConclusionNoteNeedsBeganNote: the conclusion note is always preceded
// by the began note; one without it means an entry was erased or forged.
func TestVerifyConclusionNoteNeedsBeganNote(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	forged := map[string]any{
		"analysis_id": "", "path": "", "reason": "forged", evidence.WarnKeyIngest: w.IngestID(), evidence.WarnKeySuppression: true,
		evidence.WarnKeySuppressedWarnings: 2, evidence.WarnKeySuppressedRejects: 0,
	}
	if _, err := c.Audit.Append(evidence.ActionAnalysisWarning, "", forged); err != nil {
		t.Fatal(err)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !strings.Contains(strings.Join(rep.Problems, "\n"), "the audit log holds a suppression note but no suppression began note") {
		t.Errorf("a conclusion note with no began note was accepted: %q", rep.Problems)
	}
}
