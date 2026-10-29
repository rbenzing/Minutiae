package androidcontacts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

var variants = []string{"aosp-baseline", "contacts-deleted", "contacts-wal-edit", "deleted-contacts", "duplicate-keys", "huge-contact", "multi-account", "unicode-rtl"}

// fixtureRemote is where every variant's database sits on the device; each is a lone database,
// so the oracle states this path for every row (rulings T48, T51).
const fixtureRemote = "/data/user_de/0/com.android.providers.contacts/databases/contacts2.db"

func TestFixturesArePinnedByHash(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		for role, data := range fx.Files {
			sum := sha256.Sum256(data)
			key := v + "." + role
			seen[key] = true
			if want, ok := fixturePins[key]; !ok || want != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: sha256 %x is not the pinned value %q (regenerate image, oracle and pin together)", key, sum, want)
			}
		}
	}
	for key := range fixturePins {
		if !seen[key] {
			t.Errorf("pin %s has no fixture file", key)
		}
	}
}

func TestEveryCommittedFixtureIsInTheVariantList(t *testing.T) {
	all, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var m []string
	for _, p := range all {
		if !strings.HasSuffix(p, ".recover.expect.json") { // the typed recovery oracles are checked by TestEveryRecoverOracleHasAFixture
			m = append(m, p)
		}
	}
	if len(m) != len(variants) {
		t.Errorf("%d fixtures on disk, %d in the variant list", len(m), len(variants))
	}
	for _, p := range m {
		name := strings.TrimSuffix(filepath.Base(p), ".expect.json")
		known := false
		for _, v := range variants {
			known = known || v == name
		}
		if !known {
			t.Errorf("testdata/%s is not in the variant list", name)
		}
	}
}

func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return m
}

func list(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a list", v)
	}
	return l
}

func num(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return "not a number"
}

// The oracle's literal facts, written from the spec (9.3, 8.5) and not read back from the
// generator: a generator that drifts from the rules fails here, not silently in the pins.
func TestOracleStatesTheContactRules(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	byRow := map[int64]int{}
	for i, r := range fx.Expect.Records {
		byRow[r.Rowid] = i
		if r.Type != "contact" || r.SourcePath != fixtureRemote {
			t.Errorf("row %d: type %q source_path %q", r.Rowid, r.Type, r.SourcePath)
		}
	}
	if len(fx.Expect.Records) != 12 {
		t.Fatalf("%d records, want 12 (13 raw contacts; contact 4 identifies nobody)", len(fx.Expect.Records))
	}
	if len(fx.Expect.Warnings) != 1 || fx.Expect.Warnings[0].Rowid != 4 || !strings.Contains(fx.Expect.Warnings[0].Contains, "identifies nobody") {
		t.Errorf("warnings %+v: contact 4 must warn", fx.Expect.Warnings)
	}
	if _, has := byRow[4]; has {
		t.Error("contact 4 identifies nobody and is not a record")
	}
	one := fx.Expect.Records[byRow[1]]
	p := one.Payload
	names := obj(t, p["names"])
	if names["given"] != "Alice" || names["family"] != "Example" || names["nickname"] != "Al" || names["phonetic"] != "al ex" ||
		names["prefix"] != "Dr." || names["middle"] != "Q." || names["suffix"] != "Jr." {
		t.Errorf("names %v", names)
	}
	phones := list(t, p["phones"])
	var labels []any
	for _, ph := range phones {
		labels = append(labels, obj(t, ph)["label"])
	}
	if len(phones) != 4 || labels[0] != "mobile" || labels[1] != "work" || labels[2] != "Fax Home" || labels[3] != nil || obj(t, phones[0])["primary"] != true {
		t.Errorf("phones %v", phones)
	}
	raw := obj(t, p["raw"])
	for k, want := range map[string]string{"labels_unknown": "1", "values_skipped": "1", "data_unmapped": "1", "data_other": "1", "name_rows_extra": "1", "organization_rows_extra": "1", "data_rows": "20"} {
		if num(raw[k]) != want {
			t.Errorf("raw.%s = %v, want %s", k, raw[k], want)
		}
	}
	ims := list(t, p["ims"])
	if len(ims) != 3 || obj(t, ims[0])["label"] != "skype" || obj(t, ims[1])["label"] != "aim" || obj(t, ims[2])["label"] != "XMPP" {
		t.Errorf("ims %v", ims)
	}
	if p["starred"] != true || p["source_id"] != "1" || obj(t, p["photo"])["present"] != true {
		t.Errorf("contact 1: %v", p)
	}
	three := fx.Expect.Records[byRow[3]]
	if !three.Deleted || obj(t, three.Payload["deleted"])["source"] != "raw_contacts.deleted" {
		t.Errorf("contact 3: %+v", three)
	}
	eleven := fx.Expect.Records[byRow[11]]
	if !eleven.Deleted || eleven.Summary == nil || *eleven.Summary != "deleted contact g-11" {
		t.Errorf("contact 11 is a tombstone-shaped record: %+v", eleven)
	}
	six := obj(t, fx.Expect.Records[byRow[6]].Payload["raw"])
	if fl := list(t, six["text_flags"]); len(fl) != 2 || fl[0] != "data" || fl[1] != "display_name" {
		t.Errorf("contact 6 text_flags %v", fl)
	}
	if !strings.Contains(fx.Expect.Records[byRow[6]].Payload["display_name"].(string), "�") {
		t.Error("contact 6 display_name keeps no U+FFFD")
	}
}

func TestOracleStatesTheTombstonesAndTheCaps(t *testing.T) {
	d := parsertest.LoadFixture(t, "testdata", "deleted-contacts")
	var tomb int
	for _, r := range d.Expect.Records {
		if r.Table != "deleted_contacts" {
			continue
		}
		tomb++
		if !r.Deleted || r.Type != "contact" || r.SourcePath != fixtureRemote || obj(t, r.Payload["deleted"])["source"] != "deleted_contacts" ||
			r.Summary == nil || !strings.HasPrefix(*r.Summary, "deleted contact ") {
			t.Errorf("tombstone %d: %+v", r.Rowid, r)
		}
	}
	if tomb != 2 {
		t.Errorf("%d tombstones, want 2", tomb)
	}
	h := parsertest.LoadFixture(t, "testdata", "huge-contact")
	if len(h.Expect.Records) != 1 {
		t.Fatalf("%d records in the huge contact", len(h.Expect.Records))
	}
	p := h.Expect.Records[0].Payload
	raw := obj(t, p["raw"])
	if len(list(t, p["phones"])) != 1000 || num(raw["phones_total"]) != "1200" || raw["phones_truncated"] != true ||
		len(list(t, p["emails"])) != 1000 || num(raw["emails_total"]) != "1100" || raw["emails_truncated"] != true ||
		len(list(t, raw["notes"])) != 100 || num(raw["notes_total"]) != "130" || raw["notes_truncated"] != true {
		t.Errorf("caps: phones %d emails %d raw %v", len(list(t, p["phones"])), len(list(t, p["emails"])), raw)
	}
}

func rawOf(t *testing.T, r parsertest.ExpRecord) map[string]any { return obj(t, r.Payload["raw"]) }

// T68, T69: the account comes only from accounts joined through raw_contacts.account_id, and the time is the
// AGGREGATE contact's (contacts.contact_last_updated_timestamp through contact_id), stated under a key that names
// it and with its source; Record.Time stays unset. An unresolvable join is unknown, never "no account" or a zero time.
func TestOracleStatesAccountsAndAggregateTimes(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	byRow := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		byRow[r.Rowid] = r
		if r.Table == "raw_contacts" && r.Time != nil {
			t.Errorf("row %d: the aggregate's time must not be Record.Time: %v", r.Rowid, *r.Time)
		}
	}
	acct := func(row int64) map[string]any {
		l := list(t, byRow[row].Payload["accounts"])
		if len(l) != 1 {
			t.Fatalf("row %d: accounts %v", row, l)
		}
		return obj(t, l[0])
	}
	if a := acct(1); a["type"] != "com.google" || a["name"] != "alice@example.test" {
		t.Errorf("contact 1 account %v", a)
	}
	if a := acct(7); a["type"] != "com.samsung.account" || a["name"] != "alice@samsung.test" {
		t.Errorf("contact 7 account %v", a)
	}
	// a NULL account_id: no account and nothing flagged
	if _, has := byRow[2].Payload["accounts"]; has || rawOf(t, byRow[2])["unknown_columns"] != nil {
		t.Errorf("contact 2 (NULL account_id): %v", byRow[2].Payload)
	}
	// a dangling account_id: a decided absence of the accounts row, stated; not "no account" (T72)
	r12 := rawOf(t, byRow[12])
	if _, has := byRow[12].Payload["accounts"]; has || r12["account_state"] != "no-accounts-row" || num(r12["account_id"]) != "99" || r12["unknown_columns"] != nil {
		t.Errorf("contact 12 (dangling account_id): %v", byRow[12].Payload)
	}
	for row, r := range byRow {
		if r.Table == "raw_contacts" && rawOf(t, r)["join_undecided"] != nil {
			t.Errorf("row %d: a fixture cannot force an undecidable join, yet join_undecided = %v", row, rawOf(t, r)["join_undecided"])
		}
	}
	// a valid aggregate time, with its source
	r1 := rawOf(t, byRow[1])
	if byRow[1].Times["aggregate_last_updated"] != "2023-05-01T10:00:01Z" || num(r1["contact_last_updated_timestamp"]) != "1682935201000" ||
		r1["contact_last_updated_source"] != "contacts.contact_last_updated_timestamp" || num(r1["contact_id"]) != "1" {
		t.Errorf("contact 1 time: %v %v", byRow[1].Times, r1)
	}
	// a zero and a year-2100 aggregate time: raw and status, no time
	for row, status := range map[int64]string{9: "sentinel", 10: "invalid"} {
		r := rawOf(t, byRow[row])
		if len(byRow[row].Times) != 0 || r["contact_last_updated_timestamp_status"] != status {
			t.Errorf("contact %d: times %v raw %v", row, byRow[row].Times, r)
		}
	}
	// a NULL contact_id: the time is unknown (never zero) and says why; a dangling one has no contacts row (T72)
	r11 := rawOf(t, byRow[11])
	if len(byRow[11].Times) != 0 || r11["aggregate_state"] != "no-contact-id" || r11["contact_last_updated_timestamp"] != nil || r11["unknown_columns"] != nil {
		t.Errorf("contact 11 (NULL contact_id): %v", byRow[11].Payload)
	}
	r13 := rawOf(t, byRow[13])
	if len(byRow[13].Times) != 0 || r13["aggregate_state"] != "no-contacts-row" || num(r13["contact_id"]) != "98" || r13["unknown_columns"] != nil {
		t.Errorf("contact 13 (dangling contact_id): %v", byRow[13].Payload)
	}
	// three raw contacts of one person: three distinct accounts, one aggregate time
	m := parsertest.LoadFixture(t, "testdata", "multi-account")
	want := map[int64]string{1: "com.google", 2: "com.samsung.account", 3: "com.whatsapp"}
	for _, r := range m.Expect.Records {
		l := list(t, r.Payload["accounts"])
		if len(l) != 1 || obj(t, l[0])["type"] != want[r.Rowid] || r.Times["aggregate_last_updated"] != "2023-05-01T10:00:01Z" {
			t.Errorf("multi-account row %d: %v %v", r.Rowid, r.Payload["accounts"], r.Times)
		}
	}
}

// T79: a database whose accounts and contacts tables use a plain `_id integer` column (not the rowid alias) can hold
// a key twice. The oracle states the join as undecided with the count, and no account and no time from it. The
// variant ships an empty -wal so that Probe, which refuses a table without the alias, defers to Parse.
func TestOracleStatesDuplicateJoinKeys(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "duplicate-keys")
	if _, has := fx.Files["wal"]; !has {
		t.Fatal("the variant must carry a -wal companion so the probe defers")
	}
	byRow := map[int64]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		byRow[r.Rowid] = r
	}
	r1 := rawOf(t, byRow[1])
	dup := obj(t, r1["join_duplicate"])
	und := list(t, r1["join_undecided"])
	if num(dup["accounts"]) != "2" || num(dup["contacts"]) != "3" || len(und) != 2 || und[0] != "accounts" || und[1] != "contacts" ||
		byRow[1].Payload["accounts"] != nil || len(byRow[1].Times) != 0 || r1["account_state"] != nil || r1["aggregate_state"] != nil {
		t.Errorf("contact 1 (duplicate keys): %v times %v", byRow[1].Payload, byRow[1].Times)
	}
	r2 := rawOf(t, byRow[2])
	if r2["join_duplicate"] != nil || r2["join_undecided"] != nil || len(list(t, byRow[2].Payload["accounts"])) != 1 || byRow[2].Times["aggregate_last_updated"] == "" {
		t.Errorf("contact 2 (unique keys) is decided: %v", byRow[2].Payload)
	}
	r3 := rawOf(t, byRow[3])
	if r3["account_state"] != "no-accounts-row" || r3["aggregate_state"] != "no-contacts-row" || r3["join_duplicate"] != nil {
		t.Errorf("contact 3 (dangling keys): %v", r3)
	}
}

// T81: the oracle pins the Summary of the contacts whose names hold bidirectional controls: the control is SHOWN as
// <U+XXXX>, never dropped and never kept raw.
func TestOraclePinsTheSummaryOfABidiName(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "unicode-rtl")
	var evil bool
	for _, r := range fx.Expect.Records {
		if r.Summary == nil {
			t.Errorf("row %d: the summary is not pinned", r.Rowid)
			continue
		}
		if strings.ContainsFunc(*r.Summary, isBidiControl) {
			t.Errorf("row %d: a bidi control kept raw in the pinned summary %q", r.Rowid, *r.Summary)
		}
		if strings.Contains(*r.Summary, "<U+202E>") && strings.Contains(*r.Summary, "<U+202C>") {
			evil = true
		}
	}
	if !evil {
		t.Error("no pinned summary shows <U+202E> and <U+202C>")
	}
}
