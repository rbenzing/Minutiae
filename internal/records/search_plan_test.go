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
		{"two types", records.Filter{Types: []string{"message", "call"}, Text: q}, "records_ts"},
		{"deleted only", records.Filter{Deleted: records.Only, Text: q}, "records_ts"},
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

// shapeDataset: two artifacts, a first run (parser 1.0) over both, a second complete run (parser 2.0)
// over the first artifact that really supersedes the first run there, and an orphan full-text rowid.
func shapeDataset(t *testing.T) (*evidence.Case, *records.Reader, []string, string) {
	t.Helper()
	c, a := setup(t)
	b := recordstest.AddArtifact(t, c, "b.db", mib)
	fill := func(artifact string, n int, seed int64) []records.Record {
		recs := recordstest.Records(artifact, n, seed)
		for i := range recs {
			recs[i].Summary = fmt.Sprintf("common word%d unique%d", i%7, i)
		}
		return recs
	}
	run1 := recordstest.Ingest(t, c, testParser, []string{a.ID, b.ID}, append(fill(a.ID, 50, 41), fill(b.ID, 30, 42)...))
	p2 := records.Parser{Name: testParser.Name, Version: "2.0", Hash: "abc999"}
	recordstest.Ingest(t, c, p2, []string{a.ID}, fill(a.ID, 40, 43))
	// an index row for a record that does not exist: it must match nothing in either shape
	recordstest.InventIndexText(t, c.Dir, evidence.FTSWordTable, 1<<40, "common word3 unique5", "")
	return c, newReader(t, c), []string{a.ID, b.ID}, run1.IngestID
}

// The index-first and the FTS-first shapes are two plans for one selection: for every query, filter,
// order and page size they return the same ids in the same order (the probe limit picks the shape).
func TestSearchShapesAgreeOnEveryFilter(t *testing.T) {
	_, r, arts, run1 := shapeDataset(t)
	from, to := time.Unix(1_600_000_000, 0).UTC(), time.Unix(1_600_000_000+20*3600, 0).UTC()
	conf := 50
	filters := map[string]records.Filter{
		"none":              {},
		"two types":         {Types: []string{"message", "call"}},
		"three types":       {Types: []string{"message", "call", "file"}},
		"one artifact":      {ArtifactIDs: arts[1:]},
		"untimed + window":  {From: &from, To: &to, IncludeUntimed: true},
		"deleted only":      {Deleted: records.Only},
		"deleted none":      {Deleted: records.None},
		"recovered only":    {Recovered: records.Only},
		"recovered none":    {Recovered: records.None},
		"confidence":        {MinConfidence: &conf},
		"path prefix":       {PathPrefix: "/data/1"},
		"superseded":        {IncludeSuperseded: true},
		"type + time":       {Types: []string{"message"}, From: &from, To: &to},
		"type + untimed":    {Types: []string{"message"}, From: &from, To: &to, IncludeUntimed: true},
		"parsers":           {Parsers: []records.ParserRef{{Name: testParser.Name, Version: testParser.Version}}},
		"parsers + all":     {Parsers: []records.ParserRef{{Name: testParser.Name, Version: testParser.Version}}, IncludeSuperseded: true},
		"ingest id (run 1)": {IngestID: run1},
	}
	nonEmpty := 0
	for _, in := range []string{"common", "word3", "unique1*", `"common word3"`, "common -word3"} {
		q := compileWords(t, in)
		for name, fc := range filters {
			f := fc
			f.Text = q
			for _, desc := range []bool{false, true} {
				for _, size := range []int{1, 3, 50} {
					restore := records.SetSearchProbeLimit(1) // index-first
					indexFirst := walkSearch(t, r, f, desc, size)
					restore()
					restore = records.SetSearchProbeLimit(1 << 30) // FTS-first
					ftsFirst := walkSearch(t, r, f, desc, size)
					restore()
					if !slices.Equal(indexFirst, ftsFirst) {
						t.Fatalf("%q, %s, desc=%v, page %d: index-first %d ids, FTS-first %d ids", in, name, desc, size, len(indexFirst), len(ftsFirst))
					}
					if len(indexFirst) > 0 {
						nonEmpty++
					}
				}
			}
		}
	}
	if nonEmpty < 200 {
		t.Fatalf("only %d comparisons returned records: the filters select nothing", nonEmpty)
	}
	// the data holds what the comparison needs: a superseded run is hidden by default and the orphan row matches nothing
	q := compileWords(t, "common")
	hidden := walkSearch(t, r, records.Filter{Text: q}, false, 50)
	all := walkSearch(t, r, records.Filter{Text: q, IncludeSuperseded: true}, false, 50)
	if len(all) <= len(hidden) || len(hidden) == 0 {
		t.Fatalf("no real superseded run: %d current, %d with superseded", len(hidden), len(all))
	}
	if len(all) != 120 {
		t.Fatalf("%d records match, want 120: the orphan row must match nothing", len(all))
	}
}
