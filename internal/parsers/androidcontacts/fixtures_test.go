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

var variants = []string{"aosp-baseline", "deleted-contacts", "huge-contact", "multi-account", "unicode-rtl"}

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
	m, err := filepath.Glob("testdata/*.expect.json")
	if err != nil {
		t.Fatal(err)
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
	if len(fx.Expect.Records) != 10 {
		t.Fatalf("%d records, want 10 (contact 4 identifies nobody)", len(fx.Expect.Records))
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
