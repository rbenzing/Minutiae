package records_test

import (
	"errors"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// firstCursor returns the next cursor of the first page of f.
func firstCursor(t *testing.T, r *records.Reader, f records.Filter, desc bool) string {
	t.Helper()
	res, err := r.List(ctx, f, records.Page{Limit: 3, Desc: desc})
	if err != nil || res.NextCursor == "" {
		t.Fatalf("first page: cursor %q, %v", res.NextCursor, err)
	}
	return res.NextCursor
}

func wantBadCursor(t *testing.T, r *records.Reader, f records.Filter, cur string, desc bool) {
	t.Helper()
	res, err := r.List(ctx, f, records.Page{Limit: 3, Cursor: cur, Desc: desc})
	if !errors.Is(err, records.ErrBadCursor) {
		t.Fatalf("got %v (%d rows), want ErrBadCursor", err, len(res.Rows))
	}
	if len(res.Rows) != 0 {
		t.Fatalf("a mismatched cursor returned %d rows", len(res.Rows))
	}
}

// TestCursorBoundToFilter: a cursor continues only the filter that produced it;
// any other filter is ErrBadCursor, never a page of the wrong selection.
func TestCursorBoundToFilter(t *testing.T) {
	_, r := bigDataset(t, 120)
	base := records.Filter{Types: []string{"message", "call"}}
	cur := firstCursor(t, r, base, false)

	three := 3
	t0 := time.Unix(1_600_000_000, 0)
	for _, tc := range []struct {
		name string
		f    records.Filter
	}{
		{"no filter", records.Filter{}},
		{"another type", records.Filter{Types: []string{"message", "event"}}},
		{"fewer types", records.Filter{Types: []string{"message"}}},
		{"added artifact", records.Filter{Types: base.Types, ArtifactIDs: []string{"x"}}},
		{"from", records.Filter{Types: base.Types, From: &t0}},
		{"to", records.Filter{Types: base.Types, To: &t0}},
		{"path prefix", records.Filter{Types: base.Types, PathPrefix: "/data/"}},
		{"deleted", records.Filter{Types: base.Types, Deleted: records.None}},
		{"recovered", records.Filter{Types: base.Types, Recovered: records.Only}},
		{"parser", records.Filter{Types: base.Types, Parsers: []records.ParserRef{{Name: "sms-parser"}}}},
		{"confidence", records.Filter{Types: base.Types, MinConfidence: &three}},
		{"ingest", records.Filter{Types: base.Types, IngestID: "i"}},
		{"all runs", records.Filter{Types: base.Types, IncludeSuperseded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) { wantBadCursor(t, r, tc.f, cur, false) })
	}

	// the same selection spelled differently is the same filter
	for _, f := range []records.Filter{
		{Types: []string{"call", "message"}},
		{Types: []string{"message", "call", "message"}},
		{Types: base.Types, IncludeUntimed: true}, // no time bound: IncludeUntimed selects nothing more
	} {
		if _, err := r.List(ctx, f, records.Page{Limit: 3, Cursor: cur}); err != nil {
			t.Errorf("equivalent filter %+v: %v", f, err)
		}
	}
	// and the original keeps working
	if _, err := r.List(ctx, base, records.Page{Limit: 3, Cursor: cur}); err != nil {
		t.Fatal(err)
	}
}

// TestCursorBoundToOrder: a cursor of one direction is refused by the other,
// whatever the filter.
func TestCursorBoundToOrder(t *testing.T) {
	_, r := bigDataset(t, 60)
	f := records.Filter{Types: []string{"message", "call", "event"}}
	asc, desc := firstCursor(t, r, f, false), firstCursor(t, r, f, true)
	wantBadCursor(t, r, f, asc, true)
	wantBadCursor(t, r, f, desc, false)
	if _, err := r.List(ctx, f, records.Page{Limit: 3, Cursor: asc}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(ctx, f, records.Page{Limit: 3, Cursor: desc, Desc: true}); err != nil {
		t.Fatal(err)
	}
}

// TestCursorBoundToCase: a cursor taken from another case, even one holding the
// same records, is refused.
func TestCursorBoundToCase(t *testing.T) {
	mk := func(id string) *records.Reader {
		c, err := evidence.Create(t.TempDir(), evidence.CreateOptions{ID: id, Examiner: "Examiner"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		art := recordstest.AddArtifact(t, c, "a.db", mib)
		recordstest.Ingest(t, c, testParser, []string{art.ID}, recordstest.Records(art.ID, 40, 7))
		return newReader(t, c)
	}
	a, b := mk("CASEA"), mk("CASEB")
	cur := firstCursor(t, a, records.Filter{}, false)
	wantBadCursor(t, b, records.Filter{}, cur, false)
	if _, err := a.List(ctx, records.Filter{}, records.Page{Limit: 3, Cursor: cur}); err != nil {
		t.Fatal(err)
	}
}
