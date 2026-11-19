package androidcalendar_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	remoteProvider = "/data/data/com.android.providers.calendar/databases/calendar.db"
	remoteOther    = "/sdcard/x/calendar.db"
	primaryGlob    = "android:**/com.android.providers.calendar/databases/calendar.db"
)

// eventsDDL is the identifying part of the Events table (spec 7.2: _id alias, calendar_id, dtstart, allDay, deleted).
const eventsDDL = `CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, calendar_id INTEGER NOT NULL, dtstart INTEGER,
allDay INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0)`

// goBuilt is a Go-built calendar.db with one Events row at the given user version (F05: 601 unless about the band).
func goBuilt(userVersion int32, ddl string) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: userVersion})
	if ddl != "" {
		tb := b.CreateTable("Events", ddl)
		tb.Insert(1, nil, int64(1), int64(1700000000000), int64(0), int64(0))
	}
	return b.Bytes()
}

// inputAt stores files (roles db, wal, journal) at remote through parsertest.AddAndroidFile (F19) and returns the input.
func inputAt(t *testing.T, h *parsertest.Harness, p parse.Parser, remote string, files map[string][]byte, forProbe bool) *parse.Input {
	t.Helper()
	primary := h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq1", remote, files["db"]), files["db"])
	others := map[string]parse.Artifact{}
	for role, suffix := range map[string]string{"wal": "-wal", "journal": "-journal"} {
		if data, ok := files[role]; ok {
			others[role] = h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq1", remote+suffix, data), data)
		}
	}
	return h.Input(p, primary, others, forProbe)
}

func probeAt(t *testing.T, remote string, files map[string][]byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidcalendar.Parser{}
	got, err := p.Probe(t.Context(), inputAt(t, h, p, remote, files, true))
	if err != nil {
		t.Fatalf("Probe(%s): %v", remote, err)
	}
	return got
}

func parseAt(t *testing.T, remote string, files map[string][]byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidcalendar.Parser{}
	return h.RunLenient(p, inputAt(t, h, p, remote, files, false))
}

func fixtureFiles(t *testing.T, variant string) map[string][]byte {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).Files
}

func TestMetaIsValid(t *testing.T) {
	m := androidcalendar.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "android.calendar" || m.Version != "1.0.0" || len(m.Emits) != 1 || m.Emits[0].Type != "calendar_event" || m.Emits[0].PayloadVersion != 1 {
		t.Errorf("meta %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformAndroid {
		t.Errorf("platforms %v", m.Platforms)
	}
	if len(m.Claims) != 0 {
		t.Errorf("claims %v: none until the mapping is total (plan 4I3 Task 6)", m.Claims)
	}
	if (androidcalendar.Parser{}).Mappings() != nil {
		t.Error("Mappings is not nil before the Events mapping exists (plan 4I3 Task 4)")
	}
}

// 4I3-P5: the one primary glob compiles and matches the provider's databases directory of every Android user, and
// nothing else.
func TestAndroidCalendarGlobsCompileAndMatch(t *testing.T) {
	m := androidcalendar.Parser{}.Meta()
	if len(m.Inputs) == 0 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required || len(m.Inputs[0].Globs) != 1 || m.Inputs[0].Globs[0] != primaryGlob {
		t.Fatalf("primary input %+v, want the one glob %s", m.Inputs, primaryGlob)
	}
	g, err := parse.CompileGlob(primaryGlob)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"android:/data/data/com.android.providers.calendar/databases/calendar.db",
		"android:/data/user/10/com.android.providers.calendar/databases/calendar.db",
	} {
		if !g.Match(p) {
			t.Errorf("the glob does not match %s", p)
		}
	}
	for _, p := range []string{
		"android:/data/data/com.android.providers.calendar/databases/calendar.db-wal",
		"android:/data/data/com.android.providers.contacts/databases/calendar.db",
		"android:/data/data/com.android.providers.calendar/databases/sub/calendar.db",
		"ios:HomeDomain/Library/Calendar/Calendar.sqlitedb",
	} {
		if g.Match(p) {
			t.Errorf("the glob matches %s", p)
		}
	}
}

// 4I-R21: no primary glob matches a companion; the companions reach the job only through their roles.
func TestAndroidCalendarCompanionIsNeverAPrimary(t *testing.T) {
	m := androidcalendar.Parser{}.Meta()
	for _, in := range m.Inputs {
		for _, gs := range in.Globs {
			g, err := parse.CompileGlob(gs)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"/data/data/", "/data/user/10/"} {
				for _, f := range []string{"calendar.db-wal", "calendar.db-journal", "calendar.db-shm"} {
					if p := "android:" + d + "com.android.providers.calendar/databases/" + f; g.Match(p) {
						t.Errorf("the glob %s matches the companion %s", gs, p)
					}
				}
			}
		}
	}
	roles := map[string][]string{}
	for _, in := range m.Inputs[1:] {
		roles[in.Role] = in.Companions
	}
	if len(roles) != 2 || len(roles["wal"]) != 1 || roles["wal"][0] != "-wal" || len(roles["journal"]) != 1 || roles["journal"][0] != "-journal" {
		t.Errorf("companion roles %v", roles)
	}
}

// 4I3-P5, spec 7.2 and B4: at the provider path every refusal is loud (unsupported-schema); anywhere else a database
// of another kind is not applicable, and only a present Events missing a required column is unsupported-schema.
func TestAndroidCalendarProbeByPlace(t *testing.T) {
	if got := probeAt(t, remoteProvider, fixtureFiles(t, "calendar-601")); got.Status != parse.Applicable {
		t.Errorf("calendar-601: Probe = %+v", got)
	}
	noEvents := fixtureFiles(t, "calendar-noevents")
	if got := probeAt(t, remoteProvider, noEvents); got.Status != parse.UnsupportedSchema || got.Reason != "no Events table at the calendar provider path" {
		t.Errorf("calendar-noevents at the provider path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, noEvents); got.Status != parse.NotApplicable {
		t.Errorf("calendar-noevents at %s: Probe = %+v", remoteOther, got)
	}
	text := map[string][]byte{"db": []byte(strings.Repeat("not a database. ", 7)[:100])}
	if got := probeAt(t, remoteProvider, text); got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the calendar provider path" {
		t.Errorf("text at the provider path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, text); got.Status != parse.NotApplicable {
		t.Errorf("text at %s: Probe = %+v", remoteOther, got)
	}
	withWAL := map[string][]byte{"db": text["db"], "wal": {}}
	if got := probeAt(t, remoteProvider, withWAL); got.Status != parse.UnsupportedSchema || got.Reason != "the main file is not a database but a -wal or -journal companion exists" {
		t.Errorf("text with a companion at the provider path: Probe = %+v", got)
	}
	noDeleted := goBuilt(601, `CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, calendar_id INTEGER NOT NULL, dtstart INTEGER,
allDay INTEGER NOT NULL DEFAULT 0, title TEXT)`)
	for _, remote := range []string{remoteProvider, remoteOther} {
		got := probeAt(t, remote, map[string][]byte{"db": noDeleted})
		if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "deleted") {
			t.Errorf("Events without deleted at %s: Probe = %+v", remote, got)
		}
	}
	// the Events key must be the rowid alias (joins by rowid are right only then)
	noAlias := goBuilt(601, `CREATE TABLE Events (_id INTEGER, calendar_id INTEGER NOT NULL, dtstart INTEGER, allDay INTEGER, deleted INTEGER)`)
	if got := probeAt(t, remoteProvider, map[string][]byte{"db": noAlias}); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "rowid alias") {
		t.Errorf("Events without the alias: Probe = %+v", got)
	}
	// Parse at the provider path without Events fails loudly; with a companion Probe defers to it
	noEvents["wal"] = []byte{}
	if got := probeAt(t, remoteProvider, noEvents); got.Status != parse.Applicable || got.Reason != "schema checked at parse time: a -wal or -journal companion is present" {
		t.Errorf("calendar-noevents with a companion: Probe = %+v, want the deferral", got)
	}
	res := parseAt(t, remoteProvider, noEvents)
	if !errors.Is(res.Err, androidcalendar.ErrNoEventsTable) || len(res.Records) != 0 {
		t.Errorf("calendar-noevents: Parse err %v, %d records; want ErrNoEventsTable and none", res.Err, len(res.Records))
	}
	if res := parseAt(t, remoteProvider, fixtureFiles(t, "calendar-601")); res.Err != nil {
		t.Errorf("calendar-601: Parse err %v", res.Err)
	}

	// fix round 1, M4 and ruling D3: an encrypted file is Encrypted at the provider path and not-applicable elsewhere;
	// a non-SQLite main file beside a companion is loud only at the provider path
	enc := map[string][]byte{"db": highEntropy(8192)}
	if got := probeAt(t, remoteProvider, enc); got.Status != parse.Encrypted || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted at the provider path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, enc); got.Status != parse.NotApplicable {
		t.Errorf("encrypted at %s: Probe = %+v, want not-applicable (B4: only the provider path is loud)", remoteOther, got)
	}
	if got := probeAt(t, remoteOther, withWAL); got.Status != parse.NotApplicable {
		t.Errorf("text with a companion at %s: Probe = %+v, want not-applicable", remoteOther, got)
	}

	// ruling D2: a schema not read whole, with no Events in what was read, is stated as such at the provider path
	damaged := goBuilt(601, eventsDDL)
	damaged[108], damaged[109] = 0xff, 0xff // the first schema cell pointer points outside page 1
	if got := probeAt(t, remoteProvider, map[string][]byte{"db": damaged}); got.Status != parse.UnsupportedSchema ||
		got.Reason != "the schema could not be read whole and no Events table was found in what was read" {
		t.Errorf("damaged schema at the provider path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, map[string][]byte{"db": damaged}); got.Status != parse.NotApplicable {
		t.Errorf("damaged schema at %s: Probe = %+v", remoteOther, got)
	}
	// fix round 1, I1: Parse never claims an absent Events from a schema it could not read whole
	res = parseAt(t, remoteProvider, map[string][]byte{"db": damaged, "wal": {}})
	if !errors.Is(res.Err, androidcalendar.ErrSchemaIncomplete) || errors.Is(res.Err, androidcalendar.ErrNoEventsTable) || len(res.Records) != 0 {
		t.Errorf("damaged schema: Parse err %v, %d records; want ErrSchemaIncomplete and none", res.Err, len(res.Records))
	}
	// fix round 1, I1: an Events that exists but cannot be read (a virtual table) is an error, never a silent pass
	virtual := virtualEvents()
	if got := probeAt(t, remoteProvider, map[string][]byte{"db": virtual}); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "Events") {
		t.Errorf("virtual Events at the provider path: Probe = %+v", got)
	}
	res = parseAt(t, remoteProvider, map[string][]byte{"db": virtual, "wal": {}})
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(res.Err, &us) || errors.Is(res.Err, androidcalendar.ErrNoEventsTable) {
		t.Errorf("virtual Events: Parse err %v; want the unsupported-schema error of the Events table", res.Err)
	}
}

// fix round 1, M1 (4I3-P6): with a companion Probe defers before any failure of the full open of the main file, so a
// SQLite main file that cannot be opened (here: a budget too small for one page) is checked by Parse.
func TestAndroidCalendarProbeDefersBeforeOpeningWithACompanion(t *testing.T) {
	db := goBuilt(601, eventsDDL)
	in := func(companion bool) *parse.Input {
		x := &parse.Input{
			Primary: parse.Artifact{R: bytes.NewReader(db), Size: int64(len(db)), Logical: "android:" + remoteProvider, Platform: parse.PlatformAndroid},
			Budget:  parse.NewBudget(1).View(),
		}
		x.Artifacts = map[string]parse.Artifact{"db": x.Primary}
		if companion {
			x.Artifacts["wal"] = parse.Artifact{R: bytes.NewReader(nil), Logical: "android:" + remoteProvider + "-wal", Platform: parse.PlatformAndroid}
		}
		return x
	}
	if _, err := (androidcalendar.Parser{}).Probe(t.Context(), in(false)); err == nil {
		t.Fatal("Probe without a companion opened the main file within a 1-byte budget: the fixture proves nothing")
	}
	got, err := androidcalendar.Parser{}.Probe(t.Context(), in(true))
	if err != nil || got.Status != parse.Applicable || got.Reason != "schema checked at parse time: a -wal or -journal companion is present" {
		t.Errorf("Probe with a companion = %+v, %v; want the deferral", got, err)
	}
}

// highEntropy is n deterministic bytes that look encrypted (a SHA-256 chain).
func highEntropy(n int) []byte {
	var out []byte
	sum := sha256.Sum256([]byte("android.calendar"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// virtualEvents is a Go-built database (user version 601) whose Events is a virtual table.
func virtualEvents() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 601})
	b.CreateTable("Calendars", "CREATE TABLE Calendars (_id INTEGER PRIMARY KEY, name TEXT)")
	b.AddSchemaRow("table", "Events", "Events", int64(0), "CREATE VIRTUAL TABLE Events USING fts4(title)")
	return b.Bytes()
}

// B4, 4I3-P6: a calendar.db is claimed only inside the user-version band 400-699: loudly refused at the provider
// path, not applicable elsewhere (no signature claim outside the band); Parse refuses it too, after a deferral.
func TestAndroidCalendarUserVersionBand(t *testing.T) {
	outside := map[int32][]byte{399: goBuilt(399, eventsDDL), 700: goBuilt(700, eventsDDL), 307: fixtureFiles(t, "calendar-otherversion")["db"]}
	for v, db := range outside {
		reason := "calendar.db user version " + strconv.Itoa(int(v)) + " is outside the supported band 400-699"
		if got := probeAt(t, remoteProvider, map[string][]byte{"db": db}); got.Status != parse.UnsupportedSchema || got.Reason != reason {
			t.Errorf("user version %d at the provider path: Probe = %+v, want unsupported-schema %q", v, got, reason)
		}
		if got := probeAt(t, remoteOther, map[string][]byte{"db": db}); got.Status != parse.NotApplicable || got.Reason != reason {
			t.Errorf("user version %d at %s: Probe = %+v, want not-applicable %q", v, remoteOther, got, reason)
		}
		for _, remote := range []string{remoteProvider, remoteOther} {
			if res := parseAt(t, remote, map[string][]byte{"db": db}); !errors.Is(res.Err, androidcalendar.ErrUnsupportedVersion) || len(res.Records) != 0 {
				t.Errorf("user version %d at %s: Parse err %v, %d records", v, remote, res.Err, len(res.Records))
			}
		}
	}
	for _, v := range []int32{400, 699} {
		for _, remote := range []string{remoteProvider, remoteOther} {
			if got := probeAt(t, remote, map[string][]byte{"db": goBuilt(v, eventsDDL)}); got.Status != parse.Applicable {
				t.Errorf("user version %d at %s: Probe = %+v", v, remote, got)
			}
		}
	}
	other := fixtureFiles(t, "calendar-otherversion")
	other["wal"] = []byte{}
	if got := probeAt(t, remoteProvider, other); got.Status != parse.Applicable || got.Reason != "schema checked at parse time: a -wal or -journal companion is present" {
		t.Errorf("calendar-otherversion with an empty -wal: Probe = %+v, want the deferral", got)
	}
	res := parseAt(t, remoteProvider, other)
	if !errors.Is(res.Err, androidcalendar.ErrUnsupportedVersion) || len(res.Records) != 0 {
		t.Errorf("calendar-otherversion with an empty -wal: Parse err %v, %d records; want ErrUnsupportedVersion and none", res.Err, len(res.Records))
	}
}
