package ioswacontacts_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// The warnings a contacts run may raise: the counted row warnings and the reader's WAL frames the fixtures with a -wal leave.
var rowWarnings = []string{"wal-frames-not-applied", "ZWAADDRESSBOOKCONTACT row "}

// runLenient is Harness.RunLenient plus the checks a lenient run would otherwise skip: no contract violation, no refused
// record, and every warning is one of the expected kinds (allowed; by default only the WAL frames the engine does not apply). A
// test that expects no warning passes "-" (no warning contains it).
func runLenient(t *testing.T, h *parsertest.Harness, p ioswacontacts.Parser, in *parse.Input, allowed ...string) parsertest.Result {
	t.Helper()
	return runLenientWith(t, h, p, in, allowed...)
}

func runLenientWith(t *testing.T, h *parsertest.Harness, p parse.Parser, in *parse.Input, allowed ...string) parsertest.Result {
	t.Helper()
	if len(allowed) == 0 {
		allowed = []string{"wal-frames-not-applied"}
	}
	res := h.RunLenient(p, in)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	if res.Refused != 0 {
		t.Errorf("%d records refused", res.Refused)
	}
	for _, w := range res.Warnings {
		ok := false
		for _, a := range allowed {
			ok = ok || strings.Contains(w.Reason, a)
		}
		if !ok {
			t.Errorf("unexpected warning %s: %s", w.Locator, w.Reason)
		}
	}
	return res
}

// fixtureContacts parses a fixture variant and returns its records by Z_PK.
func fixtureContacts(t *testing.T, v string) (map[int64]records.Record, parsertest.Result) {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
	p := ioswacontacts.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), rowWarnings...)
	if res.Err != nil {
		t.Fatalf("%s: %v", v, res.Err)
	}
	return byRowid(res), res
}

// warningFor is the reason of the warning whose locator names the row, "" when there is none, and how many there are.
func warningFor(res parsertest.Result, rowid int64) (string, int) {
	var reason string
	n := 0
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=ZWAADDRESSBOOKCONTACT") && rowidOf(w.Locator) == rowid {
			reason = w.Reason
			n++
		}
	}
	return reason, n
}

// 4I2-R10 (the mapping is total): a row whose JID names no person is one counted warning with the class, never a record. Rows
// 5 (a group, with a full name) and 12 (a JID that does not parse, with a name) of ios-contactsv2; Refused stays 0 (a warning is
// not a refused record).
func TestIOSWhatsAppContactsNonPersonRowsAreCountedWarnings(t *testing.T) {
	recs, res := fixtureContacts(t, "ios-contactsv2")
	for id, want := range map[int64]string{
		5:  "ZWAADDRESSBOOKCONTACT row is not a person (group)",
		12: "ZWAADDRESSBOOKCONTACT row is not a person (unparseable)",
	} {
		got, n := warningFor(res, id)
		if got != want || n != 1 {
			t.Errorf("row %d: %d warnings, %q, want exactly one %q", id, n, got, want)
		}
		if _, has := recs[id]; has {
			t.Errorf("row %d is a record", id)
		}
	}
	if res.Refused != 0 {
		t.Errorf("Refused %d, want 0", res.Refused)
	}
}

// 4I2-R10, plan "State vocabularies": a row with a user JID and neither a name nor a number is "JID only", never a contact.
func TestIOSWhatsAppContactsJIDOnlyRowsAreCountedWarnings(t *testing.T) {
	recs, res := fixtureContacts(t, "ios-contactsv2")
	got, n := warningFor(res, 4)
	if want := "ZWAADDRESSBOOKCONTACT row has no name or number (JID only)"; got != want || n != 1 {
		t.Errorf("row 4: %d warnings, %q, want exactly one %q", n, got, want)
	}
	if _, has := recs[4]; has {
		t.Error("row 4 is a record")
	}
	if res.Refused != 0 {
		t.Errorf("Refused %d, want 0", res.Refused)
	}
}

// 4I2-R10, PF16: row 6 has a given name only (the writer would refuse it: no full name), row 13's name is a BLOB (unknown is not
// absent), row 17 has no JID and no naming text at all. Each is one counted warning with its exact text, in no record.
func TestIOSWhatsAppContactsNamelessRowsAreCountedWarnings(t *testing.T) {
	recs, res := fixtureContacts(t, "ios-contactsv2")
	for id, want := range map[int64]string{
		6:  "ZWAADDRESSBOOKCONTACT row has no full name or number",
		13: "ZWAADDRESSBOOKCONTACT row has no readable name or number (a name column is damaged or not text)",
		17: "ZWAADDRESSBOOKCONTACT row has no name, number or JID",
	} {
		got, n := warningFor(res, id)
		if got != want || n != 1 {
			t.Errorf("row %d: %d warnings, %q, want exactly one %q", id, n, got, want)
		}
		if _, has := recs[id]; has {
			t.Errorf("row %d is a record", id)
		}
	}
	if res.Refused != 0 {
		t.Errorf("Refused %d, want 0", res.Refused)
	}
}

// Review Focus 5: one ZWHATSAPPID on two rows with different names is two records, each with its own locator; nothing in
// either record links them (no shared source id, no merge marker, no reference to the other row).
func TestIOSWhatsAppContactsDuplicateJIDsAreNotMerged(t *testing.T) {
	recs, _ := fixtureContacts(t, "ios-contactsv2")
	a, b := recs[1], recs[11]
	if a.Locator == "" || b.Locator == "" || a.Locator == b.Locator {
		t.Fatalf("locators %q and %q", a.Locator, b.Locator)
	}
	if a.Payload["display_name"] != "Ann Lee" || b.Payload["display_name"] != "Ann L." {
		t.Errorf("display names %v and %v", a.Payload["display_name"], b.Payload["display_name"])
	}
	ia, _ := a.Payload["ims"].([]any)
	ib, _ := b.Payload["ims"].([]any)
	if len(ia) != 1 || len(ib) != 1 || fmt.Sprint(ia) != fmt.Sprint(ib) {
		t.Fatalf("control: both rows carry the one JID: %v and %v", ia, ib)
	}
	for _, r := range []records.Record{a, b} {
		for _, k := range []string{"source_ids", "deleted", "snapshot", "recovery"} {
			if _, has := r.Payload[k]; has {
				t.Errorf("%s: payload.%s links records", r.Locator, k)
			}
		}
	}
	if b.Payload["source_id"] != nil {
		t.Errorf("row 11 took row 1's source id: %v", b.Payload["source_id"])
	}
	if _, has := rawOf(b)["lid_map"]; has {
		t.Errorf("row 11 took row 1's LID statement: %v", rawOf(b))
	}
	n := 0
	for _, r := range recs {
		if ims, _ := r.Payload["ims"].([]any); fmt.Sprint(ims) == fmt.Sprint(ia) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d records carry the JID, want exactly the two rows", n)
	}
}

// D6: lid_map comes only from the row's own ZLID. Row 1 states it; row 11 (the same phone JID, no ZLID) and every other row
// state none; a store with no ZLID column (ios-contactsv2-nolid) has no lid_map and no lid_state on any record and states the
// column absent; the phone JID is never an ims entry of a LID (no merge).
func TestIOSWhatsAppContactsLIDMappedOnlyByZLID(t *testing.T) {
	recs, res := fixtureContacts(t, "ios-contactsv2")
	lm, _ := rawOf(recs[1])["lid_map"].([]any)
	want := map[string]any{"lid": "100000000000001@lid", "jid": "15550000001@s.whatsapp.net", "source": "ZWAADDRESSBOOKCONTACT.ZLID"}
	if len(lm) != 1 || fmt.Sprint(lm[0]) != fmt.Sprint(want) {
		t.Errorf("row 1 lid_map %v, want [%v]", lm, want)
	}
	for id, r := range recs {
		raw := rawOf(r)
		if id != 1 && raw["lid_map"] != nil {
			t.Errorf("row %d: a lid_map %v without its own ZLID", id, raw["lid_map"])
		}
		if id != 1 && id != 9 && raw["lid_state"] != nil {
			t.Errorf("row %d: lid_state %v without its own ZLID", id, raw["lid_state"])
		}
		if ims, _ := r.Payload["ims"].([]any); id == 1 || id == 11 {
			if fmt.Sprint(ims) != "[map[kind:whatsapp value:15550000001@s.whatsapp.net]]" {
				t.Errorf("row %d: ims %v, want the phone JID only (the LID is never an ims entry of a phone JID)", id, ims)
			}
		}
	}
	if _, has := res.Notes["ZLID"]; has {
		t.Errorf("a store with the ZLID column states a note: %v", res.Notes)
	}
	nolid, nres := fixtureContacts(t, "ios-contactsv2-nolid")
	if len(nolid) != 3 || nres.Notes["ZLID"] != "absent" {
		t.Fatalf("nolid: %d records, notes %v", len(nolid), nres.Notes)
	}
	for id, r := range nolid {
		if _, has := rawOf(r)["lid_map"]; has {
			t.Errorf("nolid row %d: lid_map", id)
		}
		if _, has := rawOf(r)["lid_state"]; has {
			t.Errorf("nolid row %d: lid_state", id)
		}
	}
}

// The label rule of ios.addressbook, restated: the system wrapper with a non-empty X inside is the label X and the stored text
// is raw beside it; any other text (also the wrapper with nothing inside) is kept as it is; a label with no phone is not dropped.
func TestIOSWhatsAppContactsPhoneLabelIsDecodedOnlyWhenWrapped(t *testing.T) {
	const wrapMobile = "_$!<Mobile>!$_"
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZPHONENUMBER": "+1", "ZPHONENUMBERLABEL": wrapMobile}))
		put(2, contactRow(map[string]any{"ZPHONENUMBER": "+2", "ZPHONENUMBERLABEL": "_$!<>!$_"}))
		put(3, contactRow(map[string]any{"ZPHONENUMBER": "+3", "ZPHONENUMBERLABEL": "Work line"}))
		put(4, contactRow(map[string]any{"ZPHONENUMBER": "+4", "ZPHONENUMBERLABEL": "_$!<Home"}))
		put(5, contactRow(map[string]any{"ZFULLNAME": "No phone", "ZPHONENUMBERLABEL": wrapMobile}))
		put(6, contactRow(map[string]any{"ZPHONENUMBER": "+6", "ZPHONENUMBERLABEL": ""}))
		put(7, contactRow(map[string]any{"ZPHONENUMBER": "+7", "ZPHONENUMBERLABEL": []byte(wrapMobile)}))
	})
	recs, _ := runContacts(t, db, nil)
	phone := func(id int64) map[string]any {
		l, _ := recs[id].Payload["phones"].([]any)
		if len(l) != 1 {
			t.Fatalf("row %d: phones %v", id, l)
		}
		m, _ := l[0].(map[string]any)
		return m
	}
	for id, c := range map[int64]struct{ label, stored any }{
		1: {"Mobile", wrapMobile}, 2: {"_$!<>!$_", nil}, 3: {"Work line", nil}, 4: {"_$!<Home", nil}, 6: {nil, nil}, 7: {nil, nil},
	} {
		if phone(id)["label"] != c.label || rawOf(recs[id])["phone_label_stored"] != c.stored {
			t.Errorf("row %d: phone %v, raw %v, want label %v stored %v", id, phone(id), rawOf(recs[id]), c.label, c.stored)
		}
	}
	if recs[5].Payload["phones"] != nil || rawOf(recs[5])["ZPHONENUMBERLABEL"] != wrapMobile {
		t.Errorf("row 5: a label with no phone is not kept: %v", recs[5].Payload)
	}
	uc, _ := rawOf(recs[7])["unknown_columns"].(map[string]any)
	if uc["ZWAADDRESSBOOKCONTACT.ZPHONENUMBERLABEL"] != "type-mismatch" {
		t.Errorf("row 7: a BLOB label is stated, never read as absent: %v", rawOf(recs[7]))
	}
}

// PF16: a name or number column the table LACKS is as unreadable as a damaged cell: a row with a user JID and no other naming
// text is "no readable name or number", never "JID only" (the missing column may have held the name). Each of the four columns
// in turn, with a control that has every column.
func TestIOSWhatsAppContactsMissingNameColumnIsUnreadable(t *testing.T) {
	const damaged = "ZWAADDRESSBOOKCONTACT row has no readable name or number (a name column is damaged or not text)"
	jidOnly := func(drop ...string) (string, int) {
		db, _ := buildContacts(drop, func(put putRow) { put(1, contactRow(map[string]any{"ZWHATSAPPID": "15550000001@s.whatsapp.net"})) })
		recs, res := runContacts(t, db, nil, "ZWAADDRESSBOOKCONTACT row ")
		w, _ := warningFor(res, 1)
		return w, len(recs)
	}
	if w, n := jidOnly(); w != "ZWAADDRESSBOOKCONTACT row has no name or number (JID only)" || n != 0 {
		t.Fatalf("control (every column): warning %q, %d records", w, n)
	}
	for _, col := range []string{"ZFULLNAME", "ZPHONENUMBER", "ZGIVENNAME", "ZHIGHLIGHTEDNAME"} {
		if w, n := jidOnly(col); w != damaged || n != 0 {
			t.Errorf("table without %s: warning %q, %d records, want %q and none", col, w, n, damaged)
		}
	}
	// a missing column does not hide a name that is there: the other column's text still makes the record
	db, _ := buildContacts([]string{"ZFULLNAME"}, func(put putRow) {
		put(1, contactRow(map[string]any{"ZPHONENUMBER": "+1", "ZWHATSAPPID": "15550000001@s.whatsapp.net"}))
	})
	if recs, _ := runContacts(t, db, nil); len(recs) != 1 || recs[1].Payload["display_name"] != nil {
		t.Errorf("a phone with no ZFULLNAME column: %v", recs)
	}
}

// A JID too long is checked before altered and keeps no text (never cut); a cleaned JID is altered; neither is parsed, so
// neither makes the row a non-person (a long group JID is too-long, not "group").
func TestIOSWhatsAppContactsHostileJIDs(t *testing.T) {
	long := strings.Repeat("7", 1500) + "@g.us"
	longBad := strings.Repeat("\xff", 600) + "@s.whatsapp.net" // 600 bytes, 1800 once cleaned
	huge := strings.Repeat("9", 1<<20) + "@s.whatsapp.net"
	const server = "@s.whatsapp.net"
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "Long", "ZWHATSAPPID": long}))
		put(2, contactRow(map[string]any{"ZFULLNAME": "LongBad", "ZWHATSAPPID": longBad}))
		put(3, contactRow(map[string]any{"ZFULLNAME": "Huge", "ZWHATSAPPID": huge}))
		put(4, contactRow(map[string]any{"ZFULLNAME": "Bad", "ZWHATSAPPID": "1\xff" + server}))
		put(5, contactRow(map[string]any{"ZFULLNAME": "Edge", "ZWHATSAPPID": strings.Repeat("1", 1024-len(server)) + server}))
		put(6, contactRow(map[string]any{"ZFULLNAME": "Over", "ZWHATSAPPID": strings.Repeat("1", 1025-len(server)) + server}))
	})
	recs, _ := runContacts(t, db, nil)
	for id, state := range map[int64]string{1: "too-long", 2: "too-long", 3: "too-long", 4: "altered", 6: "too-long"} {
		raw := rawOf(recs[id])
		if raw["whatsapp_id_state"] != state || recs[id].Payload["ims"] != nil {
			t.Errorf("row %d: state %v ims %v, want %s and no ims", id, raw["whatsapp_id_state"], recs[id].Payload["ims"], state)
		}
		if _, has := raw["whatsapp_id_text"]; has != (state == "altered") {
			t.Errorf("row %d: whatsapp_id_text present = %v for state %s", id, has, state)
		}
	}
	if recs[5].Payload["ims"] == nil || rawOf(recs[5])["whatsapp_id_state"] != nil {
		t.Errorf("a JID of exactly 1024 bytes is a JID: %v", recs[5].Payload)
	}
}

// Raw never grows from a BLOB: a BLOB over 256 bytes in a column raw states as stored is its length only; a NaN REAL, which the
// reader gives as NULL with its serial type, is stated and never read as absent (a NaN in an INTEGER or TEXT column is a
// type mismatch, in a raw-as-stored column "non-finite").
func TestIOSWhatsAppContactsBlobsAndNaNAreStated(t *testing.T) {
	big := make([]byte, 300)
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "Blob", "ZHIGHLIGHTEDNAME": big, "ZABOUTTIMESTAMP": []byte{1, 2}}))
		put(2, contactRow(map[string]any{"ZFULLNAME": "NaN", "ZABOUTTIMESTAMP": math.NaN(), "ZHIGHLIGHTEDNAME": math.Inf(1), "Z_ENT": math.NaN(), "ZWHATSAPPID": math.NaN()}))
		put(3, contactRow(map[string]any{"ZFULLNAME": "Floats", "ZABOUTTIMESTAMP": 2.5, "ZHIGHLIGHTEDNAME": int64(7)}))
	})
	recs, _ := runContacts(t, db, nil)
	r1, r2, r3 := rawOf(recs[1]), rawOf(recs[2]), rawOf(recs[3])
	if fmt.Sprint(r1["ZHIGHLIGHTEDNAME"]) != "map[blob_len:300]" || fmt.Sprint(r1["ZABOUTTIMESTAMP"]) != "map[blob_b64:AQI=]" {
		t.Errorf("blobs: %v", r1)
	}
	if r2["ZABOUTTIMESTAMP"] != "non-finite" || r2["ZHIGHLIGHTEDNAME"] != "non-finite" {
		t.Errorf("NaN and Inf as stored: %v", r2)
	}
	uc, _ := r2["unknown_columns"].(map[string]any)
	if uc["ZWAADDRESSBOOKCONTACT.Z_ENT"] != "type-mismatch" || uc["ZWAADDRESSBOOKCONTACT.ZWHATSAPPID"] != "type-mismatch" || r2["whatsapp_id_state"] != "not-text" || r2["Z_ENT"] != nil {
		t.Errorf("NaN in an INTEGER and a TEXT column is never absent: %v", r2)
	}
	if r3["ZABOUTTIMESTAMP"] != 2.5 || r3["ZHIGHLIGHTEDNAME"] != int64(7) {
		t.Errorf("stored values: %v", r3)
	}
	for _, r := range recs {
		if r.Time != nil || r.TimeEnd != nil || len(r.Times) != 0 {
			t.Errorf("%s: a time from ZABOUTTIMESTAMP: %v %v %v", r.Locator, r.Time, r.TimeEnd, r.Times)
		}
	}
}

// Text beyond the cap: a 4 MiB about text and name are cut at 256 KiB, flagged, and the parse stays inside a small MemBudget.
func TestIOSWhatsAppContactsHugeTextIsCapped(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": big, "ZABOUTTEXT": big, "ZIDENTIFIER": big, "ZHIGHLIGHTEDNAME": big}))
	})
	res := hostileRun(t, db, nil, nil, 256<<20)
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	r := res.Records[0]
	flags, _ := rawOf(r)["text_flags"].([]any)
	for _, f := range []string{"ZFULLNAME", "ZABOUTTEXT", "ZIDENTIFIER", "ZHIGHLIGHTEDNAME"} {
		if !slices.Contains(flags, any(f)) {
			t.Errorf("text_flags %v lacks %s", flags, f)
		}
	}
	if s, _ := r.Payload["display_name"].(string); len(s) != 256<<10 {
		t.Errorf("display name %d bytes, want the cap", len(s))
	}
	if r.Body == "" || len(r.Body) > 4<<20 {
		t.Errorf("body %d bytes", len(r.Body))
	}
}

// refusing is an emitter that refuses every warning whose locator starts with prefix and drops everything else.
type refusing struct{ prefix string }

func (refusing) Emit(context.Context, records.Record) error { return nil }
func (r refusing) Warn(_ context.Context, locator, _ string) error {
	if strings.HasPrefix(locator, r.prefix) {
		return errors.New("warning refused")
	}
	return nil
}
func (refusing) Note(string, string)   {}
func (refusing) Progress(int64, int64) {}

// A warning the host refuses is the job's failure, never swallowed: a row warning (a group row) and a reader warning (a damaged
// leaf of the table).
func TestIOSWhatsAppContactsARefusedWarningFailsTheJob(t *testing.T) {
	good, _ := buildContacts(nil, func(put putRow) { put(1, contactRow(map[string]any{"ZFULLNAME": "G", "ZWHATSAPPID": "1@g.us"})) })
	damaged := damagedStore(t)
	for name, c := range map[string]struct {
		db     []byte
		prefix string
	}{"row warning": {good, "sqlite:table="}, "reader warning": {damaged, "sqlite:file="}} {
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: "refuse", Files: map[string][]byte{"db": c.db}}.BundleIOSBackup(h, domainConsumer, dbName)
		p := ioswacontacts.Parser{}
		err := p.Parse(t.Context(), b.Input(h, p, false), refusing{prefix: c.prefix})
		if err == nil || !strings.Contains(err.Error(), "warning refused") {
			t.Errorf("%s: err %v, want the refused warning", name, err)
		}
	}
}
