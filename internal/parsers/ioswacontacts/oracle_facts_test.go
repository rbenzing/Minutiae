package ioswacontacts_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator (tools/fixtures/whatsapp_ios_contacts.py) is the scenario oracle; these literal facts are written from the spec
// (6.11 ios.whatsapp.contacts, D6), the rulings 4I2-R6, R7, R10 and PF13 and the contacts vocabulary of the plan, never read back
// from the generator, so a generator that drifted from the rules fails here before any parser exists. The oracle and these facts
// are two readings of the same spec by one author (4I2-R13): they catch a divergence between the text and the implementation, not
// a reading both share.

type contactRecs struct {
	t  *testing.T
	by map[int64]parsertest.ExpRecord
}

func recsOf(t *testing.T, variant string) (contactRecs, parsertest.Fixture) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	c := contactRecs{t: t, by: map[int64]parsertest.ExpRecord{}}
	for _, r := range fx.Expect.Records {
		c.by[r.Rowid] = r
		if r.Table != "ZWAADDRESSBOOKCONTACT" || r.Type != "contact" {
			t.Errorf("%s: record %s/%d of type %q", variant, r.Table, r.Rowid, r.Type)
		}
	}
	return c, fx
}

// rec is one record view.
type rec struct {
	parsertest.ExpRecord
	raw map[string]any
}

func (c contactRecs) has(n int64) bool { _, ok := c.by[n]; return ok }

func (c contactRecs) rec(n int64) rec {
	c.t.Helper()
	r, ok := c.by[n]
	if !ok {
		c.t.Fatalf("no oracle record for ZWAADDRESSBOOKCONTACT %d", n)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	return rec{ExpRecord: r, raw: raw}
}

func (r rec) display() any { return r.Payload["display_name"] }

func (r rec) given() any {
	n, _ := r.Payload["names"].(map[string]any)
	return n["given"]
}

func (r rec) sourceID() any { return r.Payload["source_id"] }

// list is a payload list of {value, label|kind} objects, as "value|label" or "value|kind" strings.
func (r rec) list(key, second string) []string {
	var out []string
	l, _ := r.Payload[key].([]any)
	for _, e := range l {
		m, _ := e.(map[string]any)
		out = append(out, fmt.Sprintf("%v|%v", m["value"], m[second]))
	}
	return out
}

func (r rec) phones() []string { return r.list("phones", "label") }

func (r rec) ims() []string { return r.list("ims", "kind") }

func (r rec) bodyHas(s string) bool { return r.Body != nil && strings.Contains(*r.Body, s) }

func (r rec) rawAbsent(key string) bool { _, has := r.raw[key]; return !has }

// rawIs compares a raw value with want as printed (JSON numbers decode as json.Number).
func (r rec) rawIs(key string, want any) bool {
	v, has := r.raw[key]
	return has && fmt.Sprint(v) == fmt.Sprint(want)
}

func warningOf(t *testing.T, fx parsertest.Fixture, rowid int64) string {
	t.Helper()
	for _, w := range fx.Expect.Warnings {
		if w.Table == "ZWAADDRESSBOOKCONTACT" && w.Rowid == rowid {
			return w.Contains
		}
	}
	return ""
}

func TestIOSWhatsAppContactsOracleStatesTheSpecRules(t *testing.T) {
	c, fx := recsOf(t, "ios-contactsv2")
	want := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			t.Errorf(format, args...)
		}
	}
	// the mapping is total (4I2-R10): 15 rows are 10 records and 5 counted warnings, none is dropped
	var recIDs []int64
	for id := range c.by {
		recIDs = append(recIDs, id)
	}
	want(len(c.by) == 10 && len(fx.Expect.Warnings) == 5, "%d records %v and %d warnings %+v, want 10 and 5", len(c.by), recIDs, len(fx.Expect.Warnings), fx.Expect.Warnings)
	for _, id := range []int64{1, 2, 3, 7, 8, 9, 10, 11, 14, 15} {
		want(c.has(id), "row %d is no record", id)
	}
	if t.Failed() {
		return
	}

	// row 1: every column; the Apple label wrapper is decoded (the label is Mobile, the stored text is in raw); the JID is an ims
	// entry; about text is in the Body; the LID statement is the row's own ZLID (D6); no time (ZABOUTTIMESTAMP is raw)
	r1 := c.rec(1)
	want(r1.display() == "Ann Lee" && r1.given() == "Ann" && r1.sourceID() == "A1B2C3", "row 1: %v", r1.Payload)
	want(fmt.Sprint(r1.phones()) == "[+15550000001|Mobile]" && r1.raw["phone_label_stored"] == "_$!<Mobile>!$_", "row 1 phones %v raw %v", r1.phones(), r1.raw)
	want(fmt.Sprint(r1.ims()) == "[15550000001@s.whatsapp.net|whatsapp]" && r1.bodyHas("busy"), "row 1 ims %v body %v", r1.ims(), r1.Body)
	lm, _ := r1.raw["lid_map"].([]any)
	want(len(lm) == 1 && fmt.Sprint(lm[0]) == "map[jid:15550000001@s.whatsapp.net lid:100000000000001@lid source:ZWAADDRESSBOOKCONTACT.ZLID]", "row 1 lid_map %v", r1.raw["lid_map"])
	want(r1.Time == nil && r1.TimeEnd == nil && len(r1.Times) == 0 && r1.rawIs("ZABOUTTIMESTAMP", 600000000), "row 1 times %v %v %v raw %v", r1.Time, r1.TimeEnd, r1.Times, r1.raw)
	want(r1.rawAbsent("lid_state") && r1.rawAbsent("whatsapp_id_state") && r1.raw["app"] == "whatsapp" && r1.raw["app_basis"] == "backup_domain", "row 1 raw %v", r1.raw)
	want(r1.Body != nil && *r1.Body == "Ann Lee\n+15550000001\nbusy", "row 1 body %v", r1.Body)

	// row 2: a phone with a label that is not wrapped is kept as it is, with no raw copy; no name, no JID
	r2 := c.rec(2)
	want(fmt.Sprint(r2.phones()) == "[+15550000002|Work line]" && r2.rawAbsent("phone_label_stored") && r2.display() == nil, "row 2: %v", r2.Payload)

	// rows 4, 5, 6, 12, 13: counted warnings with the plan's exact texts, never records
	want(warningOf(t, fx, 4) == "ZWAADDRESSBOOKCONTACT row has no name or number (JID only)", "warning 4: %q", warningOf(t, fx, 4))
	want(warningOf(t, fx, 5) == "ZWAADDRESSBOOKCONTACT row is not a person (group)", "warning 5: %q", warningOf(t, fx, 5))
	want(warningOf(t, fx, 6) == "ZWAADDRESSBOOKCONTACT row has no full name or number", "warning 6: %q", warningOf(t, fx, 6))
	want(warningOf(t, fx, 12) == "ZWAADDRESSBOOKCONTACT row is not a person (unparseable)", "warning 12: %q", warningOf(t, fx, 12))
	want(warningOf(t, fx, 13) == "ZWAADDRESSBOOKCONTACT row has no readable name or number (a name column is damaged or not text)", "warning 13: %q", warningOf(t, fx, 13))
	for _, id := range []int64{4, 5, 6, 12, 13} {
		want(!c.has(id), "row %d is a record", id)
	}

	// row 7: no JID is no ims entry, and the absence is stated
	r7 := c.rec(7)
	want(len(r7.ims()) == 0 && r7.raw["whatsapp_id_state"] == "absent" && r7.display() == "Dee", "row 7: %v", r7.Payload)

	// row 8: a JID that cleaning changed is altered: never parsed, no ims entry, the cleaned text and its flag in raw
	r8 := c.rec(8)
	fl, _ := r8.raw["text_flags"].([]any)
	want(len(r8.ims()) == 0 && r8.raw["whatsapp_id_state"] == "altered" && r8.raw["whatsapp_id_text"] == "15550000008�@s.whatsapp.net" &&
		len(fl) == 1 && fl[0] == "whatsapp_id_text", "row 8: %v", r8.Payload)

	// row 9: a LID JID is an ims entry; its own ZLID names no phone JID, so no lid_map (never inferred)
	r9 := c.rec(9)
	want(fmt.Sprint(r9.ims()) == "[100000000000009@lid|whatsapp]" && r9.raw["lid_map"] == nil && r9.raw["lid_state"] == "jid-not-a-phone", "row 9: %v", r9.Payload)

	// row 10: a 300 KiB about text is cut at the text cap in the Body, its name in text_flags, never a record field of its own
	r10 := c.rec(10)
	fl, _ = r10.raw["text_flags"].([]any)
	want(r10.Body != nil && len(*r10.Body) == len("Gus\n")+256<<10 && len(fl) == 1 && fl[0] == "ZABOUTTEXT", "row 10 flags %v", fl)

	// row 11: the JID of row 1 on another row: two records, never merged or de-duplicated (Review Focus 5); no LID of its own
	r11 := c.rec(11)
	want(r11.display() == "Ann L." && fmt.Sprint(r11.ims()) == fmt.Sprint(r1.ims()) && r11.raw["lid_map"] == nil && r11.rawAbsent("lid_state"), "row 11: %v", r11.Payload)

	// row 14: an over-long JID is too-long (checked first): no ims entry, no raw text; the highlighted name is raw as stored
	r14 := c.rec(14)
	want(len(r14.ims()) == 0 && r14.raw["whatsapp_id_state"] == "too-long" && r14.rawAbsent("whatsapp_id_text") && r14.rawAbsent("text_flags") &&
		r14.raw["ZHIGHLIGHTEDNAME"] == "Ivy H", "row 14: %v", r14.Payload)

	// row 15: a JID cell that is not TEXT is not-text, and the unreadable cell is stated, never taken for absent
	r15 := c.rec(15)
	unk, _ := r15.raw["unknown_columns"].(map[string]any)
	want(len(r15.ims()) == 0 && r15.raw["whatsapp_id_state"] == "not-text" && fmt.Sprint(unk) == "map[ZWAADDRESSBOOKCONTACT.ZWHATSAPPID:type-mismatch]", "row 15: %v", r15.Payload)

	// only the two rows with a ZLID say anything about a LID (D6: never inferred from the JID)
	for id := range c.by {
		if id != 1 && id != 9 {
			want(c.rec(id).rawAbsent("lid_map") && c.rec(id).rawAbsent("lid_state"), "row %d states a LID: %v", id, c.rec(id).raw)
		}
	}

	// notes and warnings: the reader holds no uncommitted frame, so the warnings are the five rows, in rowid order
	want(fx.Expect.Notes["container"] == "backup_domain" && fx.Expect.Notes["app"] == "whatsapp" && fx.Expect.Notes["app_basis"] == "backup_domain" && len(fx.Expect.Notes) == 3, "notes %v", fx.Expect.Notes)
	var order []int64
	for _, w := range fx.Expect.Warnings {
		order = append(order, w.Rowid)
	}
	want(fmt.Sprint(order) == "[4 5 6 12 13]", "warning order %v", order)
	want(len(fx.Files) == 2, "files %d, want the database and its -wal", len(fx.Files))
}

// TestIOSWhatsAppContactsOracleStatesTheVariants: no ZLID column states no LID anywhere (D6: S7 saw the column absent on some
// images); an extraction names no app (4I2-R5); another app's database is refused.
func TestIOSWhatsAppContactsOracleStatesTheVariants(t *testing.T) {
	c, fx := recsOf(t, "ios-contactsv2-nolid")
	if len(c.by) != 3 || len(fx.Expect.Warnings) != 0 {
		t.Errorf("nolid: %d records, warnings %v, want rows 1-3", len(c.by), fx.Expect.Warnings)
	}
	for id := range c.by {
		if r := c.rec(id); !r.rawAbsent("lid_map") || !r.rawAbsent("lid_state") {
			t.Errorf("nolid row %d states a LID: %v", id, r.raw)
		}
	}
	if r := c.rec(1); r.display() != "Ann Lee" || fmt.Sprint(r.ims()) != "[15550000001@s.whatsapp.net|whatsapp]" {
		t.Errorf("nolid row 1: %v", r.Payload)
	}

	c, fx = recsOf(t, "ios-contactsv2-extract")
	r1 := c.rec(1)
	if len(c.by) != 3 || r1.raw["app"] != "unknown" || r1.raw["app_state"] != "no-container-metadata" || !r1.rawAbsent("app_basis") || fx.Expect.Notes["container"] != "app_group" {
		t.Errorf("extract: %d records, raw %v, notes %v", len(c.by), r1.raw, fx.Expect.Notes)
	}
	if lm, _ := r1.raw["lid_map"].([]any); len(lm) != 1 {
		t.Errorf("extract row 1 lid_map %v: the LID statement does not depend on the place", r1.raw["lid_map"])
	}
	if len(fx.Expect.ExtraFiles) != 0 || len(fx.Expect.Warnings) != 0 {
		t.Errorf("an extraction variant has extras %v and warnings %v", fx.Expect.ExtraFiles, fx.Expect.Warnings)
	}

	c, fx = recsOf(t, "ios-contactsv2-otherapp")
	if len(c.by) != 0 || len(fx.Expect.Notes) != 0 {
		t.Errorf("otherapp states %d records and notes %v; the parser refuses it", len(c.by), fx.Expect.Notes)
	}
}

// TestIOSWhatsAppContactsDeletedOracleStatesTheHistory (plan 4I2 PF2): deleted contacts are found through the WAL (the file holds
// 90 and 91, a first WAL frame 92 and 93, one committed WAL transaction deletes them with the even ones of 100 to 159). Row 93 is a
// group row: it is reported unmapped, never typed (4I2-R10 holds for recovered rows too).
func TestIOSWhatsAppContactsDeletedOracleStatesTheHistory(t *testing.T) {
	c, fx := recsOf(t, "ios-contactsv2-deleted")
	if len(c.by) != 32 || len(fx.Expect.Warnings) != 0 {
		t.Errorf("%d live records, warnings %v, want 32 (contacts 1, 2 and the 30 odd ones of 100-159) and none", len(c.by), fx.Expect.Warnings)
	}
	raw, err := os.ReadFile("testdata/ios-contactsv2-deleted.recover.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Claimed  []string `json:"claimed"`
		Unmapped []struct {
			Table, Contains string
			Rowid           int64
		} `json:"unmapped"`
		Records []struct {
			Table, Method, Relation, File string
			Rowid                         int64
			Record                        struct {
				Deleted bool
				Body    *string
				Payload map[string]any
			}
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp.Claimed) != 1 || exp.Claimed[0] != "ZWAADDRESSBOOKCONTACT" {
		t.Errorf("claimed %v", exp.Claimed)
	}
	rows := map[int64]bool{}
	files := map[string]bool{}
	for _, r := range exp.Records {
		rows[r.Rowid] = true
		files[r.File] = true
		if r.Table != "ZWAADDRESSBOOKCONTACT" || r.Method != "sqlite-wal-prior" || r.Relation != "absent-from-live" || r.Record.Deleted {
			t.Errorf("row %d: %+v", r.Rowid, r)
		}
		rawp, _ := r.Record.Payload["raw"].(map[string]any)
		if rawp["app"] != "whatsapp" || rawp["app_basis"] != "backup_domain" {
			t.Errorf("recovered contact %d at a domain place states its domain label: %v", r.Rowid, rawp)
		}
		if r.Rowid == 90 {
			lm, _ := rawp["lid_map"].([]any)
			if len(lm) != 1 || r.Record.Payload["display_name"] != "Gil Gone" || rawp["phone_label_stored"] != "_$!<Home>!$_" {
				t.Errorf("recovered contact 90: %v", r.Record.Payload)
			}
		}
	}
	for n := int64(1); n <= 200; n++ {
		want := n == 90 || n == 91 || n == 92 || (n >= 100 && n <= 159 && n%2 == 0)
		if rows[n] != want {
			t.Errorf("row %d recovered = %v, want %v", n, rows[n], want)
		}
	}
	if len(exp.Unmapped) != 1 || exp.Unmapped[0].Rowid != 93 || !strings.Contains(exp.Unmapped[0].Contains, "ZWAADDRESSBOOKCONTACT row") {
		t.Errorf("unmapped %+v, want the group row 93", exp.Unmapped)
	}
	if !files["db"] || !files["wal"] {
		t.Errorf("history in %v, want both the main file and the WAL", files)
	}
}
