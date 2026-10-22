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
	// (1, 0) forces EXISTS, (1, 1<<30) the hinted IN, the defaults choose, (1<<30, 1<<30) forces FTS-first
	for _, probe := range [][2]int{{1, 0}, {1, 1 << 30}, {4096, 262144}, {1 << 30, 1 << 30}} {
		restore := records.SetSearchBands(probe[0], probe[1])
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
						t.Fatalf("bands %v, %q, %s, desc=%v: %d ids, oracle %d", probe, qc.in, fc.name, desc, len(got), len(want))
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
		qs, err := records.SearchQueries(tc.f, false, nil, 101, records.ShapeExists)
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

// The hinted IN shape is ordered by the same index: the set is built once, the order needs no sort.
func TestHintedINPlanIsOrderedByAnIndex(t *testing.T) {
	c, _, _ := searchDataset(t)
	for _, tc := range []struct {
		name  string
		f     records.Filter
		index string
	}{
		{"no type", records.Filter{Text: compileWords(t, "common")}, "records_ts"},
		{"one type", records.Filter{Types: []string{"message"}, Text: compileWords(t, "common")}, "records_type_ts"},
		{"prefix", records.Filter{Text: compileWords(t, "unique1*")}, "records_ts"},
	} {
		qs, err := records.SearchQueries(tc.f, false, nil, 101, records.ShapeHintedIN)
		if err != nil || len(qs) == 0 {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, sq := range qs {
			plan := explain(t, c, sq.SQL, sq.Args)
			if !strings.Contains(plan, "INDEX "+tc.index) || !strings.Contains(plan, "LIST SUBQUERY") || strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") ||
				(!strings.HasPrefix(plan, "SCAN r") && !strings.HasPrefix(plan, "SEARCH r")) {
				t.Fatalf("%s: not driven by %s in time order:\n%s", tc.name, tc.index, plan)
			}
		}
	}
}

func TestIndexFirstNeverErrorsOnAnyFilterShape(t *testing.T) {
	_, r, art := searchDataset(t)
	restore := records.SetSearchBands(1, 0)
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
	restore := records.SetSearchBands(1000, 2000) // 600 hits: a rare query, one small probe (the stages are TestProbesRunInStages)
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

// shapeOf names the plan shape of a logged list statement.
func shapeOf(s string) records.SearchShape {
	exists := strings.Contains(s, "EXISTS (SELECT 1 FROM records_fts")
	hint := strings.Contains(s, "INDEXED BY")
	switch {
	case exists && hint:
		return records.ShapeExists
	case exists:
		return records.ShapeExistsNoHint
	case hint:
		return records.ShapeHintedIN
	}
	return records.ShapeFTSFirst
}

func shapeName(s records.SearchShape) string {
	return map[records.SearchShape]string{
		records.ShapeFTSFirst: "FTS-first", records.ShapeHintedIN: "hinted IN",
		records.ShapeExists: "EXISTS", records.ShapeExistsNoHint: "EXISTS without hint",
	}[s]
}

// listShapes runs one search page and returns the shape of every list statement it made.
func listShapes(t *testing.T, r *records.Reader, f records.Filter) []records.SearchShape {
	t.Helper()
	var shapes []records.SearchShape
	r.SetQueryLog(func(s string) {
		if strings.HasPrefix(s, "SELECT ") && strings.Contains(s, " FROM records r") && !strings.HasPrefix(s, "SELECT count(*)") {
			shapes = append(shapes, shapeOf(s))
		}
	})
	defer r.SetQueryLog(nil)
	if _, err := r.Search(ctx, f, records.Page{Limit: 10}, records.SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(shapes) == 0 {
		t.Fatal("no list statement logged")
	}
	return shapes
}

// The plan has three bands by the number of full-text hits: fewer than the probe limit is FTS-first,
// up to the IN limit is the hinted IN, more is EXISTS; a prefix or multi-term query never takes EXISTS.
// "common" matches exactly 600 of the 600 records, so a limit of 600 or 599 sits on the boundary.
func TestPlanFollowsTheProbe(t *testing.T) {
	_, r, _ := searchDataset(t)
	type band struct{ probe, in int }
	for _, tc := range []struct {
		name  string
		query string
		b     band
		want  records.SearchShape
	}{
		{"defaults: 600 hits are a rare query", "common", band{4096, 262144}, records.ShapeFTSFirst},
		{"hits one below the probe limit", "common", band{601, 1 << 30}, records.ShapeFTSFirst},
		{"hits at the probe limit", "common", band{600, 1 << 30}, records.ShapeHintedIN},
		{"hits at the IN limit", "common", band{1, 600}, records.ShapeHintedIN},
		{"hits one above the IN limit", "common", band{1, 599}, records.ShapeExists},
		{"a prefix never takes EXISTS", "unique1*", band{1, 0}, records.ShapeHintedIN},
		{"a multi-term query never takes EXISTS", "common word3", band{1, 0}, records.ShapeHintedIN},
		{"a phrase never takes EXISTS", `"common word3"`, band{1, 0}, records.ShapeHintedIN},
		{"a negation never takes EXISTS", "common -word3", band{1, 0}, records.ShapeHintedIN},
		{"an OR never takes EXISTS", "word3 OR word4", band{1, 0}, records.ShapeHintedIN},
		{"a single word does", "word3", band{1, 0}, records.ShapeExists},
	} {
		restore := records.SetSearchBands(tc.b.probe, tc.b.in)
		shapes := listShapes(t, r, records.Filter{Text: compileWords(t, tc.query)})
		restore()
		for _, got := range shapes {
			if got != tc.want {
				t.Errorf("%s: shape %s, want %s", tc.name, shapeName(got), shapeName(tc.want))
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

// The four plan shapes are plans for one selection: for every query, filter,
// order and page size they return the same ids in the same order (the bands and the filter probe pick the shape).
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
	// every plan shape: FTS-first, the hinted IN, EXISTS, and EXISTS without the hint (a tiny filter)
	type config struct {
		name           string
		probe, in, fil int
	}
	configs := []config{
		{"FTS-first", 1 << 30, 1 << 30, 0},
		{"hinted IN", 1, 1 << 30, 0},
		{"EXISTS", 1, 0, 0},
		{"no hint", 1, 0, 1 << 30},
		{"defaults", 4096, 262144, 4096},
	}
	// S8: each cell runs the shape its config asks for (every query has more than one hit, so the
	// documented exceptions are: a prefix, phrase, negation or OR decides at once and never takes EXISTS;
	// a filter that is not index-served, or has no condition, is not probed and so is never tiny)
	seen := map[records.SearchShape]bool{}
	multi := map[string]bool{"unique1*": true, `"common word3"`: true, "common -word3": true}
	probed := map[string]bool{
		"two types": true, "three types": true, "one artifact": true, "untimed + window": true, "deleted only": true,
		"deleted none": true, "type + time": true, "type + untimed": true,
	}
	assertShape := func(cfg, in, filter string, f records.Filter) {
		t.Helper()
		var want records.SearchShape
		switch {
		case cfg == "FTS-first" || cfg == "defaults":
			want = records.ShapeFTSFirst
		case cfg == "hinted IN" || multi[in]:
			want = records.ShapeHintedIN
		case cfg == "no hint" && probed[filter]:
			want = records.ShapeExistsNoHint
		default:
			want = records.ShapeExists
		}
		for _, got := range listShapes(t, r, f) {
			seen[got] = true
			if got != want {
				t.Errorf("%s, %q, %s: shape %s, want %s", cfg, in, filter, shapeName(got), shapeName(want))
			}
		}
	}
	nonEmpty := 0
	for _, in := range []string{"common", "word3", "unique1*", `"common word3"`, "common -word3"} {
		q := compileWords(t, in)
		for name, fc := range filters {
			f := fc
			f.Text = q
			for _, desc := range []bool{false, true} {
				for _, size := range []int{1, 3, 50} {
					var want []int64
					for k, cf := range configs {
						restoreB := records.SetSearchBands(cf.probe, cf.in)
						restoreF := records.SetSearchFilterProbe(cf.fil)
						if !desc && size == 1 {
							assertShape(cf.name, in, name, f)
						}
						got := walkSearch(t, r, f, desc, size)
						restoreF()
						restoreB()
						if k == 0 {
							want = got
							if len(got) > 0 {
								nonEmpty++
							}
						} else if !slices.Equal(got, want) {
							t.Fatalf("%q, %s, desc=%v, page %d: %s %d ids, FTS-first %d ids", in, name, desc, size, cf.name, len(got), len(want))
						}
					}
				}
			}
		}
	}
	for _, s := range []records.SearchShape{records.ShapeFTSFirst, records.ShapeHintedIN, records.ShapeExists, records.ShapeExistsNoHint} {
		if !seen[s] {
			t.Errorf("the %s shape never ran: the comparison does not cover it", shapeName(s))
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

// A filter that selects few records drops the index hint: the filter's own index drives the query and
// the full-text rowid is checked per record, instead of scanning the time index across the table.
func TestTinyFilterDropsTheHint(t *testing.T) {
	c, r, arts, _ := shapeDataset(t)
	q := compileWords(t, "common")
	f := records.Filter{ArtifactIDs: arts[1:], Text: q} // artifact b: 30 records of 120
	qs, err := records.SearchQueries(f, false, nil, 101, records.ShapeExistsNoHint)
	if err != nil || len(qs) == 0 {
		t.Fatal(err)
	}
	for _, sq := range qs {
		plan := explain(t, c, sq.SQL, sq.Args)
		if !strings.Contains(plan, "records_artifact") || strings.Contains(plan, "records_ts") || strings.Contains(plan, "INDEXED BY") {
			t.Fatalf("not driven by the filter's index:\n%s", plan)
		}
	}
	// end to end: a filter under the probe limit drops the hint, one at or over it keeps the hint
	restore := records.SetSearchBands(1, 0)
	defer restore()
	for _, tc := range []struct {
		name  string
		limit int
		want  records.SearchShape
	}{
		{"under the filter probe", 31, records.ShapeExistsNoHint},
		{"at the filter probe", 30, records.ShapeExists},
	} {
		undo := records.SetSearchFilterProbe(tc.limit)
		for _, got := range listShapes(t, r, f) {
			if got != tc.want {
				t.Errorf("%s: shape %s, want %s", tc.name, shapeName(got), shapeName(tc.want))
			}
		}
		undo()
	}
	// a prefix decides at once (hinted IN): it is neither probed against the filter nor turned into EXISTS
	undo := records.SetSearchFilterProbe(1 << 30)
	defer undo()
	for _, got := range listShapes(t, r, records.Filter{ArtifactIDs: arts[1:], Text: compileWords(t, "unique1*")}) {
		if got != records.ShapeHintedIN {
			t.Errorf("a prefix with a tiny filter: shape %s, want the hinted IN", shapeName(got))
		}
	}
}

// The filter probe is one bounded statement per page, run only when the query is not FTS-first and a
// filter is present.
func TestFilterProbeIsBoundedAndConditional(t *testing.T) {
	_, r, arts, _ := shapeDataset(t)
	var probes []string
	r.SetQueryLog(func(s string) {
		if strings.HasPrefix(s, "SELECT count(*) FROM (SELECT 1 FROM records r") {
			probes = append(probes, s)
		}
	})
	run := func(f records.Filter) int {
		probes = nil
		if _, err := r.Search(ctx, f, records.Page{Limit: 10}, records.SearchOptions{}); err != nil {
			t.Fatal(err)
		}
		return len(probes)
	}
	q := compileWords(t, "common")
	restore := records.SetSearchBands(1, 0)
	defer restore()
	if n := run(records.Filter{ArtifactIDs: arts[1:], Text: q}); n != 1 || !strings.HasSuffix(probes[0], "LIMIT ?)") {
		t.Fatalf("a filtered common query must run one bounded filter probe, got %q", probes)
	}
	if n := run(records.Filter{Text: q}); n != 0 {
		t.Fatalf("an unfiltered query needs no filter probe, got %q", probes)
	}
	// S7: a filter with a condition no index serves is never probed and keeps the hint
	for _, nf := range []records.Filter{{Types: []string{"message"}, PathPrefix: "/data/1", Text: q}, {ArtifactIDs: arts[1:], Recovered: records.Only, Text: q}, {Types: []string{"message"}, Parsers: []records.ParserRef{{Name: testParser.Name}}, Text: q}} {
		if n := run(nf); n != 0 {
			t.Fatalf("%+v: a filter no index serves must run no filter probe, got %q", nf, probes)
		}
	}
	// a prefix decides without the filter
	if n := run(records.Filter{ArtifactIDs: arts[1:], Text: compileWords(t, "unique1*")}); n != 0 {
		t.Fatalf("a prefix needs no filter probe, got %q", probes)
	}
	restore()
	restore = records.SetSearchBands(1<<30, 1<<30)
	defer restore()
	if n := run(records.Filter{ArtifactIDs: arts[1:], Text: q}); n != 0 {
		t.Fatalf("an FTS-first query needs no filter probe, got %q", probes)
	}
}

// The probes run in stages: a rare query costs one small text probe; a prefix or multi-term query with
// many hits decides at once; a plain word asks the filter first, and only a plain word with no tiny
// filter pays for the large text probe.
func TestProbesRunInStages(t *testing.T) {
	_, r, _ := searchDataset(t)
	defer records.SetSearchBands(100, 300)()
	defer records.SetSearchFilterProbe(1 << 30)()
	var got []string
	r.SetProbeLog(func(kind string, limit int) { got = append(got, fmt.Sprintf("%s:%d", kind, limit)) })
	typed := []string{"message"}
	for _, tc := range []struct {
		name  string
		f     records.Filter
		bands [2]int
		fil   int
		want  string
	}{
		{"rare query", records.Filter{Text: compileWords(t, "common")}, [2]int{1000, 2000}, 1 << 30, "text:1000"},
		{"prefix with a filter decides at once", records.Filter{Types: typed, Text: compileWords(t, "unique1*")}, [2]int{100, 300}, 1 << 30, "text:100"},
		{"phrase decides at once", records.Filter{Text: compileWords(t, `"common word3"`)}, [2]int{100, 300}, 1 << 30, "text:100"},
		{"plain word, tiny filter", records.Filter{Types: typed, Text: compileWords(t, "common")}, [2]int{100, 300}, 1 << 30, "text:100 filter:1073741824"},
		{"plain word, filter not tiny", records.Filter{Types: typed, Text: compileWords(t, "common")}, [2]int{100, 300}, 0, "text:100 filter:0 text:301"},
		{"plain word, no filter", records.Filter{Text: compileWords(t, "common")}, [2]int{100, 300}, 1 << 30, "text:100 text:301"},
	} {
		undoB := records.SetSearchBands(tc.bands[0], tc.bands[1])
		undoF := records.SetSearchFilterProbe(tc.fil)
		got = nil
		if _, err := r.Search(ctx, tc.f, records.Page{Limit: 10}, records.SearchOptions{}); err != nil {
			t.Fatal(err)
		}
		undoF()
		undoB()
		if s := strings.Join(got, " "); s != tc.want {
			t.Errorf("%s: probes %q, want %q", tc.name, s, tc.want)
		}
	}
}
