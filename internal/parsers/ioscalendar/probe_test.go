package ioscalendar_test

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The places of plan 4I3 (4I3-P5, B4, spec 8.1): the system path in a backup (HomeDomain) or an extraction, and a
// database of the same bytes elsewhere.
const (
	homeDomain    = "HomeDomain"
	calendarRel   = "Library/Calendar/Calendar.sqlitedb"
	systemLogical = "ios:HomeDomain/Library/Calendar/Calendar.sqlitedb"
	extractRemote = "/private/var/mobile/Library/Calendar/Calendar.sqlitedb"
	otherRemote   = "/private/var/mobile/Media/x/Calendar.sqlitedb"
	otherLogical  = "ios:MediaDomain/x/Calendar.sqlitedb"
	reasonDefer   = "schema checked at parse time: a -wal or -journal companion is present"
)

// The values of the CORE-iOS scenario (tools/fixtures/calendar_ios.py), in Cocoa seconds.
const (
	cD   = 721692800 // 2023-11-14T22:13:20Z
	cM   = 721699200 // 2023-11-15T00:00:00Z, a UTC midnight
	cH   = 3600
	cDay = 86400
)

// itemDDL is the part of the snapshot's CalendarItem that Task 8 reads (Go-built stores).
const itemDDL = `CREATE TABLE CalendarItem (ROWID INTEGER PRIMARY KEY, summary TEXT, start_date REAL, start_tz TEXT,
end_date REAL, end_tz TEXT, all_day INTEGER, calendar_id INTEGER, creation_date REAL, last_modified REAL, entity_type INTEGER)`

// itemCols is the column order of itemDDL after ROWID.
func itemCols() []string {
	return []string{"summary", "start_date", "start_tz", "end_date", "end_tz", "all_day", "calendar_id", "creation_date", "last_modified", "entity_type"}
}

// item is one CalendarItem row: a timed event hour from D in Europe/Berlin, overridden by kw (a nil value stores NULL).
type item struct {
	id int64
	kw map[string]any
}

func (r item) vals() []any {
	def := map[string]any{
		"summary": "Event " + strconv.FormatInt(r.id, 10), "start_date": float64(cD), "start_tz": "Europe/Berlin",
		"end_date": float64(cD + cH), "end_tz": "Europe/Berlin", "all_day": int64(0), "calendar_id": int64(1), "entity_type": int64(2),
	}
	out := []any{nil} // ROWID: the rowid alias stores NULL
	for _, c := range itemCols() {
		v, ok := r.kw[c]
		if !ok {
			v = def[c]
		}
		out = append(out, v)
	}
	return out
}

// goStore is a Go-built Calendar.sqlitedb whose CalendarItem holds rows, beside the snapshot's other tables, empty
// (so no join is undecided).
func goStore(rows ...item) []byte {
	return goItems(true, rows...)
}

// goItemsOnly is a Go-built Calendar.sqlitedb whose one table is CalendarItem: every join is undecided.
func goItemsOnly(rows ...item) []byte {
	return goItems(false, rows...)
}

func goItems(joins bool, rows ...item) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := b.CreateTable("CalendarItem", itemDDL)
	for _, r := range rows {
		tb.Insert(r.id, r.vals()...)
	}
	if joins {
		for _, td := range snapshotTables() {
			if td[0] != "CalendarItem" {
				b.CreateTable(td[0], "CREATE TABLE "+td[0]+" (ROWID INTEGER PRIMARY KEY, "+td[1]+")")
			}
		}
	}
	return b.Bytes()
}

// goStoreDDL is a Go-built Calendar.sqlitedb with one empty table of the given definition.
func goStoreDDL(name, ddl string) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable(name, ddl)
	return b.Bytes()
}

// bundleAt stores files at the system path of a backup (system) or at otherLogical through h.IOSArtifact.
func bundleAt(h *parsertest.Harness, files map[string][]byte, system bool) parsertest.Bundle {
	f := parsertest.Fixture{Name: "go-built", Files: files}
	if system {
		return f.BundleIOSBackup(h, homeDomain, calendarRel)
	}
	return f.BundleIOS(h, otherRemote, otherLogical)
}

// bundleOf places a committed fixture as tools/fixtures/calendar_ios.py states: calendar-ios-extract as an
// extraction at the system logical path, every other variant in a HomeDomain backup.
func bundleOf(h *parsertest.Harness, fx parsertest.Fixture, variant string) parsertest.Bundle {
	if variant == "calendar-ios-extract" {
		return fx.BundleIOS(h, extractRemote, systemLogical)
	}
	return fx.BundleIOSBackup(h, homeDomain, calendarRel)
}

func probeAt(t *testing.T, files map[string][]byte, system bool) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	got, err := p.Probe(t.Context(), bundleAt(h, files, system).Input(h, p, true))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return got
}

func parseAt(t *testing.T, files map[string][]byte, system bool) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	return h.RunLenient(p, bundleAt(h, files, system).Input(h, p, false))
}

func probeVariant(t *testing.T, variant string) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	got, err := p.Probe(t.Context(), bundleOf(h, parsertest.LoadFixture(t, "testdata", variant), variant).Input(h, p, true))
	if err != nil {
		t.Fatalf("%s: Probe: %v", variant, err)
	}
	return got
}

func parseVariant(t *testing.T, variant string) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	return h.RunLenient(p, bundleOf(h, parsertest.LoadFixture(t, "testdata", variant), variant).Input(h, p, false))
}

func fixtureFiles(t *testing.T, variant string) map[string][]byte {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).Files
}

// byRowid indexes the records of a run by their source_id (the CalendarItem ROWID).
func byRowid(t *testing.T, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		id, err := strconv.ParseInt(payloadStr(r, "source_id"), 10, 64)
		if err != nil {
			t.Fatalf("record %s: source_id %v", r.Locator, r.Payload["source_id"])
		}
		out[id] = r
	}
	return out
}

func payloadStr(r records.Record, key string) string {
	s, _ := r.Payload[key].(string)
	return s
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func unknownOf(r records.Record) map[string]any {
	m, _ := rawOf(r)["unknown_columns"].(map[string]any)
	return m
}

func TestMetaIsValid(t *testing.T) {
	m := ioscalendar.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "ios.calendar" || m.Version != "1.0.0" || len(m.Emits) != 1 || m.Emits[0].Type != "calendar_event" || m.Emits[0].PayloadVersion != 1 {
		t.Errorf("meta %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS {
		t.Errorf("platforms %v", m.Platforms)
	}
}

// 4I3-P5, B4: the one primary glob is exactly the system calendar path of HomeDomain; no other domain, no other file
// of the directory and no companion matches it.
func TestIOSCalendarGlobsCompileAndMatch(t *testing.T) {
	m := ioscalendar.Parser{}.Meta()
	if len(m.Inputs) == 0 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required || len(m.Inputs[0].Globs) != 1 || m.Inputs[0].Globs[0] != systemLogical {
		t.Fatalf("primary input %+v, want the one glob %s", m.Inputs, systemLogical)
	}
	g, err := parse.CompileGlob(systemLogical)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Match(systemLogical) {
		t.Errorf("the glob does not match %s", systemLogical)
	}
	for _, p := range []string{
		"ios:MediaDomain/Library/Calendar/Calendar.sqlitedb",
		"ios:AppDomain-x/Library/Calendar/Calendar.sqlitedb",
		"ios:HomeDomain/Library/Calendar/Extras.db",
		"ios:HomeDomain/Library/Calendar/Calendar.sqlitedb-wal",
		"ios:HomeDomain/Library/Calendar/sub/Calendar.sqlitedb",
		"android:/data/data/com.android.providers.calendar/databases/calendar.db",
	} {
		if g.Match(p) {
			t.Errorf("the glob matches %s", p)
		}
	}
}

// 4I-R21: no primary glob matches a companion; the companions reach the job only through their roles.
func TestIOSCalendarCompanionIsNeverAPrimary(t *testing.T) {
	m := ioscalendar.Parser{}.Meta()
	if len(m.Inputs) == 0 {
		t.Fatal("no input")
	}
	for _, in := range m.Inputs {
		for _, gs := range in.Globs {
			g, err := parse.CompileGlob(gs)
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-journal", "-shm"} {
				if p := systemLogical + suffix; g.Match(p) {
					t.Errorf("the glob %s matches the companion %s", gs, p)
				}
			}
		}
	}
	roles := map[string][]string{}
	for _, in := range m.Inputs[1:] {
		roles[in.Role] = in.Companions
	}
	if len(m.Inputs) != 3 || len(roles["wal"]) != 1 || roles["wal"][0] != "-wal" || len(roles["journal"]) != 1 || roles["journal"][0] != "-journal" {
		t.Errorf("companion roles %v", roles)
	}
}

// 4I3-P5, spec 8.1 and B4: at the system path the path names the store, so every refusal is loud (unsupported-schema
// or encrypted) and the older Event layout lands there; anywhere else rowrun's outcome holds unchanged.
func TestIOSCalendarProbeByPlace(t *testing.T) {
	for _, v := range []string{"calendar-ios", "calendar-ios-extract"} {
		if got := probeVariant(t, v); got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v", v, got)
		}
	}
	old := fixtureFiles(t, "calendar-ios-oldlayout")
	if got := probeVariant(t, "calendar-ios-oldlayout"); got.Status != parse.UnsupportedSchema || got.Reason != "no CalendarItem table at the system calendar path" {
		t.Errorf("calendar-ios-oldlayout at the system path: Probe = %+v", got)
	}
	if got := probeAt(t, old, false); got.Status != parse.NotApplicable {
		t.Errorf("calendar-ios-oldlayout at %s: Probe = %+v", otherLogical, got)
	}
	text := map[string][]byte{"db": []byte(strings.Repeat("not a database. ", 7)[:100])}
	if got := probeAt(t, text, true); got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the system calendar path" {
		t.Errorf("text at the system path: Probe = %+v", got)
	}
	if got := probeAt(t, text, false); got.Status != parse.NotApplicable {
		t.Errorf("text at %s: Probe = %+v", otherLogical, got)
	}
	noTZ := goStoreDDL("CalendarItem", `CREATE TABLE CalendarItem (ROWID INTEGER PRIMARY KEY, start_date REAL, all_day INTEGER, calendar_id INTEGER)`)
	for _, system := range []bool{true, false} {
		if got := probeAt(t, map[string][]byte{"db": noTZ}, system); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "start_tz") {
			t.Errorf("CalendarItem without start_tz (system %v): Probe = %+v", system, got)
		}
	}
	noAlias := goStoreDDL("CalendarItem", `CREATE TABLE CalendarItem (ROWID INTEGER, start_date REAL, start_tz TEXT, all_day INTEGER, calendar_id INTEGER)`)
	if got := probeAt(t, map[string][]byte{"db": noAlias}, true); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "rowid alias") {
		t.Errorf("CalendarItem without the alias: Probe = %+v", got)
	}
	enc := map[string][]byte{"db": highEntropy(8192)}
	if got := probeAt(t, enc, true); got.Status != parse.Encrypted {
		t.Errorf("encrypted at the system path: Probe = %+v", got)
	}

	// with a companion Probe defers; Parse at the system path then fails loudly without CalendarItem
	old["wal"] = []byte{}
	if got := probeAt(t, old, true); got.Status != parse.Applicable || got.Reason != reasonDefer {
		t.Errorf("calendar-ios-oldlayout with an empty -wal: Probe = %+v, want the deferral", got)
	}
	res := parseAt(t, old, true)
	if !errors.Is(res.Err, ioscalendar.ErrNoCalendarItemTable) || len(res.Records) != 0 {
		t.Errorf("calendar-ios-oldlayout with an empty -wal: Parse err %v, %d records; want ErrNoCalendarItemTable and none", res.Err, len(res.Records))
	}
	// elsewhere a database without CalendarItem is of another kind: no record, no error
	res = parseAt(t, fixtureFiles(t, "calendar-ios-oldlayout"), false)
	if res.Err != nil || len(res.Records) != 0 || res.Notes["not_applicable"] == "" {
		t.Errorf("calendar-ios-oldlayout at %s: Parse err %v, %d records, notes %v", otherLogical, res.Err, len(res.Records), res.Notes)
	}

	// lesson (b) of Task 5: a schema not read whole never lets the parser say CalendarItem is absent
	damaged := goStore(item{1, nil})
	damaged[108], damaged[109] = 0xff, 0xff // the first schema cell pointer points outside page 1
	if got := probeAt(t, map[string][]byte{"db": damaged}, true); got.Status != parse.UnsupportedSchema ||
		got.Reason != "the schema could not be read whole and no CalendarItem table was found in what was read" {
		t.Errorf("damaged schema at the system path: Probe = %+v", got)
	}
	res = parseAt(t, map[string][]byte{"db": damaged, "wal": {}}, true)
	if !errors.Is(res.Err, ioscalendar.ErrSchemaIncomplete) || errors.Is(res.Err, ioscalendar.ErrNoCalendarItemTable) || len(res.Records) != 0 {
		t.Errorf("damaged schema: Parse err %v, %d records; want ErrSchemaIncomplete and none", res.Err, len(res.Records))
	}

	// Task 8 review M2 (a): a present join table read by key (Get) must have ROWID as its rowid alias, else the store
	// is refused loudly at the system path
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("CalendarItem", itemDDL)
	b.CreateTable("Calendar", `CREATE TABLE Calendar (ROWID INTEGER, title TEXT, store_id INTEGER)`)
	if got := probeAt(t, map[string][]byte{"db": b.Bytes()}, true); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "Calendar") || !strings.Contains(got.Reason, "alias") {
		t.Errorf("Calendar without its alias at the system path: Probe = %+v", got)
	}

	// Task 8 review M2 (b): a CalendarItem the census sees but that cannot be opened (a virtual table: the reader
	// cannot read it) is an error that names it, never "absent" and never "unknown", even when another schema entry
	// is lost (the schema is then read in part)
	b = sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("Aaa", `CREATE TABLE Aaa (v INTEGER)`)
	b.AddSchemaRow("table", "CalendarItem", "CalendarItem", int64(0), "CREATE VIRTUAL TABLE CalendarItem USING fts5(summary)")
	lost := b.Bytes()
	lost[108], lost[109] = 0xff, 0xff // the first schema cell (Aaa) points outside page 1
	res = parseAt(t, map[string][]byte{"db": lost, "wal": {}}, true)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "CalendarItem") || errors.Is(res.Err, ioscalendar.ErrNoCalendarItemTable) || errors.Is(res.Err, ioscalendar.ErrSchemaIncomplete) || len(res.Records) != 0 {
		t.Errorf("an unopenable CalendarItem the census sees: Parse err %v, %d records", res.Err, len(res.Records))
	}
}

// highEntropy is n deterministic bytes that look encrypted (a SHA-256 chain).
func highEntropy(n int) []byte {
	var out []byte
	sum := sha256.Sum256([]byte("ios.calendar"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}
