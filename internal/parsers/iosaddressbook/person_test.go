package iosaddressbook_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fullBook holds one person who exercises every row of the mapping contract.
func fullBook(t *testing.T) *book {
	k := newBook(t, "addressbook-current.sql")
	k.labels("_$!<Mobile>!$_", "Lab line", "_$!<Work>!$_", "_$!<HomePage>!$_", "_$!<Home>!$_", "Skype")
	k.keys("Street", "City", "State", "ZIP", "Country", "CountryCode", "Odd")
	k.row("ABStore", 1, map[string]any{"Name": "iCloud"})
	k.row("ABGroup", 1, map[string]any{"Name": "Zeta"})
	k.row("ABGroup", 2, map[string]any{"Name": "Alpha"})
	k.person(1, map[string]any{
		"Prefix": "Dr.", "First": "Ann", "Middle": "M.", "Last": "Lee", "Suffix": "PhD", "Nickname": "Annie",
		"FirstPhonetic": "An", "MiddlePhonetic": "Em", "LastPhonetic": "Lee",
		"Organization": "Acme", "Department": "R&D", "JobTitle": "CEO",
		"Note": "a note", "Kind": int64(1), "Birthday": "1990-05-06",
		"CreationDate": int64(700000000), "ModificationDate": 700000100.5, "StoreID": int64(1),
		"guid": "G-1", "ExternalIdentifier": "E-1", "ExternalUUID": "U-1",
	})
	k.mv(1, 3, int64(1), "555-0001")
	k.mv(1, 3, int64(2), "555-0002")
	k.mv(1, 3, int64(99), "555-0003")
	k.mv(1, 4, int64(3), "ann@example.test")
	k.mv(1, 22, int64(4), "https://example.test")
	k.mv(1, 13, int64(6), "ann_im")
	a := k.mv(1, 5, int64(5), "1 Main St")
	k.entry(a, 1, "1 Main St")
	k.entry(a, 2, "Town")
	k.entry(a, 3, "ST")
	k.entry(a, 4, "12345")
	k.entry(a, 5, "Land")
	k.entry(a, 6, "ld")
	k.entry(a, 7, "odd-value")
	// group 1 holds person 1 and 2 and a non-person member of person 1's id; group 2 holds person 1
	for i, m := range [][3]int64{{1, 0, 1}, {2, 0, 1}, {1, 1, 1}, {1, 2, 1}, {1, 0, 2}} {
		k.row("ABGroupMembers", int64(i+1), map[string]any{"group_id": m[0], "member_type": m[1], "member_id": m[2]})
	}
	return k
}

func TestContactMappingFromTheSpec(t *testing.T) {
	res := runBook(t, fullBook(t).bytes())
	if res.Err != nil || len(res.Warnings) != 0 || len(res.Records) != 1 {
		t.Fatalf("err %v, %d warnings, %d records", res.Err, len(res.Warnings), len(res.Records))
	}
	r := res.Records[0]
	if r.Type != "contact" || r.Locator != "sqlite:table=ABPerson;rowid=1" {
		t.Errorf("type %q locator %q", r.Type, r.Locator)
	}
	p, raw := r.Payload, rawIn(r)
	want := func(what string, got, exp any) {
		t.Helper()
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s = %#v, want %#v", what, got, exp)
		}
	}
	want("names", p["names"], map[string]any{"given": "Ann", "family": "Lee", "middle": "M.", "prefix": "Dr.", "suffix": "PhD", "nickname": "Annie", "phonetic": "An Em Lee"})
	want("display_name", p["display_name"], "Dr. Ann M. Lee PhD")
	want("display_name_source", raw["display_name_source"], "composed")
	want("organization", p["organization"], map[string]any{"name": "Acme", "department": "R&D", "title": "CEO"})
	want("phones", p["phones"], []any{
		map[string]any{"value": "555-0001", "label": "Mobile"},
		map[string]any{"value": "555-0002", "label": "Lab line"},
		map[string]any{"value": "555-0003"},
	})
	want("emails", p["emails"], []any{map[string]any{"value": "ann@example.test", "label": "Work"}})
	want("urls", p["urls"], []any{map[string]any{"value": "https://example.test", "label": "HomePage"}})
	want("label_raw", raw["label_raw"], []any{
		map[string]any{"list": "phones", "index": int64(0), "label": "_$!<Mobile>!$_"},
		map[string]any{"list": "emails", "index": int64(0), "label": "_$!<Work>!$_"},
		map[string]any{"list": "urls", "index": int64(0), "label": "_$!<HomePage>!$_"},
		map[string]any{"list": "addresses", "index": int64(0), "label": "_$!<Home>!$_"},
	})
	want("labels_unresolved", raw["labels_unresolved"], int64(1))
	want("addresses", p["addresses"], []any{map[string]any{"formatted": "1 Main St", "street": "1 Main St", "city": "Town", "region": "ST", "postcode": "12345", "country": "Land", "label": "Home"}})
	want("address_keys_unmapped", raw["address_keys_unmapped"], []any{
		map[string]any{"index": int64(0), "key": "CountryCode", "value": "ld"},
		map[string]any{"index": int64(0), "key": "Odd", "value": "odd-value"},
	})
	want("other_values", raw["other_values"], []any{map[string]any{"property": int64(13), "label": "Skype", "value": "ann_im"}})
	want("accounts", p["accounts"], []any{map[string]any{"type": "ab-store", "name": "iCloud"}})
	want("groups", raw["groups"], []any{"Alpha", "Zeta"})
	want("group_members_other", raw["group_members_other"], int64(2))
	want("birthday", p["birthday"], nil)
	want("raw.birthday", raw["birthday"], "1990-05-06")
	want("Kind", raw["Kind"], int64(1))
	want("Note", raw["Note"], "a note")
	want("source_id", p["source_id"], "1")
	want("source_ids", p["source_ids"], []any{"G-1", "E-1", "U-1"})
	want("photo", p["photo"], nil)
	want("CreationDate", raw["CreationDate"], int64(700000000))
	want("ModificationDate", raw["ModificationDate"], 700000100.5)
	// Times, Time: modified wins
	cocoa := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	created, modified := cocoa.Add(700000000*time.Second), cocoa.Add(700000100*time.Second+500*time.Millisecond)
	got := map[string]time.Time{}
	for _, nt := range r.Times {
		got[nt.Kind] = nt.Time.T
	}
	if len(got) != 2 || !got["created"].Equal(created) || !got["modified"].Equal(modified) {
		t.Errorf("times %v", r.Times)
	}
	if r.Time == nil || !r.Time.T.Equal(modified) {
		t.Errorf("time %v, want the modification date", r.Time)
	}
	if !strings.HasPrefix(r.Body, "Dr. Ann M. Lee PhD\n555-0001") || r.Summary != "Dr. Ann M. Lee PhD 555-0001" {
		t.Errorf("body %q summary %q", r.Body, r.Summary)
	}
	if r.Deleted {
		t.Error("an address book person is live")
	}
}

// 4E-R24: the stored DisplayName when non-empty, else the composed name, else none. Nickname is not part of the
// composed name.
func TestDisplayNameRule(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson", "ABMultiValue")
	k.person(1, map[string]any{"First": "Fa", "Last": "La", "DisplayName": "Stored Name"})
	k.person(2, map[string]any{"First": "Fb", "Last": "Lb", "DisplayName": ""})
	k.person(3, map[string]any{"Nickname": "Nick"})
	k.mv(3, 3, nil, "+1 555 0003")
	k.person(4, map[string]any{"Prefix": "Mr", "Suffix": "Sr", "Middle": "Mid"})
	k.mv(4, 4, nil, "d@example.test")
	got := byRow(t, runBook(t, k.bytes()))
	for id, w := range map[int64][2]string{1: {"Stored Name", "stored"}, 2: {"Fb Lb", "composed"}, 3: {"", ""}, 4: {"Mr Mid Sr", "composed"}} {
		r := got[id]
		dn, _ := r.Payload["display_name"].(string)
		src, _ := rawIn(r)["display_name_source"].(string)
		if dn != w[0] || src != w[1] {
			t.Errorf("person %d: display_name %q (%q), want %q (%q)", id, dn, src, w[0], w[1])
		}
		if w[0] == "" {
			if _, has := r.Payload["display_name"]; has {
				t.Errorf("person %d: an absent display name must be absent, not empty", id)
			}
			if _, has := rawIn(r)["display_name_source"]; has {
				t.Errorf("person %d: display_name_source without a display name", id)
			}
		}
	}
}

// 4E-R23: a birthday is never derived; the stored value is kept whatever its storage class.
func TestBirthdayIsNeverDerived(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	k.person(1, map[string]any{"First": "A", "Birthday": "1985-03-04"})
	k.person(2, map[string]any{"First": "B", "Birthday": int64(489024000)})
	k.person(3, map[string]any{"First": "C", "Birthday": 489024000.5})
	k.person(4, map[string]any{"First": "D"})
	got := byRow(t, runBook(t, k.bytes()))
	for id, w := range map[int64]any{1: "1985-03-04", 2: int64(489024000), 3: 489024000.5, 4: nil} {
		r := got[id]
		if _, has := r.Payload["birthday"]; has {
			t.Errorf("person %d: payload.birthday is derived", id)
		}
		if g := rawIn(r)["birthday"]; g != w {
			t.Errorf("person %d: raw.birthday = %#v, want %#v", id, g, w)
		}
		if u := asMap(rawIn(r)["unknown_columns"]); u["Birthday"] != nil {
			t.Errorf("person %d: a numeric birthday is a value, not an unknown: %v", id, u)
		}
	}
}

// Dates: a sentinel or invalid CreationDate is stated with its status and gives no time; a wrong storage class
// is unknown (no raw value); Time falls back to created.
func TestTimesAndTheirStatuses(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	k.person(1, map[string]any{"First": "A", "CreationDate": int64(0), "ModificationDate": int64(700000300)})
	k.person(2, map[string]any{"First": "B", "CreationDate": int64(700000200), "ModificationDate": 1e12})
	k.person(3, map[string]any{"First": "C", "CreationDate": "yesterday"})
	k.person(4, map[string]any{"First": "D"})
	got := byRow(t, runBook(t, k.bytes()))
	r := got[1]
	if raw := rawIn(r); raw["CreationDate"] != int64(0) || raw["CreationDate_status"] != "sentinel" || len(r.Times) != 1 || r.Times[0].Kind != "modified" {
		t.Errorf("person 1: raw %v times %v", rawIn(r), r.Times)
	}
	r = got[2]
	if raw := rawIn(r); raw["ModificationDate_status"] != "invalid" || len(r.Times) != 1 || r.Times[0].Kind != "created" || r.Time == nil || !r.Time.T.Equal(r.Times[0].Time.T) {
		t.Errorf("person 2: raw %v times %v time %v", rawIn(r), r.Times, r.Time)
	}
	r = got[3]
	if raw := rawIn(r); asMap(raw["unknown_columns"])["CreationDate"] != "type-mismatch" || raw["CreationDate"] != nil || r.Time != nil || len(r.Times) != 0 {
		t.Errorf("person 3: raw %v", rawIn(r))
	}
	if r := got[4]; r.Time != nil || len(r.Times) != 0 {
		t.Errorf("person 4: a person without dates has no time: %v %v", r.Time, r.Times)
	}
}

// A store id with no ABStore row is stated; a NULL StoreID and a variant without the column state nothing;
// a missing ABStore table is undecided, not "no store".
func TestStoreJoinStatesEveryOutcome(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson", "ABStore")
	k.row("ABStore", 1, map[string]any{"Name": "iCloud"})
	k.person(1, map[string]any{"First": "A", "StoreID": int64(1)})
	k.person(2, map[string]any{"First": "B", "StoreID": int64(7)})
	k.person(3, map[string]any{"First": "C"})
	k.person(4, map[string]any{"First": "D", "StoreID": "x"})
	got := byRow(t, runBook(t, k.bytes()))
	if len(asList(got[1].Payload["accounts"])) != 1 {
		t.Errorf("person 1: %v", got[1].Payload)
	}
	if r := rawIn(got[2]); r["store_state"] != "no-store-row" || got[2].Payload["accounts"] != nil {
		t.Errorf("person 2: %v", r)
	}
	if r := rawIn(got[3]); r["store_state"] != nil || got[3].Payload["accounts"] != nil {
		t.Errorf("person 3: %v", r)
	}
	if asMap(rawIn(got[4])["unknown_columns"])["StoreID"] != "type-mismatch" {
		t.Errorf("person 4: %v", rawIn(got[4]))
	}
	// no ABStore table at all
	k2 := newBook(t, "addressbook-current.sql", "ABPerson")
	k2.person(1, map[string]any{"First": "A", "StoreID": int64(1)})
	r := byRow(t, runBook(t, k2.bytes()))[1]
	if raw := rawIn(r); !slices.Contains(asList(raw["join_undecided"]), any("ABStore")) || raw["store_state"] != nil || r.Payload["accounts"] != nil {
		t.Errorf("no ABStore table: %v", rawIn(r))
	}
}

// Lists are capped at 1000 with the totals stated, in the order of the rows.
func TestListCapStatesTheTotal(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson", "ABMultiValue")
	k.person(1, map[string]any{"First": "Big"})
	for i := range 1003 {
		k.mv(1, 3, nil, fmt.Sprintf("+1 555 %04d", i))
	}
	r := byRow(t, runBook(t, k.bytes()))[1]
	ph := asList(r.Payload["phones"])
	if len(ph) != 1000 || asMap(ph[999])["value"] != "+1 555 0999" {
		t.Errorf("%d phones", len(ph))
	}
	if raw := rawIn(r); raw["phones_total"] != int64(1003) || raw["phones_truncated"] != true {
		t.Errorf("raw %v", raw)
	}
}

// A person with none of display name, phone, e-mail and organization identifies nobody: a counted warning, no
// record (4D pattern).
func TestPersonThatIdentifiesNobodyIsAWarning(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson", "ABMultiValue")
	k.person(1, map[string]any{"Nickname": "ghost", "Note": "just a note"})
	k.mv(1, 22, nil, "https://example.test/ghost")
	k.person(2, map[string]any{"First": "Real"})
	res := runBook(t, k.bytes())
	if res.Err != nil || len(res.Records) != 1 || res.Records[0].Locator != "sqlite:table=ABPerson;rowid=2" {
		t.Fatalf("err %v records %v", res.Err, res.Records)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, "identifies nobody") || !strings.Contains(res.Warnings[0].Locator, "rowid=1") {
		t.Errorf("warnings %+v", res.Warnings)
	}
}

// lossyMultiValue builds 300 people with one phone each and zeroes the cell count of one leaf of ABMultiValue
// (page size 512): lost holds the people whose phone was on it.
func lossyMultiValue(t *testing.T) (db []byte, lost map[int64]bool) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	defs := snapshot(t, "addressbook-current.sql")
	pe := b.CreateTable("ABPerson", defs["ABPerson"].ddl)
	mv := b.CreateTable("ABMultiValue", defs["ABMultiValue"].ddl)
	for i := int64(1); i <= 300; i++ {
		vals := make([]any, len(defs["ABPerson"].cols))
		vals[0], vals[1], vals[2] = i, "First", fmt.Sprintf("Last%d", i)
		pe.Insert(i, vals...)
		mv.Insert(i, i, i, int64(3), nil, nil, fmt.Sprintf("+1 555 %04d", i), nil)
	}
	db = b.Bytes()
	leaves := mv.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := mv.CellBytes(i); pg == leaves[1] {
			lost[i] = true
		}
	}
	if len(lost) == 0 || len(lost) == 300 {
		t.Fatalf("%d ids on the damaged leaf", len(lost))
	}
	page := db[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0
	return db, lost
}

// An undecidable multi-value join is unknown, never an empty list (spec 4B D7): a missing table and an index
// that lost rows both list the table in raw.join_undecided and state no phones: []. A person with only a name
// still validates.
func TestUndecidableMultiValueJoinIsUnknownNotEmpty(t *testing.T) {
	// no ABMultiValue table
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	k.person(1, map[string]any{"First": "Only", "Last": "Name"})
	res := runBook(t, k.bytes())
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v records %d", res.Err, len(res.Records))
	}
	r := res.Records[0]
	if !slices.Contains(asList(rawIn(r)["join_undecided"]), any("ABMultiValue")) {
		t.Errorf("join_undecided %v", rawIn(r)["join_undecided"])
	}
	for _, key := range []string{"phones", "emails", "urls", "addresses"} {
		if _, has := r.Payload[key]; has {
			t.Errorf("%s present although the join is undecided: %v", key, r.Payload[key])
		}
	}
	// an index that lost rows
	db, lost := lossyMultiValue(t)
	res = runBook(t, db)
	if res.Err != nil || len(res.Records) != 300 {
		t.Fatalf("err %v records %d", res.Err, len(res.Records))
	}
	for id, r := range byRow(t, res) {
		u := asList(rawIn(r)["join_undecided"])
		if !slices.Contains(u, any("ABMultiValue")) {
			t.Errorf("person %d: join_undecided %v (a hit on an index that lost rows is stated too)", id, u)
		}
		_, has := r.Payload["phones"]
		if lost[id] == has {
			t.Errorf("person %d: lost row %v but phones present %v", id, lost[id], has)
		}
	}
}

// A row whose record the decoder found damaged says so in raw.row_flags.
func TestRowFlagsStateDecoderDamage(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	k.person(1, map[string]any{"First": "Fine"})
	// the record says (ROWID null, First = 1 byte of text) but its body is longer: a length mismatch
	k.tables["ABPerson"].InsertRaw(2, []uint64{0, 15}, []byte{'a', 'Z'})
	got := byRow(t, runBook(t, k.bytes()))
	if _, has := rawIn(got[1])["row_flags"]; has {
		t.Errorf("an intact row has no row_flags: %v", rawIn(got[1]))
	}
	if f := asList(rawIn(got[2])["row_flags"]); !slices.Contains(f, any("length_mismatch")) {
		t.Errorf("row_flags %v (raw %v)", f, rawIn(got[2]))
	}
}

// Hostile text is cleaned and flagged, never kept raw; the Note is cut at 64 KiB with raw.note_truncated.
func TestTextIsCleanedAndFlagged(t *testing.T) {
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	k.person(1, map[string]any{"First": "Bad\xffName", "Note": strings.Repeat("n", 70000)})
	r := byRow(t, runBook(t, k.bytes()))[1]
	names := asMap(r.Payload["names"])
	if names["given"] != "Bad�Name" {
		t.Errorf("given %q", names["given"])
	}
	raw := rawIn(r)
	if f := asList(raw["text_flags"]); !slices.Equal(f, []any{"First", "Note"}) {
		t.Errorf("text_flags %v", f)
	}
	if n, _ := raw["Note"].(string); len(n) != 64<<10 || raw["note_truncated"] != true {
		t.Errorf("Note %d bytes, truncated %v", len(n), raw["note_truncated"])
	}
}
