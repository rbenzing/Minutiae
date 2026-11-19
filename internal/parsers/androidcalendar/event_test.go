package androidcalendar_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// recordedVariants are the fixtures whose oracle states records.
var recordedVariants = []string{"calendar-601", "calendar-600", "calendar-502", "calendar-403", "calendar-403-upgraded-601", "calendar-deleted"}

// parsed is one Parse run of a fixture or a Go-built database: the result and its records by Events rowid.
type parsed struct {
	res  parsertest.Result
	recs map[int64]records.Record
}

func byRowid(t *testing.T, res parsertest.Result) parsed {
	t.Helper()
	p := parsed{res: res, recs: map[int64]records.Record{}}
	for _, r := range res.Records {
		rest, ok := strings.CutPrefix(r.Locator, "sqlite:table=Events;rowid=")
		if !ok {
			t.Fatalf("locator %q is not an Events row", r.Locator)
		}
		num, _, _ := strings.Cut(rest, ";")
		id, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		p.recs[id] = r
	}
	return p
}

func parseVariant(t *testing.T, variant string) parsed {
	t.Helper()
	p := byRowid(t, parseAt(t, remoteProvider, fixtureFiles(t, variant)))
	if p.res.Err != nil || len(p.res.Violations) != 0 {
		t.Fatalf("%s: Parse err %v, violations %v", variant, p.res.Err, p.res.Violations)
	}
	return p
}

func (p parsed) rec(t *testing.T, id int64) records.Record {
	t.Helper()
	r, ok := p.recs[id]
	if !ok {
		t.Fatalf("no record for Events row %d", id)
	}
	return r
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func objOf(r records.Record, k string) map[string]any {
	m, _ := r.Payload[k].(map[string]any)
	return m
}

// warningsWith are the warnings whose reason starts with prefix.
func warningsWith(res parsertest.Result, prefix string) []parsertest.Warning {
	var out []parsertest.Warning
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Reason, prefix) {
			out = append(out, w)
		}
	}
	return out
}

// Spec 6.3 and B7: an app delete pending sync (deleted = 1, dirty = 1, a mutator) is Deleted by Events.deleted, never
// live; dirty and mutators are stated as stored.
func TestAndroidCalendarSoftDeletedIsNotLive(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 12)
	raw := rawOf(r)
	if !r.Deleted || objOf(r, "deleted")["source"] != "Events.deleted" || raw["dirty"] != int64(1) || raw["mutators"] != "com.example.app" {
		t.Errorf("row 12: Deleted %v, deleted %v, dirty %v, mutators %v", r.Deleted, r.Payload["deleted"], raw["dirty"], raw["mutators"])
	}
}

// Spec 6.3 and Q9 (coordinator ruling 9): the events of a deleted calendar are Deleted by Calendars.deleted.
func TestAndroidCalendarDeletedCalendarHidesItsEvents(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 13)
	if !r.Deleted || objOf(r, "deleted")["source"] != "Calendars.deleted" || rawOf(r)["deleted_via"] != "calendar" {
		t.Errorf("row 13: Deleted %v, deleted %v, deleted_via %v", r.Deleted, r.Payload["deleted"], rawOf(r)["deleted_via"])
	}
}

// Spec 6.3, Q2 and B3: the provider's last-synced copy is Deleted with the relation last-synced-copy and shares the
// sync id of its live row, which stays live; the decoded payload of the copy is never Live.
func TestAndroidCalendarLastSyncedCopyIsNotLive(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	cp, live := p.rec(t, 15), p.rec(t, 14)
	d := objOf(cp, "deleted")
	if !cp.Deleted || d["source"] != "Events.lastSynced" || d["relation"] != "last-synced-copy" || rawOf(cp)["last_synced"] != int64(1) || cp.Payload["sync_id"] != "s14" {
		t.Errorf("row 15: Deleted %v, deleted %v, last_synced %v, sync_id %v", cp.Deleted, d, rawOf(cp)["last_synced"], cp.Payload["sync_id"])
	}
	if live.Deleted || live.Payload["deleted"] != nil || live.Payload["sync_id"] != "s14" {
		t.Errorf("row 14: Deleted %v, deleted %v, sync_id %v", live.Deleted, live.Payload["deleted"], live.Payload["sync_id"])
	}
	for id, r := range map[int64]records.Record{15: cp, 14: live} {
		b, err := json.Marshal(r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		e, err := calendar.Decode(calendar.PayloadVersion, b)
		if err != nil {
			t.Fatal(err)
		}
		if e.Live() != (id == 14) {
			t.Errorf("row %d: Live() = %v", id, e.Live())
		}
	}
}

// Spec 6.3, B7, B9 (F07, F26): an unreadable or out-of-set deletion flag is stated, never deleted-by-guess and never
// live-by-default; one counted warning per column after Run, and each affected record carries its own raw state.
func TestAndroidCalendarUndecidedFlagsAreStated(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	for _, c := range []struct {
		id         int64
		key, ucCol string
	}{{32, "deleted_state", "Events.deleted"}, {33, "deleted_via_state", "Calendars.deleted"}, {34, "lastSynced_state", "Events.lastSynced"}} {
		r := p.rec(t, c.id)
		raw := rawOf(r)
		uc, _ := raw["unknown_columns"].(map[string]any)
		if r.Deleted || r.Payload["deleted"] != nil || raw[c.key] != "undecided" || uc[c.ucCol] != "type-mismatch" {
			t.Errorf("row %d: Deleted %v, %s %v, unknown_columns %v", c.id, r.Deleted, c.key, raw[c.key], uc)
		}
	}
	want := map[string]string{
		"calendar-flag-undecided: Events.deleted: 1 records":    "sqlite:table=Events",
		"calendar-flag-undecided: Events.lastSynced: 1 records": "sqlite:table=Events",
		"calendar-flag-undecided: Calendars.deleted: 1 records": "sqlite:table=Calendars",
	}
	got := warningsWith(p.res, "calendar-flag-undecided")
	if len(got) != len(want) {
		t.Errorf("flag warnings %v, want one per column: %v", got, want)
	}
	for _, w := range got {
		if loc, ok := want[w.Reason]; !ok || loc != w.Locator {
			t.Errorf("warning %+v is not one of %v", w, want)
		}
	}
}

// Review Focus 4: a last-synced copy whose live row is gone stays Deleted with its relation and sync id, and no claim
// about a live row is invented: none of the named raw keys is present.
func TestAndroidCalendarOrphanLastSyncedCopyStaysNotLive(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 23)
	if !r.Deleted || objOf(r, "deleted")["relation"] != "last-synced-copy" || r.Payload["sync_id"] != "s23" {
		t.Errorf("row 23: Deleted %v, deleted %v, sync_id %v", r.Deleted, r.Payload["deleted"], r.Payload["sync_id"])
	}
	for _, k := range []string{"original_state", "deleted_via", "deletion_also", "join_undecided"} {
		if v, has := rawOf(r)[k]; has {
			t.Errorf("row 23: raw.%s = %v: nothing about another row is claimed", k, v)
		}
	}
}

// Review Focus 3: one event in two calendars (the same uid2445 and _sync_id) is two records, never merged.
func TestAndroidCalendarSameUIDInTwoCalendarsIsNotMerged(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	for id, title := range map[int64]string{1: "Personal", 22: "Team"} {
		r := p.rec(t, id)
		if r.Locator != "sqlite:table=Events;rowid="+strconv.FormatInt(id, 10) || objOf(r, "calendar")["title"] != title || r.Payload["uid"] != "uid-1@example.test" {
			t.Errorf("row %d: locator %q, calendar %v, uid %v", id, r.Locator, r.Payload["calendar"], r.Payload["uid"])
		}
	}
}

// Spec 7.3: Events.organizer has NUMERIC affinity; a number is not the text that was written.
func TestAndroidCalendarNumericOrganizerIsNotText(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 16)
	raw := rawOf(r)
	if r.Payload["organizer"] != nil || raw["organizer"] != int64(15550000001) || raw["organizer_state"] != "not-text" {
		t.Errorf("row 16: organizer %v, raw.organizer %#v, organizer_state %v", r.Payload["organizer"], raw["organizer"], raw["organizer_state"])
	}
}

// Spec 6.2 and 4.3 (F28): an exception links its series and states the replaced occurrence as original_start; a
// cancelled exception is a status; a dangling original_id is stated.
func TestAndroidCalendarExceptionLinksItsSeries(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	r9 := p.rec(t, 9)
	if x := objOf(r9, "exception_of"); len(x) != 1 || x["event_id"] != "2" {
		t.Errorf("row 9: exception_of %v", x)
	}
	if len(r9.Times) != 1 || r9.Times[0].Kind != calendar.OriginalStart || r9.Times[0].Time.Basis != records.BasisUTC ||
		r9.Times[0].Time.T.UTC().Format("2006-01-02T15:04:05Z") != "2023-11-21T22:13:20Z" {
		t.Errorf("row 9: Times %+v", r9.Times)
	}
	if !strings.HasPrefix(r9.Summary, "calendar exception ") {
		t.Errorf("row 9: summary %q", r9.Summary)
	}
	if r10 := p.rec(t, 10); !strings.HasSuffix(r10.Summary, " [cancelled]") || r10.Deleted {
		t.Errorf("row 10: summary %q, Deleted %v", r10.Summary, r10.Deleted)
	}
	if r11 := p.rec(t, 11); rawOf(r11)["original_state"] != "no-events-row" || objOf(r11, "exception_of")["sync_id"] != "s99" {
		t.Errorf("row 11: original_state %v, exception_of %v", rawOf(r11)["original_state"], r11.Payload["exception_of"])
	}
}

// Spec 6.1: one record per stored Events row, never an occurrence; a series is one record at its first occurrence.
// The count of stored rows is the oracle's, which the generator reads from the engine's view of the same files.
func TestCalendarRecurrenceIsNeverExpanded(t *testing.T) {
	for _, v := range recordedVariants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if got := len(parseVariant(t, v).recs); got != len(fx.Expect.Records) {
			t.Errorf("%s: %d records, the engine's live view holds %d Events rows", v, got, len(fx.Expect.Records))
		}
	}
	r2 := parseVariant(t, "calendar-601").rec(t, 2)
	if !strings.HasPrefix(r2.Summary, "calendar series ") || objOf(r2, "recurrence")["rrule"] != "FREQ=WEEKLY;BYDAY=TU" {
		t.Errorf("row 2: summary %q, recurrence %v", r2.Summary, r2.Payload["recurrence"])
	}
}

// Spec 9: an identifier over its cap is never cut: absent, with its state, length and the SHA-256 of the stored bytes.
func TestCalendarIdentifiersAreNeverCut(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	for _, c := range []struct {
		id         int64
		field, col string
		stored     string
	}{{38, "start_timezone", "eventTimezone", "Zone/" + strings.Repeat("x", 295)}, {39, "uid", "uid2445", "uid-" + strings.Repeat("x", 1996)}} {
		r := p.rec(t, c.id)
		raw := rawOf(r)
		sum := sha256.Sum256([]byte(c.stored))
		if r.Payload[c.field] != nil || raw[c.col+"_state"] != "too-long" || raw[c.col+"_len"] != int64(len(c.stored)) || raw[c.col+"_sha256"] != hex.EncodeToString(sum[:]) {
			t.Errorf("row %d: %s %v, raw %v %v %v", c.id, c.field, r.Payload[c.field], raw[c.col+"_state"], raw[c.col+"_len"], raw[c.col+"_sha256"])
		}
	}
}

// Spec 5.1: the basis of a record's times follows its timing; no offset is ever stated.
func TestCalendarTimingDecidesTimeBasis(t *testing.T) {
	n := 0
	for _, v := range recordedVariants {
		for _, r := range parseVariant(t, v).res.Records {
			n++
			timing, _ := r.Payload["timing"].(string)
			want := map[string]records.Basis{"timed": records.BasisUTC, "all_day": records.BasisLocalUnknown, "floating": records.BasisLocalUnknown}[timing]
			ts := []*records.Time{r.Time, r.TimeEnd}
			for i := range r.Times {
				ts = append(ts, &r.Times[i].Time)
			}
			if timing == "unknown" && (r.Time != nil || r.TimeEnd != nil) {
				t.Errorf("%s %s: unknown timing with a time", v, r.Locator)
			}
			for i, tm := range ts {
				if tm == nil {
					continue
				}
				if tm.OffsetMin != 0 || tm.Basis == records.BasisLocalOffset {
					t.Errorf("%s %s: time %d states an offset: %+v", v, r.Locator, i, tm)
				}
				if i < 2 && tm.Basis != want {
					t.Errorf("%s %s: timing %q, basis %q", v, r.Locator, timing, tm.Basis)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no record")
	}
}

// ---------------------------------------------------------------- Go-built databases (user version 601, F05)

// snapshotDDL returns the CREATE TABLE statement of table in the 601 snapshot.
func snapshotDDL(t *testing.T, table string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "calendar-android", "601.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "CREATE TABLE "+table+" (") {
			return strings.TrimSuffix(strings.TrimSpace(l), ";")
		}
	}
	t.Fatalf("no CREATE TABLE %s in 601.sql", table)
	return ""
}

// columnsOf is the column names of a CREATE TABLE statement, in order (a comma inside quotes or parentheses does not
// split; table constraints are skipped).
func columnsOf(ddl string) []string {
	body := ddl[strings.Index(ddl, "(")+1 : strings.LastIndex(ddl, ")")]
	var parts []string
	depth, quote, start := 0, false, 0
	for i, c := range body {
		switch {
		case c == '\'':
			quote = !quote
		case quote:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, body[start:i])
			start = i + 1
		}
	}
	parts = append(parts, body[start:])
	var cols []string
	for _, p := range parts {
		f := strings.Fields(p)
		if len(f) == 0 || f[0] == "UNIQUE" || f[0] == "PRIMARY" || f[0] == "FOREIGN" || f[0] == "CHECK" {
			continue
		}
		cols = append(cols, f[0])
	}
	return cols
}

// row is one Go-built row: its rowid and the stored values by column (a missing column stores the default of the
// scenario, a nil value NULL).
type row struct {
	id int64
	kw map[string]any
}

const (
	tD = int64(1700000000000) // 2023-11-14T22:13:20Z
	tH = int64(3600000)
)

func eventDefaults() map[string]any {
	return map[string]any{
		"calendar_id": int64(1), "allDay": int64(0), "dtstart": tD, "dtend": tD + tH, "eventTimezone": "Europe/Berlin",
		"deleted": int64(0), "lastSynced": int64(0), "dirty": int64(0),
	}
}

func calendarDefaults() map[string]any {
	return map[string]any{
		"calendar_displayName": "Personal", "account_type": "com.example.sync", "account_name": "ann@example.test",
		"ownerAccount": "ann@example.test", "deleted": int64(0), "visible": int64(1), "sync_events": int64(1),
	}
}

func insertRows(tb *sqlitetest.Table, cols []string, defaults map[string]any, rows []row) {
	for _, r := range rows {
		vals := make([]any, len(cols))
		for i, c := range cols {
			if c == "_id" {
				continue // the rowid alias stores NULL
			}
			v, ok := r.kw[c]
			if !ok {
				v = defaults[c]
			}
			vals[i] = v
		}
		tb.Insert(r.id, vals...)
	}
}

// buildCalendarDB is a Go-built calendar.db (user version 601) with the 601 Events table and, unless calendars is
// nil, the 601 Calendars table.
func buildCalendarDB(t *testing.T, events, calendars []row) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 601})
	if calendars != nil {
		ddl := snapshotDDL(t, "Calendars")
		insertRows(b.CreateTable("Calendars", ddl), columnsOf(ddl), calendarDefaults(), calendars)
	}
	ddl := snapshotDDL(t, "Events")
	insertRows(b.CreateTable("Events", ddl), columnsOf(ddl), eventDefaults(), events)
	return b.Bytes()
}

// parseBuilt parses a Go-built database at the provider path (with an empty -wal when wal is set: Probe defers).
func parseBuilt(t *testing.T, db []byte, wal bool) parsed {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal {
		files["wal"] = []byte{}
	}
	p := byRowid(t, parseAt(t, remoteProvider, files))
	if p.res.Err != nil || len(p.res.Violations) != 0 {
		t.Fatalf("Parse err %v, violations %v", p.res.Err, p.res.Violations)
	}
	return p
}

func TestMappingsAreTheEventsTable(t *testing.T) {
	m := androidcalendar.Parser{}.Mappings()
	if len(m) != 1 || m[0].Table != "Events" || m[0].Map == nil {
		t.Fatalf("Mappings = %+v, want the one Events mapping (plan 4I3 Task 4)", m)
	}
	var req []string
	for _, c := range m[0].Columns {
		if c.Required {
			req = append(req, c.Name)
		}
	}
	if fmt.Sprint(req) != "[_id calendar_id dtstart allDay deleted]" {
		t.Errorf("required columns %v (spec 7.2)", req)
	}
}
