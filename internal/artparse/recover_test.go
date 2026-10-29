package artparse_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

const recFixtureDir = "../sqlitefile/testdata"

const (
	dbPathA = "/data/data/com.a/databases/app.db"
	dbPathB = "/data/data/com.b/databases/app.db"
)

func fixtureFile(t testing.TB, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join(recFixtureDir, name+".gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recParser is a claiming RowMapper over the table people / notes of the committed sqlitefile fixtures.
type recParser struct {
	claimParser
	mapFn   func(parse.MapContext, parse.Row) ([]records.Record, error)
	parseFn func(context.Context, *parse.Input, parse.Emitter) error
}

func newRecParser(name string, tables ...string) *recParser {
	return &recParser{claimParser: claimParser{name: name, status: parse.Applicable, claims: tables}}
}

func (p *recParser) Parse(ctx context.Context, in *parse.Input, em parse.Emitter) error {
	if p.parseFn != nil {
		return p.parseFn(ctx, in, em)
	}
	return nil
}

func defaultMap(_ parse.MapContext, row parse.Row) ([]records.Record, error) {
	id, _ := row.Rowid()
	return []records.Record{{Type: message.Type, Summary: fmt.Sprintf("row %d", id), Payload: recordstest.ValidPayload(message.Type)}}, nil
}

func (p *recParser) Mappings() []parse.TableMapping {
	fn := p.mapFn
	if fn == nil {
		fn = defaultMap
	}
	var out []parse.TableMapping
	for _, t := range p.claims {
		out = append(out, parse.TableMapping{Table: t, Map: fn})
	}
	return out
}

// recCase is a case with one acquisition holding the fixture's database (and WAL) at each path.
type recCase struct {
	t   *testing.T
	c   *evidence.Case
	dbs []evidence.ManifestRecord
	wal []evidence.ManifestRecord
}

func newRecCase(t *testing.T, fixture string, withWAL bool, paths ...string) *recCase {
	t.Helper()
	if len(paths) == 0 {
		paths = []string{dbPathA}
	}
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rc := &recCase{t: t, c: c}
	db := fixtureFile(t, fixture+".db")
	var wal []byte
	if withWAL {
		wal = fixtureFile(t, fixture+".db-wal")
	}
	for _, p := range paths {
		rc.dbs = append(rc.dbs, putFile(t, c, "D1", "A1", p, string(db)))
		if withWAL {
			rc.wal = append(rc.wal, putFile(t, c, "D1", "A1", p+"-wal", string(wal)))
		}
	}
	return rc
}

func (rc *recCase) host(mod func(*artparse.Options), ps ...artparse.Registered) *artparse.Host {
	rc.t.Helper()
	return hostWith(rc.t, rc.c, func(o *artparse.Options) {
		artparse.SkipLimitMinimums(o)
		if mod != nil {
			mod(o)
		}
	}, ps...)
}

func (rc *recCase) rows(f records.Filter) []records.Row {
	rc.t.Helper()
	rd, err := records.NewReader(rc.c)
	if err != nil {
		rc.t.Fatal(err)
	}
	res, err := rd.List(context.Background(), f, records.Page{Limit: 500})
	if err != nil {
		rc.t.Fatal(err)
	}
	if res.NextCursor != "" {
		rc.t.Fatal("more rows than one page: the fixture grew")
	}
	return res.Rows
}

func byParser(name string) records.Filter {
	return records.Filter{Parsers: []records.ParserRef{{Name: name}}}
}

func (rc *recCase) full(id int64) records.Full {
	rc.t.Helper()
	rd, err := records.NewReader(rc.c)
	if err != nil {
		rc.t.Fatal(err)
	}
	f, err := rd.Get(context.Background(), id)
	if err != nil {
		rc.t.Fatal(err)
	}
	return f
}

func (rc *recCase) audit(actions ...string) []evidence.AuditEntry {
	rc.t.Helper()
	all, err := rc.c.ReadAudit()
	if err != nil {
		rc.t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		if len(actions) == 0 || slices.Contains(actions, e.Action) {
			out = append(out, e)
		}
	}
	return out
}

func recoverOK(t *testing.T, h *artparse.Host, opt artparse.RecoverOptions) artparse.RunSummary {
	t.Helper()
	sum, err := h.RecoverTyped(context.Background(), opt)
	if err != nil {
		t.Fatalf("RecoverTyped: %v", err)
	}
	return sum
}

func liveRun(t *testing.T, h *artparse.Host, reparse bool) artparse.RunSummary {
	t.Helper()
	sum, err := h.Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Reparse: reparse}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum
}

func regRec(t *testing.T, p *recParser) artparse.Registered { return regFake(t, p, p.name) }

// The typed pass records under "<name>.recover" (same version, the injected recovery hash): its records
// are the recovered ones and the live parser's identity holds none.
func TestTypedRecoveryIdentityIsSeparate(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	reg := regRec(t, newRecParser("rm", "people"))
	rid, _ := reg.RecoveryIdentity()
	sum := recoverOK(t, rc.host(nil, reg), artparse.RecoverOptions{})
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs: %+v", len(sum.Jobs), sum.Jobs)
	}
	j := sum.Jobs[0]
	if j.Outcome != artparse.OutcomeComplete || j.Parser != "rm.recover" || j.Version != "1.0.0" || j.Hash != rid.Hash || j.Hash == reg.Identity().Hash {
		t.Fatalf("job %+v, want complete under rm.recover@1.0.0 with the recovery hash %s", j, rid.Hash)
	}
	if !slices.Equal(j.Claims, []string{"people"}) || j.LiveParser != "rm 1.0.0" {
		t.Errorf("claims %v live parser %q", j.Claims, j.LiveParser)
	}
	got := rc.rows(byParser("rm.recover"))
	if len(got) == 0 || int64(len(got)) != j.Records {
		t.Fatalf("%d records under rm.recover, job says %d", len(got), j.Records)
	}
	for _, r := range got {
		if !r.Deleted || !r.Recovered || r.Method == "" || r.Confidence == nil || r.Locator == "" || r.Parser.Name != "rm.recover" || r.Parser.Hash != rid.Hash {
			t.Fatalf("record %+v is not a stamped recovered record of the recover identity", r)
		}
	}
	if live := rc.rows(byParser("rm")); len(live) != 0 {
		t.Errorf("%d records under the live identity after a typed pass alone", len(live))
	}
}

// A typed pass and a live parse never supersede each other; only a run of the same identity does.
func TestTypedRecoveryNeverSupersedesLive(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	p := newRecParser("rm", "people")
	p.parseFn = func(_ context.Context, in *parse.Input, em parse.Emitter) error {
		return em.Emit(context.Background(), records.Record{Type: message.Type, ArtifactID: in.Primary.ID, Summary: "live", Payload: recordstest.ValidPayload(message.Type)})
	}
	h := rc.host(nil, regRec(t, p))
	current := func(name string) int { return len(rc.rows(byParser(name))) }

	if s := liveRun(t, h, false); s.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("live run: %+v", s.Jobs[0])
	}
	recovered := recoverOK(t, h, artparse.RecoverOptions{}).Jobs[0]
	if recovered.Outcome != artparse.OutcomeComplete || recovered.Records == 0 {
		t.Fatalf("typed pass: %+v", recovered)
	}
	n := int(recovered.Records)
	if current("rm") != 1 || current("rm.recover") != n {
		t.Fatalf("live %d recovered %d after both, want 1 and %d", current("rm"), current("rm.recover"), n)
	}
	if s := liveRun(t, h, true); s.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("live reparse: %+v", s.Jobs[0])
	}
	all := records.Filter{IncludeSuperseded: true, Parsers: []records.ParserRef{{Name: "rm"}}}
	if current("rm") != 1 || len(rc.rows(all)) != 2 || current("rm.recover") != n {
		t.Errorf("after the live reparse: live current %d (of %d), recovered %d, want 1 (of 2) and %d", current("rm"), len(rc.rows(all)), current("rm.recover"), n)
	}
	// a typed re-run supersedes only the earlier typed run
	if s := recoverOK(t, h, artparse.RecoverOptions{Selection: artparse.Selection{Reparse: true}}); s.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("typed reparse: %+v", s.Jobs[0])
	}
	allRec := records.Filter{IncludeSuperseded: true, Parsers: []records.ParserRef{{Name: "rm.recover"}}}
	if current("rm.recover") != n || len(rc.rows(allRec)) != 2*n || current("rm") != 1 {
		t.Errorf("after the typed reparse: recovered current %d (of %d), live %d, want %d (of %d) and 1", current("rm.recover"), len(rc.rows(allRec)), current("rm"), n, 2*n)
	}
}

// The typed record's stamps come from the row, over a real database (the emitter's rules are pinned by
// TestTypedRecoveryStampsRowFacts and its siblings in recovery_emitter_test.go).
func TestTypedRecoveryStampsRealRows(t *testing.T) {
	rc := newRecCase(t, "builder-wal-generations", true)
	reg := regRec(t, newRecParser("rm", "notes"))
	sum := recoverOK(t, rc.host(nil, reg), artparse.RecoverOptions{})
	if j := sum.Jobs[0]; j.Outcome != artparse.OutcomeComplete || j.Records == 0 {
		t.Fatalf("job %+v", j)
	}
	sawWAL := false
	for _, r := range rc.rows(byParser("rm.recover")) {
		f := rc.full(r.ID)
		var p map[string]any
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			t.Fatal(err)
		}
		rec, _ := p["recovery"].(map[string]any)
		if rec["via"] != "typed-recovery" || rec["relation"] == nil || rec["file"] == nil {
			t.Fatalf("payload.recovery = %v", rec)
		}
		if r.ArtifactID == rc.wal[0].ID {
			sawWAL = true
			if rec["file"] != "wal" || rec["wal"] == nil {
				t.Errorf("a record on the WAL artifact: payload.recovery = %v", rec)
			}
		}
		if !strings.HasPrefix(r.Locator, "sqlite:table=notes;") || r.Range == nil {
			t.Errorf("record %+v lacks a locator or a range", r)
		}
	}
	if !sawWAL {
		t.Error("no typed record lives on the WAL artifact")
	}
}

func TestRecoverTypedAuditsJobs(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	reg := regRec(t, newRecParser("rm", "people"))
	sum := recoverOK(t, rc.host(nil, reg), artparse.RecoverOptions{MinConfidence: 0, OnlyPrimary: rc.dbs[0].ID, ExpectCompanions: map[string]string{"wal": ""}})
	if sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("job %+v", sum.Jobs[0])
	}
	all := rc.audit()
	idx := func(action string) int {
		for i, e := range all {
			if e.Action == action {
				return i
			}
		}
		return -1
	}
	as, js, je := idx(artparse.ActionAnalysisStart), idx(artparse.ActionParseJobStart), idx(artparse.ActionParseJobEnd)
	if as < 0 || js < as || je < js {
		t.Fatalf("analysis.start %d parse.job.start %d parse.job.end %d, want that order", as, js, je)
	}
	between := 0
	for _, e := range all[js+1 : je] {
		if strings.HasPrefix(e.Action, "records.") {
			between++
		}
	}
	if between == 0 {
		t.Error("the writer's entries are not between parse.job.start and parse.job.end")
	}
	start := decodeEntry[artparse.AnalysisStart](t, all[as])
	if start.Op != "parse-recover" || start.RecoverStart == nil || start.OnlyPrimary != rc.dbs[0].ID || start.MinConfidence != 0 || start.ExpectCompanions["wal"] != "" || len(start.Parsers) != 1 || start.Parsers[0].Name != "rm.recover" {
		t.Errorf("analysis.start = %+v", start)
	}
	// RecoverTyped resolves no backups: the audit states it as unknown (null and backups_resolved false), never as an
	// empty list, which would claim that the case has no backup (4E carry-over, Task 16).
	if raw := all[as].Details["backups"]; raw != nil || all[as].Details["backups_resolved"] != false || start.Backups != nil || start.RecoverStart.BackupsResolved {
		t.Errorf("analysis.start backups = %#v resolved %#v, want null and false", raw, all[as].Details["backups_resolved"])
	}
	jobStart := decodeEntry[artparse.JobStart](t, all[js])
	if jobStart.RecoverJobStart == nil || !slices.Equal(jobStart.ClaimedTables, []string{"people"}) || jobStart.LiveParser.Name != "rm" || jobStart.Parser.Name != "rm.recover" {
		t.Errorf("parse.job.start = %+v", jobStart)
	}
	end := decodeEntry[artparse.JobEnd](t, all[je])
	if end.RecoverJobEnd == nil || end.Narrowed == nil || len(end.Narrowed) != 0 || end.UnmappedRecovered != 0 || end.BelowMinConfidence != 0 || end.Outcome != artparse.OutcomeComplete {
		t.Errorf("parse.job.end = %+v", end)
	}
	// a live run's entries carry none of these keys
	live := newRecCase(t, "sqlite3-freelist", false)
	liveRun(t, live.host(nil, regRec(t, newRecParser("rm", "people"))), false)
	for _, e := range live.audit(artparse.ActionParseJobStart, artparse.ActionParseJobEnd) {
		for _, k := range []string{"claimed_tables", "live_parser", "unmapped_recovered", "below_min_confidence", "narrowed"} {
			if _, ok := e.Details[k]; ok {
				t.Errorf("%s of a live run carries %s", e.Action, k)
			}
		}
	}
}

func TestRecoverTypedOnlyPrimaryAndExpectCompanions(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false, dbPathA, dbPathB)
	reg := regRec(t, newRecParser("rm", "people"))
	var opens atomic.Int64
	h := rc.host(func(o *artparse.Options) { artparse.WithOnOpen(o, func(string) { opens.Add(1) }) }, reg)

	// ExpectCompanions that the bundle does not meet: skipped before anything is opened
	sum := recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: rc.dbs[1].ID, ExpectCompanions: map[string]string{"wal": "other"}})
	if len(sum.Jobs) != 1 || sum.Jobs[0].ArtifactID != rc.dbs[1].ID {
		t.Fatalf("OnlyPrimary: %+v", sum.Jobs)
	}
	if j := sum.Jobs[0]; j.Outcome != artparse.OutcomeSkipped || !strings.HasPrefix(j.Reason, "bundle-differs: wal (none) vs other") || j.IngestID != "" {
		t.Errorf("job %+v, want skipped bundle-differs with no ingest", j)
	}
	if opens.Load() != 0 || len(rc.rows(byParser("rm.recover"))) != 0 {
		t.Errorf("%d artifacts opened, %d records written for a skipped job", opens.Load(), len(rc.rows(byParser("rm.recover"))))
	}
	if sum.Class() != artparse.ClassOK {
		t.Errorf("class %v, a bundle-differs skip is not a failure", sum.Class())
	}

	// OnlyPrimary alone selects one of the two jobs
	sum = recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: rc.dbs[0].ID})
	if len(sum.Jobs) != 1 || sum.Jobs[0].ArtifactID != rc.dbs[0].ID || sum.Jobs[0].Job != 1 || sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Errorf("OnlyPrimary: %+v", sum.Jobs)
	}
	// no restriction: the other database is still to do
	sum = recoverOK(t, h, artparse.RecoverOptions{})
	outcomes := map[string]string{}
	for _, j := range sum.Jobs {
		outcomes[j.ArtifactID] = j.Outcome
	}
	if outcomes[rc.dbs[0].ID] != artparse.OutcomeSkipped || outcomes[rc.dbs[1].ID] != artparse.OutcomeComplete {
		t.Errorf("outcomes %v", outcomes)
	}
}

func TestRecoverTypedMinConfidenceNarrows(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	h := rc.host(nil, regRec(t, newRecParser("rm", "people")))
	full := recoverOK(t, h, artparse.RecoverOptions{}).Jobs[0]
	if full.Outcome != artparse.OutcomeComplete || full.Records == 0 {
		t.Fatalf("full run %+v", full)
	}
	narrowed := recoverOK(t, h, artparse.RecoverOptions{MinConfidence: 100, Selection: artparse.Selection{Reparse: true}})
	j := narrowed.Jobs[0]
	if j.Outcome != artparse.OutcomeIncomplete || j.Reason != "narrowed: min-confidence" || !slices.Equal(j.Narrowed, []string{"min-confidence"}) {
		t.Fatalf("narrowed job %+v", j)
	}
	if j.BelowMinConfidence != full.Records || j.Records != 0 {
		t.Errorf("below min %d records %d, want every one of the %d records dropped and counted", j.BelowMinConfidence, j.Records, full.Records)
	}
	if narrowed.Class() != artparse.ClassPartial {
		t.Errorf("class %v, want partial", narrowed.Class())
	}
	// the narrowed run never supersedes the full one
	if got := len(rc.rows(byParser("rm.recover"))); int64(got) != full.Records {
		t.Errorf("%d current records after a narrowed re-run, want the full run's %d", got, full.Records)
	}
	end := decodeEntry[artparse.JobEnd](t, rc.audit(artparse.ActionParseJobEnd)[1])
	if end.RecoverJobEnd == nil || end.BelowMinConfidence != full.Records || !slices.Equal(end.Narrowed, []string{"min-confidence"}) {
		t.Errorf("parse.job.end = %+v", end)
	}
	if _, err := h.RecoverTyped(context.Background(), artparse.RecoverOptions{MinConfidence: 101}); err == nil {
		t.Error("a minimum confidence of 101 was accepted")
	}
}

func TestRecoverTypedPanicAbortsJob(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false, dbPathA, dbPathB)
	p := newRecParser("rm", "people")
	var calls atomic.Int64
	p.mapFn = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
		if calls.Add(1) == 2 {
			panic("mapper bug")
		}
		return defaultMap(mc, row)
	}
	sum := recoverOK(t, rc.host(nil, regRec(t, p)), artparse.RecoverOptions{})
	if len(sum.Jobs) != 2 {
		t.Fatalf("%d jobs", len(sum.Jobs))
	}
	first, second := sum.Jobs[0], sum.Jobs[1]
	// the library's own guard turns the panic in the visit into an error before the host's does
	if first.Outcome != artparse.OutcomeIncomplete || !strings.Contains(first.Reason, "mapper bug") {
		t.Errorf("first job %+v, want incomplete after the panic", first)
	}
	if second.Outcome != artparse.OutcomeComplete || second.Records == 0 {
		t.Errorf("second job %+v, want it to run to the end", second)
	}
	kept := rc.rows(records.Filter{IncludeSuperseded: true, ArtifactIDs: []string{first.ArtifactID}, Parsers: []records.ParserRef{{Name: "rm.recover"}}})
	if len(kept) != 1 {
		t.Fatalf("%d records of the aborted job, want the first row's", len(kept))
	}
	if g := rc.full(kept[0].ID); g.Gap == nil && (g.Run == nil || g.Run.Outcome == "complete") {
		t.Errorf("the kept record is not flagged as belonging to an incomplete run: %+v", g.Run)
	}
	if sum.Class() != artparse.ClassPartial {
		t.Errorf("class %v", sum.Class())
	}
}

func TestRecoverTypedSkipsAlreadyRecovered(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	h := rc.host(nil, regRec(t, newRecParser("rm", "people")))
	recoverOK(t, h, artparse.RecoverOptions{})
	j := recoverOK(t, h, artparse.RecoverOptions{}).Jobs[0]
	if j.Outcome != artparse.OutcomeSkipped || j.Reason != "already parsed by rm.recover 1.0.0" {
		t.Errorf("second call %+v, want skipped: already parsed by rm.recover 1.0.0", j)
	}
}

// refLocators walks the history of the fixture with the library alone and returns the locator of every
// row the typed pass is given (ClaimMatch accepts it) whose rowid is lost or odd.
func refUnmapped(t *testing.T, db []byte, table string) (locators []string, total int) {
	t.Helper()
	lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sch, err := lib.Live().Schema(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := lib.History()
	defer h.Release()
	_, err = h.Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
		if sch.ClaimMatch(rr, func(n string) bool { return strings.EqualFold(n, table) }) != sqlitefile.ClaimYes {
			return true
		}
		total++
		if rr.Rowid == nil || *rr.Rowid%2 != 0 {
			locators = append(locators, sqlitefile.RecoveredLocator(rr))
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return locators, total
}

// The host keeps the digests of the unmapped rows' locators for the composed command: exactly
// dbrecover.LocatorDigest(sqlitefile.RecoveredLocator(row)), sorted and without duplicates.
func TestRecoverTypedKeepsUnmappedLocators(t *testing.T) {
	odd := func(_ parse.MapContext, row parse.Row) ([]records.Record, error) {
		if id, ok := row.Rowid(); !ok || id%2 != 0 {
			return nil, rowrun.Unmappable("odd")
		}
		return defaultMap(nil, row)
	}
	build := func(mod func(*artparse.Options)) (*recCase, artparse.JobResult) {
		rc := newRecCase(t, "sqlite3-freelist", false)
		p := newRecParser("rm", "people")
		p.mapFn = odd
		return rc, recoverOK(t, rc.host(mod, regRec(t, p)), artparse.RecoverOptions{}).Jobs[0]
	}
	rc, j := build(nil)
	locs, total := refUnmapped(t, fixtureFile(t, "sqlite3-freelist.db"), "people")
	if len(locs) == 0 || len(locs) == total {
		t.Fatalf("%d of %d rows unmapped: the fixture does not exercise both", len(locs), total)
	}
	var want [][16]byte
	for _, l := range locs {
		want = append(want, dbrecover.LocatorDigest(l))
	}
	slices.SortFunc(want, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
	want = slices.Compact(want)
	if j.Outcome != artparse.OutcomeComplete || j.Unmapped != int64(len(locs)) || j.UnmappedOverflow {
		t.Fatalf("job %+v, want complete with %d unmapped rows", j, len(locs))
	}
	if !slices.Equal(j.UnmappedLocators, want) {
		t.Errorf("%d digests, want the %d LocatorDigests of the library's locators", len(j.UnmappedLocators), len(want))
	}
	// the same digests as the warnings' locators say
	var warned [][16]byte
	for _, e := range rc.audit(evidence.ActionAnalysisWarning) {
		if r, _ := e.Details["reason"].(string); strings.Contains(r, "recovered row not mapped") {
			warned = append(warned, dbrecover.LocatorDigest(e.Details["path"].(string)))
		}
	}
	slices.SortFunc(warned, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(slices.Compact(warned), want) {
		t.Errorf("the warnings' locators digest to %d values, want %d", len(warned), len(want))
	}
	end := decodeEntry[artparse.JobEnd](t, rc.audit(artparse.ActionParseJobEnd)[0])
	if end.RecoverJobEnd == nil || end.UnmappedRecovered != int64(len(locs)) {
		t.Errorf("parse.job.end = %+v", end)
	}

	// past the bound: the overflow flag, and no more than the bound
	_, jo := build(func(o *artparse.Options) { artparse.WithMaxUnmapped(o, 2) })
	if !jo.UnmappedOverflow || len(jo.UnmappedLocators) != 2 || jo.Unmapped != int64(len(locs)) {
		t.Errorf("bound 2: overflow %v, %d digests, %d unmapped; want true, 2, %d", jo.UnmappedOverflow, len(jo.UnmappedLocators), jo.Unmapped, len(locs))
	}
}

// A claimed row whose facts were invalid is neither typed nor unmapped: the job is incomplete (so its
// claims are not honored), non-zero, and says how many. The count is the runner's, not the (capped) notes.
func TestRecoverTypedSkippedInvalidFactsMakeTheJobIncomplete(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	reg := regRec(t, newRecParser("rm", "people"))
	h := rc.host(func(o *artparse.Options) {
		artparse.WithTypedRunner(o, func(context.Context, *parse.Input, parse.RecoveryEmitter, []parse.TableMapping, []parse.TableClaim) (recrun.Report, error) {
			return recrun.Report{Skipped: 3}, nil // no notes, no warnings: only the count says it
		})
	}, reg)
	sum := recoverOK(t, h, artparse.RecoverOptions{})
	j := sum.Jobs[0]
	if j.Outcome != artparse.OutcomeIncomplete || j.SkippedInvalid != 3 || !strings.Contains(j.Reason, "3 claimed recovered rows skipped") {
		t.Fatalf("job %+v, want incomplete naming the 3 skipped rows", j)
	}
	if sum.Class() != artparse.ClassPartial {
		t.Errorf("class %v, want partial (a non-zero exit)", sum.Class())
	}
	end := decodeEntry[artparse.JobEnd](t, rc.audit(artparse.ActionParseJobEnd)[0])
	if end.RecoverJobEnd == nil || end.SkippedInvalidFacts != 3 || end.Outcome != artparse.OutcomeIncomplete {
		t.Errorf("parse.job.end = %+v", end)
	}
}

func TestRecoverTypedRefusesAParserThatClaimsNothing(t *testing.T) {
	rc := newRecCase(t, "sqlite3-freelist", false)
	h := rc.host(nil, regRec(t, newRecParser("rm", "people")), register(t, "plain", "1.0.0"))
	_, err := h.RecoverTyped(context.Background(), artparse.RecoverOptions{Selection: artparse.Selection{Parsers: []artparse.ParserRef{{Name: "plain"}}}})
	if !errors.Is(err, artparse.ErrSelection) || !strings.Contains(err.Error(), "claims no tables") {
		t.Errorf("RecoverTyped = %v, want ErrSelection: claims no tables", err)
	}
	// without --parser only the claiming parser has a job
	sum := recoverOK(t, h, artparse.RecoverOptions{})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Parser != "rm.recover" {
		t.Errorf("jobs %+v", sum.Jobs)
	}
}

// A recovered database is admitted by OnlyPrimary (the composed command's input), and its records carry
// the artifact's cap and method.
func TestRecoverTypedOnRecoveredPrimary(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	c55 := 55
	d := &evidence.Derivation{
		ParentID: "missing", FSType: "ext4", FSPath: dbPathA,
		Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &c55},
	}
	db := put(t, c, "D1", "A1", "rec", evidence.Source{Kind: "recover", RemotePath: dbPathA, Derived: d}, string(fixtureFile(t, "sqlite3-freelist.db")))
	rc := &recCase{t: t, c: c}
	h := rc.host(nil, regRec(t, newRecParser("rm", "people")))
	if s := recoverOK(t, h, artparse.RecoverOptions{}); len(s.Jobs) != 0 {
		t.Fatalf("a recovered database is a job without being named: %+v", s.Jobs)
	}
	sum := recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: db.ID})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeComplete || sum.Jobs[0].Records == 0 {
		t.Fatalf("jobs %+v", sum.Jobs)
	}
	for _, r := range rc.rows(byParser("rm.recover")) {
		if r.Confidence == nil || *r.Confidence > 55 || !r.Deleted {
			t.Fatalf("record %+v, want deleted with a confidence capped at the artifact's 55", r)
		}
	}
}

func TestClaimedTablesNameTheRecoverIdentity(t *testing.T) {
	h, rec, r := claimHost(t, parse.Applicable, "sms")
	got, err := h.ClaimedTables(context.Background(), rec.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("claims %+v err %v", got, err)
	}
	rid, _ := r.RecoveryIdentity()
	if got[0].RecoverParser != "cp.recover" || got[0].RecoverParser != rid.Name || got[0].RecoverHash != rid.Hash || got[0].RecoverHash == got[0].Hash {
		t.Errorf("claim %+v, want the identity %+v", got[0], rid)
	}
}

// recoveredPut stores data as a recovered artifact whose logical path is path.
func recoveredPut(t *testing.T, c *evidence.Case, rel, path string, data []byte) evidence.ManifestRecord {
	t.Helper()
	c55 := 55
	d := &evidence.Derivation{
		ParentID: "missing", FSType: "ext4", FSPath: path,
		Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: &c55},
	}
	return put(t, c, "D1", "A1", rel, evidence.Source{Kind: "recover", RemotePath: path, Derived: d}, string(data))
}

// admitRecovered: a recovered database and a recovered WAL are admitted as input only for the bundle the
// caller names (OnlyPrimary and ExpectCompanions); the expected WAL gives exactly one typed job and a
// different recovered WAL id is bundle-differs.
func TestAdmitRecoveredBundle(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := recoveredPut(t, c, "rec-db", dbPathA, fixtureFile(t, "builder-wal-generations.db"))
	wal := recoveredPut(t, c, "rec-wal", dbPathA+"-wal", fixtureFile(t, "builder-wal-generations.db-wal"))
	other := recoveredPut(t, c, "rec-wal2", dbPathB+"-wal", fixtureFile(t, "builder-wal-generations.db-wal"))
	rc := &recCase{t: t, c: c}
	h := rc.host(nil, regRec(t, newRecParser("rm", "notes")))

	sum := recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: db.ID, ExpectCompanions: map[string]string{"wal": wal.ID}})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeComplete || sum.Jobs[0].Records == 0 {
		t.Fatalf("jobs %+v, want exactly one complete typed job", sum.Jobs)
	}

	sum = recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: db.ID, ExpectCompanions: map[string]string{"wal": other.ID}})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeSkipped || !strings.HasPrefix(sum.Jobs[0].Reason, "bundle-differs: wal ") || !strings.HasSuffix(sum.Jobs[0].Reason, " vs "+other.ID) {
		t.Fatalf("jobs %+v, want one skipped bundle-differs", sum.Jobs)
	}
}

// Companion ids are admitted as recovered input only together with OnlyPrimary: without it a recovered
// WAL named in ExpectCompanions does not join a live database's bundle.
func TestAdmitRecoveredCompanionNeedsOnlyPrimary(t *testing.T) {
	rc := newRecCase(t, "builder-wal-generations", false)
	wal := recoveredPut(t, rc.c, "rec-wal", dbPathA+"-wal", fixtureFile(t, "builder-wal-generations.db-wal"))
	h := rc.host(nil, regRec(t, newRecParser("rm", "notes")))
	sum := recoverOK(t, h, artparse.RecoverOptions{ExpectCompanions: map[string]string{"wal": wal.ID}})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeSkipped || !strings.HasPrefix(sum.Jobs[0].Reason, "bundle-differs: wal (none) vs "+wal.ID) {
		t.Fatalf("jobs %+v, want skipped bundle-differs: the recovered WAL was not admitted", sum.Jobs)
	}
}
