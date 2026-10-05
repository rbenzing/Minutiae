package records_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestTermHits: counts are exact, ids ascending and capped, terms are literal (no grammar), a
// substring term works, an empty list is refused.
func TestTermHits(t *testing.T) {
	fx := newTextFix(t)
	terms := []records.Term{
		{Text: "cafe"},                           // 1, 2
		{Text: "STRASSE"},                        // 3
		{Text: "555) 123", Substring: true},      // 6
		{Text: "NEAR(a b)"},                      // 12: literal text, the phrase "near a b"
		{Text: "-z"},                             // 12: a minus is text, not a negation
		{Text: `"x"`},                            // 7, 12: quotes are text
		{Text: "and"},                            // 5, 8, 11
		{Text: "nothing like this"},              // none
		{Text: "summaryneedle", Substring: true}, // 15
	}
	got, err := fx.r.TermHits(ctx, records.Filter{}, terms, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []records.TermHit{
		{Term: terms[0], Count: 2, RecordIDs: []int64{1, 2}},
		{Term: terms[1], Count: 1, RecordIDs: []int64{3}},
		{Term: terms[2], Count: 1, RecordIDs: []int64{6}},
		{Term: terms[3], Count: 1, RecordIDs: []int64{12}},
		{Term: terms[4], Count: 1, RecordIDs: []int64{12}},
		{Term: terms[5], Count: 2, RecordIDs: []int64{7, 12}},
		{Term: terms[6], Count: 3, RecordIDs: []int64{5, 8}}, // exact count 3, ids capped at 2
		{Term: terms[7], Count: 0},
		{Term: terms[8], Count: 1, RecordIDs: []int64{15}},
	}
	if len(got) != len(want) {
		t.Fatalf("%d results, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Term != want[i].Term || got[i].Count != want[i].Count || !sameIDs(got[i].RecordIDs, want[i].RecordIDs) {
			t.Errorf("term %d %+v: got %+v, want %+v", i, terms[i], got[i], want[i])
		}
	}
	// counts only
	got, err = fx.r.TermHits(ctx, records.Filter{}, terms[:1:1], 0)
	if err != nil || len(got) != 1 || got[0].Count != 2 || len(got[0].RecordIDs) != 0 {
		t.Errorf("perTermLimit 0: %+v, %v", got, err)
	}
	// the filter applies: nothing in another artifact
	got, err = fx.r.TermHits(ctx, records.Filter{ArtifactIDs: []string{"nope"}}, terms[:1:1], 5)
	if err != nil || got[0].Count != 0 {
		t.Errorf("filtered: %+v, %v", got, err)
	}
	// R48: an empty list is refused (nothing to count), before the index is looked at
	if _, err := fx.r.TermHits(ctx, records.Filter{}, nil, 5); !errors.Is(err, records.ErrInvalidFilter) {
		t.Errorf("empty list: %v, want ErrInvalidFilter", err)
	}
	recordstest.SetFTSNormVersion(t, fx.c.Dir, "")
	if _, err := fx.r.TermHits(ctx, records.Filter{}, nil, 5); !errors.Is(err, records.ErrInvalidFilter) || errors.Is(err, records.ErrIndexNotCurrent) {
		t.Errorf("empty list on an unbuilt index: %v, want ErrInvalidFilter first", err)
	}
	recordstest.SetFTSNormVersion(t, fx.c.Dir, evidence.FTSNormVersion())
	// R45: the text comes from the terms; a Filter.Text as well has two meanings and is refused
	if _, err := fx.r.TermHits(ctx, records.Filter{Text: mustCompile(t, "cafe", records.TextOptions{})}, terms[:1:1], 5); !errors.Is(err, records.ErrInvalidFilter) {
		t.Errorf("TermHits with Filter.Text: %v, want ErrInvalidFilter", err)
	}
	// limits and bad terms
	for _, n := range []int{-1, 100001} {
		if _, err := fx.r.TermHits(ctx, records.Filter{}, terms[:1:1], n); !errors.Is(err, records.ErrInvalidPage) {
			t.Errorf("perTermLimit %d: %v, want ErrInvalidPage", n, err)
		}
	}
	if _, err := fx.r.TermHits(ctx, records.Filter{}, terms[:1:1], 100000); err != nil {
		t.Errorf("perTermLimit 100000: %v", err)
	}
	for _, bad := range []records.Term{{Text: ""}, {Text: "..."}, {Text: "ab", Substring: true}} {
		if _, err := fx.r.TermHits(ctx, records.Filter{}, []records.Term{bad}, 5); !errors.Is(err, records.ErrInvalidQuery) {
			t.Errorf("term %+v: %v, want ErrInvalidQuery", bad, err)
		}
	}
}

// TestTermHitsOneSnapshot: every statement of one call runs inside one read transaction, so a writer
// on another connection is locked out for the whole call.
func TestTermHitsOneSnapshot(t *testing.T) {
	fx := newTextFix(t)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(fx.c.Dir, "artifacts.db"))+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var calls int
	var refused []bool
	fx.r.SetBeforeQuery(func() {
		calls++
		_, err := db.Exec(`CREATE TABLE zz_probe (x)`)
		refused = append(refused, err != nil && strings.Contains(strings.ToLower(err.Error()), "locked"))
		if err == nil {
			_, _ = db.Exec(`DROP TABLE zz_probe`)
		}
	})
	_, err = fx.r.TermHits(ctx, records.Filter{}, []records.Term{{Text: "cafe"}, {Text: "cafe"}, {Text: "strasse"}}, 3)
	fx.r.SetBeforeQuery(nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Fatalf("the seam ran %d times, want at least once per term", calls)
	}
	for i, r := range refused {
		if !r {
			t.Errorf("statement %d ran while a writer could commit (no read transaction held)", i)
		}
	}
	if _, err := db.Exec(`CREATE TABLE zz_probe (x)`); err != nil {
		t.Errorf("after the call the case is writable again: %v", err)
	}
}

// TestTermHitsSameFoldAsSearch: a term finds exactly the records Search finds for the same text, so
// a keyword list and a search never disagree about folding.
func TestTermHitsSameFoldAsSearch(t *testing.T) {
	fx := newTextFix(t)
	for _, tc := range []struct {
		text      string
		substring bool
	}{
		{"cafe", false},
		{"CAF" + cp(0xc9), false},
		{"cafe" + cp(0x301), false},
		{"Stra" + cp(0xdf) + "e", false},
		{cp(0xff21, 0xff22, 0xff23, 0xff11, 0xff12, 0xff13), false},
		{cp(0x130) + "stanbul", false},
		{cp(0x131) + "sp" + cp(0x131) + "rta", false},
		{"pass" + cp(0x202e) + "word", false},
		{"pass" + cp(0x200b) + "word", false},
		{cp(0x4f60, 0x597d, 0x4e16, 0x754c), false},
		{recordstest.LongNeedle, false},
		{"cafe", true},
		{"Stra" + cp(0xdf) + "e", true},
		{"abc123", true},
		{"pass word", true},
		{cp(0x597d, 0x4e16, 0x754c), true},
		{"555) 123", true},
		{"ample.com/pa", true},
	} {
		q := mustCompile(t, tc.text, records.TextOptions{Substring: tc.substring})
		res, err := fx.r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: records.MaxLimit}, records.SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs := hitIDs(res.Hits)
		slices.Sort(wantIDs)
		th, err := fx.r.TermHits(ctx, records.Filter{}, []records.Term{{Text: tc.text, Substring: tc.substring}}, 100)
		if err != nil || len(th) != 1 {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if !sameIDs(th[0].RecordIDs, wantIDs) || int(th[0].Count) != len(wantIDs) {
			t.Errorf("term %q (substring %v): TermHits %v (count %d), Search %v", tc.text, tc.substring, th[0].RecordIDs, th[0].Count, wantIDs)
		}
		if len(wantIDs) == 0 {
			t.Errorf("term %q finds nothing: the comparison is vacuous", tc.text)
		}
	}
}

// TestIndexStatus: the state, the stored and the current version and the document counts of both
// tables; a v2 case has no index.
func TestIndexStatus(t *testing.T) {
	fx := newTextFix(t)
	cur := evidence.FTSNormVersion()
	check := func(name string, want records.IndexStatus) {
		t.Helper()
		got, err := fx.r.IndexStatus(ctx)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v, want %+v", name, got, err, want)
		}
	}
	check("current", records.IndexStatus{Kind: "current", Value: cur, Current: cur, WordDocs: 15, SubstringDocs: 15})
	for _, tc := range []struct{ name, value, kind string }{
		{"unbuilt", "", "unbuilt"},
		{"building", "building", "building"},
		{"stale", "fts0/old/build", "stale"},
		{"invalid", "garbage value", "invalid"},
	} {
		recordstest.SetFTSNormVersion(t, fx.c.Dir, tc.value)
		check(tc.name, records.IndexStatus{Kind: tc.kind, Value: tc.value, Current: cur, WordDocs: 15, SubstringDocs: 15})
	}
	recordstest.SetFTSNormVersion(t, fx.c.Dir, cur)
	recordstest.EmptyFTSIndex(t, fx.c.Dir)
	check("emptied", records.IndexStatus{Kind: "current", Value: cur, Current: cur})

	r2 := newReader(t, openCase(t, recordstest.CopyV2Case(t)))
	got, err := r2.IndexStatus(ctx)
	if err != nil || got.Kind != "unavailable" {
		t.Errorf("v2 case: %+v, %v, want Kind unavailable", got, err)
	}
}
