package records_test

import (
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestGetResolvesArtifactByIDOnly: a record whose artifact_id was rewritten to
// another artifact's PATH must not resolve to that artifact (FindArtifact falls
// back to paths); Get fails and returns no partial record.
func TestGetResolvesArtifactByIDOnly(t *testing.T) {
	c := recordstest.NewCase(t)
	a1 := recordstest.AddArtifact(t, c, "one.db", mib)
	a2 := recordstest.AddArtifact(t, c, "two.db", mib)
	res := recordstest.Ingest(t, c, testParser, []string{a1.ID}, []records.Record{{Type: "note", ArtifactID: a1.ID, Payload: map[string]any{}}})
	r := newReader(t, c)
	if _, err := r.Get(ctx, res.FirstID); err != nil {
		t.Fatal(err)
	}
	recordstest.RepointRecord(t, c.Dir, res.FirstID, a2.Path)
	full, err := r.Get(ctx, res.FirstID)
	if !errors.Is(err, evidence.ErrUnknownArtifact) || !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Get after pointing at another artifact's path = %+v, %v; want ErrUnknownArtifact and ErrIntegrity", full.Artifact, err)
	}
	if !reflect.DeepEqual(full, records.Full{}) {
		t.Fatalf("an error returned a partial record: %+v", full)
	}
}

// TestGetMissingArtifactIsError: a record whose artifact is gone from the
// manifest is an error, never a record without provenance.
func TestGetMissingArtifactIsError(t *testing.T) {
	c := recordstest.NewCase(t)
	a1 := recordstest.AddArtifact(t, c, "one.db", mib)
	res := recordstest.Ingest(t, c, testParser, []string{a1.ID}, []records.Record{{Type: "note", ArtifactID: a1.ID, Payload: map[string]any{}}})
	r := newReader(t, c)
	recordstest.RemoveArtifactEverywhere(t, c.Dir, a1.ID)
	full, err := r.Get(ctx, res.FirstID)
	if !errors.Is(err, evidence.ErrUnknownArtifact) || !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Get = %v, want ErrUnknownArtifact and ErrIntegrity (exit 4)", err)
	}
	if !reflect.DeepEqual(full, records.Full{}) {
		t.Fatalf("an error returned a partial record: %+v", full)
	}
}

// TestPathPrefixInvalidTextIsErrInvalidFilter: a prefix with a NUL or invalid
// UTF-8 can match nothing a record may hold; it is refused, not run.
func TestPathPrefixInvalidTextIsErrInvalidFilter(t *testing.T) {
	_, r := bigDataset(t, 5)
	for _, p := range []string{"x\x00y", "\x00", "/data/\x00", "\xff", "ab\xc3", "\xed\xa0\x80"} {
		f := records.Filter{PathPrefix: p}
		if _, err := r.List(ctx, f, records.Page{}); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("List prefix %q: %v, want ErrInvalidFilter", p, err)
		}
		if _, _, err := r.Count(ctx, f, 0); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("Count prefix %q: %v, want ErrInvalidFilter", p, err)
		}
		if _, err := r.Stats(ctx, f, "type"); !errors.Is(err, records.ErrInvalidFilter) {
			t.Errorf("Stats prefix %q: %v, want ErrInvalidFilter", p, err)
		}
	}
}

// timedCase ingests one record per spec, in order (so ids follow the specs); a
// nil spec is untimed. Extreme instants need a ts_note.
func timedCase(t *testing.T, specs []*time.Time) *records.Reader {
	t.Helper()
	c, art := setup(t)
	recs := make([]records.Record, len(specs))
	for i, s := range specs {
		recs[i] = records.Record{
			Type: "note", ArtifactID: art.ID, Summary: "r" + strconv.Itoa(i),
			Payload: map[string]any{"ts_note": "synthetic instant"},
		}
		if s != nil {
			recs[i].Time = &records.Time{T: *s}
		}
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return newReader(t, c)
}

// checkPagesEqualWhole walks every page size in both directions and requires the
// pages to equal the un-paged listing (and the order to be the promised one).
func checkPagesEqualWhole(t *testing.T, r *records.Reader, f records.Filter, sizes []int) {
	t.Helper()
	whole, err := r.List(ctx, f, records.Page{Limit: records.MaxLimit})
	if err != nil || whole.NextCursor != "" || len(whole.Rows) == 0 {
		t.Fatalf("whole listing: %d rows, cursor %q, %v", len(whole.Rows), whole.NextCursor, err)
	}
	if want := wantOrder(whole.Rows); !reflect.DeepEqual(ids(want), ids(whole.Rows)) {
		t.Fatalf("order is not (untimed last, ts, id):\n got %v\nwant %v", ids(whole.Rows), ids(want))
	}
	rev, err := r.List(ctx, f, records.Page{Limit: records.MaxLimit, Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(whole.Rows)-1; i < len(whole.Rows); i, j = i+1, j-1 {
		if rev.Rows[i].ID != whole.Rows[j].ID {
			t.Fatalf("descending is not the exact reverse: %v vs %v", ids(rev.Rows), ids(whole.Rows))
		}
	}
	for _, size := range sizes {
		if got := pages(t, r, f, size, false, len(whole.Rows)); !reflect.DeepEqual(got, whole.Rows) {
			t.Fatalf("size %d ascending:\n got %v\nwant %v", size, ids(got), ids(whole.Rows))
		}
		if got := pages(t, r, f, size, true, len(whole.Rows)); !reflect.DeepEqual(got, rev.Rows) {
			t.Fatalf("size %d descending:\n got %v\nwant %v", size, ids(got), ids(rev.Rows))
		}
	}
}

func TestPaginationNegativeZeroAndExtremeTimestamps(t *testing.T) {
	at := func(sec int64, nsec int64) *time.Time { v := time.Unix(sec, nsec).UTC(); return &v }
	r := timedCase(t, []*time.Time{
		at(0, 0), at(-1, 0), at(-1, 999_999_000), at(0, 1000), at(1<<40, 0), at(-(1 << 40), 0), at(0, 0),
		at(-1<<40, 0), at(1<<40, 0), nil, at(-5, 0), nil, at(1_700_000_000, 0),
	})
	checkPagesEqualWhole(t, r, records.Filter{}, []int{1, 2, 3, 4, 5, 7, 12, 13, 100})
	// the extremes survive the round trip through the cursor as stored
	res, err := r.List(ctx, records.Filter{}, records.Page{Limit: 1})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].TS == nil || res.Rows[0].TS.T.Unix() != -(1<<40) {
		t.Fatalf("first row %+v, %v", res.Rows, err)
	}
}

func TestPaginationAllNullAndAllTimed(t *testing.T) {
	at := func(h int) *time.Time { v := time.Unix(1_700_000_000+int64(h)*3600, 0).UTC(); return &v }
	nulls := timedCase(t, []*time.Time{nil, nil, nil, nil, nil, nil, nil})
	checkPagesEqualWhole(t, nulls, records.Filter{}, []int{1, 2, 3, 6, 7, 8})
	timedOnly := timedCase(t, []*time.Time{at(3), at(1), at(3), at(2), at(3), at(1), at(9)})
	checkPagesEqualWhole(t, timedOnly, records.Filter{}, []int{1, 2, 3, 6, 7, 8})
}

// TestPaginationPageEndsOnTimedUntimedBoundary: pages whose last row is the last
// timed row (or the last untimed row) continue (or end) correctly in both
// directions.
func TestPaginationPageEndsOnTimedUntimedBoundary(t *testing.T) {
	at := func(h int) *time.Time { v := time.Unix(1_700_000_000+int64(h)*3600, 0).UTC(); return &v }
	// 6 timed (with ties) then 4 untimed, mixed in insertion order
	r := timedCase(t, []*time.Time{nil, at(2), at(2), nil, at(1), at(5), nil, at(5), at(3), nil})
	checkPagesEqualWhole(t, r, records.Filter{}, []int{1, 2, 3, 4, 5, 6, 7, 10})
	// size 6 ascending ends exactly after the last timed row; descending ends
	// exactly after the last untimed row at size 4
	res, err := r.List(ctx, records.Filter{}, records.Page{Limit: 6})
	if err != nil || res.NextCursor == "" || res.Rows[5].TS == nil {
		t.Fatalf("ascending size 6: %+v, %v", res, err)
	}
	next, err := r.List(ctx, records.Filter{}, records.Page{Limit: 6, Cursor: res.NextCursor})
	if err != nil || len(next.Rows) != 4 || next.NextCursor != "" || next.Rows[0].TS != nil {
		t.Fatalf("after the timed rows: %d rows (first untimed=%v), cursor %q, %v", len(next.Rows), len(next.Rows) > 0 && next.Rows[0].TS == nil, next.NextCursor, err)
	}
	d, err := r.List(ctx, records.Filter{}, records.Page{Limit: 4, Desc: true})
	if err != nil || d.NextCursor == "" || d.Rows[3].TS != nil {
		t.Fatalf("descending size 4: %+v, %v", d, err)
	}
	dn, err := r.List(ctx, records.Filter{}, records.Page{Limit: 4, Desc: true, Cursor: d.NextCursor})
	if err != nil || len(dn.Rows) != 4 || dn.Rows[0].TS == nil {
		t.Fatalf("descending after the untimed rows: %+v, %v", dn, err)
	}
}

func TestPaginationDescendingWithSupersession(t *testing.T) {
	c, art := setup(t)
	mk := func(tag string, n int) []records.Record {
		out := recordstest.Records(art.ID, n, 11)
		for i := range out {
			out[i].Summary = tag + strconv.Itoa(i)
		}
		return out
	}
	old := recordstest.Ingest(t, c, testParser, []string{art.ID}, mk("old", 30))
	cur := recordstest.Ingest(t, c, records.Parser{Name: testParser.Name, Version: "2.0", Hash: "def456"}, []string{art.ID}, mk("new", 30)) // supersedes the first run
	r := newReader(t, c)
	if len(superseded(t, c)) == 0 {
		t.Fatal("no run was superseded")
	}
	checkPagesEqualWhole(t, r, records.Filter{}, []int{1, 3, 7})
	checkPagesEqualWhole(t, r, records.Filter{IncludeSuperseded: true}, []int{1, 3, 7})
	whole, err := r.List(ctx, records.Filter{}, records.Page{Limit: records.MaxLimit, Desc: true})
	if err != nil || len(whole.Rows) != 30 {
		t.Fatalf("current run: %d rows, %v", len(whole.Rows), err)
	}
	for _, row := range whole.Rows {
		if row.IngestID != cur.IngestID || row.Superseded {
			t.Fatalf("row %d of ingest %s listed as current (superseded=%v)", row.ID, row.IngestID, row.Superseded)
		}
	}
	all, err := r.List(ctx, records.Filter{IncludeSuperseded: true}, records.Page{Limit: records.MaxLimit, Desc: true})
	if err != nil || len(all.Rows) != 60 {
		t.Fatalf("all runs: %d rows, %v", len(all.Rows), err)
	}
	n := 0
	for _, row := range all.Rows {
		if row.IngestID == old.IngestID {
			n++
			if !row.Superseded {
				t.Fatalf("row %d of the old run is not marked superseded", row.ID)
			}
		}
	}
	if n != 30 {
		t.Fatalf("%d old rows, want 30", n)
	}
}

// TestWellFormedCursorWithExtremeValues: a cursor that decodes and carries the
// right fingerprint but extreme numbers is a valid position, not an error or a
// panic; it selects exactly the rows after it.
func TestWellFormedCursorWithExtremeValues(t *testing.T) {
	at := func(h int) *time.Time { v := time.Unix(1_700_000_000+int64(h)*3600, 0).UTC(); return &v }
	r := timedCase(t, []*time.Time{at(1), nil, at(2), nil, at(3)})
	first, err := r.List(ctx, records.Filter{}, records.Page{Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	firstDesc, err := r.List(ctx, records.Filter{}, records.Page{Limit: 1, Desc: true})
	if err != nil || firstDesc.NextCursor == "" {
		t.Fatal(firstDesc, err)
	}
	base, err := records.DecodeCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	baseDesc, err := records.DecodeCursor(firstDesc.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	labels := func(rows []records.Row) []string {
		var out []string
		for _, row := range rows {
			out = append(out, row.Summary)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		cur  records.Cursor
		desc bool
		want []string
	}{
		{"asc after the largest timed position", records.Cursor{TS: math.MaxInt64, ID: math.MaxInt64, F: base.F}, false, []string{"r1", "r3"}},
		{"asc after the smallest timed position", records.Cursor{TS: math.MinInt64, ID: 1, F: base.F}, false, []string{"r0", "r2", "r4", "r1", "r3"}},
		{"asc after the last untimed position", records.Cursor{N: 1, ID: math.MaxInt64, F: base.F}, false, nil},
		{"asc after the first untimed position", records.Cursor{N: 1, ID: 1, F: base.F}, false, []string{"r1", "r3"}},
		{"desc before the smallest timed position", records.Cursor{D: 1, TS: math.MinInt64, ID: 1, F: baseDesc.F}, true, nil},
		{"desc before the largest timed position", records.Cursor{D: 1, TS: math.MaxInt64, ID: math.MaxInt64, F: baseDesc.F}, true, []string{"r4", "r2", "r0"}},
		{"desc after the first untimed position", records.Cursor{D: 1, N: 1, ID: 1, F: baseDesc.F}, true, []string{"r4", "r2", "r0"}},
		{"desc after the last untimed position", records.Cursor{D: 1, N: 1, ID: math.MaxInt64, F: baseDesc.F}, true, []string{"r3", "r1", "r4", "r2", "r0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.List(ctx, records.Filter{}, records.Page{Limit: 10, Cursor: tc.cur.Encode(), Desc: tc.desc})
			if err != nil {
				t.Fatal(err)
			}
			if got := labels(res.Rows); !reflect.DeepEqual(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
