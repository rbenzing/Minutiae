package dbrecover

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

var typedPeople = []ExcludedTable{{Table: "people", Parser: "p.recover", Version: "1.0.0", Typed: true, TypedAnalysisID: "A"}}

// fitPeopleLocators are the locators of the people rows a plain run writes at
// basis fit: the rows a claim on people excludes (the oracle of
// TestClaimExclusionNeedsLiveFit: 12), sorted.
func fitPeopleLocators(t *testing.T) (locs []string, written int64, other string) {
	t.Helper()
	c, in := fixtureCase(t, "sqlite3-freelist")
	res := mustRun(t, c, in, Options{})
	for _, f := range storedRecords(t, c) {
		p := payloadOf(t, f)
		switch {
		case p["table"] == "people" && p["table_basis"] == "fit":
			locs = append(locs, f.Locator)
		case p["table"] != "people" && f.Locator != "":
			other = f.Locator
		}
	}
	slices.Sort(locs)
	return locs, res.Counts.Written, other
}

func digests(locs ...string) (out [][16]byte) {
	for _, l := range locs {
		out = append(out, LocatorDigest(l))
	}
	return out
}

func TestKeepClaimedWritesTheNamedRows(t *testing.T) {
	locs, plain, other := fitPeopleLocators(t)
	if len(locs) != 12 || other == "" {
		t.Fatalf("scenario: %d fit people locators (want 12), other %q", len(locs), other)
	}
	if got := len(slices.Compact(slices.Clone(locs))); got != 12 {
		t.Fatalf("scenario: %d distinct locators, want 12", got)
	}
	keep := []string{locs[2], locs[7]}
	c, in := fixtureCase(t, "sqlite3-freelist")
	// a digest of a row that is not claimed is not a kept claimed row
	res := mustRun(t, c, in, Options{ExcludeTables: typedPeople, KeepClaimed: digests(locs[2], locs[7], other)})
	if res.Counts.KeptUnmappedClaimed != 2 {
		t.Errorf("KeptUnmappedClaimed %d, want 2", res.Counts.KeptUnmappedClaimed)
	}
	if got := res.Counts.SkippedByReason["excluded-by-claim"]; got != 10 {
		t.Errorf("excluded-by-claim %d, want 10 (12 claimed rows less the 2 kept)", got)
	}
	if res.Counts.Written != plain-10 {
		t.Errorf("written %d, want %d (the plain run less the 10 excluded)", res.Counts.Written, plain-10)
	}
	if res.Ingest.Outcome != "complete" || len(res.Counts.Narrowed) != 0 {
		t.Errorf("outcome %q narrowed %v: keeping rows is not narrowing", res.Ingest.Outcome, res.Counts.Narrowed)
	}
	var got []string
	for _, f := range storedRecords(t, c) {
		if p := payloadOf(t, f); p["table"] == "people" && p["table_basis"] == "fit" {
			got = append(got, f.Locator)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, keep) {
		t.Errorf("fit people rows written %v, want exactly %v", got, keep)
	}
	es := readAudit(t, c)
	end := lastEntry(t, es, "analysis.end").Details
	if n := auditInt(end["kept_unmapped_claimed"]); n != 2 {
		t.Errorf("analysis.end kept_unmapped_claimed %v, want 2", end["kept_unmapped_claimed"])
	}
	opts, _ := lastEntry(t, es, "analysis.start").Details["options"].(map[string]any)
	if n := auditInt(opts["keep_claimed"]); n != 3 {
		t.Errorf("analysis.start options.keep_claimed %v, want 3 (the digests given)", opts["keep_claimed"])
	}
}

func TestKeepClaimedBoundIsAnOptionError(t *testing.T) {
	if _, _, err := (Options{KeepClaimed: make([][16]byte, MaxKeepClaimed)}).normalized(); err != nil {
		t.Errorf("MaxKeepClaimed entries: %v, want accepted", err)
	}
	c, in := fixtureCase(t, "sqlite3-freelist")
	_, err := Run(t.Context(), c, in, Options{KeepClaimed: make([][16]byte, MaxKeepClaimed+1)})
	if refusalReason(err) != ReasonBadOption {
		t.Fatalf("MaxKeepClaimed+1 entries: %v, want %s", err, ReasonBadOption)
	}
	if n := count(readAudit(t, c), "analysis.start"); n != 0 {
		t.Errorf("%d analysis.start entries: the bound is checked before the analysis starts", n)
	}
}

func TestTypedExclusionIsNotNarrowing(t *testing.T) {
	c, in := fixtureCase(t, "sqlite3-freelist")
	res := mustRun(t, c, in, Options{ExcludeTables: typedPeople})
	if len(res.Counts.Narrowed) != 0 || res.Ingest.Outcome != "complete" {
		t.Errorf("narrowed %v outcome %q, want none and complete", res.Counts.Narrowed, res.Ingest.Outcome)
	}
	if got := res.Counts.SkippedByReason["excluded-by-claim"]; got != 12 {
		t.Errorf("excluded-by-claim %d, want 12 (the d_fit rows)", got)
	}
	es := readAudit(t, c)
	if count(es, evidence.ActionIngestEnd) != 1 || count(es, evidence.ActionIngestError) != 0 {
		t.Errorf("ingest.end %d ingest.error %d: a typed exclusion ends the ingest", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError))
	}
	ex, _ := lastEntry(t, es, "analysis.start").Details["excluded_tables"].([]any)
	if len(ex) != 1 {
		t.Fatalf("excluded_tables %v", ex)
	}
	m, _ := ex[0].(map[string]any)
	if m["typed"] != true || m["typed_analysis_id"] != "A" || m["parser"] != "p.recover" || m["table"] != "people" || m["version"] != "1.0.0" {
		t.Errorf("excluded table entry %v", m)
	}
	nl, _ := lastEntry(t, es, "analysis.end").Details["narrowed"].([]any)
	if len(nl) != 0 {
		t.Errorf("analysis.end narrowed %v, want none", nl)
	}
}

// PASSES ON ARRIVAL: today every exclusion narrows. Mutation (recorded in the
// report): make operator exclusions non-narrowing, see it fail, restore.
func TestOperatorExclusionStillNarrows(t *testing.T) {
	op := ExcludedTable{Table: "people", Parser: "examiner"}
	for name, ex := range map[string][]ExcludedTable{
		"operator":                    {op},
		"operator beside a typed one": {typedPeople[0], op},
	} {
		t.Run(name, func(t *testing.T) {
			c, in := fixtureCase(t, "sqlite3-freelist")
			res := mustRun(t, c, in, Options{ExcludeTables: ex})
			if !slices.Contains(res.Counts.Narrowed, "exclude-tables") || res.Ingest.Outcome != "incomplete" {
				t.Errorf("narrowed %v outcome %q, want exclude-tables and incomplete", res.Counts.Narrowed, res.Ingest.Outcome)
			}
			es := readAudit(t, c)
			if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) == 0 {
				t.Errorf("ingest.end %d ingest.error %d: a narrowed run is aborted", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError))
			}
		})
	}
}

func TestTypedExclusionNamesItsAnalysis(t *testing.T) {
	for name, ex := range map[string]ExcludedTable{
		"typed without an analysis":       {Table: "people", Parser: "p", Typed: true},
		"an analysis without being typed": {Table: "people", Parser: "p", TypedAnalysisID: "A"},
	} {
		if _, _, err := (Options{ExcludeTables: []ExcludedTable{ex}}).normalized(); refusalReason(err) != ReasonBadOption {
			t.Errorf("%s: %v, want %s", name, err, ReasonBadOption)
		}
	}
}
