package records_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// searchDataset: 600 records whose summaries are "common word<i%7> unique<i>"; record i has id i+1.
func searchDataset(t *testing.T) (*evidence.Case, *records.Reader, []string) {
	t.Helper()
	c, art := setup(t)
	recs := recordstest.Records(art.ID, 600, 31)
	for i := range recs {
		recs[i].Summary = fmt.Sprintf("common word%d unique%d", i%7, i)
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	return c, newReader(t, c), []string{art.ID}
}

func compileWords(t *testing.T, in string) *records.TextQuery {
	t.Helper()
	q, err := records.CompileQuery(in, records.TextOptions{})
	if err != nil {
		t.Fatalf("compile %q: %v", in, err)
	}
	return q
}

// walkSearch returns the ids of every page of a search, in page order.
func walkSearch(t *testing.T, r *records.Reader, f records.Filter, desc bool, limit int) []int64 {
	t.Helper()
	var got []int64
	cur := ""
	for pages := 0; ; pages++ {
		res, err := r.Search(ctx, f, records.Page{Limit: limit, Cursor: cur, Desc: desc}, records.SearchOptions{})
		if err != nil {
			t.Fatalf("search page %d: %v", pages, err)
		}
		for _, h := range res.Hits {
			got = append(got, h.Row.ID)
		}
		if res.NextCursor == "" {
			return got
		}
		if cur = res.NextCursor; pages > 10_000 {
			t.Fatal("paging does not end")
		}
	}
}

func TestSearchShapesReturnIdenticalPages(t *testing.T) {
	_, r, _ := searchDataset(t)
	all := wantOrder(allRows(t, r))
	from, to := time.Unix(1_600_000_000, 0).UTC(), time.Unix(1_600_000_000+20*3600, 0).UTC()
	queries := []struct {
		in   string
		keep func(i int) bool
	}{
		{"common", func(int) bool { return true }},
		{"word3", func(i int) bool { return i%7 == 3 }},
		{"unique1*", func(i int) bool { return strings.HasPrefix(fmt.Sprintf("unique%d", i), "unique1") }},
		{`"common word3"`, func(i int) bool { return i%7 == 3 }},
		{"common -word3", func(i int) bool { return i%7 != 3 }},
	}
	filters := []struct {
		name string
		f    records.Filter
		keep func(records.Row) bool
	}{
		{"none", records.Filter{}, func(records.Row) bool { return true }},
		{"one type", records.Filter{Types: []string{"message"}}, func(x records.Row) bool { return x.Type == "message" }},
		{"time window", records.Filter{From: &from, To: &to}, func(x records.Row) bool {
			return x.TS != nil && !x.TS.T.Before(from) && x.TS.T.Before(to)
		}},
	}
	// 1 forces the index-first shape for every query, the default chooses, 1<<30 forces FTS-first
	for _, probe := range []int{1, 4096, 1 << 30} {
		restore := records.SetSearchProbeLimit(probe)
		for _, qc := range queries {
			q := compileWords(t, qc.in)
			for _, fc := range filters {
				for _, desc := range []bool{false, true} {
					var want []int64
					for _, x := range all {
						if fc.keep(x) && qc.keep(int(x.ID-1)) {
							want = append(want, x.ID)
						}
					}
					if desc {
						slices.Reverse(want)
					}
					f := fc.f
					f.Text = q
					if got := walkSearch(t, r, f, desc, 7); !slices.Equal(got, want) {
						t.Fatalf("probe %d, %q, %s, desc=%v: %d ids, oracle %d", probe, qc.in, fc.name, desc, len(got), len(want))
					}
				}
			}
		}
		restore()
	}
}

func TestIndexFirstPlanIsOrderedByAnIndex(t *testing.T) {
	c, _, _ := searchDataset(t)
	q := compileWords(t, "common")
	for _, tc := range []struct {
		name  string
		f     records.Filter
		index string
	}{
		{"no type", records.Filter{Text: q}, "records_ts"},
		{"one type", records.Filter{Types: []string{"message"}, Text: q}, "records_type_ts"},
	} {
		qs, err := records.SearchQueries(tc.f, false, nil, 101, true)
		if err != nil || len(qs) == 0 {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, sq := range qs {
			plan := explain(t, c, sq.SQL, sq.Args)
			if !strings.Contains(plan, "INDEX "+tc.index) || strings.Contains(plan, "LIST SUBQUERY") || !strings.Contains(plan, " EXISTS VIRTUAL TABLE INDEX 0:=") || strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") ||
				(!strings.HasPrefix(plan, "SCAN r") && !strings.HasPrefix(plan, "SEARCH r")) {
				t.Fatalf("%s: not driven by %s in time order:\n%s", tc.name, tc.index, plan)
			}
		}
	}
}

func TestIndexFirstNeverErrorsOnAnyFilterShape(t *testing.T) {
	_, r, art := searchDataset(t)
	restore := records.SetSearchProbeLimit(1)
	defer restore()
	from, to := time.Unix(1_600_000_000, 0).UTC(), time.Unix(1_600_000_000+20*3600, 0).UTC()
	q := compileWords(t, "common")
	for _, f := range map[string]records.Filter{
		"none":      {Text: q},
		"one type":  {Types: []string{"message"}, Text: q},
		"two types": {Types: []string{"message", "call"}, Text: q},
		"artifact":  {ArtifactIDs: art, Text: q},
		"deleted":   {Deleted: records.Only, Text: q},
		"time":      {From: &from, To: &to, Text: q},
		"type+time": {Types: []string{"message"}, From: &from, To: &to, Text: q},
	} {
		for _, desc := range []bool{false, true} {
			// walking every page reaches cursors after timed and after untimed rows
			walkSearch(t, r, f, desc, 5)
		}
	}
}

func TestSearchProbeIsBoundedAndOncePerPage(t *testing.T) {
	_, r, _ := searchDataset(t)
	restore := records.SetSearchProbeLimit(5)
	defer restore()
	var probes []string
	r.SetQueryLog(func(q string) {
		if strings.HasPrefix(q, "SELECT count(*) FROM (SELECT 1 FROM records_fts") {
			probes = append(probes, q)
		}
	})
	if _, err := r.Search(ctx, records.Filter{Text: compileWords(t, "common")}, records.Page{Limit: 10}, records.SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || !strings.HasSuffix(probes[0], "LIMIT ?)") {
		t.Fatalf("a page must run exactly one bounded probe, got %q", probes)
	}
}

func TestPlanFollowsTheProbe(t *testing.T) {
	_, r, _ := searchDataset(t)
	q := compileWords(t, "common")
	for _, tc := range []struct {
		name       string
		limit      int
		wantExists bool
	}{{"common query (probe at 1)", 1, true}, {"rare query (probe above the hits)", 1 << 30, false}} {
		restore := records.SetSearchProbeLimit(tc.limit)
		var lists []string
		r.SetQueryLog(func(s string) {
			if strings.HasPrefix(s, "SELECT r.") || strings.HasPrefix(s, "SELECT ") && strings.Contains(s, " FROM records r") {
				lists = append(lists, s)
			}
		})
		if _, err := r.Search(ctx, records.Filter{Text: q}, records.Page{Limit: 10}, records.SearchOptions{}); err != nil {
			t.Fatal(err)
		}
		restore()
		if len(lists) == 0 {
			t.Fatalf("%s: no list statement logged", tc.name)
		}
		for _, s := range lists {
			if got := strings.Contains(s, "EXISTS (SELECT 1 FROM records_fts"); got != tc.wantExists {
				t.Errorf("%s: index-first shape = %v, want %v:\n%s", tc.name, got, tc.wantExists, s)
			}
			if got := strings.Contains(s, "INDEXED BY records_ts"); got != tc.wantExists {
				t.Errorf("%s: index hint = %v, want %v", tc.name, got, tc.wantExists)
			}
		}
	}
}
