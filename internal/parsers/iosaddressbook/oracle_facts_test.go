package iosaddressbook_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// cocoa is the Apple epoch, 2001-01-01T00:00:00Z.
var cocoa = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func rawOf(r parsertest.ExpRecord) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func list(v any) []any { l, _ := v.([]any); return l }

func byRowid(t *testing.T, variant string) map[int64]parsertest.ExpRecord {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		if r.Type != "contact" || r.Table != "ABPerson" {
			t.Errorf("%s rowid %d: type %q table %q", variant, r.Rowid, r.Type, r.Table)
		}
		out[r.Rowid] = r
	}
	return out
}

func num(t *testing.T, v any) string {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("%v is not a JSON number", v)
	}
	return n.String()
}

func wantTime(t *testing.T, label string, got *string, want time.Time) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: no time, want %s", label, want.Format(time.RFC3339Nano))
		return
	}
	g, err := time.Parse(time.RFC3339Nano, *got)
	if err != nil || !g.Equal(want) {
		t.Errorf("%s: time %q, want %s", label, *got, want.Format(time.RFC3339Nano))
	}
}

// wantTimes checks the created and modified entries of Times: a nil want means the key is absent.
func wantTimes(t *testing.T, label string, r parsertest.ExpRecord, created, modified *time.Time) {
	t.Helper()
	for key, want := range map[string]*time.Time{"created": created, "modified": modified} {
		got, has := r.Times[key]
		switch {
		case want == nil && has:
			t.Errorf("%s: times.%s = %q, want none", label, key, got)
		case want != nil && !has:
			t.Errorf("%s: no times.%s, want %s", label, key, want.Format(time.RFC3339Nano))
		case want != nil:
			wantTime(t, label+" times."+key, &got, *want)
		}
	}
	if len(r.Times) > 2 {
		t.Errorf("%s: times %v", label, r.Times)
	}
}

func strsOf(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	for _, e := range list(v) {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%v is not a list of strings", v)
		}
		out = append(out, s)
	}
	return out
}

func values(v any) (vals, labels []string) {
	for _, e := range list(v) {
		m := obj(e)
		s, _ := m["value"].(string)
		vals = append(vals, s)
		l, _ := m["label"].(string)
		labels = append(labels, l)
	}
	return vals, labels
}

// The spec facts of the current-schema scenario, written from the rules (4E-R23, R24 and the contract table of
// ios.addressbook), not read back from the generator.
func TestOracleStatesTheSpecFacts(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "current")
	rows := byRowid(t, "current")
	if len(rows) != 12 {
		t.Fatalf("%d records, the scenario writes 13 persons and one of them identifies nobody", len(rows))
	}
	// a person with none of display name, phones, e-mails and organization is a warning, never a record
	if _, has := rows[9]; has {
		t.Errorf("person 9 identifies nobody and has a record")
	}
	if w := fx.Expect.Warnings; len(w) != 1 || w[0].Table != "ABPerson" || w[0].Rowid != 9 || !strings.Contains(w[0].Contains, "identifies nobody") {
		t.Errorf("warnings %+v, want one 'identifies nobody' for ABPerson 9", w)
	}
	p1 := rows[1]
	// label rule: _$!<X>!$_ is X with the stored text in raw.label_raw (row order, index within the list); a custom label
	// is kept as is; a dangling label id is counted and gives no label
	vals, labs := values(p1.Payload["phones"])
	if !slices.Equal(vals, []string{"+1 555 0100", "+1 555 0101", "+1 555 0102", "+1 555 0103"}) ||
		!slices.Equal(labs, []string{"Mobile", "Home", "Lab line", ""}) {
		t.Errorf("phones %v labels %v", vals, labs)
	}
	vals, labs = values(p1.Payload["emails"])
	if !slices.Equal(vals, []string{"alice@example.test", "alice.home@example.test"}) || !slices.Equal(labs, []string{"Work", ""}) {
		t.Errorf("emails %v labels %v", vals, labs)
	}
	vals, labs = values(p1.Payload["urls"])
	if !slices.Equal(vals, []string{"https://example.test/alice"}) || !slices.Equal(labs, []string{"HomePage"}) {
		t.Errorf("urls %v labels %v", vals, labs)
	}
	var gotRaw []string
	for _, e := range list(rawOf(p1)["label_raw"]) {
		m := obj(e)
		gotRaw = append(gotRaw, m["list"].(string)+"/"+num(t, m["index"])+"/"+m["label"].(string))
	}
	wantRaw := []string{"phones/0/_$!<Mobile>!$_", "emails/0/_$!<Work>!$_", "phones/1/_$!<Home>!$_", "urls/0/_$!<HomePage>!$_", "addresses/0/_$!<Home>!$_"}
	if !slices.Equal(gotRaw, wantRaw) {
		t.Errorf("raw.label_raw %v, want %v", gotRaw, wantRaw)
	}
	if num(t, rawOf(p1)["labels_unresolved"]) != "1" {
		t.Errorf("raw.labels_unresolved %v, want 1 (phone 4 names label 99)", rawOf(p1)["labels_unresolved"])
	}
	// property 13 is not mapped to a field: it is in raw.other_values, and there is no ims list
	ov := list(rawOf(p1)["other_values"])
	if len(ov) != 1 || num(t, obj(ov[0])["property"]) != "13" || obj(ov[0])["value"] != "alice_im" || obj(ov[0])["label"] != "Skype" {
		t.Errorf("raw.other_values %v", rawOf(p1)["other_values"])
	}
	if _, has := p1.Payload["ims"]; has {
		t.Errorf("payload.ims %v: property 13 is not an instant-messaging mapping", p1.Payload["ims"])
	}
	// 4E-R23: no birthday field anywhere; the stored value is in raw.birthday
	for id, r := range rows {
		if _, has := r.Payload["birthday"]; has {
			t.Errorf("person %d has payload.birthday", id)
		}
	}
	if rawOf(p1)["birthday"] != "1985-03-04" || rawOf(rows[11])["birthday"] != "489024000" {
		t.Errorf("raw.birthday %v and %v, want the stored texts", rawOf(p1)["birthday"], rawOf(rows[11])["birthday"])
	}
	if _, has := rawOf(rows[2])["birthday"]; has {
		t.Errorf("person 2 (NULL birthday) has raw.birthday")
	}
	// structured addresses: the five mapped keys, the multi-value value as formatted, CountryCode and unknown keys in raw
	addrs := list(p1.Payload["addresses"])
	if len(addrs) != 2 {
		t.Fatalf("%d addresses, want 2", len(addrs))
	}
	a0, a1 := obj(addrs[0]), obj(addrs[1])
	if a0["formatted"] != "1 Main St\nSpringfield" || a0["street"] != "1 Main St" || a0["city"] != "Springfield" || a0["region"] != "IL" ||
		a0["postcode"] != "62701" || a0["country"] != "United States" || a0["label"] != "Home" || len(a0) != 7 {
		t.Errorf("address 0 %v", a0)
	}
	if a1["street"] != "2 Side Rd" || a1["country"] != "Canada" || len(a1) != 2 {
		t.Errorf("address 1 %v: no formatted (NULL value), no label (NULL), no CountryCode", a1)
	}
	var unmapped []string
	for _, e := range list(rawOf(p1)["address_keys_unmapped"]) {
		m := obj(e)
		unmapped = append(unmapped, num(t, m["index"])+"/"+m["key"].(string)+"="+m["value"].(string))
	}
	if !slices.Equal(unmapped, []string{"0/CountryCode=us", "0/Weird=x", "1/CountryCode=ca"}) {
		t.Errorf("raw.address_keys_unmapped %v", unmapped)
	}
	// 4E-R24: a stored display name wins over the composed one; a name with no DisplayName is composed
	if p1.Payload["display_name"] != "Alice Smith" || rawOf(p1)["display_name_source"] != "stored" {
		t.Errorf("person 1 display %v source %v", p1.Payload["display_name"], rawOf(p1)["display_name_source"])
	}
	if rows[2].Payload["display_name"] != "Bob Jones" || rawOf(rows[2])["display_name_source"] != "composed" {
		t.Errorf("person 2 display %v source %v", rows[2].Payload["display_name"], rawOf(rows[2])["display_name_source"])
	}
	// an empty stored DisplayName with no name parts: no display name and no source; the organization identifies the person
	if _, has := rows[3].Payload["display_name"]; has || rawOf(rows[3])["display_name_source"] != nil || obj(rows[3].Payload["organization"])["name"] != "Initech" {
		t.Errorf("person 3 (organization only): %v", rows[3].Payload)
	}
	if rows[3].Summary == nil || *rows[3].Summary != "Initech" {
		t.Errorf("person 3 summary %v", rows[3].Summary)
	}
	// names with phonetics; the organization, title and department columns
	n := obj(p1.Payload["names"])
	if n["given"] != "Alice" || n["family"] != "Smith" || n["middle"] != "B." || n["prefix"] != "Dr." || n["suffix"] != "Jr." ||
		n["nickname"] != "Ali" || n["phonetic"] != "Ah-liss Bee Smyth" {
		t.Errorf("names %v", n)
	}
	if o := obj(p1.Payload["organization"]); o["name"] != "Acme Corp" || o["title"] != "CEO" || o["department"] != "R&D" || len(o) != 3 {
		t.Errorf("organization %v", o)
	}
	// accounts through StoreID; a dangling store id is stated; no StoreID, no account
	if a := list(p1.Payload["accounts"]); len(a) != 1 || obj(a[0])["type"] != "ab-store" || obj(a[0])["name"] != "iCloud" {
		t.Errorf("person 1 accounts %v", p1.Payload["accounts"])
	}
	if a := list(rows[2].Payload["accounts"]); len(a) != 1 || obj(a[0])["name"] != "On My iPhone" {
		t.Errorf("person 2 accounts %v", rows[2].Payload["accounts"])
	}
	if _, has := rows[12].Payload["accounts"]; has || rawOf(rows[12])["store_state"] != "no-store-row" {
		t.Errorf("person 12 (store 9 does not exist): accounts %v raw %v", rows[12].Payload["accounts"], rawOf(rows[12]))
	}
	if _, has := rows[5].Payload["accounts"]; has || rawOf(rows[5])["store_state"] != nil {
		t.Errorf("person 5 (NULL StoreID): accounts %v raw %v", rows[5].Payload["accounts"], rawOf(rows[5]))
	}
	// groups: sorted names for member type 0; other member types are counted, never listed
	if g := strsOf(t, rawOf(p1)["groups"]); !slices.Equal(g, []string{"Family", "Friends"}) {
		t.Errorf("person 1 groups %v", g)
	}
	if g := strsOf(t, rawOf(rows[12])["groups"]); !slices.Equal(g, []string{"Work"}) || num(t, rawOf(rows[12])["group_members_other"]) != "2" {
		t.Errorf("person 12 groups %v other %v", g, rawOf(rows[12])["group_members_other"])
	}
	if _, has := rawOf(p1)["group_members_other"]; has {
		t.Errorf("person 1 has group_members_other")
	}
	// identity: the decimal ROWID and the non-empty guid, ExternalIdentifier, ExternalUUID in that order
	if p1.Payload["source_id"] != "1" || !slices.Equal(strsOf(t, p1.Payload["source_ids"]), []string{"G-0001", "ext-0001", "UUID-0001"}) {
		t.Errorf("person 1 source_id %v source_ids %v", p1.Payload["source_id"], p1.Payload["source_ids"])
	}
	if !slices.Equal(strsOf(t, rows[2].Payload["source_ids"]), []string{"ext-0002"}) {
		t.Errorf("person 2 source_ids %v", rows[2].Payload["source_ids"])
	}
	// the Note is in raw (cleaned); the Kind column is kept
	if rawOf(p1)["Note"] != "Likes tea\nand coffee" || num(t, rawOf(p1)["Kind"]) != "0" || num(t, rawOf(rows[3])["Kind"]) != "1" {
		t.Errorf("person 1 raw %v", rawOf(p1))
	}
	// hostile text: U+202E stays in the stored name and is shown as <U+202E> in the summary; NUL and invalid UTF-8 become
	// U+FFFD and are listed in raw.text_flags
	if s, _ := rows[4].Payload["display_name"].(string); !strings.Contains(s, "\u202e") || rows[4].Summary == nil || !strings.Contains(*rows[4].Summary, "<U+202E>") {
		t.Errorf("person 4 display %q summary %v", rows[4].Payload["display_name"], rows[4].Summary)
	}
	if rawOf(rows[7])["Note"] != "before�after" || !slices.Equal(strsOf(t, rawOf(rows[7])["text_flags"]), []string{"Note"}) {
		t.Errorf("person 7 raw %v", rawOf(rows[7]))
	}
	if obj(rows[8].Payload["names"])["family"] != "Bad�Name" || rows[8].Payload["display_name"] != "Eve Bad�Name" ||
		!slices.Equal(strsOf(t, rawOf(rows[8])["text_flags"]), []string{"Last"}) {
		t.Errorf("person 8: names %v display %q raw %v", rows[8].Payload["names"], rows[8].Payload["display_name"], rawOf(rows[8]))
	}
	if obj(rows[5].Payload["names"])["given"] != "太郎" || obj(rows[6].Payload["names"])["given"] != "Zoë \U0001F600" {
		t.Errorf("CJK and emoji names: %v %v", rows[5].Payload["names"], rows[6].Payload["names"])
	}
	// a wrong storage class is unknown, never a value
	r13 := rawOf(rows[13])
	uc := obj(r13["unknown_columns"])
	if uc["Kind"] != "type-mismatch" || uc["CreationDate"] != "type-mismatch" || len(uc) != 2 {
		t.Errorf("person 13 unknown_columns %v", uc)
	}
	if _, has := r13["Kind"]; has {
		t.Errorf("person 13 has raw.Kind %v", r13["Kind"])
	}
	if rows[13].Time != nil || len(rows[13].Times) != 0 {
		t.Errorf("person 13 has a time %v %v", rows[13].Time, rows[13].Times)
	}
	// the unmapped columns of ABPerson in DDL order
	if got := fx.Expect.Notes["unmapped_columns.ABPerson"]; got != "FirstSort,LastSort,CompositeNameFallback,ExternalModificationTag,ExternalRepresentation" {
		t.Errorf("unmapped_columns.ABPerson = %q", got)
	}
	if !strings.HasPrefix(fx.Expect.Notes["schema.ABPerson"], "columns:28;optional:First,Last,Middle,") {
		t.Errorf("schema.ABPerson = %q", fx.Expect.Notes["schema.ABPerson"])
	}
	for id, r := range rows {
		if r.Deleted || r.TimeEnd != nil {
			t.Errorf("person %d: deleted %v time_end %v", id, r.Deleted, r.TimeEnd)
		}
	}
}

// Times: created and modified are Cocoa seconds; Time is modified when valid, else created when valid; 0 is a
// sentinel, year 2100 and a non-finite REAL are invalid (P-4-1: IEEE bits and a status, no raw value).
func TestOracleStatesTheContactTimes(t *testing.T) {
	rows := byRowid(t, "current")
	at := func(sec float64) *time.Time {
		v := cocoa.Add(time.Duration(sec * float64(time.Second)))
		return &v
	}
	// person 1: REAL modified, INTEGER created
	wantTime(t, "person 1", rows[1].Time, *at(700000100.5))
	wantTimes(t, "person 1", rows[1], at(700000000), at(700000100.5))
	if num(t, rawOf(rows[1])["ModificationDate"]) != "700000100.5" || num(t, rawOf(rows[1])["CreationDate"]) != "700000000" {
		t.Errorf("person 1 raw %v", rawOf(rows[1]))
	}
	// person 2: only a creation date
	wantTime(t, "person 2", rows[2].Time, *at(700000200))
	wantTimes(t, "person 2", rows[2], at(700000200), nil)
	// person 3: only a modification date
	wantTime(t, "person 3", rows[3].Time, *at(700000300))
	wantTimes(t, "person 3", rows[3], nil, at(700000300))
	// person 10: 0 is a sentinel, 2100-06-01 is out of range: no time at all
	r10 := rawOf(rows[10])
	if rows[10].Time != nil || len(rows[10].Times) != 0 || r10["CreationDate_status"] != "sentinel" || r10["ModificationDate_status"] != "invalid" ||
		num(t, r10["CreationDate"]) != "0" {
		t.Errorf("person 10: time %v times %v raw %v", rows[10].Time, rows[10].Times, r10)
	}
	// person 11: a valid creation date and an infinite modification date: Time is the creation date
	r11 := rawOf(rows[11])
	wantTime(t, "person 11", rows[11].Time, *at(700000500))
	wantTimes(t, "person 11", rows[11], at(700000500), nil)
	if r11["ModificationDate_ieee"] != "7ff0000000000000" || r11["ModificationDate_status"] != "invalid" {
		t.Errorf("person 11 raw %v", r11)
	}
	if _, has := r11["ModificationDate"]; has {
		t.Errorf("person 11: a non-finite REAL has no raw.ModificationDate: %v", r11)
	}
	if _, has := r11["CreationDate_status"]; has {
		t.Errorf("person 11: a valid time has no status: %v", r11)
	}
	// a fractional second survives
	wantTime(t, "person 12", rows[12].Time, *at(700000600.25))
}

// 4E-R25 and 4E-R41: photo presence comes from what was written into the images database, only when there is one.
func TestOracleStatesPhotoPresenceFromTheImagesDatabase(t *testing.T) {
	rows := byRowid(t, "with-images")
	if len(rows) != 3 {
		t.Fatalf("%d records, want 3", len(rows))
	}
	for id, want := range map[int64]bool{1: true, 2: true, 3: false} {
		ph := obj(rows[id].Payload["photo"])
		got, ok := ph["present"].(bool)
		if !ok || got != want || len(ph) != 1 {
			t.Errorf("person %d: photo %v, want {present: %v}", id, rows[id].Payload["photo"], want)
		}
	}
	for _, v := range []string{"current", "older-schema", "huge-contact"} {
		for id, r := range byRowid(t, v) {
			if _, has := r.Payload["photo"]; has {
				t.Errorf("%s person %d has a photo statement but the variant has no images database", v, id)
			}
		}
	}
}

// The older schema has no DisplayName, StoreID, guid, ExternalUUID or ABStore: composed names, no accounts, only the
// ExternalIdentifier as a source id.
func TestOracleStatesTheOlderSchemaFacts(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "older-schema")
	rows := byRowid(t, "older-schema")
	if len(rows) != 4 || len(fx.Expect.Warnings) != 0 {
		t.Fatalf("%d records %d warnings, want 4 and 0", len(rows), len(fx.Expect.Warnings))
	}
	for id, r := range rows {
		if _, has := r.Payload["accounts"]; has {
			t.Errorf("person %d has accounts", id)
		}
		if s := rawOf(r)["display_name_source"]; s == "stored" {
			t.Errorf("person %d: stored display name in a schema without the column", id)
		}
		if rawOf(r)["store_state"] != nil {
			t.Errorf("person %d: store_state in a schema without StoreID", id)
		}
		if s, ok := r.Payload["source_ids"]; ok && !slices.Equal(strsOf(t, s), []string{"old-1"}) {
			t.Errorf("person %d source_ids %v", id, s)
		}
		// the composed name equals the joined parts, in the order Prefix First Middle Last Suffix
		if rawOf(r)["display_name_source"] == "composed" {
			n := obj(r.Payload["names"])
			var parts []string
			for _, k := range []string{"prefix", "given", "middle", "family", "suffix"} {
				if s, _ := n[k].(string); s != "" {
					parts = append(parts, s)
				}
			}
			if r.Payload["display_name"] != strings.Join(parts, " ") {
				t.Errorf("person %d: display name %q, parts %q", id, r.Payload["display_name"], parts)
			}
		}
	}
	if rows[1].Payload["display_name"] != "Ms. Carol White" || rows[3].Payload["display_name"] != "Dan Q. Public III" {
		t.Errorf("composed names %q %q", rows[1].Payload["display_name"], rows[3].Payload["display_name"])
	}
	if _, has := rows[2].Payload["display_name"]; has || obj(rows[2].Payload["organization"])["title"] != "Buyer" {
		t.Errorf("person 2 (organization only): %v", rows[2].Payload)
	}
	// a person with only a nickname and an e-mail address: identified by the e-mail, no display name
	if _, has := rows[4].Payload["display_name"]; has || obj(rows[4].Payload["names"])["nickname"] != "Zed" || rows[4].Summary == nil || *rows[4].Summary != "zed@example.test" {
		t.Errorf("person 4: %v summary %v", rows[4].Payload, rows[4].Summary)
	}
	wantTime(t, "person 1", rows[1].Time, cocoa.Add(700000900*time.Second))
	if num(t, rawOf(rows[1])["CreationDate"]) != "700000000.75" {
		t.Errorf("person 1 raw.CreationDate %v", rawOf(rows[1])["CreationDate"])
	}
	if !strings.HasPrefix(fx.Expect.Notes["schema.ABPerson"], "columns:24;optional:First,") || strings.Contains(fx.Expect.Notes["schema.ABPerson"], "DisplayName") {
		t.Errorf("schema.ABPerson = %q", fx.Expect.Notes["schema.ABPerson"])
	}
}

// One person with 1200 phones and 1100 e-mail addresses: each list is cut at 1000 and says so.
func TestOracleStatesTheHugeContactCaps(t *testing.T) {
	rows := byRowid(t, "huge-contact")
	if len(rows) != 1 {
		t.Fatalf("%d records, want 1", len(rows))
	}
	r := rows[1]
	for _, c := range []struct {
		key, total, first, last string
	}{{"phones", "1200", "+15550000000", "+15550000999"}, {"emails", "1100", "user0@example.test", "user999@example.test"}} {
		vals, _ := values(r.Payload[c.key])
		if len(vals) != 1000 {
			t.Errorf("%s: %d values, want 1000", c.key, len(vals))
			continue
		}
		if len(vals) != 1000 || vals[0] != c.first || vals[len(vals)-1] != c.last {
			t.Errorf("%s: %d values, first %q last %q", c.key, len(vals), vals[0], vals[len(vals)-1])
		}
		if num(t, rawOf(r)[c.key+"_total"]) != c.total || rawOf(r)[c.key+"_truncated"] != true {
			t.Errorf("%s: raw %v, want %s total and truncated", c.key, rawOf(r), c.total)
		}
	}
	if r.Body == nil || strings.Count(*r.Body, "\n") != 2000 {
		t.Errorf("body lines: the name and the 2000 kept values expected")
	}
}
