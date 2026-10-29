package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const (
	contactsLogical = "/data/data/com.android.providers.contacts/databases/contacts2.db"
	chromeLogical   = "/data/data/com.android.chrome/app_chrome/Default/History"
)

type typedVariant struct{ dir, variant, remote string }

// the fixtures whose claimed tables hold history (the recover oracles of Tasks 9 and 10 exist for them)
var typedVariants = []typedVariant{
	{"androidmms", "av-freelist-512", mmsLogical},
	{"androidmms", "wal-uncheckpointed", mmsLogical},
	{"androidmms", "hot-journal", mmsLogical},
	{"androidmms", "mmssms-pdu-deleted", mmsLogical},
	{"androidcontacts", "contacts-deleted", contactsLogical},
	{"chromium", "chromium-deleted", chromeLogical},
}

func readGzFixture(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false
	}
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
	return b, true
}

type typedEnv struct {
	dir                 string
	db                  evidence.ManifestRecord
	wal, journal, other string // artifact ids; other is a second WAL that is not the database's by name
}

// newTypedEnv is a case holding a committed fixture at its logical path, with its -wal and -journal when the
// fixture has them. withOther adds a second write-ahead log under another name.
func newTypedEnv(t *testing.T, v typedVariant, withOther bool) *typedEnv {
	t.Helper()
	base := filepath.Join("..", "parsers", v.dir, "testdata", v.variant)
	db, ok := readGzFixture(t, base+".db.gz")
	if !ok {
		t.Fatalf("fixture %s has no database", v.variant)
	}
	c := recordstest.NewCase(t)
	if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "logical"}); err != nil {
		t.Fatal(err)
	}
	e := &typedEnv{dir: c.Dir}
	e.db = pcapture(t, c, "files/main.db", evidence.Source{Kind: "file", RemotePath: v.remote}, string(db))
	var walBytes []byte
	if b, ok := readGzFixture(t, base+".db-wal.gz"); ok {
		walBytes = b
		e.wal = pcapture(t, c, "files/main.db-wal", evidence.Source{Kind: "file", RemotePath: v.remote + "-wal"}, string(b)).ID
	}
	if b, ok := readGzFixture(t, base+".db-journal.gz"); ok {
		e.journal = pcapture(t, c, "files/main.db-journal", evidence.Source{Kind: "file", RemotePath: v.remote + "-journal"}, string(b)).ID
	}
	if withOther {
		if walBytes == nil {
			walBytes = []byte("not a wal")
		}
		e.other = pcapture(t, c, "files/other.db-wal", evidence.Source{Kind: "file", RemotePath: "/data/other/unrelated.db-wal"}, string(walBytes)).ID
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *typedEnv) recover(t *testing.T, reg regFn, args ...string) (int, string, string) {
	t.Helper()
	return runP(t, reg, append([]string{"sqlite", "recover", "--case", e.dir}, args...)...)
}

// emptyRegistry registers no parser: the uncomposed 3J run.
func emptyRegistry() regFn {
	return func() ([]artparse.Registered, error) { return nil, nil }
}

type typedReport struct {
	AnalysisID string `json:"analysis_id"`
	Counts     struct {
		Written             int64            `json:"written"`
		SkippedByReason     map[string]int64 `json:"skipped_by_reason"`
		KeptUnmappedClaimed int64            `json:"kept_unmapped_claimed"`
		Narrowed            []string         `json:"narrowed"`
	} `json:"counts"`
	Typed      *typedJSON `json:"typed"`
	Incomplete bool       `json:"incomplete"`
}

func (e *typedEnv) report(t *testing.T, reg regFn, wantCode int, args ...string) typedReport {
	t.Helper()
	code, out, errOut := e.recover(t, reg, append([]string{"--json", e.db.ID}, args...)...)
	if code != wantCode {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, wantCode, out, errOut)
	}
	var r typedReport
	if err := json.Unmarshal(jsonPart(out), &r); err != nil {
		t.Fatalf("report json: %v\n%s", err, out)
	}
	return r
}

func listAllRecords(t *testing.T, dir string, extra ...string) []rowJSON {
	t.Helper()
	args := append([]string{"records", "list", "--case", dir, "--json", "--limit", strconv.Itoa(records.MaxLimit)}, extra...)
	code, out, _ := runP(t, nil, args...)
	if code != 0 {
		t.Fatalf("records list: %d\n%s", code, out)
	}
	var l struct {
		Records []rowJSON `json:"records"`
	}
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatalf("records list json: %v", err)
	}
	if len(l.Records) >= records.MaxLimit {
		t.Fatalf("%d records fill the page: the test cannot see them all", len(l.Records))
	}
	return l.Records
}

func isTypedRecover(r rowJSON) bool {
	return r.Type != "db_row" && strings.HasSuffix(r.Parser.Name, ".recover")
}

func locatorTable(loc string) string {
	_, rest, ok := strings.Cut(loc, "table=")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, ";")
	return name
}

type locSplit struct {
	typed, db map[string]rowJSON
	typedAll  []rowJSON
}

// splitCase reads every current recovered record of a case: typed ones and db_rows, by locator.
func splitCase(t *testing.T, dir string) locSplit {
	t.Helper()
	s := locSplit{typed: map[string]rowJSON{}, db: map[string]rowJSON{}}
	for _, r := range listAllRecords(t, dir, "--recovered", "only") {
		switch {
		case isTypedRecover(r):
			s.typedAll = append(s.typedAll, r)
			s.typed[r.Locator] = r
		case r.Type == "db_row":
			s.db[r.Locator] = r
		default:
			t.Fatalf("recovered record of type %s from %s is neither typed recovery nor db_row", r.Type, r.Parser.Name)
		}
	}
	return s
}

// typedDelivered sums the recovered_rows.<table> notes of every typed job.end in the audit log.
func typedDelivered(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, e := range auditOf(t, dir) {
		if e.Action != "parse.job.end" {
			continue
		}
		notes, _ := e.Details["notes"].(map[string]any)
		for k, v := range notes {
			if strings.HasPrefix(k, "recovered_rows.") {
				x, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
				if err != nil {
					t.Fatalf("note %s = %v", k, v)
				}
				n += x
			}
		}
	}
	return n
}

// oracleUnmapped is the number of rows the variant's recover oracle lists as unmapped: rows of a claimed table that
// the mapping cannot make a record of (a visit whose URL row is gone, a nameless raw contact).
func oracleUnmapped(t *testing.T, v typedVariant) int64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "parsers", v.dir, "testdata", v.variant+".recover.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Unmapped []json.RawMessage `json:"unmapped"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	return int64(len(o.Unmapped))
}

// Spec section 14: a claimed table never loses a row. After one sqlite recover every history row is a typed
// record or a db_row, once.
func TestClaimedTablesNeverLoseRows(t *testing.T) {
	for _, v := range typedVariants {
		t.Run(v.variant, func(t *testing.T) {
			composed := newTypedEnv(t, v, false)
			rep := composed.report(t, nil, 0)
			if rep.Typed == nil || len(rep.Typed.Honored) == 0 {
				t.Fatalf("typed pass %+v, want honored claims", rep.Typed)
			}
			for _, j := range rep.Typed.Jobs { // a claiming parser that does not apply to the database is skipped
				if j.Outcome != artparse.OutcomeComplete && j.Outcome != artparse.OutcomeSkipped {
					t.Errorf("typed job %+v is neither complete nor skipped", j)
				}
			}
			delivered := typedDelivered(t, composed.dir)
			excluded := rep.Counts.SkippedByReason["excluded-by-claim"]
			if delivered == 0 || excluded+rep.Counts.KeptUnmappedClaimed != delivered {
				t.Errorf("excluded-by-claim %d + kept_unmapped_claimed %d != %d rows the typed pass delivered", excluded, rep.Counts.KeptUnmappedClaimed, delivered)
			}
			// the rows the fixture oracle says no record can be made of are kept by the generic run as db_rows (4G-R21)
			if n := oracleUnmapped(t, v); rep.Counts.KeptUnmappedClaimed != n {
				t.Errorf("kept_unmapped_claimed %d, the fixture oracle lists %d unmapped rows: the generic run must keep each as a db_row (4G-R21)", rep.Counts.KeptUnmappedClaimed, n)
			}

			// the same files recovered without any claim: the full set of locators
			plain := newTypedEnv(t, v, false)
			plain.report(t, emptyRegistry(), 0)
			want := splitCase(t, plain.dir)
			got := splitCase(t, composed.dir)
			if len(want.typed) != 0 || len(want.db) == 0 {
				t.Fatalf("uncomposed run: %d typed, %d db_row", len(want.typed), len(want.db))
			}
			union := map[string]bool{}
			for loc := range got.typed {
				if _, both := got.db[loc]; both {
					t.Errorf("row %s is both a typed record and a db_row", loc)
				}
				union[loc] = true
			}
			for loc := range got.db {
				union[loc] = true
			}
			for loc := range want.db {
				if !union[loc] {
					t.Errorf("row %s was recorded by the uncomposed run and is lost by the composed one", loc)
				}
			}
			for loc := range union {
				if _, ok := want.db[loc]; !ok {
					t.Errorf("composed run recorded %s, which the uncomposed run does not have", loc)
				}
			}
			// every unmapped warning is a db_row
			for _, e := range auditOf(t, composed.dir) {
				if e.Action != "analysis.warning" {
					continue
				}
				if reason, _ := e.Details["reason"].(string); strings.HasPrefix(reason, "recovered row not mapped: ") {
					loc, _ := e.Details["path"].(string)
					if _, ok := got.db[loc]; !ok {
						t.Errorf("unmapped row %s is not a db_row", loc)
					}
				}
			}
		})
	}
}

// A typed record points at the row exactly where the db_row of an uncomposed run does.
func TestTypedLocatorEqualsDBRowLocator(t *testing.T) {
	for _, v := range typedVariants {
		t.Run(v.variant, func(t *testing.T) {
			composed := newTypedEnv(t, v, false)
			composed.report(t, nil, 0)
			plain := newTypedEnv(t, v, false)
			plain.report(t, emptyRegistry(), 0)
			want := splitCase(t, plain.dir)
			got := splitCase(t, composed.dir)
			if len(got.typed) == 0 {
				t.Fatal("no typed record")
			}
			for loc, r := range got.typed {
				d, ok := want.db[loc]
				if !ok {
					t.Errorf("typed record %s has no db_row in the uncomposed run", loc)
					continue
				}
				if d.Range == nil || r.Range == nil || *d.Range != *r.Range {
					t.Errorf("%s: range %v, db_row range %v", loc, r.Range, d.Range)
				}
				if id := composed.roleArtifact(loc); r.ArtifactID != id || d.ArtifactID != plain.roleArtifact(loc) {
					t.Errorf("%s: artifact %s (want %s), db_row artifact %s (want %s)", loc, r.ArtifactID, id, d.ArtifactID, plain.roleArtifact(loc))
				}
			}
		})
	}
}

// oracleDigests counts, per claimed table, the history versions the fixture's recover oracle says must come back
// typed (one digest per version the independent page walker found).
func oracleDigests(t *testing.T, v typedVariant) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "parsers", v.dir, "testdata", v.variant+".recover.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Digests []struct {
			Table string `json:"table"`
		} `json:"digests"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, d := range o.Digests {
		n[d.Table]++
	}
	return n
}

// Every table of the recovered database is covered exactly once (F3): the claimed tables by typed records, plus
// db_rows for the page-slack rows no claim takes, and every other recovered row (the deleted addr and part rows
// and the like, which no live definition names) by an unlabelled db_row. Exact tables and counts, pinned.
func TestSQLiteRecoverCoversEveryTableOnce(t *testing.T) {
	v := typedVariants[3]
	if v.variant != "mmssms-pdu-deleted" {
		t.Fatalf("typedVariants[3] is %s", v.variant)
	}
	e := newTypedEnv(t, v, false)
	e.report(t, nil, 0)
	s := splitCase(t, e.dir)
	typed := map[string]int{}
	for _, r := range s.typedAll {
		if r.Type != "message" || !r.Deleted || !r.Recovered {
			t.Errorf("typed record %+v: want a recovered, deleted message", r)
		}
		typed[locatorTable(r.Locator)]++
	}
	// every version the oracle's independent page walker found comes back typed; the pass may type more (cells of
	// in-page deletions that fit the live definition), never fewer
	for tbl, n := range oracleDigests(t, v) {
		if typed[tbl] < n {
			t.Errorf("table %s: %d typed records, the oracle lists %d versions that must come back typed", tbl, typed[tbl], n)
		}
	}
	if want := map[string]int{"sms": 45, "pdu": 16}; !maps.Equal(typed, want) {
		t.Errorf("typed records by table %v, want exactly %v", typed, want)
	}
	// the db_rows: the claimed tables' only db_rows are rows carved from page slack (4G-R22); the rest carry no table
	// label at all (the scenario deletes the addr and part rows of 20 pdu: no live definition names them)
	tables, methods := map[string]int{}, map[string]int{}
	for loc, r := range s.db {
		tbl := locatorTable(loc)
		tables[tbl]++
		methods[tbl+"|"+r.RecoveryMethod]++
		if (tbl == "sms" || tbl == "pdu") && r.RecoveryMethod != "sqlite-page-slack" {
			t.Errorf("db_row %s of claimed table %s (%s) should be a typed record", loc, tbl, r.RecoveryMethod)
		}
	}
	if want := map[string]int{"": 43, "pdu": 11, "sms": 8}; !maps.Equal(tables, want) {
		t.Errorf("db_rows by table %v, want exactly %v (\"\" is a row no live table definition names)", tables, want)
	}
	if want := map[string]int{"|sqlite-freelist": 14, "|sqlite-page-slack": 29, "pdu|sqlite-page-slack": 11, "sms|sqlite-page-slack": 8}; !maps.Equal(methods, want) {
		t.Errorf("db_rows by table and method %v, want exactly %v", methods, want)
	}
}

// ---- parsers that fail in the ways the composition must survive

type mapWrap struct {
	androidmms.Parser
	wrap func(orig func(parse.MapContext, parse.Row) ([]records.Record, error)) func(parse.MapContext, parse.Row) ([]records.Record, error)
}

func (p mapWrap) Mappings() []parse.TableMapping {
	ms := p.Parser.Mappings()
	out := make([]parse.TableMapping, len(ms))
	copy(out, ms)
	for i := range out {
		out[i].Map = p.wrap(out[i].Map)
	}
	return out
}

func claimingRegistry(p parse.Parser) regFn {
	return func() ([]artparse.Registered, error) {
		m := p.Meta()
		r, err := artparse.Register(p, parsertest.FakeHash(m.Name+"@"+m.Version))
		if err != nil {
			return nil, err
		}
		r, err = r.WithRecoveryHash(parsertest.FakeHash(m.Name + ".recover@" + m.Version))
		if err != nil {
			return nil, err
		}
		return []artparse.Registered{r}, nil
	}
}

// panicOnSecond is the real mapping, except that it panics on the second row it is given.
func panicOnSecond() regFn {
	var n atomic.Int32
	return claimingRegistry(mapWrap{wrap: func(orig func(parse.MapContext, parse.Row) ([]records.Record, error)) func(parse.MapContext, parse.Row) ([]records.Record, error) {
		return func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			if n.Add(1) == 2 {
				panic("test mapper panic")
			}
			return orig(m, row)
		}
	}})
}

// unmappableFirst refuses the first n rows it is given.
func unmappableFirst(n int32) regFn {
	var seen atomic.Int32
	return claimingRegistry(mapWrap{wrap: func(orig func(parse.MapContext, parse.Row) ([]records.Record, error)) func(parse.MapContext, parse.Row) ([]records.Record, error) {
		return func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			if seen.Add(1) <= n {
				return nil, rowrun.Unmappable("test refusal")
			}
			return orig(m, row)
		}
	}})
}

type encryptedMMS struct{ androidmms.Parser }

func (encryptedMMS) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: parse.Encrypted, Reason: "test: \x1b[31mthe database is encrypted"}, nil
}

func TestSQLiteRecoverTypedFailureKeepsGenericCoverage(t *testing.T) {
	v := typedVariants[0]
	e := newTypedEnv(t, v, false)
	rep := e.report(t, panicOnSecond(), ExitError)
	if rep.Typed == nil || len(rep.Typed.Jobs) != 1 || rep.Typed.Jobs[0].Outcome != artparse.OutcomeIncomplete {
		t.Fatalf("typed %+v, want one incomplete job", rep.Typed)
	}
	if len(rep.Typed.Honored) != 0 {
		t.Errorf("honored %v after a failed typed pass", rep.Typed.Honored)
	}
	var tables []string
	for _, n := range rep.Typed.NotHonored {
		tables = append(tables, n.Table)
	}
	slices.Sort(tables)
	if !slices.Equal(tables, []string{"pdu", "sms"}) {
		t.Errorf("not_honored tables %v, want pdu and sms", tables)
	}
	if n := rep.Counts.SkippedByReason["excluded-by-claim"]; n != 0 || len(rep.Counts.Narrowed) != 0 {
		t.Errorf("the generic run excluded %d rows / narrowed %v after a failed typed pass", n, rep.Counts.Narrowed)
	}
	plain := newTypedEnv(t, v, false)
	plain.report(t, emptyRegistry(), 0)
	want, got := splitCase(t, plain.dir), splitCase(t, e.dir)
	for loc := range want.db {
		if _, ok := got.db[loc]; !ok {
			t.Errorf("row %s is not a db_row after the failed typed pass", loc)
		}
	}
	if len(got.db) != len(want.db) {
		t.Errorf("%d db_rows, uncomposed run has %d", len(got.db), len(want.db))
	}
}

func TestSQLiteRecoverNotHonoredIsLinked(t *testing.T) {
	e := newTypedEnv(t, typedVariants[0], false)
	code, out, errOut := e.recover(t, panicOnSecond(), e.db.ID)
	if code != ExitError {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "also recorded as an incomplete typed run") {
		t.Errorf("the text report does not link the incomplete typed run:\n%s", out)
	}
	typed := ""
	var start map[string]any
	for _, en := range auditOf(t, e.dir) {
		if en.Action == "analysis.start" && en.Details["op"] == "parse-recover" {
			typed, _ = en.Details["analysis_id"].(string)
		}
		if en.Action == "analysis.start" && en.Details["op"] == dbrecover.Op {
			start = en.Details
		}
	}
	nh, _ := start["typed_not_honored"].([]any)
	if typed == "" || len(nh) != 1 {
		t.Fatalf("typed analysis %q, typed_not_honored %v", typed, start["typed_not_honored"])
	}
	m, _ := nh[0].(map[string]any)
	tabs, _ := m["tables"].([]any)
	if m["typed_analysis_id"] != typed || len(tabs) != 2 || m["reason"] == "" || m["parser"] != "android.mmssms.recover" {
		t.Errorf("typed_not_honored[0] = %v, want the typed analysis %s with its tables and reason", m, typed)
	}
	rep := e.report(t, emptyRegistry(), 0, "--allow-reingest") // the JSON form of the same case, uncomposed: no link
	if rep.Typed != nil {
		t.Errorf("uncomposed run has typed %+v", rep.Typed)
	}
	e2 := newTypedEnv(t, typedVariants[0], false)
	rep2 := e2.report(t, panicOnSecond(), ExitError)
	if rep2.Typed == nil || len(rep2.Typed.NotHonored) == 0 || !rep2.Typed.NotHonored[0].AlsoRecorded {
		t.Errorf("JSON does not say also_recorded_as_incomplete_typed_run: %+v", rep2.Typed)
	}
}

func TestSQLiteRecoverBundleDiffersHonorsNoClaim(t *testing.T) {
	v := typedVariants[1] // wal-uncheckpointed: the bundle has a WAL
	e := newTypedEnv(t, v, true)
	rep := e.report(t, nil, 0, "--wal", e.other)
	if rep.Typed == nil || len(rep.Typed.Jobs) != 1 || rep.Typed.Jobs[0].Outcome != artparse.OutcomeSkipped || !strings.Contains(rep.Typed.Jobs[0].Reason, "bundle-differs") {
		t.Fatalf("typed %+v, want one job skipped with bundle-differs", rep.Typed)
	}
	if len(rep.Typed.Honored) != 0 || rep.Counts.SkippedByReason["excluded-by-claim"] != 0 {
		t.Errorf("a claim was honored for a different bundle: %+v", rep)
	}
	plain := newTypedEnv(t, v, true)
	plain.report(t, emptyRegistry(), 0, "--wal", plain.other)
	want, got := splitCase(t, plain.dir), splitCase(t, e.dir)
	if len(got.typed) != 0 || len(got.db) != len(want.db) {
		t.Errorf("composed: %d typed, %d db_rows; the generic run alone has %d db_rows", len(got.typed), len(got.db), len(want.db))
	}
}

func TestSQLiteRecoverRepeatDoesNotDoubleRecord(t *testing.T) {
	e := newTypedEnv(t, typedVariants[0], false)
	e.report(t, nil, 0)
	n1 := len(listAllRecords(t, e.dir))
	code, out, errOut := e.recover(t, nil, e.db.ID)
	if code == 0 {
		t.Fatalf("a repeated run succeeded:\n%s\n%s", out, errOut)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "already parsed") {
		t.Errorf("the typed pass was not skipped as already recovered:\n%s", out)
	}
	if n2 := len(listAllRecords(t, e.dir)); n2 != n1 {
		t.Errorf("the repeated run changed the record count from %d to %d", n1, n2)
	}
	again := e.report(t, nil, 0, "--allow-reingest")
	if again.Typed == nil || again.Typed.Jobs[0].Outcome != "complete" {
		t.Fatalf("typed with --allow-reingest: %+v", again.Typed)
	}
	if n3 := len(listAllRecords(t, e.dir)); n3 != n1 {
		t.Errorf("after --allow-reingest the default query has %d records, the first run had %d", n3, n1)
	}
}

func TestSQLiteRecoverListShowsClaims(t *testing.T) {
	e := newTypedEnv(t, typedVariants[0], false)
	before := fileSnapshot(t, e.dir)
	nAudit := len(auditActionsOf(t, e.dir))
	code, out, errOut := e.recover(t, nil, e.db.ID, "--list")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	want := "claimed by android.mmssms.recover " + androidmms.Parser{}.Meta().Version + ": sms, pdu"
	if !strings.Contains(out, want) {
		t.Errorf("--list does not say %q:\n%s", want, out)
	}
	if !mapsEqual2(before, fileSnapshot(t, e.dir)) {
		t.Error("--list changed a file of the case")
	}
	for _, a := range auditActionsOf(t, e.dir)[nAudit:] {
		if a != "case.open" {
			t.Errorf("--list appended the audit entry %q", a)
		}
	}
}

var idLine = regexp.MustCompile(`(?m)^(ingest|analysis) \S+`)

// (PASSES ON ARRIVAL: with no claiming parser the composition is a no-op.) With nothing registered that
// claims a table, the command is the 3J command: same counts as a direct dbrecover.Run, same text.
func TestSQLiteRecoverNoClaimingParserIsUnchanged(t *testing.T) {
	v := typedVariants[0]
	e := newTypedEnv(t, v, false)
	direct := newTypedEnv(t, v, false)
	c, err := openCase(direct.dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := dbrecover.Run(context.Background(), c, dbrecover.Inputs{DB: direct.db.ID}, dbrecover.Options{})
	_ = c.Close()
	if err != nil {
		t.Fatal(err)
	}
	rep := e.report(t, emptyRegistry(), 0)
	want := countsJSON2(res.Counts)
	if rep.Typed != nil || rep.Counts.Written != want.Written || !maps.Equal(rep.Counts.SkippedByReason, want.SkippedByReason) {
		t.Errorf("counts %+v typed %v, direct run %+v", rep.Counts, rep.Typed, want)
	}
	plain := newTypedEnv(t, v, false)
	_, text, _ := plain.recover(t, registry(t, "", good("a")), plain.db.ID)
	direct2 := newTypedEnv(t, v, false)
	c2, err := openCase(direct2.dir)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := dbrecover.Run(context.Background(), c2, dbrecover.Inputs{DB: direct2.db.ID}, dbrecover.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := printSQLiteResult(&buf, c2, res2, nil, false, typedResult{}); err != nil {
		t.Fatal(err)
	}
	_ = c2.Close()
	norm := func(s string) string {
		return idLine.ReplaceAllString(s, "$1 ID")
	}
	// artifact ids differ between cases: compare the shape (ids replaced)
	idRe := regexp.MustCompile(`artifact \S+`)
	a, b := idRe.ReplaceAllString(norm(text), "artifact ID"), idRe.ReplaceAllString(norm(buf.String()), "artifact ID")
	if a != b {
		t.Errorf("the command's text differs from the direct 3J report:\n%s\n---\n%s", a, b)
	}
}

// The 4E rule: a claim the parser could not decide (it cannot parse the database now) is never treated as no
// claim. The generic run is not started, the report says so, the exit is partial.
func TestSQLiteRecoverUndecidedClaimsDoNotRunGeneric(t *testing.T) {
	e := newTypedEnv(t, typedVariants[0], false)
	code, out, errOut := e.recover(t, claimingRegistry(encryptedMMS{}), e.db.ID)
	if code != ExitError {
		t.Fatalf("exit %d, want 1\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "claims undecided") || !strings.Contains(errOut, "claims undecided") {
		t.Errorf("neither report says the claims are undecided:\n%s\n%s", out, errOut)
	}
	if rows := listAllRecords(t, e.dir); len(rows) != 0 {
		t.Errorf("%d records written for a database whose claims are undecided", len(rows))
	}
	for _, en := range auditOf(t, e.dir) {
		if en.Action == "analysis.start" && en.Details["op"] == dbrecover.Op {
			t.Error("the generic run was started")
		}
	}
	rep := e.report(t, claimingRegistry(encryptedMMS{}), ExitError)
	if rep.Typed == nil || !rep.Typed.Undecided {
		t.Errorf("JSON typed %+v, want claims_undecided", rep.Typed)
	}
}

// Rows the typed pass could not map are db_rows (4G-R21), counted, and not lost.
func TestSQLiteRecoverUnmappedRowsAreKeptAsDBRows(t *testing.T) {
	v := typedVariants[0]
	e := newTypedEnv(t, v, false)
	rep := e.report(t, unmappableFirst(2), 0)
	if rep.Typed == nil || rep.Typed.Jobs[0].UnmappedRecovered != 2 || len(rep.Typed.Honored) == 0 {
		t.Fatalf("typed %+v, want two unmapped rows and honored claims", rep.Typed)
	}
	if rep.Counts.KeptUnmappedClaimed != 2 {
		t.Errorf("kept_unmapped_claimed %d, want 2", rep.Counts.KeptUnmappedClaimed)
	}
	plain := newTypedEnv(t, v, false)
	plain.report(t, emptyRegistry(), 0)
	want, got := splitCase(t, plain.dir), splitCase(t, e.dir)
	if len(got.typed)+len(got.db) != len(want.db) {
		t.Errorf("%d typed + %d db_rows != %d rows of the uncomposed run", len(got.typed), len(got.db), len(want.db))
	}
}

// 4G-R25: a job that overflowed its unmapped-locator list honors no claim; the generic run records the whole table.
func TestSQLiteRecoverUnmappedOverflowHonorsNothing(t *testing.T) {
	typedUnmappedCap = 1
	t.Cleanup(func() { typedUnmappedCap = 0 })
	v := typedVariants[0]
	e := newTypedEnv(t, v, false)
	rep := e.report(t, unmappableFirst(2), 0)
	if rep.Typed == nil || !rep.Typed.Jobs[0].UnmappedOverflow || len(rep.Typed.Honored) != 0 || len(rep.Typed.NotHonored) == 0 {
		t.Fatalf("typed %+v, want an overflowing job that honors nothing", rep.Typed)
	}
	if n := rep.Counts.SkippedByReason["excluded-by-claim"]; n != 0 {
		t.Errorf("%d rows excluded by a claim of an overflowing job", n)
	}
	if len(rep.Typed.Warnings) == 0 || !strings.Contains(rep.Typed.Warnings[0], "more unmapped rows than can be tracked") {
		t.Errorf("no warning says the claims are not honored: %v", rep.Typed.Warnings)
	}
	plain := newTypedEnv(t, v, false)
	plain.report(t, emptyRegistry(), 0)
	want, got := splitCase(t, plain.dir), splitCase(t, e.dir)
	if len(got.db) != len(want.db) {
		t.Errorf("%d db_rows, the whole table is %d", len(got.db), len(want.db))
	}
}

// Slack material the generic run carves, and every history row, end as a typed record or a db_row (4G-R22).
func TestEveryHistoryAndSlackRowIsTypedOrDBRow(t *testing.T) {
	v := typedVariants[3]
	plain := newTypedEnv(t, v, false)
	plain.report(t, emptyRegistry(), 0)
	want := splitCase(t, plain.dir)
	slack := 0
	for _, r := range want.db {
		if r.RecoveryMethod == "sqlite-page-slack" {
			slack++
		}
	}
	if slack == 0 {
		t.Fatal("mmssms-pdu-deleted yields no slack-carved row: the fixture no longer has in-page deleted cells")
	}
	composed := newTypedEnv(t, v, false)
	composed.report(t, nil, 0)
	got := splitCase(t, composed.dir)
	for loc, r := range want.db {
		_, typed := got.typed[loc]
		d, isDB := got.db[loc]
		if !typed && !isDB {
			t.Errorf("row %s (%s) is neither typed nor a db_row", loc, r.RecoveryMethod)
		}
		if r.RecoveryMethod == "sqlite-page-slack" && (typed || !isDB || d.RecoveryMethod != r.RecoveryMethod) {
			t.Errorf("slack row %s must be a db_row of the same method", loc)
		}
	}
}

func TestSQLiteRecoverTypedJSONIsTerminalSafe(t *testing.T) {
	hostile := typedVariants[0]
	hostile.remote = "/data/\x1b[31m\nevil/com.android.providers.telephony/databases/mmssms.db"
	e := newTypedEnv(t, hostile, false)
	_, out, _ := e.recover(t, claimingRegistry(encryptedMMS{}), "--json", e.db.ID)
	if r, bad := rawControl(out); bad {
		t.Errorf("the JSON report holds the raw control character %q", r)
	}
	var v map[string]any
	if err := json.Unmarshal(jsonPart(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if typed, ok := v["typed"].(map[string]any); !ok || typed["jobs"] == nil {
		t.Errorf("no typed object: %v", v["typed"])
	}
	_, text, _ := e.recover(t, claimingRegistry(encryptedMMS{}), e.db.ID)
	if r, bad := rawControl(text); bad {
		t.Errorf("the text report holds the raw control character %q", r)
	}
}

// roleArtifact is the artifact the locator's file= role names in this case.
func (e *typedEnv) roleArtifact(loc string) string {
	switch {
	case strings.HasSuffix(loc, ";file=wal"):
		return e.wal
	case strings.HasSuffix(loc, ";file=journal"):
		return e.journal
	}
	return e.db.ID
}
