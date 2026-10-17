package androidcontacts_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func run(t *testing.T, variant string) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", variant).Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

func rec(t *testing.T, res parsertest.Result, table, rowid string) records.Record {
	t.Helper()
	loc := "sqlite:table=" + table + ";rowid=" + rowid
	for _, r := range res.Records {
		if r.Locator == loc {
			return r
		}
	}
	t.Fatalf("no record %s", loc)
	return records.Record{}
}

func listOf(p map[string]any, key string) []any {
	l, _ := p[key].([]any)
	return l
}

func rawOfRec(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func timeKinds(r records.Record) map[string]string {
	out := map[string]string{}
	for _, nt := range r.Times {
		out[nt.Kind] = nt.Time.T.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out
}

func TestContactMappingFromTheSpec(t *testing.T) {
	res := run(t, "aosp-baseline")
	one := rec(t, res, "raw_contacts", "1")
	p := one.Payload
	phones := listOf(p, "phones")
	if p["display_name"] != "Alice Example" || len(phones) != 4 || p["starred"] != true || p["source_id"] != "1" ||
		phones[0].(map[string]any)["label"] != "mobile" || phones[0].(map[string]any)["primary"] != true ||
		phones[2].(map[string]any)["label"] != "Fax Home" {
		t.Errorf("contact 1: %v", p)
	}
	if _, has := phones[3].(map[string]any)["label"]; has {
		t.Error("an unrecognised phone type got a label")
	}
	names := p["names"].(map[string]any)
	if names["given"] != "Alice" || names["nickname"] != "Al" || names["phonetic"] != "al ex" {
		t.Errorf("names %v", names)
	}
	if p["photo"].(map[string]any)["present"] != true || p["organization"].(map[string]any)["name"] != "Acme" {
		t.Errorf("photo/organization %v %v", p["photo"], p["organization"])
	}
	if !strings.HasPrefix(one.Body, "Alice Example\n+1 555-123-0001\n+15551230002") || one.Deleted || one.Recovery != "" {
		t.Errorf("body %q deleted %v", one.Body, one.Deleted)
	}
	if one.SourcePath != fixtureRemote {
		t.Errorf("source path %q (ruling T48)", one.SourcePath)
	}
	// the application's own deleted flag: Deleted, no Recovery (spec C8)
	three := rec(t, res, "raw_contacts", "3")
	if !three.Deleted || three.Recovery != "" || three.Payload["deleted"].(map[string]any)["source"] != "raw_contacts.deleted" {
		t.Errorf("contact 3: deleted %v recovery %q payload %v", three.Deleted, three.Recovery, three.Payload["deleted"])
	}
	// number-only contact, per-account duplicates are each a row of evidence
	if rec(t, res, "raw_contacts", "2").Payload["display_name"] != "+15559998888" || rec(t, res, "raw_contacts", "7").Payload["display_name"] != "Alice Example" {
		t.Error("number-only or duplicate contact")
	}
	// hostile text: NUL and invalid UTF-8 in a name never reach the payload
	six := rec(t, res, "raw_contacts", "6")
	if six.Payload["display_name"] != "Bad\ufffdName\ufffd" {
		t.Errorf("contact 6 name %q", six.Payload["display_name"])
	}
	// a contact that identifies nobody is a warning, not a record and not a silent drop
	var warned bool
	for _, w := range res.Warnings {
		warned = warned || (w.Locator == "sqlite:table=raw_contacts;rowid=4" && strings.Contains(w.Reason, "identifies nobody"))
	}
	if !warned {
		t.Errorf("no warning for the empty contact 4: %+v", res.Warnings)
	}
}

// T69: the account comes through the accounts join; a NULL account_id is no account; a dangling id is stated
// as such and is neither an account nor "no account".
func TestAccountsComeThroughTheAccountsJoin(t *testing.T) {
	res := run(t, "aosp-baseline")
	acct := func(row string) map[string]any {
		l := listOf(rec(t, res, "raw_contacts", row).Payload, "accounts")
		if len(l) != 1 {
			t.Fatalf("contact %s: accounts %v", row, l)
		}
		return l[0].(map[string]any)
	}
	if a := acct("1"); a["type"] != "com.google" || a["name"] != "alice@example.test" {
		t.Errorf("contact 1 account %v", a)
	}
	if a := acct("7"); a["type"] != "com.samsung.account" || a["name"] != "alice@samsung.test" {
		t.Errorf("contact 7 account %v", a)
	}
	two := rec(t, res, "raw_contacts", "2") // a NULL account_id: a device-local contact
	if _, has := two.Payload["accounts"]; has || rawOfRec(two)["account_state"] != nil || rawOfRec(two)["join_undecided"] != nil || rawOfRec(two)["unknown_columns"] != nil {
		t.Errorf("contact 2 (NULL account_id): %v", two.Payload)
	}
	twelve := rec(t, res, "raw_contacts", "12") // a dangling account_id
	r12 := rawOfRec(twelve)
	if _, has := twelve.Payload["accounts"]; has || r12["account_state"] != "no-accounts-row" || r12["account_id"] != int64(99) {
		t.Errorf("contact 12 (dangling account_id): %v", twelve.Payload)
	}
	m := run(t, "multi-account")
	want := map[string]string{"1": "com.google", "2": "com.samsung.account", "3": "com.whatsapp"}
	for row, typ := range want {
		l := listOf(rec(t, m, "raw_contacts", row).Payload, "accounts")
		if len(l) != 1 || l[0].(map[string]any)["type"] != typ {
			t.Errorf("multi-account contact %s: %v", row, l)
		}
	}
}

// T69, T73: the time is the AGGREGATE contact's last update, stated under times.aggregate_last_updated with its
// source; Record.Time stays unset for every raw contact; sentinels and invalid values keep the raw value and say why.
func TestAggregateTimeIsStatedAsTheAggregatesNotTheRawContacts(t *testing.T) {
	res := run(t, "aosp-baseline")
	for _, r := range res.Records {
		if r.Time != nil || r.TimeEnd != nil {
			t.Errorf("%s: Record.Time is the raw contact's own time, which this source lacks: %v", r.Locator, r.Time)
		}
	}
	one := rec(t, res, "raw_contacts", "1")
	if got := timeKinds(one); len(got) != 1 || got[contact.AggregateLastUpdated] != "2023-05-01T10:00:01Z" {
		t.Errorf("contact 1 times %v", got)
	}
	r1 := rawOfRec(one)
	if r1["contact_last_updated_source"] != "contacts.contact_last_updated_timestamp" || r1["contact_last_updated_timestamp"] != int64(1682935201000) || r1["contact_id"] != int64(1) {
		t.Errorf("contact 1 raw %v", r1)
	}
	for id, st := range map[string]string{"9": "sentinel", "10": "invalid"} {
		r := rec(t, res, "raw_contacts", id)
		raw := rawOfRec(r)
		if len(r.Times) != 0 || raw["contact_last_updated_timestamp_status"] != st || raw["contact_last_updated_timestamp"] == nil || raw["contact_last_updated_source"] == nil {
			t.Errorf("contact %s: times %v raw %v", id, r.Times, raw)
		}
	}
	r11 := rec(t, res, "raw_contacts", "11") // a NULL contact_id
	if raw := rawOfRec(r11); len(r11.Times) != 0 || raw["aggregate_state"] != "no-contact-id" || raw["contact_last_updated_timestamp"] != nil || raw["unknown_columns"] != nil {
		t.Errorf("contact 11: times %v raw %v", r11.Times, raw)
	}
	r13 := rec(t, res, "raw_contacts", "13") // a dangling contact_id
	if raw := rawOfRec(r13); len(r13.Times) != 0 || raw["aggregate_state"] != "no-contacts-row" || raw["contact_id"] != int64(98) || raw["contact_last_updated_timestamp"] != nil {
		t.Errorf("contact 13: times %v raw %v", r13.Times, raw)
	}
	// three raw contacts of one person share one aggregate time
	for _, r := range run(t, "multi-account").Records {
		if got := timeKinds(r); got[contact.AggregateLastUpdated] != "2023-05-01T10:00:01Z" {
			t.Errorf("%s: times %v", r.Locator, got)
		}
	}
}

func TestDeletedContactsTableGivesTombstones(t *testing.T) {
	res := run(t, "deleted-contacts")
	tomb := rec(t, res, "deleted_contacts", "77")
	if !tomb.Deleted || tomb.Recovery != "" || tomb.Payload["source_ids"].([]any)[0] != "77" ||
		tomb.Payload["deleted"].(map[string]any)["source"] != "deleted_contacts" || tomb.Time == nil || tomb.SourcePath != fixtureRemote {
		t.Errorf("tombstone 77: %+v", tomb)
	}
	if t78 := rec(t, res, "deleted_contacts", "78"); t78.Time != nil || t78.Payload["raw"].(map[string]any)["contact_deleted_timestamp_status"] != "sentinel" {
		t.Errorf("tombstone 78 (timestamp 0): time %v", t78.Time)
	}
	if _, has := tomb.Payload["display_name"]; has {
		t.Error("a tombstone invented a name")
	}
}

func TestHugeContactIsCappedWithCounts(t *testing.T) {
	r := rec(t, run(t, "huge-contact"), "raw_contacts", "1")
	raw := r.Payload["raw"].(map[string]any)
	if len(listOf(r.Payload, "phones")) != 1000 || raw["phones_total"] != int64(1200) || raw["phones_truncated"] != true ||
		len(listOf(r.Payload, "emails")) != 1000 || raw["emails_total"] != int64(1100) || len(raw["notes"].([]any)) != 100 || raw["notes_total"] != int64(130) || raw["notes_truncated"] != true {
		t.Errorf("caps: phones %d total %v, emails %d, notes %d", len(listOf(r.Payload, "phones")), raw["phones_total"], len(listOf(r.Payload, "emails")), len(raw["notes"].([]any)))
	}
}

// damagedDataDB: 300 raw contacts, each with one phone data row (data row N belongs to contact N);
// the second leaf of the data table has its cell count zeroed.
func damagedDataDB(t *testing.T) (data []byte, lost map[int64]bool) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	mt := b.CreateTable("mimetypes", "create table mimetypes(_id integer primary key, mimetype text not null)")
	mt.Insert(1, int64(1), "vnd.android.cursor.item/phone_v2")
	rc := b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text, deleted integer)")
	dt := b.CreateTable("data", "create table data(_id integer primary key, mimetype_id integer, raw_contact_id integer, is_primary integer, data1 text, data2 text)")
	for i := int64(1); i <= 300; i++ {
		rc.Insert(i, i, "Name", int64(0))
		dt.Insert(i, i, int64(1), i, int64(0), "+1555000", "2")
	}
	data = b.Bytes()
	leaves := dt.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := dt.CellBytes(i); pg == leaves[1] {
			lost[i] = true
		}
	}
	page := data[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0
	return data, lost
}

// Review Focus 1: a contact whose data rows cannot be read has unknown phones, not no phones.
func TestContactDataJoinUndecidedIsUnknownNotEmpty(t *testing.T) {
	data, lost := damagedDataDB(t)
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "lossy", Files: map[string][]byte{"db": data}}.Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if len(res.Records) != 300 {
		t.Fatalf("%d records, want 300: a contact whose data cannot be read still has its name", len(res.Records))
	}
	for _, r := range res.Records {
		var id int64
		if _, err := fmt.Sscanf(r.Locator, "sqlite:table=raw_contacts;rowid=%d", &id); err != nil {
			t.Fatalf("locator %q: %v", r.Locator, err)
		}
		raw := r.Payload["raw"].(map[string]any)
		if u, _ := raw["join_undecided"].([]any); len(u) != 1 || u[0] != "data" {
			t.Fatalf("contact %d: join_undecided = %v", id, raw["join_undecided"])
		}
		if lost[id] && len(listOf(r.Payload, "phones")) != 0 {
			t.Errorf("contact %d invented phones it could not read", id)
		}
		if !lost[id] && len(listOf(r.Payload, "phones")) != 1 {
			t.Errorf("contact %d lost a phone it could read", id)
		}
	}
}

// Review Focus 5: fan-out is bounded by the budget with a typed error, not by the heap.
func TestHostileFanOutIsBoundedByTheBudget(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("mimetypes", "create table mimetypes(_id integer primary key, mimetype text)").Insert(1, int64(1), "vnd.android.cursor.item/phone_v2")
	b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)").Insert(1, int64(1), "Hostile")
	dt := b.CreateTable("data", "create table data(_id integer primary key, mimetype_id integer, raw_contact_id integer, data1 text)")
	for i := int64(1); i <= 20000; i++ {
		dt.Insert(i, i, int64(1), int64(1), "+1555000")
	}
	h := parsertest.NewHarness(t)
	bundle := parsertest.Fixture{Name: "fan", Files: map[string][]byte{"db": b.Bytes()}}.Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	in := bundle.Input(h, p, false)
	in.Budget = parse.NewBudget(4 << 20).View()
	res := h.RunLenient(p, in)
	if !errors.Is(res.Err, parse.ErrBudget) {
		t.Errorf("err = %v, want parse.ErrBudget (a typed refusal, not an unbounded allocation)", res.Err)
	}
	ample := bundle.Input(h, p, false)
	res = h.RunLenient(p, ample)
	if res.Err != nil || len(res.Records) != 1 || res.Records[0].Payload["raw"].(map[string]any)["phones_total"] != int64(20000) {
		t.Errorf("with the default budget: err %v, %d records", res.Err, len(res.Records))
	}
}
