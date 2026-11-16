package evidence_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// The journal classes take either the scope "unallocated" (F2FS roll-forward copies, which lie in free space) or the
// scope "journal" (ext4 journal copies and reports, which lie in the filesystem's own allocated journal): 3G-R15.

func journalBlockRecord(scope string, a evidence.AllocSummary) evidence.ManifestRecord {
	return evidence.ManifestRecord{ID: "a1", Path: "artifacts/d/a/journal/p1-ext4/seq1-fsblk1.bin", Size: a.Free + a.Allocated + a.Unknown, Source: evidence.Source{Kind: evidence.KindJournal, Derived: &evidence.Derivation{
		Recovery: &evidence.Recovery{
			Class: evidence.ClassJournalBlock, Method: "ext4-journal", Scope: scope, Alloc: a, Algorithm: evidence.AlgorithmRecover,
			Journal: &evidence.JournalRef{Seq: 1, Region: "live"},
		},
	}}}
}

func TestJournalClassesAllowScopeJournal(t *testing.T) {
	rec := journalBlockRecord("journal", evidence.AllocSummary{Allocated: 1024})
	if got := rec.Source.Derived.Recovery.Check(evidence.KindJournal); len(got) != 0 {
		t.Errorf("Check: %v, want none", got)
	}
	if got := evidence.CheckRecoveredRuns(rec, []evidence.Run{{Offset: 4096, Length: 1024}}); len(got) != 0 {
		t.Errorf("CheckRecoveredRuns: %q, want none", got)
	}
	rep := evidence.Recovery{Class: evidence.ClassJournalReport, Method: "ext4-journal", Scope: "journal", Algorithm: evidence.AlgorithmRecover}
	if got := rep.Check(evidence.KindReport); len(got) != 0 {
		t.Errorf("report Check: %v, want none", got)
	}
}

func TestJournalScopeRefusesFreeBytes(t *testing.T) {
	for _, a := range []evidence.AllocSummary{{Free: 1, Allocated: 1023}, {Unknown: 1, Allocated: 1023}} {
		rec := journalBlockRecord("journal", a)
		got := evidence.CheckRecoveredRuns(rec, []evidence.Run{{Offset: 4096, Length: 1024}})
		if len(got) != 1 || !strings.Contains(got[0], "scope journal but") {
			t.Errorf("alloc %+v: %q, want one scope-journal problem", a, got)
		}
	}
}

func TestJournalClassEmptyScopeIsUnallocated(t *testing.T) {
	rec := journalBlockRecord("", evidence.AllocSummary{Allocated: 1024})
	got := evidence.CheckRecoveredRuns(rec, []evidence.Run{{Offset: 4096, Length: 1024}})
	if len(got) != 1 || !strings.Contains(got[0], "scope unallocated but 1024 allocated and 0 unknown bytes") {
		t.Errorf("empty stored scope: %q, want the unallocated R5 problem", got)
	}
}

func TestJournalClassUnknownScopeRefused(t *testing.T) {
	for _, class := range []string{evidence.ClassJournalBlock, evidence.ClassJournalReport} {
		kind := evidence.KindJournal
		if class == evidence.ClassJournalReport {
			kind = evidence.KindReport
		}
		r := evidence.Recovery{Class: class, Method: "ext4-journal", Scope: "volume", Algorithm: evidence.AlgorithmRecover, Journal: &evidence.JournalRef{Seq: 1, Region: "live"}}
		got := r.Check(kind)
		if len(got) != 1 || !strings.Contains(got[0], `scope "volume"`) || !strings.Contains(got[0], "unallocated, journal") {
			t.Errorf("%s in scope volume: %q, want one problem naming the allowed scopes", class, got)
		}
	}
}

func TestJournalClassRowsListScopes(t *testing.T) {
	for _, class := range []string{evidence.ClassJournalBlock, evidence.ClassJournalReport} {
		ci, ok := evidence.LookupClass(class)
		if !ok || !slices.Equal(ci.Scopes, []string{"unallocated", "journal"}) {
			t.Errorf("%s row %+v (found %v): want Scopes unallocated, journal", class, ci, ok)
		}
	}
	for _, class := range []string{evidence.ClassDeletedFile, evidence.ClassSlack, evidence.ClassCarved, evidence.ClassPostCheckpointFile} {
		if ci, _ := evidence.LookupClass(class); len(ci.Scopes) != 0 {
			t.Errorf("%s row carries Scopes %v; only the journal classes do", class, ci.Scopes)
		}
	}
}
