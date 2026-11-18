package androidwacontacts_test

import (
	"encoding/json"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator is the scenario oracle; these literal facts are written from the spec (6.6 and R2), not read back
// from the generator, so a generator that drifted from the rules fails here before any parser exists.
func TestWAContactsOracleStatesTheSpecRules(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "wa-current")
	byRow := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		byRow[r.Rowid] = r
		if r.Type != "contact" || r.Table != "wa_contacts" || r.Deleted || r.Time != nil || r.TimeEnd != nil || len(r.Times) != 0 {
			t.Errorf("row %d: type %q deleted %v time %v end %v times %v; a contact has no time of its own (spec 6.6)", r.Rowid, r.Type, r.Deleted, r.Time, r.TimeEnd, r.Times)
		}
	}
	for _, id := range []int64{1, 2, 3, 5, 13, 14} {
		if _, ok := byRow[id]; !ok {
			t.Errorf("row %d is not a record", id)
		}
	}
	if len(byRow) != 6 {
		t.Errorf("%d records, want 6 (rows 1, 2, 3, 5, 13, 14)", len(byRow))
	}
	raw := func(id int64) map[string]any {
		m, _ := byRow[id].Payload["raw"].(map[string]any)
		return m
	}
	if byRow[1].Payload["display_name"] != "Ann" || raw(1)["display_name_source"] != "address_book" {
		t.Errorf("row 1: display_name %v source %v", byRow[1].Payload["display_name"], raw(1)["display_name_source"])
	}
	if byRow[2].Payload["display_name"] != "Bob the Builder" || raw(2)["display_name_source"] != "wa_name_push_name" {
		t.Errorf("row 2: display_name %v source %v", byRow[2].Payload["display_name"], raw(2)["display_name_source"])
	}
	if _, has := byRow[3].Payload["display_name"]; has {
		t.Errorf("row 3 has a display name %v", byRow[3].Payload["display_name"])
	}
	phones, _ := byRow[3].Payload["phones"].([]any)
	if len(phones) != 1 {
		t.Fatalf("row 3 phones %v", byRow[3].Payload["phones"])
	}
	if p, _ := phones[0].(map[string]any); len(p) != 1 || p["value"] != "+15550000003" {
		t.Errorf("row 3 phone %v, want {value: +15550000003} and nothing else", phones[0])
	}
	ims, _ := byRow[1].Payload["ims"].([]any)
	if len(ims) != 1 {
		t.Fatalf("row 1 ims %v", byRow[1].Payload["ims"])
	}
	if im, _ := ims[0].(map[string]any); len(im) != 2 || im["value"] != "15550000001@s.whatsapp.net" || im["kind"] != "whatsapp" {
		t.Errorf("row 1 im %v", ims[0])
	}
	if raw(1)["label_is_whatsapp_user"] != "whatsapp_user" {
		t.Errorf("row 1 label %v", raw(1)["label_is_whatsapp_user"])
	}
	if v, has := raw(13)["is_whatsapp_user"].(json.Number); !has || v.String() != "0" {
		t.Errorf("row 13 is_whatsapp_user %v, want the stored 0", raw(13)["is_whatsapp_user"])
	}
	if _, has := raw(13)["label_is_whatsapp_user"]; has {
		t.Errorf("row 13 has a label for is_whatsapp_user 0")
	}
	if raw(1)["status"] != "busy" || raw(1)["thumb_ts"] == nil || raw(1)["photo_id_timestamp"] == nil || raw(1)["status_timestamp"] == nil {
		t.Errorf("row 1 raw %v: status and the avatar and status times stay raw", raw(1))
	}
	if _, has := byRow[5].Payload["ims"]; !has || byRow[5].Payload["display_name"] != "Lidia" {
		t.Errorf("row 5 (a LID user) %v", byRow[5].Payload)
	}
	// the 300 KiB status is cut and flagged (C6)
	if s, _ := raw(14)["status"].(string); len(s) != 256<<10 {
		t.Errorf("row 14 status holds %d bytes, want the cut of 256 KiB", len(s))
	}
	if fl, _ := raw(14)["text_flags"].([]any); len(fl) != 1 || fl[0] != "status" {
		t.Errorf("row 14 text_flags %v, want [status]", raw(14)["text_flags"])
	}
	if raw(1)["app"] != "whatsapp" || raw(1)["app_basis"] != "path" {
		t.Errorf("row 1 app %v basis %v", raw(1)["app"], raw(1)["app_basis"])
	}

	// R2: JID-only and nameless rows, and the rows that are not persons, are counted warnings
	want := map[int64]string{
		4:  "wa_contacts row has no name or number (JID only)",
		12: "wa_contacts row has no display name, push name or number",
		6:  "wa_contacts row is not a person (group)",
		7:  "wa_contacts row is not a person (broadcast)",
		8:  "wa_contacts row is not a person (status)",
		9:  "wa_contacts row is not a person (newsletter)",
		10: "wa_contacts row is not a person (other)",
		11: "wa_contacts row is not a person (unparseable)",
	}
	got := map[int64]string{}
	for _, w := range fx.Expect.Warnings {
		if w.Table != "wa_contacts" {
			t.Errorf("warning %+v is not a row warning", w)
		}
		got[w.Rowid] = w.Contains
	}
	if len(got) != len(want) {
		t.Errorf("%d warnings %v, want %d", len(got), got, len(want))
	}
	for id, text := range want {
		if got[id] != text {
			t.Errorf("row %d warning %q, want %q", id, got[id], text)
		}
		if _, rec := byRow[id]; rec {
			t.Errorf("row %d is a record and a warning", id)
		}
	}
	// the live view: the WAL deleted row 15 and edited row 13
	if _, ok := byRow[15]; ok {
		t.Error("row 15 (deleted in the WAL) is a record")
	}
	if raw(13)["status"] != "away" {
		t.Errorf("row 13 status %v, want the WAL's \"away\"", raw(13)["status"])
	}
	for _, r := range fx.Expect.Records {
		for _, k := range []string{"emails", "urls", "addresses", "organization", "birthday"} {
			if _, has := r.Payload[k]; has {
				t.Errorf("row %d carries %s: nothing in wa_contacts states it", r.Rowid, k)
			}
		}
	}
	// the variants that are not parsed state no record
	for _, v := range []string{"wa-no-table", "wa-crypt15", "wa-backup-plain", "wa-other-location"} {
		if f := parsertest.LoadFixture(t, "testdata", v); len(f.Expect.Records) != 0 {
			t.Errorf("%s states %d records", v, len(f.Expect.Records))
		}
	}
	for _, r := range parsertest.LoadFixture(t, "testdata", "wa-business").Expect.Records {
		if m, _ := r.Payload["raw"].(map[string]any); m["app"] != "whatsapp_business" {
			t.Errorf("wa-business row %d app %v", r.Rowid, m["app"])
		}
	}
}

// The oracle states the path of the file that holds the bytes (T48, T51): every row of a lone-database variant names the
// variant's own remote path, so a parser that fixed one primary path would fail. A variant with a WAL states none: the
// path follows the role of the file that holds the cell.
func TestWAContactsOracleStatesTheSourcePathOfEveryRow(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		for _, r := range fx.Expect.Records {
			want := remoteFor(v)
			if len(fx.Files) > 1 {
				want = ""
			}
			if r.SourcePath != want {
				t.Errorf("%s row %d: source_path %q, want %q", v, r.Rowid, r.SourcePath, want)
			}
		}
	}
	if remoteFor("wa-business") != "/data/data/com.whatsapp.w4b/databases/wa.db" || remoteFor("wa-crypt15") != "/sdcard/WhatsApp/Databases/wa.db.crypt15" {
		t.Error("remoteFor does not name the documented paths")
	}
}
