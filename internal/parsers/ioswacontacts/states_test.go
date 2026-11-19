package ioswacontacts_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The in-test databases of the state vocabulary (4I2-R7) and of the other unit tests of this package: built with the test
// builder, so every state is pinned on a row the test itself wrote, never on the parser's own output. The DDL is the column list
// of tools/fixtures/schemas/whatsapp-ios/contactsv2.sql.

var contactCols = []string{"Z_PK", "Z_ENT", "Z_OPT", "ZABOUTTIMESTAMP", "ZABOUTTEXT", "ZFULLNAME", "ZGIVENNAME", "ZHIGHLIGHTEDNAME", "ZIDENTIFIER", "ZLID", "ZPHONENUMBER", "ZPHONENUMBERLABEL", "ZWHATSAPPID"}

type putRow func(pk int64, kv map[string]any)

// contactDDL is the DDL of the table without the dropped columns.
func contactDDL(drop []string) (string, []string) {
	var cols []string
	var decl []string
	for _, c := range contactCols {
		if c != "Z_PK" && sliceHas(drop, c) {
			continue
		}
		cols = append(cols, c)
		switch c {
		case "Z_PK":
			decl = append(decl, "Z_PK INTEGER PRIMARY KEY")
		case "Z_ENT", "Z_OPT":
			decl = append(decl, c+" INTEGER")
		case "ZABOUTTIMESTAMP":
			decl = append(decl, c+" TIMESTAMP")
		default:
			decl = append(decl, c+" VARCHAR")
		}
	}
	return "CREATE TABLE ZWAADDRESSBOOKCONTACT (" + strings.Join(decl, ", ") + ")", cols
}

func sliceHas(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// buildContacts builds a ContactsV2-like store without the columns in drop; fill puts the rows. The second result is a WAL of the
// pages (a companion that defers the schema checks to Parse).
func buildContacts(drop []string, fill func(put putRow)) (db, wal []byte) {
	b, _ := contactBuilder(4096, drop, fill)
	return b.Bytes(), b.NewWAL(false, 1, 2, 0).Bytes()
}

// contactBuilder is buildContacts with the builder handed back, for a test that needs a page size, a snapshot or a journal.
func contactBuilder(pageSize int, drop []string, fill func(put putRow)) (*sqlitetest.Builder, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: pageSize})
	ddl, cols := contactDDL(drop)
	tb := b.CreateTable("ZWAADDRESSBOOKCONTACT", ddl)
	fill(func(pk int64, kv map[string]any) {
		vals := make([]any, 0, len(cols))
		for _, n := range cols {
			vals = append(vals, kv[n]) // the key slot is nil: the rowid alias is stored as NULL in the record
		}
		tb.Insert(pk, vals...)
	})
	return b, tb
}

// contactRow is a ZWAADDRESSBOOKCONTACT row: Z_ENT 1, a name and no JID unless kv says otherwise.
func contactRow(kv map[string]any) map[string]any {
	r := map[string]any{"Z_ENT": int64(1), "Z_OPT": int64(1)}
	for k, v := range kv {
		r[k] = v
	}
	return r
}

// runContacts parses a Go-built store at the consumer backup domain (with a -wal when wal is not nil) and returns the records by
// Z_PK. Warnings other than the allowed kinds fail the test.
func runContacts(t *testing.T, db, wal []byte, allowed ...string) (map[int64]records.Record, parsertest.Result) {
	t.Helper()
	res := runContactsErr(t, db, wal, allowed...)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	return byRowid(res), res
}

func runContactsErr(t *testing.T, db, wal []byte, allowed ...string) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswacontacts.Parser{}
	if len(allowed) == 0 {
		allowed = []string{"ZWAADDRESSBOOKCONTACT row "}
	}
	return runLenient(t, h, p, b.Input(h, p, false), allowed...)
}

func byRowid(res parsertest.Result) map[int64]records.Record {
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		out[rowidOf(r.Locator)] = r
	}
	return out
}

func rowidOf(loc string) int64 {
	_, rest, _ := strings.Cut(loc, ";rowid=")
	num, _, _ := strings.Cut(rest, ";")
	n, _ := strconv.ParseInt(num, 10, 64)
	return n
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// 4I2-R7: every raw state key and value of the contacts parser, pinned on a row of a database written here with hard-coded
// expectations. A value the vocabulary does not name fails (the plan's names, not the implementation's, are the oracle).
func TestIOSWhatsAppContactsStateVocabularyIsPinned(t *testing.T) {
	const user = "15550000001@s.whatsapp.net"
	const lid = "100000000000001@lid"
	big := strings.Repeat("7", 2000) + "@s.whatsapp.net"
	db, _ := buildContacts(nil, func(put putRow) {
		// whatsapp_id_state
		put(1, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": nil}))
		put(2, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": ""}))
		put(3, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": "1\x00@s.whatsapp.net"}))
		put(4, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": big}))
		put(5, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": int64(5)}))
		put(6, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": []byte(user)}))
		put(7, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user}))
		// lid_map and lid_state
		put(10, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": lid}))
		put(11, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": "1\x00@lid"}))
		put(12, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": int64(12)}))
		put(13, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": lid, "ZLID": lid}))
		put(14, contactRow(map[string]any{"ZFULLNAME": "a", "ZLID": lid}))
		put(15, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": ""}))
		put(16, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": nil}))
		put(17, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": user, "ZLID": strings.Repeat("1", 2000) + "@lid"}))
		put(18, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": "1\x00@s.whatsapp.net", "ZLID": lid}))
		put(19, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": "120363@g.us", "ZPHONENUMBER": "+1", "ZLID": lid}))
		put(21, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": big, "ZLID": lid}))
		put(22, contactRow(map[string]any{"ZFULLNAME": "a", "ZWHATSAPPID": int64(22), "ZLID": lid}))
		// phone_label_stored
		put(20, contactRow(map[string]any{"ZPHONENUMBER": "+1", "ZPHONENUMBERLABEL": "_$!<Home>!$_"}))
	})
	recs, _ := runContacts(t, db, nil, "ZWAADDRESSBOOKCONTACT row ")
	state := func(id int64, key string) any { return rawOf(recs[id])[key] }
	idStates := map[int64]any{1: "absent", 2: "absent", 3: "altered", 4: "too-long", 5: "not-text", 6: "not-text", 7: nil}
	for id, want := range idStates {
		if got := state(id, "whatsapp_id_state"); got != want {
			t.Errorf("row %d: whatsapp_id_state %v, want %v", id, got, want)
		}
	}
	if state(3, "whatsapp_id_text") != "1�@s.whatsapp.net" {
		t.Errorf("row 3: whatsapp_id_text %v", state(3, "whatsapp_id_text"))
	}
	for _, id := range []int64{1, 2, 3, 4, 5, 6} {
		if recs[id].Payload["ims"] != nil {
			t.Errorf("row %d: ims %v without a usable JID", id, recs[id].Payload["ims"])
		}
	}
	if recs[7].Payload["ims"] == nil {
		t.Error("row 7: a usable JID has no ims entry")
	}
	wantMap := []any{map[string]any{"lid": lid, "jid": user, "source": "ZWAADDRESSBOOKCONTACT.ZLID"}}
	if !reflect.DeepEqual(state(10, "lid_map"), wantMap) || state(10, "lid_state") != nil {
		t.Errorf("row 10: lid_map %v lid_state %v", state(10, "lid_map"), state(10, "lid_state"))
	}
	// amendment 4I2-A5: an undecided JID (altered, too-long, not-text) is "jid-undecided"; "jid-not-a-phone" is a decided JID
	// of another class or an absent one. Whenever no lid_map is written, raw.ZLID keeps the stored LID (cleaned, text_flags on
	// alteration) unless it is too long.
	lidStates := map[int64]any{
		11: "altered", 12: "not-text", 13: "jid-not-a-phone", 14: "jid-not-a-phone", 15: nil, 16: nil, 17: "too-long",
		18: "jid-undecided", 21: "jid-undecided", 22: "jid-undecided",
	}
	lidStored := map[int64]any{11: "1�@lid", 12: int64(12), 13: lid, 14: lid, 15: nil, 16: nil, 17: nil, 18: lid, 21: lid, 22: lid}
	for id, want := range lidStates {
		if got := state(id, "lid_state"); got != want {
			t.Errorf("row %d: lid_state %v, want %v", id, got, want)
		}
		if state(id, "lid_map") != nil {
			t.Errorf("row %d: a lid_map %v without a decided user JID and a clean ZLID", id, state(id, "lid_map"))
		}
		if got := state(id, "ZLID"); got != lidStored[id] {
			t.Errorf("row %d: raw.ZLID %#v, want %#v", id, got, lidStored[id])
		}
	}
	if state(10, "ZLID") != nil {
		t.Errorf("row 10: raw.ZLID %v beside lid_map", state(10, "ZLID"))
	}
	if !reflect.DeepEqual(state(11, "text_flags"), []any{"ZLID"}) {
		t.Errorf("row 11: text_flags %v, want [ZLID]", state(11, "text_flags"))
	}
	// the LID's own record: its ims is the LID, never merged with the phone JID of another row
	if got := recs[13].Payload["ims"]; !reflect.DeepEqual(got, []any{map[string]any{"value": lid, "kind": "whatsapp"}}) {
		t.Errorf("row 13: ims %#v, want exactly the LID", got)
	}
	// final review M4: a ZWHATSAPPID that is not text keeps its stored value in raw (a blob by its marker)
	if state(5, "ZWHATSAPPID") != int64(5) || !reflect.DeepEqual(state(6, "ZWHATSAPPID"), map[string]any{"blob_b64": "MTU1NTAwMDAwMDFAcy53aGF0c2FwcC5uZXQ="}) {
		t.Errorf("rows 5 and 6: raw.ZWHATSAPPID %#v and %#v", state(5, "ZWHATSAPPID"), state(6, "ZWHATSAPPID"))
	}
	for _, id := range []int64{15, 16, 7} {
		if state(id, "lid_map") != nil || state(id, "lid_state") != nil {
			t.Errorf("row %d: states a LID without a ZLID text: %v", id, rawOf(recs[id]))
		}
	}
	if _, has := recs[19]; has {
		t.Error("row 19 (a group JID) is a record")
	}
	if state(20, "phone_label_stored") != "_$!<Home>!$_" {
		t.Errorf("row 20: phone_label_stored %v", state(20, "phone_label_stored"))
	}
	for id, r := range recs {
		for _, k := range []string{"app", "app_basis"} {
			if rawOf(r)[k] == nil {
				t.Errorf("row %d: raw.%s missing", id, k)
			}
		}
		if rawOf(r)["app"] != "whatsapp" || rawOf(r)["app_basis"] != "backup_domain" || rawOf(r)["app_state"] != nil {
			t.Errorf("row %d: raw %v", id, rawOf(r))
		}
	}
}
