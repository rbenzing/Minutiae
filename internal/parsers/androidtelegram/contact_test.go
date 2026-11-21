package androidtelegram_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// contactsOf are the contact records of a users row of a result ("" when none).
func contactsOf(res parsertest.Result) map[string]records.Record {
	out := map[string]records.Record{}
	for _, r := range res.Records {
		if r.Type == contact.Type {
			out[locatorRow(r.Locator)] = r
		}
	}
	return out
}

// Spec 8.9, T4: one contact per users row (U1-U10 of telegram-v181). The self user is flagged and counted (note
// self_users); U2's address is the participant address of R1; a min stub and a deleted account are states, never
// Record.Deleted; a 32-bit user keeps its id; the status is raw only, with its label or its expiry; every layer of
// telegram-layers is a record whose blob is decoded or decoded-prefix (F18), and an unknown constructor states it.
func TestTelegramUsersAreContacts(t *testing.T) {
	res := parseFixture(t, "telegram-v181")
	got := contactsOf(res)
	if len(got) != 10 {
		t.Fatalf("%d contacts, want 10", len(got))
	}
	u := func(n string) records.Record { return got["users/"+n] }
	if r := rawOf(u("1000001")); r["self"] != true || res.Notes["self_users"] != "1" {
		t.Errorf("U1: raw.self %v, note self_users %q", r["self"], res.Notes["self_users"])
	}
	r1 := byRow(res.Records)["messages_v2/1"]
	ims, _ := u("1000002").Payload["ims"].([]any)
	first, _ := ims[0].(map[string]any)
	if first["value"] != party(r1, "from") || first["kind"] != "telegram" {
		t.Errorf("U2: ims[0] %v, want the participant address %q of R1 (T4)", first, party(r1, "from"))
	}
	if rawOf(u("1000004"))["min"] != true {
		t.Errorf("U4: raw.min %v", rawOf(u("1000004"))["min"])
	}
	if r := u("1000005"); rawOf(r)["account_deleted"] != true || r.Deleted {
		t.Errorf("U5: raw.account_deleted %v, Deleted %v", rawOf(r)["account_deleted"], r.Deleted)
	}
	if u("1000008").Payload["source_id"] != "1000008" {
		t.Errorf("U8: source_id %v", u("1000008").Payload["source_id"])
	}
	if rawOf(u("1000003"))["status_label"] != "recently" || rawOf(u("1000010"))["status_label"] != "last_week_by_me" {
		t.Errorf("U3/U10 status labels %v %v", rawOf(u("1000003"))["status_label"], rawOf(u("1000010"))["status_label"])
	}
	if r := rawOf(u("1000002")); r["status_expires"] != int64(1700003600) || r["status_label"] != nil {
		t.Errorf("U2: status_expires %v status_label %v", r["status_expires"], r["status_label"])
	}
	if r := rawOf(u("1000009")); r["data_state"] != "unknown-constructor" {
		t.Errorf("U9: data_state %v", r["data_state"])
	}
	for n, r := range got {
		if r.Time != nil || r.Deleted {
			t.Errorf("%s: Time %v Deleted %v: a user has no time and is live", n, r.Time, r.Deleted)
		}
	}

	layers := contactsOf(parseFixture(t, "telegram-layers"))
	if len(layers) != 29 {
		t.Errorf("telegram-layers: %d contacts, want one per users row (29)", len(layers))
	}
	for n, r := range layers {
		if s := rawOf(r)["data_state"]; s != "decoded" && s != "decoded-prefix" {
			t.Errorf("layers %s: data_state %v (F18)", n, s)
		}
	}
}

// Spec 8.9 (4I R2 shape, B9): a row with no name, phone or username (U5, U6, U9) is still a record and one counted
// warning at its locator; 1,500 such rows give 1,000 warnings and one summary of the rest.
func TestTelegramUsersWithoutNameAreCountedWarnings(t *testing.T) {
	res := parseFixture(t, "telegram-v181")
	var warned []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "users row has no name, phone or username") {
			warned = append(warned, w.Locator)
		}
	}
	want := []string{"1000005", "1000006", "1000009"}
	if len(warned) != 3 {
		t.Fatalf("warnings %v, want one for each of %v", warned, want)
	}
	got := contactsOf(res)
	for i, id := range want {
		if !strings.Contains(warned[i], "table=users;rowid="+id) {
			t.Errorf("warning %d locator %q, want users rowid %s", i, warned[i], id)
		}
		if _, ok := got["users/"+id]; !ok {
			t.Errorf("users %s: no record kept", id)
		}
	}

	var rows []builtRow
	for i := range 1500 {
		rows = append(rows, builtRow{int64(i + 1), map[string]any{"name": "x", "status": int64(0)}}) // a NULL blob: no name
	}
	big := parseAt(t, remoteApp, dbOnly(goBuiltWith(181, nil, builtTable{
		name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: rows,
	})))
	one, summary := 0, ""
	for _, w := range big.Warnings {
		switch {
		case strings.HasPrefix(w.Reason, "users row has no name, phone or username:"):
			summary = w.Reason
		case strings.Contains(w.Reason, "users row has no name, phone or username"):
			one++
		}
	}
	if one != 1000 || summary != "users row has no name, phone or username: 1500 records, 500 not warned one by one" {
		t.Errorf("%d warnings and summary %q, want 1000 and the 1500/500 summary", one, summary)
	}
	if n := len(contactsOf(big)); n != 1500 {
		t.Errorf("%d contacts, want all 1500 kept", n)
	}
}

// Review Focus 3: U7 has a present but empty first name: no names.given, display_name "Solo", no username entry;
// the payload passes the contract validator, and no string anywhere in a contact payload is empty.
func TestTelegramEmptyNamePartsAreAbsent(t *testing.T) {
	res := parseFixture(t, "telegram-v181")
	u7 := contactsOf(res)["users/1000007"]
	names, _ := u7.Payload["names"].(map[string]any)
	if _, has := names["given"]; has || names["family"] != "Solo" || u7.Payload["display_name"] != "Solo" {
		t.Errorf("U7: names %v display_name %v", names, u7.Payload["display_name"])
	}
	ims, _ := u7.Payload["ims"].([]any)
	for _, e := range ims {
		if m, _ := e.(map[string]any); m["kind"] == "telegram_username" {
			t.Errorf("U7: an empty username is an ims entry: %v", e)
		}
	}
	if err := contact.Validate(u7.Payload); err != nil {
		t.Errorf("U7 payload: %v", err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if x == "" {
				t.Errorf("empty string at %s", path)
			}
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		}
	}
	for n, r := range contactsOf(parseFixture(t, "telegram-layers")) {
		walk(n, r.Payload)
	}
	for n, r := range contactsOf(res) {
		walk(n, r.Payload)
	}
}

// Review I1 (Task 12 fix 1): a recovered users row whose rowid is lost has no ims, so with no name or phone either it
// identifies nobody: it is unmapped (a counted warning), never a record the contact contract would refuse.
func TestTelegramRecoveredNamelessUserWithoutRowidIsUnmapped(t *testing.T) {
	users := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: []builtRow{
		{7, map[string]any{"name": "x", "status": int64(0)}}, // a NULL blob: no name, no phone
		{8, map[string]any{"name": "x", "status": int64(0), "data": tlUser(8, 0, false, ptr("Ann"), nil, nil, nil)}},
	}}
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	in := inputAt(t, h, p, remoteApp, dbOnly(goBuiltWith(181, nil, users)), false)
	sink := &recordSink{}
	rep, err := rowrun.Run(t.Context(), in, sink, []rowrun.Step{{Mapping: androidtelegram.LostRowidUsersMapping()}})
	if err != nil {
		t.Fatal(err)
	}
	var unmapped int64
	for _, tr := range rep.Tables {
		unmapped += tr.Unmapped
	}
	if len(sink.recs) != 1 || unmapped != 1 {
		t.Fatalf("%d records and %d unmapped rows, want the named row kept and the nameless one unmapped", len(sink.recs), unmapped)
	}
	if err := contact.Validate(sink.recs[0].Payload); err != nil {
		t.Errorf("the kept record: %v", err)
	}
}

func ptr(s string) *string { return &s }
