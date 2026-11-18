//go:build scale

package androidwhatsapp

// TestScaleLookupCost counts, over the 1,000,000-message store of the artparse whatsapp-gen phase, what the parse costs in
// lookups: the side-table rows each message row reaches (Get and Match of the mapping context) and the searches of the case
// each attachment makes (Lookuper.Find). It is manual (go test -tags scale), like TestScaleParse, and records the numbers
// in the log for docs/performance.md.
//
//	MINUTIAE_SCALE_WA_DB=<MINUTIAE_SCALE_DIR>/input/msgstore-1m.db go test -tags scale -run TestScaleLookupCost ./internal/parsers/androidwhatsapp

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

type lookupCounts struct {
	gets, matches map[string]int64
	finds         int64
}

type countCtx struct {
	parse.MapContext
	c *lookupCounts
}

func (m countCtx) Get(table string, id int64) (parse.Row, bool, error) {
	m.c.gets[table]++
	return m.MapContext.Get(table, id)
}

func (m countCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	m.c.matches[table+"."+col]++
	return m.MapContext.Match(table, col, key)
}

type countLookup struct {
	parse.Lookuper
	c *lookupCounts
}

func (l countLookup) Find(glob string) []parse.Artifact {
	l.c.finds++
	return l.Lookuper.Find(glob)
}

type countEmitter struct {
	records, attachments, warnings int64
}

func (e *countEmitter) Emit(_ context.Context, r records.Record) error {
	e.records++
	if a, ok := r.Payload["attachments"].([]any); ok {
		e.attachments += int64(len(a))
	}
	return nil
}
func (e *countEmitter) Warn(context.Context, string, string) error { e.warnings++; return nil }
func (*countEmitter) Note(string, string)                          {}
func (*countEmitter) Progress(int64, int64)                        {}

func TestScaleLookupCost(t *testing.T) {
	path := os.Getenv("MINUTIAE_SCALE_WA_DB")
	if path == "" {
		t.Skip("set MINUTIAE_SCALE_WA_DB to the msgstore-1m.db of the whatsapp-gen phase (manual, see docs/testing.md)")
	}
	data, err := os.ReadFile(path) //nolint:gosec // the operator's work directory
	if err != nil {
		t.Fatal(err)
	}
	h := parsertest.NewHarness(t)
	p := Parser{}
	in := parsertest.Fixture{Name: "scale", Files: map[string][]byte{"db": data}}.Bundle(h, "/data/data/com.whatsapp/databases/msgstore.db").Input(h, p, false)
	in.Budget = parse.NewBudget(1 << 30).View()
	counts := &lookupCounts{gets: map[string]int64{}, matches: map[string]int64{}}
	in.Lookup = countLookup{Lookuper: in.Lookup, c: counts}

	ctx := context.Background()
	out := &countEmitter{}
	censusStart := time.Now()
	d, err := detect(ctx, in) // the census: a second open of the file and one extra scan of each counted table (4I-R2)
	censusWall := time.Since(censusStart)
	if err != nil {
		t.Fatal(err)
	}
	dp := classifyDB(in.Primary.Logical)
	d.note(out, dp)
	if err := d.warn(ctx, out); err != nil {
		t.Fatal(err)
	}
	g, err := checkJoinSchema(ctx, in, d, out)
	if err != nil {
		t.Fatal(err)
	}
	js, err := loadJobs(ctx, in, maxJobs, g)
	if err != nil {
		t.Fatal(err)
	}
	d.judge(js)
	if err := js.emit(ctx, out); err != nil {
		t.Fatal(err)
	}
	steps := d.steps(g, js)
	for i := range steps {
		orig := steps[i].Mapping.Map
		steps[i].Mapping.Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			return orig(countCtx{MapContext: m, c: counts}, row)
		}
	}
	start := time.Now()
	rep, err := rowrun.Run(ctx, in, out, steps)
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var rows int64
	for _, tr := range rep.Tables {
		if tr.Table == "message" {
			rows = tr.Rows
		}
	}
	var gets, matches int64
	for _, n := range counts.gets {
		gets += n
	}
	for _, n := range counts.matches {
		matches += n
	}
	t.Logf("message rows %d, records %d, attachments %d, mapping wall %s, census wall %s (%.1f%% of the parse)", rows, out.records, out.attachments,
		wall.Round(time.Millisecond), censusWall.Round(time.Millisecond), 100*censusWall.Seconds()/(censusWall+wall).Seconds())
	t.Logf("Get calls %d, Match calls %d: %.2f side-table lookups per message row", gets, matches, float64(gets+matches)/float64(rows))
	var tables []string
	for tb := range counts.gets {
		tables = append(tables, tb)
	}
	sort.Strings(tables)
	for _, tb := range tables {
		t.Logf("  Get %-28s %d (%.2f per row)", tb, counts.gets[tb], float64(counts.gets[tb])/float64(rows))
	}
	tables = tables[:0]
	for tb := range counts.matches {
		tables = append(tables, tb)
	}
	sort.Strings(tables)
	for _, tb := range tables {
		t.Logf("  Match %-26s %d (%.2f per row)", tb, counts.matches[tb], float64(counts.matches[tb])/float64(rows))
	}
	if out.attachments > 0 {
		t.Logf("case searches (Lookuper.Find) %d: %.2f per attachment", counts.finds, float64(counts.finds)/float64(out.attachments))
	}
}
