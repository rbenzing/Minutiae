package androidwacontacts_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// rowOf is the rowid a row locator names (sqlite:table=wa_contacts;rowid=N...).
func rowOf(loc string) int64 {
	_, rest, _ := strings.Cut(loc, "rowid=")
	digits, _, _ := strings.Cut(rest, ";")
	n, _ := strconv.ParseInt(digits, 10, 64)
	return n
}

// warningsByRow collects the counted warnings of a run that name a wa_contacts row.
func warningsByRow(res parsertest.Result) map[int64]string {
	out := map[int64]string{}
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Locator, "sqlite:table=wa_contacts;") {
			out[rowOf(w.Locator)] = w.Reason
		}
	}
	return out
}

func recordRows(res parsertest.Result) map[int64]bool {
	out := map[int64]bool{}
	for _, r := range res.Records {
		out[rowOf(r.Locator)] = true
	}
	return out
}

// Spec 6.6: a row whose JID is a group, a broadcast list, the status feed, a channel or any other server, or does not
// parse, is not a contact: one counted warning naming the class, never a record and never a refused record.
func TestWAContactsNonPersonRowsAreCountedWarnings(t *testing.T) {
	res := runVariant(t, "wa-current")
	if res.Err != nil || res.Refused != 0 || len(res.Violations) != 0 {
		t.Fatalf("err %v, refused %d, violations %v", res.Err, res.Refused, res.Violations)
	}
	warns, recs := warningsByRow(res), recordRows(res)
	for id, class := range map[int64]string{6: "group", 7: "broadcast", 8: "status", 9: "newsletter", 10: "other", 11: "unparseable"} {
		if !strings.Contains(warns[id], "wa_contacts row is not a person ("+class+")") {
			t.Errorf("row %d warning %q, want a non-person warning naming %s", id, warns[id], class)
		}
		if recs[id] {
			t.Errorf("row %d is a record", id)
		}
	}
	// a LID is a person: a record, as the user rows are
	for _, id := range []int64{1, 2, 3, 5, 13, 14} {
		if !recs[id] {
			t.Errorf("row %d (a user or LID) is not a record", id)
		}
	}
}

// R2: a user with no display name, no push name and no number (only a JID) is not a contact record: one counted warning.
func TestWAContactsJIDOnlyRowsAreCountedWarnings(t *testing.T) {
	res := runVariant(t, "wa-current")
	warns, recs := warningsByRow(res), recordRows(res)
	if !strings.Contains(warns[4], "wa_contacts row has no name or number (JID only)") || recs[4] || res.Refused != 0 {
		t.Errorf("row 4: warning %q, record %v, refused %d", warns[4], recs[4], res.Refused)
	}
	// empty strings are no name either: JID only
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("wa_contacts", "create table wa_contacts(_id integer primary key, jid text, display_name text, wa_name text, number text)")
	tbl.Insert(1, nil, "15550000001@s.whatsapp.net", "", nil, "")
	tbl.Insert(2, nil, "15550000002@s.whatsapp.net", nil, nil, nil)
	got := parseBytes(t, b.Bytes())
	for _, id := range []int64{1, 2} {
		if !strings.Contains(got.warnings[id], "(JID only)") || got.records[id] {
			t.Errorf("row %d: warning %q record %v", id, got.warnings[id], got.records[id])
		}
	}
	if got.res.Refused != 0 {
		t.Errorf("refused %d", got.res.Refused)
	}
}

// Review Focus 4: a row with only given_name, family_name, company or title is one counted warning that names the row,
// never a writer-rejected record that would make the job incomplete.
func TestWAContactsNamelessRowsAreCountedWarnings(t *testing.T) {
	res := runVariant(t, "wa-current")
	warns, recs := warningsByRow(res), recordRows(res)
	if !strings.Contains(warns[12], "wa_contacts row has no display name, push name or number") || recs[12] || res.Refused != 0 {
		t.Errorf("row 12: warning %q, record %v, refused %d", warns[12], recs[12], res.Refused)
	}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("wa_contacts", "create table wa_contacts(_id integer primary key, jid text, display_name text, number text, wa_name text, given_name text, family_name text, company text, title text)")
	only := func(rowid int64, given, family, company, title any) {
		tbl.Insert(rowid, nil, "1555000"+strconv.FormatInt(rowid, 10)+"@s.whatsapp.net", nil, nil, nil, given, family, company, title)
	}
	only(1, "Gina", nil, nil, nil)
	only(2, nil, "Fox", nil, nil)
	only(3, nil, nil, "Acme", nil)
	only(4, nil, nil, nil, "CEO")
	got := parseBytes(t, b.Bytes())
	for id := int64(1); id <= 4; id++ {
		if !strings.Contains(got.warnings[id], "wa_contacts row has no display name, push name or number") || got.records[id] {
			t.Errorf("row %d: warning %q record %v", id, got.warnings[id], got.records[id])
		}
	}
	if got.res.Err != nil || got.res.Refused != 0 || len(got.res.Violations) != 0 {
		t.Errorf("err %v refused %d violations %v: a nameless row must be a warning, not a refused record", got.res.Err, got.res.Refused, got.res.Violations)
	}
}

type parsed struct {
	res      parsertest.Result
	warnings map[int64]string
	records  map[int64]bool
}

func parseBytes(t *testing.T, db []byte) parsed {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "built", Files: map[string][]byte{"db": db}}.Bundle(h, remoteLive)
	p := androidwacontacts.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	return parsed{res: res, warnings: warningsByRow(res), records: recordRows(res)}
}

// Unknown is not absent: a name cell that is not text, a JID that is not text or is over 1024 bytes, a NUL in a JID,
// and a device-suffixed user JID are each stated, never read as empty.
func TestWAContactsHostileRowsAreStatedNotInvented(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tbl := b.CreateTable("wa_contacts", "create table wa_contacts(_id integer primary key, jid text, display_name text, number text, wa_name text)")
	tbl.Insert(1, nil, "15550000001@s.whatsapp.net", int64(42), nil, nil)              // a name cell of another class and nothing else
	tbl.Insert(2, nil, int64(7), "Ann", nil, nil)                                      // a JID that is not text
	tbl.Insert(3, nil, strings.Repeat("1", 2000)+"@s.whatsapp.net", "Ann", nil, nil)   // a JID over 1024 bytes
	tbl.Insert(4, nil, "1555\x00@s.whatsapp.net", "Ann", nil, nil)                     // a NUL in the JID
	tbl.Insert(5, nil, "15550000005:7@s.whatsapp.net", "Eve", "+1555", nil)            // a device suffix: a user, kept as stored
	tbl.Insert(6, nil, "15550000006@s.whatsapp.net", nil, nil, []byte("pushname\xff")) // a BLOB wa_name and nothing else
	got := parseBytes(t, b.Bytes())
	if got.res.Err != nil || got.res.Refused != 0 || len(got.res.Violations) != 0 {
		t.Fatalf("err %v refused %d violations %v", got.res.Err, got.res.Refused, got.res.Violations)
	}
	for id, want := range map[int64]string{
		1: "no readable name or number", 2: "no readable JID", 3: "JID over 1024 bytes", 4: "(unparseable)", 6: "no readable name or number",
	} {
		if !strings.Contains(got.warnings[id], want) || got.records[id] {
			t.Errorf("row %d: warning %q record %v, want a warning containing %q", id, got.warnings[id], got.records[id], want)
		}
	}
	if !got.records[5] || got.warnings[5] != "" {
		t.Fatalf("row 5 (a device-suffixed user): warning %q, record %v", got.warnings[5], got.records[5])
	}
	for _, r := range got.res.Records {
		ims, _ := r.Payload["ims"].([]any)
		if len(ims) != 1 || ims[0].(map[string]any)["value"] != "15550000005:7@s.whatsapp.net" {
			t.Errorf("ims %v: the JID is kept as stored, suffix included", ims)
		}
	}
}

// The app label comes from the path (spec 6.8): WhatsApp Business is raw.app whatsapp_business.
func TestWAContactsAppLabel(t *testing.T) {
	for v, want := range map[string]string{"wa-business": "whatsapp_business", "wa-current": "whatsapp", "wa-2.11": "whatsapp"} {
		res := runVariant(t, v)
		if len(res.Records) == 0 {
			t.Fatalf("%s: no records", v)
		}
		for _, r := range res.Records {
			raw, _ := r.Payload["raw"].(map[string]any)
			if raw["app"] != want || raw["app_basis"] != "path" {
				t.Errorf("%s %s: raw.app %v basis %v, want %s from the path", v, r.Locator, raw["app"], raw["app_basis"], want)
			}
		}
		if res.Notes["app"] != want {
			t.Errorf("%s: note app %q", v, res.Notes["app"])
		}
	}
	// the stub tables of wa.db are named in the note, never mapped
	if got := runVariant(t, "wa-current").Notes["unmapped_tables"]; got != "wa_biz_profiles,wa_block_list,wa_group_admin_settings,wa_vnames" {
		t.Errorf("unmapped_tables %q", got)
	}
}

// R2, 4I-R4: no record of this parser carries a time of its own, a deleted marker or a recovery stamp.
func TestWAContactsRecordsAreLiveAndTimeless(t *testing.T) {
	for _, r := range runVariant(t, "wa-current").Records {
		if r.Time != nil || r.TimeEnd != nil || len(r.Times) != 0 || r.Deleted || r.Recovery != "" {
			t.Errorf("%s: time %v end %v times %v deleted %v recovery %q", r.Locator, r.Time, r.TimeEnd, r.Times, r.Deleted, r.Recovery)
		}
		if _, has := r.Payload["deleted"]; has {
			t.Errorf("%s: a deleted marker", r.Locator)
		}
	}
}
