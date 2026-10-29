package dbrecover

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// libraryOracle runs the library alone over the scenario and returns the rows it
// delivers and the rows it counted as duplicates of live data.
func libraryOracle(t *testing.T, f dbFiles) (rows []sqlitefile.RecoveredRow, dup int64) {
	t.Helper()
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachWAL(bytesAt{f.wal}, int64(len(f.wal))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachJournal(bytesAt{f.journal}, int64(len(f.journal))); err != nil {
		t.Fatal(err)
	}
	h := db.History()
	defer h.Release()
	st, err := h.Rows(context.Background(), func(r sqlitefile.RecoveredRow) bool {
		rows = append(rows, r)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, st.DuplicateOfLive
}

// auditInt reads a number out of audit details, which decode as json.Number.
func auditInt(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return -1
	}
	i, err := n.Int64()
	if err != nil {
		return -1
	}
	return i
}

func TestMethodsFilter(t *testing.T) {
	e := newRunEnv(t)
	all, _ := libraryOracle(t, e.f)
	want := map[string]int64{}
	for _, r := range all {
		want[r.Method]++
	}
	if len(want) < 2 || want[sqlitefile.MethodFreelist] == 0 {
		t.Fatalf("the scenario does not offer a choice of methods: %v", want)
	}
	res := mustRun(t, e.c, e.in(), Options{Methods: []string{sqlitefile.MethodFreelist}})
	if res.Counts.Written != want[sqlitefile.MethodFreelist] || len(res.Counts.ByMethod) != 1 {
		t.Errorf("written %d by method %v, want only %d freelist rows", res.Counts.Written, res.Counts.ByMethod, want[sqlitefile.MethodFreelist])
	}
	if got := res.Counts.SkippedByReason["method-filtered"]; got != int64(len(all))-want[sqlitefile.MethodFreelist] {
		t.Errorf("method-filtered %d, want %d", got, int64(len(all))-want[sqlitefile.MethodFreelist])
	}
	for _, r := range storedRecords(t, e.c) {
		if r.Method != sqlitefile.MethodFreelist {
			t.Errorf("record %d has method %q", r.ID, r.Method)
		}
	}
	// naming every method is not a narrowing
	full := newRunEnv(t)
	r2 := mustRun(t, full.c, full.in(), Options{Methods: Methods()})
	if len(r2.Counts.Narrowed) != 0 || r2.Ingest.Outcome != "complete" {
		t.Errorf("all methods: narrowed %v outcome %q", r2.Counts.Narrowed, r2.Ingest.Outcome)
	}
}

func TestMethodsUnknownTokenIsRefused(t *testing.T) {
	for _, tok := range []string{"sqlite-nonsense", "SQLITE-FREELIST", ""} {
		e := newRunEnv(t)
		_, err := Run(context.Background(), e.c, e.in(), Options{Methods: []string{sqlitefile.MethodFreelist, tok}})
		if refusalReason(err) != ReasonBadOption {
			t.Errorf("token %q: err %v, want bad-option", tok, err)
		}
		noIngest(t, e.c)
	}
}

// cappedEnv is a run whose database is a recovered artifact with a cap of 50.
func cappedEnv(t *testing.T) (*evidence.Case, Inputs, dbFiles) {
	t.Helper()
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	img := evidencetest.AddImage(t, c, f.db)
	cap50 := 50
	spec := carvedSpec()
	spec.Confidence = &cap50
	spec.Runs = []evidence.Run{{Offset: 0, Length: int64(len(f.db))}}
	db := evidencetest.AddRecovered(t, c, img, f.db, spec)
	wal := addFile(t, c, "/data/other.db-wal", f.wal)
	jr := addFile(t, c, "/data/other.db-journal", f.journal)
	return c, Inputs{DB: db.ID, WAL: wal.ID, Journal: jr.ID}, f
}

func TestMinConfidenceAppliesAfterCap(t *testing.T) {
	cap50 := 50
	c, in, f := cappedEnv(t)
	all, _ := libraryOracle(t, f)
	var uncappedHigh, keep50 int64
	for _, r := range all {
		if sqlitefile.Confidence(r) >= 60 {
			uncappedHigh++
		}
		if sqlitefile.CapConfidence(sqlitefile.Confidence(r), &cap50) >= 50 {
			keep50++
		}
	}
	if uncappedHigh == 0 {
		t.Fatal("no row has a method confidence of 60 or more: the scenario cannot tell the cap from no cap")
	}
	// 60 is reached by the method confidence but never by the capped one
	res := mustRun(t, c, in, Options{MinConfidence: 60})
	if res.Counts.Written != 0 || res.Counts.SkippedByReason["below-min-confidence"] != int64(len(all)) {
		t.Errorf("min 60 under a cap of 50: written %d skipped %v of %d", res.Counts.Written, res.Counts.SkippedByReason, len(all))
	}
	c2, in2, _ := cappedEnv(t)
	r2 := mustRun(t, c2, in2, Options{MinConfidence: 50})
	if r2.Counts.Written != keep50 || r2.Counts.SkippedByReason["below-min-confidence"] != int64(len(all))-keep50 {
		t.Errorf("min 50: written %d skipped %v, want %d kept of %d", r2.Counts.Written, r2.Counts.SkippedByReason, keep50, len(all))
	}
	for _, r := range storedRecords(t, c2) {
		if r.Confidence == nil || *r.Confidence < 50 {
			t.Errorf("record %d has a confidence below the minimum", r.ID)
		}
	}
}

func TestMinConfidenceBoundsAreRefused(t *testing.T) {
	for _, bad := range []int{-1, 101, 1 << 20} {
		e := newRunEnv(t)
		if _, err := Run(context.Background(), e.c, e.in(), Options{MinConfidence: bad}); refusalReason(err) != ReasonBadOption {
			t.Errorf("min %d: err %v, want bad-option", bad, err)
		}
		noIngest(t, e.c)
	}
	for _, ok := range []int{0, 100} {
		e := newRunEnv(t)
		if _, err := Run(context.Background(), e.c, e.in(), Options{MinConfidence: ok}); err != nil {
			t.Errorf("min %d: %v", ok, err)
		}
	}
}

// claimSchema is a live schema with the given two-column tables, a and b untyped.
func claimSchema(t *testing.T, names ...string) *sqlitefile.Schema {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: testPageSize})
	for _, n := range names {
		b.CreateTable(n, `create table "`+n+`"(a, b)`)
	}
	raw := b.Bytes()
	db, err := sqlitefile.Open(bytesAt{raw}, int64(len(raw)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := db.Live().Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestExcludeTablesSkipsOnlyClaimedFittingTableRows(t *testing.T) {
	// the rule on rows built by hand: only a table row of basis schema or fit,
	// whose table is claimed and whose values fit the live table, is skipped
	upper, lower := string(rune(0xC9)), string(rune(0xE9))
	sch := claimSchema(t, "Messages", "Other", upper+"t"+lower)
	flt := newFilter(Options{ExcludeTables: []ExcludedTable{{Table: "Messages", Parser: "p", Version: "1"}}}, sch)
	vals := func(n int) []sqlitefile.Value {
		v := make([]sqlitefile.Value, n)
		for i := range v {
			v[i] = sqlitefile.Value{Kind: sqlitefile.KindInt, Int: int64(i)}
		}
		return v
	}
	row := func(table, index string, basis sqlitefile.TableBasis) sqlitefile.RecoveredRow {
		return sqlitefile.RecoveredRow{Method: sqlitefile.MethodFreelist, Table: table, Index: index, TableBasis: basis, Values: vals(2)}
	}
	slack := row("Messages", "", sqlitefile.BasisSchema)
	slack.Method = sqlitefile.MethodPageSlack
	wide := row("Messages", "", sqlitefile.BasisSchema)
	wide.Values = vals(3) // three values do not fit the two live columns
	for _, tc := range []struct {
		name string
		r    sqlitefile.RecoveredRow
		keep bool
	}{
		{"schema row, same case", row("Messages", "", sqlitefile.BasisSchema), false},
		{"schema row, ASCII fold", row("mESSAGES", "", sqlitefile.BasisSchema), false},
		{"fit row that fits", row("Messages", "", sqlitefile.BasisFit), false},
		{"schema row not fitting the live table", wide, true},
		{"guess row", row("Messages", "", sqlitefile.BasisGuess), true},
		{"no-table row", row("", "", sqlitefile.BasisNone), true},
		{"index entry of the table", row("Messages", "idx_m", sqlitefile.BasisSchema), true},
		{"other table", row("Other", "", sqlitefile.BasisSchema), true},
		{"slack row", slack, true},
	} {
		keep, why := flt.keep(tc.r, 80)
		if keep != tc.keep || (!keep && why != "excluded-by-claim") {
			t.Errorf("%s: keep %v reason %q", tc.name, keep, why)
		}
	}
	// the fold is ASCII only: a non-ASCII letter does not fold
	uf := newFilter(Options{ExcludeTables: []ExcludedTable{{Table: upper + "t" + lower, Parser: "p", Version: "1"}}}, sch)
	if keep, _ := uf.keep(row(lower+"t"+upper, "", sqlitefile.BasisSchema), 80); !keep {
		t.Error("a non-ASCII case difference was folded")
	}
	if keep, _ := uf.keep(row(upper+"T"+lower, "", sqlitefile.BasisSchema), 80); keep {
		t.Error("the ASCII letters of a non-ASCII name were not folded")
	}

	// and through a run: every table row of t at basis schema or fit is skipped; every row
	// of the scenario has the two values of t(a, b), so each fits the live table strictly
	e := newRunEnv(t)
	all, _ := libraryOracle(t, e.f)
	var claimed, fitRows int64
	for _, r := range all {
		if r.Table == "t" && r.Index == "" && (r.TableBasis == sqlitefile.BasisSchema || r.TableBasis == sqlitefile.BasisFit) && len(r.Values) == 2 {
			claimed++
			if r.TableBasis == sqlitefile.BasisFit {
				fitRows++
			}
		}
	}
	if claimed == 0 || fitRows == 0 {
		t.Fatalf("the scenario has %d claimable rows of table t, %d of them at basis fit", claimed, fitRows)
	}
	res := mustRun(t, e.c, e.in(), Options{ExcludeTables: []ExcludedTable{{Table: "T", Parser: "p", Version: "1"}}})
	if got := res.Counts.SkippedByReason["excluded-by-claim"]; got != claimed {
		t.Errorf("excluded-by-claim %d, want %d", got, claimed)
	}
	if res.Counts.Written != int64(len(all))-claimed {
		t.Errorf("written %d, want %d", res.Counts.Written, int64(len(all))-claimed)
	}
	if !slices.Contains(res.Counts.Narrowed, "exclude-tables") {
		t.Errorf("narrowed %v", res.Counts.Narrowed)
	}
}

func TestExcludedTablesAreAudited(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{ExcludeTables: []ExcludedTable{{Table: "t", Parser: "chat-parser", Version: "2.1"}}})
	es := readAudit(t, e.c)
	start := lastEntry(t, es, "analysis.start").Details
	ex, _ := start["excluded_tables"].([]any)
	if len(ex) != 1 {
		t.Fatalf("excluded_tables %v", start["excluded_tables"])
	}
	m, _ := ex[0].(map[string]any)
	if m["table"] != "t" || m["parser"] != "chat-parser" || m["version"] != "2.1" {
		t.Errorf("excluded table entry %v", m)
	}
	end := lastEntry(t, es, "analysis.end").Details
	skipped, _ := end["skipped_by_reason"].(map[string]any)
	if n := auditInt(skipped["excluded-by-claim"]); n != res.Counts.SkippedByReason["excluded-by-claim"] || n == 0 {
		t.Errorf("analysis.end skipped_by_reason %v, counts %v", skipped, res.Counts.SkippedByReason)
	}
}

func TestSkippedRowsAreCountedNeverSilent(t *testing.T) {
	// Every row of the scenario belongs to table t, so a claim on t would exclude them all and
	// leave no row for the writer to reject: in the exclude case the rows of the WAL and the
	// journal are named by guess (never claimed) before they are mapped.
	hooks := map[string]func(sqlitefile.RecoveredRow) sqlitefile.RecoveredRow{
		"exclude": func(r sqlitefile.RecoveredRow) sqlitefile.RecoveredRow {
			if r.Loc.File != sqlitefile.FileDB {
				r.TableBasis = sqlitefile.BasisGuess
			}
			return r
		},
	}
	for name, opts := range map[string]Options{
		"none":    {},
		"methods": {Methods: []string{sqlitefile.MethodFreelist}},
		"conf":    {MinConfidence: 70},
		"exclude": {ExcludeTables: []ExcludedTable{{Table: "t", Parser: "p", Version: "1"}}},
		"all":     {Methods: []string{sqlitefile.MethodFreelist, sqlitefile.MethodWALPrior}, MinConfidence: 40, ExcludeTables: []ExcludedTable{{Table: "x", Parser: "p", Version: "1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRunEnv(t)
			all, dup := libraryOracle(t, e.f)
			if h := hooks[name]; h != nil {
				defer SetRowHookForTest(h)()
			}
			n := 0
			restore := SetCheckForTest(func(p map[string]any) error {
				if n++; n == 2 {
					return records.ErrRecordTooLarge
				}
				return checkPayload(p)
			})
			var lastProgress Progress
			opts.Progress = func(p Progress) { lastProgress = p }
			res := mustRun(t, e.c, e.in(), opts)
			restore()
			c := res.Counts
			if c.SkippedByReason["writer-rejected"] <= 0 {
				t.Errorf("the rejected row was not counted as writer-rejected: %v", c.SkippedByReason)
			}
			if lastProgress.Skipped != sumMap(c.SkippedByReason) || lastProgress.Rows != c.Written {
				t.Errorf("final progress rows %d skipped %d, counts written %d skipped %v", lastProgress.Rows, lastProgress.Skipped, c.Written, c.SkippedByReason)
			}
			if got := c.Written + sumMap(c.SkippedByReason); got != int64(len(all)) {
				t.Errorf("written %d + skipped %v = %d, the library delivered %d rows", c.Written, c.SkippedByReason, got, len(all))
			}
			if c.DuplicateOfLive != dup {
				t.Errorf("duplicate_of_live %d, library counted %d", c.DuplicateOfLive, dup)
			}
			end := lastEntry(t, readAudit(t, e.c), "analysis.end").Details
			sk, _ := end["skipped_by_reason"].(map[string]any)
			var audited int64
			for _, v := range sk {
				audited += auditInt(v)
			}
			if audited != sumMap(c.SkippedByReason) {
				t.Errorf("audited skips %v, counted %v", sk, c.SkippedByReason)
			}
		})
	}
}

func visibleRecords(t *testing.T, c *evidence.Case) []records.Row {
	t.Helper()
	r, err := records.NewReader(c)
	if err != nil {
		t.Fatal(err)
	}
	var out []records.Row
	cursor := ""
	for {
		res, err := r.List(context.Background(), records.Filter{}, records.Page{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res.Rows...)
		if res.NextCursor == "" {
			return out
		}
		cursor = res.NextCursor
	}
}

func TestNarrowedRunNeverSupersedesAFullRun(t *testing.T) {
	e := newRunEnv(t)
	full := mustRun(t, e.c, e.in(), Options{})
	if full.Ingest.Outcome != "complete" {
		t.Fatalf("full run %+v", full.Ingest)
	}
	nar, err := Run(context.Background(), e.c, e.in(), Options{Methods: []string{sqlitefile.MethodFreelist}, AllowReingest: true})
	if err != nil {
		t.Fatalf("narrowed run: %v", err)
	}
	if nar.Ingest.Outcome != "incomplete" {
		t.Errorf("narrowed ingest %q, want incomplete", nar.Ingest.Outcome)
	}
	var fullVisible, narVisible int64
	for _, row := range visibleRecords(t, e.c) {
		switch row.IngestID {
		case full.Ingest.IngestID:
			fullVisible++
		case nar.Ingest.IngestID:
			narVisible++
		}
	}
	if fullVisible != full.Counts.Written || fullVisible == 0 {
		t.Errorf("records of the full run visible in the default list: %d of %d", fullVisible, full.Counts.Written)
	}
	if narVisible != nar.Counts.Written {
		t.Errorf("narrowed records visible %d, written %d", narVisible, nar.Counts.Written)
	}
	for _, row := range storedRecords(t, e.c) {
		if row.IngestID == full.Ingest.IngestID && row.Superseded {
			t.Errorf("record %d of the full run was superseded by a narrowed run", row.ID)
		}
	}
	end := lastEntry(t, readAudit(t, e.c), "analysis.end").Details
	nl, _ := end["narrowed"].([]any)
	if len(nl) != 1 || nl[0] != "methods" {
		t.Errorf("analysis.end narrowed %v, want [methods]", end["narrowed"])
	}
}

func TestUnnarrowedRunSupersedes(t *testing.T) {
	e := newRunEnv(t)
	first := mustRun(t, e.c, e.in(), Options{})
	second := mustRun(t, e.c, e.in(), Options{AllowReingest: true})
	if second.Ingest.Outcome != "complete" || len(second.Counts.Narrowed) != 0 {
		t.Fatalf("second run %+v narrowed %v", second.Ingest, second.Counts.Narrowed)
	}
	var firstVisible, secondVisible int64
	for _, row := range visibleRecords(t, e.c) {
		switch row.IngestID {
		case first.Ingest.IngestID:
			firstVisible++
		case second.Ingest.IngestID:
			secondVisible++
		}
	}
	if firstVisible != 0 || secondVisible != second.Counts.Written || secondVisible == 0 {
		t.Errorf("visible after a full re-run: first %d, second %d of %d", firstVisible, secondVisible, second.Counts.Written)
	}
}
