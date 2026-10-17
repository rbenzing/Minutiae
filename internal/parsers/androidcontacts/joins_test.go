package androidcontacts_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The joins a fixture cannot force: every case here is a database built in the test (T69, T70, T72). The
// pinned fixtures hold only decided joins; these hold a missing table, a table the engine cannot answer from,
// a column of the wrong storage class and an old schema, each with its own assertion.

const (
	rcDDL        = "create table raw_contacts(_id integer primary key, account_id integer, contact_id integer, display_name text)"
	accountsDDL  = "create table accounts(_id integer primary key, account_name text, account_type text, data_set text)"
	contactsDDL  = "create table contacts(_id integer primary key, contact_last_updated_timestamp integer)"
	phoneMime    = "vnd.android.cursor.item/phone_v2"
	aggregateKey = "aggregate_last_updated"
)

func newDB(page int) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: page})
	b.CreateTable("mimetypes", "create table mimetypes(_id integer primary key, mimetype text not null)").Insert(1, int64(1), phoneMime)
	b.CreateTable("data", "create table data(_id integer primary key, mimetype_id integer, raw_contact_id integer, is_primary integer, data1 text, data2 text)")
	return b
}

func runDB(t *testing.T, db []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}.Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

func byRowid(t *testing.T, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		var id int64
		if _, err := fmt.Sscanf(r.Locator, "sqlite:table=raw_contacts;rowid=%d", &id); err != nil {
			t.Fatalf("locator %q: %v", r.Locator, err)
		}
		out[id] = r
	}
	return out
}

func undecided(r records.Record) []string {
	var out []string
	for _, v := range listOf(rawOfRec(r), "join_undecided") {
		out = append(out, v.(string))
	}
	return out
}

func hasAccount(r records.Record) bool { return len(listOf(r.Payload, "accounts")) > 0 }

func requireNoRun(t *testing.T, res parsertest.Result) {
	t.Helper()
	if res.Err != nil || len(res.Warnings) != 0 {
		t.Fatalf("err %v, warnings %+v", res.Err, res.Warnings)
	}
}

// requireDamageReported: the lost page is reported by the engine as a warning, and the run does not fail.
func requireDamageReported(t *testing.T, res parsertest.Result) {
	t.Helper()
	if res.Err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, "btree-shape") {
		t.Fatalf("err %v, warnings %+v: want one btree-shape warning for the zeroed page", res.Err, res.Warnings)
	}
}

// Channel 1 (T69): a database without an accounts table cannot say whether a contact has an account. The account is
// unknown (join_undecided), never "no account"; a NULL account_id is still decided: no account.
func TestMissingAccountsTableIsUnknownNotNoAccount(t *testing.T) {
	b := newDB(1024)
	rc := b.CreateTable("raw_contacts", rcDDL)
	rc.Insert(1, int64(1), int64(1), int64(1), "With Account Id")
	rc.Insert(2, int64(2), nil, nil, "Device Local")
	b.CreateTable("contacts", contactsDDL).Insert(1, int64(1), int64(1682935201000))
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	got := byRowid(t, res)
	one, two := got[1], got[2]
	if hasAccount(one) || !slices.Equal(undecided(one), []string{"accounts"}) || rawOfRec(one)["account_state"] != nil || rawOfRec(one)["account_id"] != int64(1) {
		t.Errorf("contact 1 (no accounts table): %v", one.Payload)
	}
	if hasAccount(two) || undecided(two) != nil || rawOfRec(two)["account_state"] != nil || rawOfRec(two)["unknown_columns"] != nil {
		t.Errorf("contact 2 (NULL account_id): %v", two.Payload)
	}
	if timeKinds(one)[aggregateKey] != "2023-05-01T10:00:01Z" {
		t.Errorf("the contacts join is decided and must still state the time: %v", timeKinds(one))
	}
}

// Channel 1 for the aggregate time: no contacts table leaves the time unknown, never zero and never absent.
func TestMissingContactsTableIsUnknownTime(t *testing.T) {
	b := newDB(1024)
	rc := b.CreateTable("raw_contacts", rcDDL)
	rc.Insert(1, int64(1), nil, int64(1), "Aggregated")
	rc.Insert(2, int64(2), nil, nil, "Not Aggregated")
	b.CreateTable("accounts", accountsDDL)
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	got := byRowid(t, res)
	one, two := got[1], got[2]
	raw := rawOfRec(one)
	if len(one.Times) != 0 || !slices.Equal(undecided(one), []string{"contacts"}) || raw["aggregate_state"] != nil || raw["contact_last_updated_timestamp"] != nil || raw["contact_id"] != int64(1) {
		t.Errorf("contact 1 (no contacts table): times %v raw %v", one.Times, raw)
	}
	if raw := rawOfRec(two); len(two.Times) != 0 || undecided(two) != nil || raw["aggregate_state"] != "no-contact-id" {
		t.Errorf("contact 2 (NULL contact_id): %v", raw)
	}
}

// Both joins undecided at once: both tables are named, in the fixed order accounts, contacts.
func TestBothJoinsUndecidedAreListedInOrder(t *testing.T) {
	b := newDB(1024)
	b.CreateTable("raw_contacts", rcDDL).Insert(1, int64(1), int64(1), int64(1), "Neither Join")
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	if u := undecided(byRowid(t, res)[1]); !slices.Equal(u, []string{"accounts", "contacts"}) {
		t.Errorf("join_undecided = %v", u)
	}
}

// bigJoinDB: 300 accounts and 300 contacts (page size 512, several leaves each) and 300 raw contacts, contact i
// pointing at account i and aggregate i. The second leaf of the table named by damage has its cell count
// zeroed; lost holds the ids on that leaf.
func bigJoinDB(t *testing.T, damage string) (db []byte, lost map[int64]bool) {
	t.Helper()
	b := newDB(512)
	rc := b.CreateTable("raw_contacts", rcDDL)
	ac := b.CreateTable("accounts", accountsDDL)
	ct := b.CreateTable("contacts", contactsDDL)
	for i := int64(1); i <= 300; i++ {
		rc.Insert(i, i, i, i, "Name")
		ac.Insert(i, i, "a@example.test", "com.google", nil)
		ct.Insert(i, i, int64(1682935200000)+i)
	}
	db = b.Bytes()
	tbl := map[string]*sqlitetest.Table{"accounts": ac, "contacts": ct}[damage]
	leaves := tbl.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%s: %d leaves", damage, len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := tbl.CellBytes(i); pg == leaves[1] {
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

// Channel 2 (T69): accounts rows the engine cannot read. A contact whose account row was on the lost page is
// neither shown as having no account nor given "no-accounts-row": it is undecided. A contact whose account
// row survived still has its account.
func TestAccountsRowsOnADamagedPageAreUndecidedNotAbsent(t *testing.T) {
	db, lost := bigJoinDB(t, "accounts")
	res := runDB(t, db)
	requireDamageReported(t, res)
	got := byRowid(t, res)
	if len(got) != 300 {
		t.Fatalf("%d records, want 300", len(got))
	}
	for id, r := range got {
		raw := rawOfRec(r)
		if raw["account_state"] != nil {
			t.Errorf("contact %d: account_state %v; a row that cannot be read is not a row that is absent", id, raw["account_state"])
		}
		if lost[id] {
			if hasAccount(r) || !slices.Contains(undecided(r), "accounts") {
				t.Errorf("contact %d (lost account row): %v", id, r.Payload)
			}
		} else if !hasAccount(r) {
			t.Errorf("contact %d lost an account it could read: %v", id, r.Payload)
		} else if !slices.Contains(undecided(r), "accounts") {
			// the row was found, but the index lost rows, so the answer may be short: stated, not trusted
			t.Errorf("contact %d: a hit on an index that lost rows must still list accounts as undecided", id)
		}
		if raw["aggregate_state"] != nil || timeKinds(r)[aggregateKey] == "" {
			t.Errorf("contact %d: the intact contacts join must still state the time: %v", id, raw)
		}
	}
}

// Channel 2 for the aggregate time.
func TestContactsRowsOnADamagedPageAreUndecidedNotAbsent(t *testing.T) {
	db, lost := bigJoinDB(t, "contacts")
	res := runDB(t, db)
	requireDamageReported(t, res)
	got := byRowid(t, res)
	if len(got) != 300 {
		t.Fatalf("%d records, want 300", len(got))
	}
	for id, r := range got {
		raw := rawOfRec(r)
		if raw["aggregate_state"] != nil {
			t.Errorf("contact %d: aggregate_state %v; a row that cannot be read is not a row that is absent", id, raw["aggregate_state"])
		}
		if lost[id] {
			if len(r.Times) != 0 || !slices.Contains(undecided(r), "contacts") {
				t.Errorf("contact %d (lost contacts row): times %v raw %v", id, r.Times, raw)
			}
		} else if timeKinds(r)[aggregateKey] == "" {
			t.Errorf("contact %d lost a time it could read: %v", id, raw)
		} else if !slices.Contains(undecided(r), "contacts") {
			t.Errorf("contact %d: a hit on an index that lost rows must still list contacts as undecided", id)
		}
		if raw["account_state"] != nil || !hasAccount(r) {
			t.Errorf("contact %d: the intact accounts join must still state the account: %v", id, r.Payload)
		}
	}
}

// Channel 3 (T69): a table the join cannot be run against (no _id column to match) is undecided, not an error
// that fails the job and not an absence.
func TestJoinTargetWithoutItsKeyColumnIsUndecided(t *testing.T) {
	b := newDB(1024)
	b.CreateTable("raw_contacts", rcDDL).Insert(1, int64(1), int64(1), int64(1), "Keyless Targets")
	b.CreateTable("accounts", "create table accounts(account_name text, account_type text)").Insert(1, "a@example.test", "com.google")
	b.CreateTable("contacts", "create table contacts(contact_last_updated_timestamp integer)").Insert(1, int64(1682935201000))
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	r := byRowid(t, res)[1]
	if hasAccount(r) || len(r.Times) != 0 || !slices.Equal(undecided(r), []string{"accounts", "contacts"}) || rawOfRec(r)["account_state"] != nil || rawOfRec(r)["aggregate_state"] != nil {
		t.Errorf("keyless targets: %v", r.Payload)
	}
}

// Channel 4: a column of the wrong storage class is unknown, flagged where it is read: in the mapped row's own
// columns for account_id and contact_id, and under joined_unknown_columns (qualified by table) for a cell of a
// joined row. None is shown as an absent account or time.
func TestWrongStorageClassInAJoinIsUnknownNotAbsent(t *testing.T) {
	b := newDB(1024)
	rc := b.CreateTable("raw_contacts", rcDDL)
	rc.Insert(1, int64(1), "x", "y", "Text Ids")
	rc.Insert(2, int64(2), int64(2), int64(2), "Blob Name And Text Time")
	b.CreateTable("accounts", accountsDDL).Insert(2, int64(2), []byte{0xff, 0x00}, "com.example", nil)
	b.CreateTable("contacts", contactsDDL).Insert(2, int64(2), "not a number")
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	got := byRowid(t, res)
	one := rawOfRec(got[1])
	unk, _ := one["unknown_columns"].(map[string]any)
	if unk["account_id"] != "type-mismatch" || unk["contact_id"] != "type-mismatch" || one["account_state"] != nil || one["aggregate_state"] != nil || got[1].Payload["accounts"] != nil || len(got[1].Times) != 0 || undecided(got[1]) != nil {
		t.Errorf("text ids: %v", got[1].Payload)
	}
	two := rawOfRec(got[2])
	jun, _ := two["joined_unknown_columns"].(map[string]any)
	accts := listOf(got[2].Payload, "accounts")
	if jun["accounts.account_name"] != "type-mismatch" || jun["contacts.contact_last_updated_timestamp"] != "type-mismatch" || len(jun) != 2 ||
		len(accts) != 1 || accts[0].(map[string]any)["type"] != "com.example" || accts[0].(map[string]any)["name"] != nil || len(got[2].Times) != 0 || two["unknown_columns"] != nil {
		t.Errorf("blob name, text time: accounts %v raw %v", accts, two)
	}
}

// T69: an old schema keeps the account inline on raw_contacts; honoured ONLY because the columns are physically
// present, and it has no accounts table to consult. The time is never read inline.
func TestInlineAccountColumnsAreHonouredOnlyWhenPresent(t *testing.T) {
	b := newDB(1024)
	rc := b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, account_name text, account_type text, display_name text)")
	rc.Insert(1, int64(1), "old@example.test", "com.old", "Old Schema")
	rc.Insert(2, int64(2), nil, nil, "Old Schema, No Account")
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	got := byRowid(t, res)
	a := listOf(got[1].Payload, "accounts")
	if len(a) != 1 || a[0].(map[string]any)["type"] != "com.old" || a[0].(map[string]any)["name"] != "old@example.test" || undecided(got[1]) != nil || rawOfRec(got[1])["account_state"] != nil {
		t.Errorf("inline account: %v", got[1].Payload)
	}
	if hasAccount(got[2]) || undecided(got[2]) != nil || rawOfRec(got[2])["account_state"] != nil {
		t.Errorf("inline NULL account: %v", got[2].Payload)
	}
	// no contact_id column: the aggregate cannot be reached, which is stated and is not a time
	if raw := rawOfRec(got[1]); raw["aggregate_state"] != "no-contact-id-column" || len(got[1].Times) != 0 {
		t.Errorf("old schema aggregate: %v", raw)
	}
	// the inline columns win only because they are there: a current schema ignores a stray accounts table
	cur := newDB(1024)
	cur.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, account_id integer, display_name text)").Insert(1, int64(1), int64(1), "Current Schema")
	cur.CreateTable("accounts", accountsDDL).Insert(1, int64(1), "cur@example.test", "com.cur", nil)
	if a := listOf(byRowid(t, runDB(t, cur.Bytes()))[1].Payload, "accounts"); len(a) != 1 || a[0].(map[string]any)["type"] != "com.cur" {
		t.Errorf("join account: %v", a)
	}
}

// A raw_contacts with neither account_id nor the inline columns cannot say anything about an account: stated,
// never "no account".
func TestNoAccountColumnsAtAllIsStated(t *testing.T) {
	b := newDB(1024)
	b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, display_name text)").Insert(1, int64(1), "Bare")
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	raw := rawOfRec(byRowid(t, res)[1])
	if raw["account_state"] != "no-account-id-column" || raw["aggregate_state"] != "no-contact-id-column" {
		t.Errorf("raw %v", raw)
	}
}

// T70: the platform's raw_contacts is indexed on columns collated PHONEBOOK, a collation only the platform
// registers. The decoder does not need the collation to read the table: every row is read, none is dropped or
// reordered and nothing fails. (The index here is built with a stand-in order; the engine's join machinery
// builds its own in-memory index from the table, never from this one.)
func TestPhonebookCollatedIndexDoesNotStopReading(t *testing.T) {
	b := newDB(512)
	rc := b.CreateTable("raw_contacts", "create table raw_contacts(_id integer primary key, account_id integer, contact_id integer, display_name text, sort_key text collate phonebook, phonebook_bucket integer)")
	b.CreateTable("accounts", accountsDDL).Insert(1, int64(1), "a@example.test", "com.google", nil)
	b.CreateTable("contacts", contactsDDL).Insert(1, int64(1), int64(1682935201000))
	const n = 250
	for i := int64(1); i <= n; i++ {
		rc.Insert(i, i, int64(1), int64(1), fmt.Sprintf("Name %03d", n-i), fmt.Sprintf("name %03d", n-i), i%27)
	}
	b.CreateIndex("raw_contacts_sort_key_index", "raw_contacts", "create index raw_contacts_sort_key_index on raw_contacts(sort_key collate phonebook, phonebook_bucket)", 4, 5)
	b.CreateIndex("raw_contacts_bucket_index", "raw_contacts", "create index raw_contacts_bucket_index on raw_contacts(phonebook_bucket, sort_key collate phonebook)", 5, 4)
	res := runDB(t, b.Bytes())
	requireNoRun(t, res)
	got := byRowid(t, res)
	if len(got) != n {
		t.Fatalf("%d records, want %d: a PHONEBOOK-collated index must not cost a row", len(got), n)
	}
	for id, r := range got {
		if !hasAccount(r) || timeKinds(r)[aggregateKey] == "" || undecided(r) != nil {
			t.Fatalf("contact %d: %v", id, r.Payload)
		}
	}
}

// T77: an _id that names more than one row is a corrupt table. The join is undecided (picking a row would be a
// guess), and the record also says why: raw.join_duplicate holds the number of rows the key matched, per table.
// The tables here have a plain _id column, not the rowid alias, which the schema probe refuses; an empty -wal
// companion defers that decision to Parse, the way T18 does.
func TestDuplicateJoinMatchIsUndecidedAndCounted(t *testing.T) {
	b := newDB(1024)
	rc := b.CreateTable("raw_contacts", rcDDL)
	rc.Insert(1, int64(1), int64(1), int64(1), "Twice Named")
	rc.Insert(2, int64(2), int64(2), int64(2), "Once Named")
	ac := b.CreateTable("accounts", "create table accounts(_id integer, account_name text, account_type text, data_set text)")
	ac.Insert(1, int64(1), "a@example.test", "com.google", nil)
	ac.Insert(2, int64(1), "b@example.test", "com.other", nil)
	ac.Insert(3, int64(1), "c@example.test", "com.third", nil)
	ac.Insert(4, int64(2), "d@example.test", "com.google", nil)
	ct := b.CreateTable("contacts", "create table contacts(_id integer, contact_last_updated_timestamp integer)")
	ct.Insert(1, int64(1), int64(1682935201000))
	ct.Insert(2, int64(1), int64(1682935202000))
	ct.Insert(3, int64(2), int64(1682935203000))
	h := parsertest.NewHarness(t)
	bun := parsertest.Fixture{Name: "dup", Files: map[string][]byte{"db": b.Bytes(), "wal": {}}}.Bundle(h, fixtureRemote)
	p := androidcontacts.Parser{}
	res := h.RunLenient(p, bun.Input(h, p, false))
	requireNoRun(t, res)
	got := byRowid(t, res)
	one, two := got[1], got[2]
	raw := rawOfRec(one)
	dup, _ := raw["join_duplicate"].(map[string]any)
	if hasAccount(one) || len(one.Times) != 0 || !slices.Equal(undecided(one), []string{"accounts", "contacts"}) ||
		len(dup) != 2 || dup["accounts"] != int64(3) || dup["contacts"] != int64(2) || raw["account_state"] != nil || raw["aggregate_state"] != nil {
		t.Errorf("duplicate keys: times %v raw %v", one.Times, raw)
	}
	if rawOfRec(two)["join_duplicate"] != nil || !hasAccount(two) || timeKinds(two)[aggregateKey] == "" || undecided(two) != nil {
		t.Errorf("a key that names one row is decided and not marked: %v", two.Payload)
	}
}
