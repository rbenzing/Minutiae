package contact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

func ptr[T any](v T) *T { return &v }

// minimal is the smallest contact the contract accepts: a name alone.
func minimal() contact.Contact { return contact.Contact{DisplayName: "Alex"} }

// phoneOnly has no name: a number-only contact (the provider allows it).
func phoneOnly() contact.Contact {
	return contact.Contact{Phones: []contact.Value{{Value: "+15551234567"}}}
}

func full() contact.Contact {
	return contact.Contact{
		DisplayName: "Dr. Alex Q. Smith Jr.",
		Names:       &contact.Names{Given: "Alex", Family: "Smith", Middle: "Q.", Prefix: "Dr.", Suffix: "Jr.", Nickname: "Al", Phonetic: "Aleks"},
		Organization: &contact.Organization{
			Name: "Acme", Title: "Engineer", Department: "R&D",
		},
		Phones: []contact.Value{
			{Value: "+1 (555) 123-4567", Label: "mobile", Kind: "phone", Primary: ptr(true)},
			{Value: "555-0100", Primary: ptr(false)},
		},
		Emails: []contact.Value{{Value: "alex@example.com", Label: "home"}, {Value: "alex.work@example.org", Kind: "email"}},
		URLs:   []contact.Value{{Value: "https://example.com/alex"}},
		IMs:    []contact.Value{{Value: "alex_im", Label: "Signal"}},
		Addresses: []contact.Address{
			{Formatted: "1 Main St, Springfield", Street: "1 Main St", City: "Springfield", Region: "IL", Postcode: "62701", Country: "US", Label: "home"},
			{Street: "2 Oak Ave", City: "Portland", Region: "OR", Postcode: "97201", Country: "US"},
		},
		Accounts:        []contact.Account{{Type: "com.google", Name: "alex@gmail.com"}, {Type: "local"}},
		Birthday:        "1980-02-29",
		Starred:         ptr(false),
		PhotoPresent:    ptr(true),
		PhotoArtifactID: "art-2",
		SourceID:        "17",
		SourceIDs:       []string{"17", "204"},
		Deleted:         map[string]any{"source": "raw_contacts.deleted"},
		Recovery:        map[string]any{"relation": "absent-from-live", "via": "wal", "wal": map[string]any{"frame": json.Number("3"), "committed": true}},
		Snapshot:        map[string]any{"name": "com.apple.snap", "xid": json.Number("12")},
		Raw:             map[string]any{"x_times_contacted": json.Number("4"), "nested": map[string]any{"a": []any{"x", json.Number("2"), json.Number("1.5")}}},
	}
}

// TestPayloadValidatorsAcceptParserOutput (contact part): what the builder makes
// validates, and a real records.Writer over a temp case accepts it.
func TestPayloadValidatorsAcceptParserOutput(t *testing.T) {
	contacts := map[string]contact.Contact{
		"name only": minimal(), "phone only": phoneOnly(), "full": full(),
		"email only":        {Emails: []contact.Value{{Value: "a@example.com"}}},
		"organization only": {Organization: &contact.Organization{Title: "CTO"}},
	}
	for name, c := range contacts {
		t.Run(name, func(t *testing.T) {
			if err := contact.Validate(c.Payload()); err != nil {
				t.Fatalf("Validate(builder output) = %v", err)
			}
		})
	}

	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "contacts2.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "contact-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for name, c := range contacts {
		rec := records.Record{Type: contact.Type, ArtifactID: a.ID, Summary: contact.Summary(c), Body: contact.Body(c), Payload: c.Payload()}
		if err := w.Add(ctx, rec); err != nil {
			t.Errorf("the writer refused the %s contact: %v", name, err)
		}
	}
	// the same writer refuses a violation with the typed error
	bad := records.Record{Type: contact.Type, ArtifactID: a.ID, Summary: "bad", Payload: map[string]any{"display_name": ""}}
	if err := w.Add(ctx, bad); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(invalid contact) = %v, want ErrInvalidPayload", err)
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(contacts)) || res.Rejected != 1 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

func TestContactRegisteredValidator(t *testing.T) {
	for _, ti := range records.Types() {
		if ti.Name == contact.Type {
			if !ti.HasValidator {
				t.Error("contact has no validator although its package is imported")
			}
			if ti.PayloadVersion != contact.PayloadVersion {
				t.Errorf("registered payload version %d, package says %d", ti.PayloadVersion, contact.PayloadVersion)
			}
			return
		}
	}
	t.Error("contact is not a registered type")
}

func mutate(fn func(p map[string]any)) map[string]any {
	p := full().Payload()
	fn(p)
	return p
}

// objOf returns the object under key in a payload (an empty one, after failing the test, when there is none).
func objOf(t *testing.T, p map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := p[key].(map[string]any)
	if !ok {
		t.Errorf("the payload has no %s object: %v", key, p)
		return map[string]any{}
	}
	return o
}

// elem returns element i of the list under key (the full payload's lists).
func elem(p map[string]any, key string, i int) map[string]any {
	list, _ := p[key].([]any)
	if i >= len(list) {
		return map[string]any{}
	}
	e, _ := list[i].(map[string]any)
	if e == nil {
		return map[string]any{}
	}
	return e
}

func makeValues(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"value": "x"}
	}
	return out
}

// TestPayloadValidatorsRejectViolations (contact part): one row per F10 rule.
// Every error names the path (and the reason), and none repeats a value.
func TestPayloadValidatorsRejectViolations(t *testing.T) {
	const marker = "SECRET-VALUE-4417"
	type row struct {
		name string
		p    map[string]any
		want string
	}
	var rows []row
	add := func(name string, p map[string]any, want string) { rows = append(rows, row{name, p, want}) }

	// the identifying-field rule
	const identifying = "at least one of display_name,phones,emails,organization must be present and not empty"
	add("nil payload", nil, identifying)
	add("empty payload", map[string]any{}, identifying)
	// full() carries the deleted marker and source ids, which would make it a valid tombstone: drop the marker too
	add("nothing identifying left", mutate(func(p map[string]any) {
		for _, k := range []string{"display_name", "phones", "emails", "organization", "deleted"} {
			delete(p, k)
		}
	}), identifying)

	// wrong types: scalars and objects
	wrong := map[string]any{
		"display_name": int64(1), "names": "x", "organization": []any{}, "phones": "x", "emails": map[string]any{}, "urls": int64(1), "ims": true,
		"addresses": "x", "birthday": int64(19800229), "accounts": "x", "starred": "yes", "photo": true, "source_id": int64(17), "source_ids": "x",
		"deleted": "x", "raw": "x", "recovery": "x", "snapshot": "x",
	}
	names := make([]string, 0, len(wrong))
	for k := range wrong {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, f := range names {
		add("wrong type for "+f, mutate(func(p map[string]any) { p[f] = wrong[f] }), "payload."+f)
	}
	for _, f := range []string{"given", "family", "middle", "prefix", "suffix", "nickname", "phonetic"} {
		add("names."+f+" wrong type", mutate(func(p map[string]any) { objOf(t, p, "names")[f] = int64(1) }), "payload.names."+f)
	}
	for _, f := range []string{"name", "title", "department"} {
		add("organization."+f+" wrong type", mutate(func(p map[string]any) { objOf(t, p, "organization")[f] = int64(1) }), "payload.organization."+f)
	}
	add("photo.present wrong type", mutate(func(p map[string]any) { objOf(t, p, "photo")["present"] = "yes" }), "payload.photo.present")
	add("photo.artifact_id wrong type", mutate(func(p map[string]any) { objOf(t, p, "photo")["artifact_id"] = int64(1) }), "payload.photo.artifact_id")
	add("photo.artifact_id empty", mutate(func(p map[string]any) { objOf(t, p, "photo")["artifact_id"] = "" }), "payload.photo.artifact_id")
	add("source_ids entry wrong type", mutate(func(p map[string]any) { p["source_ids"] = []any{"17", int64(2)} }), "payload.source_ids")

	// value lists
	for _, list := range []string{"phones", "emails", "urls", "ims"} {
		add(list+" element not an object", mutate(func(p map[string]any) { p[list] = []any{"x"} }), "payload."+list+"[0]")
		add(list+" element without value", mutate(func(p map[string]any) { delete(elem(p, list, 0), "value") }), "payload."+list+"[0].value: required field is missing")
		add(list+" element with empty value", mutate(func(p map[string]any) { elem(p, list, 0)["value"] = "" }), "payload."+list+"[0].value: must not be empty")
		add(list+" element value wrong type", mutate(func(p map[string]any) { elem(p, list, 0)["value"] = int64(5551234) }), "payload."+list+"[0].value")
		add(list+" element label wrong type", mutate(func(p map[string]any) { elem(p, list, 0)["label"] = int64(1) }), "payload."+list+"[0].label")
		add(list+" element kind wrong type", mutate(func(p map[string]any) { elem(p, list, 0)["kind"] = []any{} }), "payload."+list+"[0].kind")
		add(list+" element primary wrong type", mutate(func(p map[string]any) { elem(p, list, 0)["primary"] = "true" }), "payload."+list+"[0].primary")
		add(list+" array of 10,001", mutate(func(p map[string]any) { p[list] = makeValues(10001) }), "payload."+list)
	}
	add("second phone broken", mutate(func(p map[string]any) { delete(elem(p, "phones", 1), "value") }), "payload.phones[1].value")

	// addresses and accounts
	add("address not an object", mutate(func(p map[string]any) { p["addresses"] = []any{"x"} }), "payload.addresses[0]")
	for _, f := range []string{"formatted", "street", "city", "region", "postcode", "country", "label"} {
		add("address."+f+" wrong type", mutate(func(p map[string]any) { elem(p, "addresses", 1)[f] = int64(1) }), "payload.addresses[1]."+f)
	}
	add("addresses array of 10,001", mutate(func(p map[string]any) { p["addresses"] = make([]any, 10001) }), "payload.addresses")
	add("account not an object", mutate(func(p map[string]any) { p["accounts"] = []any{int64(1)} }), "payload.accounts[0]")
	add("account.type wrong type", mutate(func(p map[string]any) { elem(p, "accounts", 0)["type"] = int64(1) }), "payload.accounts[0].type")
	add("account.name wrong type", mutate(func(p map[string]any) { elem(p, "accounts", 0)["name"] = []any{} }), "payload.accounts[0].name")
	add("accounts array of 10,001", mutate(func(p map[string]any) { p["accounts"] = make([]any, 10001) }), "payload.accounts")
	add("source_ids array of 10,001", mutate(func(p map[string]any) { p["source_ids"] = make([]any, 10001) }), "payload.source_ids")

	// birthday (the full forms are in TestContactBirthdayForms)
	for _, bad := range []string{"", "1980-02-30", "1981-02-29", "--13-01", "19800229", marker} {
		add(fmt.Sprintf("birthday %q", bad), mutate(func(p map[string]any) { p["birthday"] = bad }), "payload.birthday: not a date")
	}

	// provenance objects
	add("deleted without source", mutate(func(p map[string]any) { p["deleted"] = map[string]any{} }), "payload.deleted.source")
	add("deleted with empty source", mutate(func(p map[string]any) { p["deleted"] = map[string]any{"source": ""} }), "payload.deleted.source")
	add("recovery relation", mutate(func(p map[string]any) { p["recovery"] = map[string]any{"relation": "found"} }), "payload.recovery.relation")
	add("deleted source of the wrong type", mutate(func(p map[string]any) { p["deleted"] = map[string]any{"source": int64(1)} }), "payload.deleted.source")
	add("recovery free key of an unsupported type", mutate(func(p map[string]any) {
		p["recovery"] = map[string]any{"relation": "absent-from-live", "x": struct{}{}}
	}), "payload.recovery: another key holds an unsupported value")
	add("snapshot without name", mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"xid": int64(1)} }), "payload.snapshot.name")
	add("snapshot negative xid", mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "s", "xid": int64(-1)} }), "payload.snapshot.xid")
	for _, in := range []string{"1e3", "2.0", "1E3"} {
		add("snapshot xid as "+in, mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "s", "xid": json.Number(in)} }), "payload.snapshot.xid")
	}

	// depth and size of raw
	add("raw nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{}}}}
	}), "payload.raw")
	add("raw array nested to depth 5", mutate(func(p map[string]any) {
		p["raw"] = map[string]any{"a": []any{[]any{[]any{int64(1)}}}}
	}), "payload.raw")
	add("raw array of 10,001", mutate(func(p map[string]any) { p["raw"] = map[string]any{"x": make([]any, 10001)} }), "payload.raw")
	add("a value in an error is never echoed", mutate(func(p map[string]any) { elem(p, "phones", 0)["primary"] = marker }), "payload.phones[0].primary")

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := contact.Validate(r.p)
			if err == nil {
				t.Fatalf("an invalid payload was accepted")
			}
			if !strings.Contains(err.Error(), r.want) {
				t.Errorf("error %q does not contain %q", err, r.want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error repeats a payload value: %q", err)
			}
		})
	}

	// the boundaries that are accepted
	ok := map[string]map[string]any{
		"raw at depth 4": mutate(func(p map[string]any) {
			p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": int64(1)}}}
		}),
		"10,000 phones":     mutate(func(p map[string]any) { p["phones"] = makeValues(10000) }),
		"10,000 source ids": mutate(func(p map[string]any) { p["source_ids"] = makeStrings(10000) }),
		"unknown fields": mutate(func(p map[string]any) {
			p["future_field"] = map[string]any{"x": int64(1)}
			elem(p, "phones", 0)["future"] = true
		}),
		"value of any text":      mutate(func(p map[string]any) { elem(p, "phones", 0)["value"] = "not a number at all" }),
		"primary false":          mutate(func(p map[string]any) { elem(p, "phones", 0)["primary"] = false }),
		"json.Number xid":        mutate(func(p map[string]any) { p["snapshot"] = map[string]any{"name": "s", "xid": json.Number("7")} }),
		"photo without artifact": mutate(func(p map[string]any) { p["photo"] = map[string]any{"present": false} }),
		"empty lists besides a name": mutate(func(p map[string]any) {
			p["phones"], p["emails"], p["urls"], p["ims"], p["addresses"], p["accounts"] = []any{}, []any{}, []any{}, []any{}, []any{}, []any{}
		}),
		"empty address and account objects": mutate(func(p map[string]any) {
			p["addresses"] = []any{map[string]any{}}
			p["accounts"] = []any{map[string]any{}}
		}),
		"recovery": mutate(func(p map[string]any) { p["recovery"] = map[string]any{"relation": "uncommitted", "notes": "x"} }),
	}
	for name, p := range ok {
		if err := contact.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestContactAtLeastOneIdentifyingField: a contact must say who it is by a name, a
// number, an e-mail address or an organization; an empty field does not count.
func TestContactAtLeastOneIdentifyingField(t *testing.T) {
	v := func(s string) []any { return []any{map[string]any{"value": s}} }
	refused := map[string]map[string]any{
		"empty display_name alone":              {"display_name": ""},
		"no fields":                             {},
		"only a birthday":                       {"birthday": "1980-01-01"},
		"only a source id":                      {"source_id": "17", "source_ids": []any{"17"}},
		"only an account":                       {"accounts": []any{map[string]any{"type": "local"}}},
		"only an url":                           {"urls": v("https://example.com")},
		"only an im":                            {"ims": v("alex")},
		"only an address":                       {"addresses": []any{map[string]any{"city": "Springfield"}}},
		"empty display_name and empty lists":    {"display_name": "", "phones": []any{}, "emails": []any{}},
		"empty organization object":             {"organization": map[string]any{}},
		"organization of empty strings":         {"organization": map[string]any{"name": "", "title": "", "department": ""}},
		"names only (given name, no display)":   {"names": map[string]any{"given": "Alex"}},
		"source ids without the deleted marker": {"source_ids": []any{"9"}},
		"only photo and starred":                {"photo": map[string]any{"present": true}, "starred": true},
	}
	for name, p := range refused {
		err := contact.Validate(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "at least one of display_name,phones,emails,organization") {
			t.Errorf("%s: error %q does not name the rule", name, err)
		}
	}
	accepted := map[string]map[string]any{
		"display_name alone":       {"display_name": "Alex"},
		"a phone alone":            {"phones": v("+15551234567")},
		"an e-mail alone":          {"emails": v("a@example.com")},
		"an organization name":     {"organization": map[string]any{"name": "Acme"}},
		"an organization title":    {"organization": map[string]any{"title": "CTO"}},
		"empty name, with a phone": {"display_name": "", "phones": v("555-0100")},
		"a phone with all optional fields": {
			"phones": []any{map[string]any{"value": "555", "label": "x", "kind": "y", "primary": true}},
			"urls":   v("https://example.com"),
		},
	}
	for name, p := range accepted {
		if err := contact.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the builder cannot make a contact that holds nothing identifying
	for name, c := range map[string]contact.Contact{
		"zero":               {},
		"empty organization": {Organization: &contact.Organization{}},
		"empty names":        {Names: &contact.Names{}},
		"only a birthday":    {Birthday: "1980-01-01"},
	} {
		if err := contact.Validate(c.Payload()); err == nil {
			t.Errorf("builder %s: validated", name)
		}
	}
	if err := contact.Validate(phoneOnly().Payload()); err != nil {
		t.Errorf("builder phone only: %v", err)
	}
}

// TestContactDeletedTombstone: spec 9.3 turns a row of the deleted_contacts table
// into a minimal contact that carries the deleted marker and source_ids only. Such
// a tombstone is valid; its identifying fields are optional. A deleted marker
// without source ids identifies nothing and is still refused.
func TestContactDeletedTombstone(t *testing.T) {
	del := func() any { return map[string]any{"source": "deleted_contacts"} }
	accepted := map[string]map[string]any{
		"tombstone: deleted and one source id":   {"deleted": del(), "source_ids": []any{"9"}},
		"tombstone: several source ids":          {"deleted": del(), "source_ids": []any{"9", "10"}},
		"tombstone: one empty id, one real":      {"deleted": del(), "source_ids": []any{"", "9"}},
		"tombstone with a recovery relation":     {"deleted": del(), "source_ids": []any{"9"}, "recovery": map[string]any{"relation": "absent-from-live"}},
		"deleted contact that still has a name":  {"deleted": del(), "display_name": "Alex"},
		"deleted contact with a name and an id":  {"deleted": del(), "display_name": "Alex", "source_ids": []any{"9"}},
		"tombstone with raw and a source_id too": {"deleted": del(), "source_ids": []any{"9"}, "source_id": "9", "raw": map[string]any{"deleted_timestamp": int64(1700000000000)}},
	}
	for name, p := range accepted {
		if err := contact.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	refused := map[string]map[string]any{
		"deleted without source ids":         {"deleted": del()},
		"deleted with an empty id list":      {"deleted": del(), "source_ids": []any{}},
		"deleted with only an empty id":      {"deleted": del(), "source_ids": []any{""}},
		"deleted with only a source_id":      {"deleted": del(), "source_id": "9"},
		"deleted with only a birthday":       {"deleted": del(), "birthday": "1980-01-01"},
		"source ids without deleted":         {"source_ids": []any{"9"}},
		"deleted marker that is not valid":   {"deleted": map[string]any{"source": ""}, "source_ids": []any{"9"}},
		"deleted marker of the wrong type":   {"deleted": true, "source_ids": []any{"9"}},
		"deleted marker that is a string":    {"deleted": "deleted_contacts", "source_ids": []any{"9"}},
		"deleted marker that is null":        {"deleted": nil, "source_ids": []any{"9"}},
		"deleted without source, with an id": {"deleted": map[string]any{}, "source_ids": []any{"9"}},
	}
	for name, p := range refused {
		if err := contact.Validate(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// the builder makes the tombstone, a real writer accepts it, Decode reads it back
	c := contact.Contact{Deleted: map[string]any{"source": "deleted_contacts"}, SourceIDs: []string{"9"}}
	if err := contact.Validate(c.Payload()); err != nil {
		t.Fatalf("Validate(builder tombstone) = %v", err)
	}
	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "contacts2.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "contact-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(ctx, records.Record{Type: contact.Type, ArtifactID: a.ID, Summary: contact.Summary(c), Body: contact.Body(c), Deleted: true, Payload: c.Payload()}); err != nil {
		t.Errorf("the writer refused a contact tombstone: %v", err)
	}
	// deleted without source ids is refused by the real writer, with the typed error
	bad := contact.Contact{Deleted: map[string]any{"source": "deleted_contacts"}}
	if err := w.Add(ctx, records.Record{Type: contact.Type, ArtifactID: a.ID, Summary: "bad", Deleted: true, Payload: bad.Payload()}); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(deleted without source ids) = %v, want ErrInvalidPayload", err)
	}
	if res, err := w.End(ctx); err != nil || res.Records != 1 || res.Rejected != 1 {
		t.Fatalf("End = %+v, %v", res, err)
	}
	raw, err := json.Marshal(c.Payload())
	if err != nil {
		t.Fatal(err)
	}
	got, err := contact.Decode(contact.PayloadVersion, raw)
	if err != nil {
		t.Fatalf("Decode(tombstone) = %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("Decode(tombstone) = %+v, want %+v", got, c)
	}
}

// TestContactBirthdayForms: a date, not an instant: YYYY-MM-DD (a real day) or
// --MM-DD (no year, February 29 allowed).
func TestContactBirthdayForms(t *testing.T) {
	with := func(v any) error {
		p := minimal().Payload()
		p["birthday"] = v
		return contact.Validate(p)
	}
	for _, good := range []string{
		"1980-02-29", "2000-02-29", "2024-02-29", "1999-12-31", "2024-01-15", "0001-01-01",
		"--02-29", "--01-01", "--12-31", "--04-30",
	} {
		if err := with(good); err != nil {
			t.Errorf("birthday %q refused: %v", good, err)
		}
	}
	// built from numbers so this file holds no look-alike digits: Arabic-Indic and fullwidth digits
	arabic := string([]rune{0x0661, 0x0669, 0x0668, 0x0660}) + "-02-01"
	fullwidth := string([]rune{0xFF11, 0xFF19, 0xFF18, 0xFF10}) + "-02-01"
	for _, bad := range []string{
		"", " ", "1980", "1980-02", "1980-2-1", "80-02-01", "19800201", "1980/02/01", "1980.02.01", "1980-02-01 ", " 1980-02-01",
		"1980-02-30", "1981-02-29", "1900-02-29", "2100-02-29", "1980-00-10", "1980-13-10", "1980-04-31", "1980-01-00", "1980-01-32",
		"--13-01", "--00-10", "--02-30", "--04-31", "--2-29", "--02-9", "-02-29", "---02-29", "--02/29", "--0229",
		"1980-02-29T00:00:00Z", "1980-02-29T00:00:00", "1980-W05-1", "+1980-02-01", "-1980-02-01", "1980-0a-01", arabic, fullwidth, "1980-02-01\n",
	} {
		if err := with(bad); err == nil {
			t.Errorf("birthday %q accepted", bad)
		}
	}
	for name, v := range map[string]any{"int": int64(19800201), "float": 1980.0201, "nil": nil, "bool": true, "list": []any{"1980-02-01"}, "json.Number": json.Number("1980")} {
		if err := with(v); err == nil {
			t.Errorf("birthday of type %s accepted", name)
		}
	}
	// the builder carries the string as it is and never invents a date
	c := minimal()
	if _, has := c.Payload()["birthday"]; has {
		t.Error("an absent birthday was written")
	}
	c.Birthday = "--02-29"
	if got := c.Payload()["birthday"]; got != "--02-29" {
		t.Errorf("birthday = %v", got)
	}
	// Decode reads both forms back
	for _, in := range []string{"1980-02-29", "--02-29"} {
		got, err := contact.Decode(1, []byte(`{"display_name":"A","birthday":"`+in+`"}`))
		if err != nil || got.Birthday != in {
			t.Errorf("Decode(birthday %s) = %q, %v", in, got.Birthday, err)
		}
	}
	if _, err := contact.Decode(1, []byte(`{"display_name":"A","birthday":"1981-02-29"}`)); err == nil {
		t.Error("Decode accepted a birthday that is not a date")
	}
}

// TestContactBodyListsEveryNumber: the body is what the substring index searches,
// so it holds the display name and then one line for every phone, e-mail, address
// and organization, values exactly as stored, never a cut list.
func TestContactBodyListsEveryNumber(t *testing.T) {
	want := strings.Join([]string{
		"Dr. Alex Q. Smith Jr.",
		"+1 (555) 123-4567",
		"555-0100",
		"alex@example.com",
		"alex.work@example.org",
		"1 Main St, Springfield",
		"2 Oak Ave, Portland, OR, 97201, US",
		"Acme, Engineer, R&D",
	}, "\n")
	if got := contact.Body(full()); got != want {
		t.Errorf("Body(full) =\n%s\nwant\n%s", got, want)
	}
	// a partial number is found by a substring search
	body := contact.Body(full())
	for _, part := range []string{"555-0100", "123-4567", "(555)", "0100", "alex.work@"} {
		if !strings.Contains(body, part) {
			t.Errorf("a partial %q is not in the body", part)
		}
	}

	// every number, however many: nothing is dropped after the first
	c := contact.Contact{DisplayName: "Many"}
	var nums []string
	for i := range 60 {
		n := fmt.Sprintf("+1555000%04d", i)
		nums = append(nums, n)
		c.Phones = append(c.Phones, contact.Value{Value: n})
	}
	lines := strings.Split(contact.Body(c), "\n")
	if len(lines) != 61 || lines[0] != "Many" || !slices.Equal(lines[1:], nums) {
		t.Errorf("Body of 60 phones has %d lines; first %q last %q", len(lines), lines[0], lines[len(lines)-1])
	}

	// the shapes: no name, name only, empty address, order of the groups
	cases := []struct {
		name string
		c    contact.Contact
		want string
	}{
		{"no name", contact.Contact{Phones: []contact.Value{{Value: "111"}}, Emails: []contact.Value{{Value: "a@b.co"}}}, "111\na@b.co"},
		{"name only", contact.Contact{DisplayName: "Solo"}, "Solo"},
		{"nothing", contact.Contact{}, ""},
		{"organization name only", contact.Contact{Organization: &contact.Organization{Name: "Acme"}}, "Acme"},
		{"organization title only", contact.Contact{DisplayName: "A", Organization: &contact.Organization{Title: "CTO"}}, "A\nCTO"},
		{"formatted address wins", contact.Contact{DisplayName: "A", Addresses: []contact.Address{{Formatted: "F", Street: "S", City: "C"}}}, "A\nF"},
		{"parts joined", contact.Contact{Addresses: []contact.Address{{Street: "S", Postcode: "P", Country: "K"}}}, "S, P, K"},
		{"label alone is not a line", contact.Contact{DisplayName: "A", Addresses: []contact.Address{{Label: "home"}}}, "A"},
		{"urls and ims are not listed", contact.Contact{DisplayName: "A", URLs: []contact.Value{{Value: "https://x"}}, IMs: []contact.Value{{Value: "im"}}}, "A"},
		{"groups in order", contact.Contact{
			Organization: &contact.Organization{Name: "O"}, Addresses: []contact.Address{{City: "Ci"}},
			Emails: []contact.Value{{Value: "e"}}, Phones: []contact.Value{{Value: "p"}}, DisplayName: "N",
		}, "N\np\ne\nCi\nO"},
		{"empty values are skipped", contact.Contact{DisplayName: "A", Phones: []contact.Value{{Value: ""}, {Value: "2"}}}, "A\n2"},
	}
	for _, tc := range cases {
		if got := contact.Body(tc.c); got != tc.want {
			t.Errorf("%s: Body = %q, want %q", tc.name, got, tc.want)
		}
	}

	// stored text is unchanged: format characters, bidi controls and spaces stay as they are
	rlo, zwsp := string(rune(0x202E)), string(rune(0x200B))
	odd := contact.Contact{DisplayName: " Ba" + zwsp + "nk ", Phones: []contact.Value{{Value: rlo + "+1 555  0100"}}}
	if got := contact.Body(odd); got != " Ba"+zwsp+"nk \n"+rlo+"+1 555  0100" {
		t.Errorf("Body changed stored text: %q", got)
	}
}

// TestContactBuilderOmitsAbsent (C3): an empty string, an empty object or list and
// a nil pointer never appear; a set pointer does, even when it holds false.
func TestContactBuilderOmitsAbsent(t *testing.T) {
	if got, want := minimal().Payload(), map[string]any{"display_name": "Alex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("minimal payload = %#v, want %#v", got, want)
	}
	// empty sub-objects and elements that hold nothing are not written
	c := contact.Contact{
		DisplayName:  "A",
		Names:        &contact.Names{},
		Organization: &contact.Organization{},
		Addresses:    []contact.Address{{}},
		Accounts:     []contact.Account{{}},
		Phones:       []contact.Value{{}},
		SourceIDs:    []string{},
	}
	if got, want := c.Payload(), (map[string]any{"display_name": "A"}); !reflect.DeepEqual(got, want) {
		t.Errorf("payload with empty parts = %#v, want %#v", got, want)
	}
	// only the fields that are set are written
	c = contact.Contact{
		DisplayName:  "A",
		Names:        &contact.Names{Given: "G"},
		Organization: &contact.Organization{Department: "D"},
		Phones:       []contact.Value{{Value: "1", Primary: ptr(false)}},
		Addresses:    []contact.Address{{City: "C"}},
		Accounts:     []contact.Account{{Type: "t"}},
		Starred:      ptr(false),
		PhotoPresent: ptr(false),
	}
	want := map[string]any{
		"display_name": "A", "names": map[string]any{"given": "G"}, "organization": map[string]any{"department": "D"},
		"phones":    []any{map[string]any{"value": "1", "primary": false}},
		"addresses": []any{map[string]any{"city": "C"}}, "accounts": []any{map[string]any{"type": "t"}},
		"starred": false, "photo": map[string]any{"present": false},
	}
	if got := c.Payload(); !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v\nwant %#v", got, want)
	}
	// a photo artifact alone is a photo object; a label alone is a (refused) value, not silently dropped
	c = minimal()
	c.PhotoArtifactID = "art-1"
	if got := c.Payload()["photo"]; !reflect.DeepEqual(got, map[string]any{"artifact_id": "art-1"}) {
		t.Errorf("photo = %#v", got)
	}
	c = minimal()
	c.Phones = []contact.Value{{Label: "mobile"}}
	if err := contact.Validate(c.Payload()); err == nil {
		t.Error("a phone with no number validated")
	}

	// the full payload holds every field once, and nothing empty anywhere
	fp := full().Payload()
	if len(fp) != 18 {
		t.Errorf("full payload has %d keys, want 18: %v", len(fp), fp)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if x == "" {
				t.Errorf("%s is an empty string", path)
			}
		case map[string]any:
			if len(x) == 0 {
				t.Errorf("%s is an empty object", path)
			}
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			if len(x) == 0 {
				t.Errorf("%s is an empty list", path)
			}
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("payload", fp)
}

// TestContactPayloadIsCanonicalTypes: only the types the records package
// canonicalizes, and the payload does not alias the Contact.
func TestContactPayloadIsCanonicalTypes(t *testing.T) {
	for name, c := range map[string]contact.Contact{"minimal": minimal(), "phone": phoneOnly(), "full": full()} {
		if !canonical(c.Payload()) {
			t.Errorf("%s payload holds a type outside string, bool, int64, float64, json.Number, []any, map[string]any", name)
		}
	}
	c := full()
	p := c.Payload()
	objOf(t, p, "raw")["x_times_contacted"] = "changed"
	if nested, ok := objOf(t, p, "raw")["nested"].(map[string]any); ok {
		if arr, ok := nested["a"].([]any); ok && len(arr) > 0 {
			arr[0] = "changed"
		}
	}
	objOf(t, p, "recovery")["via"] = "changed"
	objOf(t, p, "snapshot")["name"] = "changed"
	objOf(t, p, "deleted")["source"] = "changed"
	elem(p, "phones", 0)["value"] = "changed"
	if ids, ok := p["source_ids"].([]any); ok && len(ids) > 0 {
		ids[0] = "changed"
	}
	objOf(t, p, "names")["given"] = "changed"
	if c.Raw["x_times_contacted"] != json.Number("4") || c.Raw["nested"].(map[string]any)["a"].([]any)[0] != "x" ||
		c.Recovery["via"] != "wal" || c.Snapshot["name"] != "com.apple.snap" || c.Deleted["source"] != "raw_contacts.deleted" {
		t.Error("the payload aliases the contact's raw, recovery, snapshot or deleted map")
	}
	if c.Phones[0].Value != "+1 (555) 123-4567" || c.SourceIDs[0] != "17" || c.Names.Given != "Alex" {
		t.Error("the payload aliases the contact's lists or sub-objects")
	}
	// the primary flag is copied, not pointed to
	c = phoneOnly()
	c.Phones[0].Primary = ptr(true)
	p = c.Payload()
	*c.Phones[0].Primary = false
	if elem(p, "phones", 0)["primary"] != true {
		t.Error("primary followed a later change of the pointer")
	}
	// two payloads share nothing
	a, b := full().Payload(), full().Payload()
	objOf(t, a, "raw")["x_times_contacted"] = "changed"
	if objOf(t, b, "raw")["x_times_contacted"] == "changed" {
		t.Error("payloads share state")
	}
}

func canonical(v any) bool {
	switch x := v.(type) {
	case string, bool, int64, float64, json.Number:
		return true
	case []any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range x {
			if !canonical(e) {
				return false
			}
		}
		return true
	}
	return false
}

func TestSummary(t *testing.T) {
	cases := []struct {
		name string
		c    contact.Contact
		want string
	}{
		{"name and first phone", full(), "Dr. Alex Q. Smith Jr. +1 (555) 123-4567"},
		{"name only", minimal(), "Alex"},
		{"phone only", phoneOnly(), "+15551234567"},
		{"no phone: first e-mail", contact.Contact{Emails: []contact.Value{{Value: "a@example.com"}, {Value: "b@example.com"}}}, "a@example.com"},
		{"no phone: name and e-mail", contact.Contact{DisplayName: "A", Emails: []contact.Value{{Value: "a@example.com"}}}, "A"},
		{"organization name only", contact.Contact{Organization: &contact.Organization{Name: "Acme", Title: "CTO"}}, "Acme"},
		{"organization without name", contact.Contact{Organization: &contact.Organization{Title: "CTO"}}, "CTO"},
		{"skips an empty first phone", contact.Contact{DisplayName: "A", Phones: []contact.Value{{Value: ""}, {Value: "2"}}}, "A 2"},
		{"collapses whitespace", contact.Contact{DisplayName: "A\nB\t C", Phones: []contact.Value{{Value: "1 \n 2"}}}, "A B C 1 2"},
		{"nothing", contact.Contact{}, ""},
		{"tombstone shows its first source id", contact.Contact{Deleted: map[string]any{"source": "deleted_contacts"}, SourceIDs: []string{"", "9", "10"}}, "deleted contact 9"},
		{"a named deleted contact keeps its name", contact.Contact{DisplayName: "Alex", Deleted: map[string]any{"source": "x"}, SourceIDs: []string{"9"}}, "Alex"},
		{"source ids without a deleted marker show nothing", contact.Contact{SourceIDs: []string{"9"}}, ""},
	}
	for _, tc := range cases {
		if got := contact.Summary(tc.c); got != tc.want {
			t.Errorf("%s: Summary = %q, want %q", tc.name, got, tc.want)
		}
	}
	// the name is cut at 80 CHARACTERS, the phone at 32 bytes; the whole stays under the 512-byte cap
	cjk := strings.Repeat(string(rune(0x65E5))+string(rune(0x672C)), 100)
	long := contact.Summary(contact.Contact{DisplayName: cjk, Phones: []contact.Value{{Value: strings.Repeat("9", 100)}}})
	name, phone, _ := strings.Cut(long, " "+strings.Repeat("9", 29)+"...")
	if utf8.RuneCountInString(name) != 80 || !strings.HasSuffix(name, "...") || phone != "" {
		t.Errorf("long summary = %q (name %d characters)", long, utf8.RuneCountInString(name))
	}
	// hostile parts stay one line, show invisible and bidi characters, and are bounded
	rlo, zwsp := string(rune(0x202E)), string(rune(0x200B))
	emoji := strings.Repeat(string(rune(0x1F600)), 1000)
	h := contact.Summary(contact.Contact{DisplayName: rlo + "evil\n" + emoji, Phones: []contact.Value{{Value: strings.Repeat("9", 1000) + "\x00"}}})
	if strings.ContainsAny(h, "\n\x00"+rlo) || len(h) > 512 {
		t.Errorf("hostile summary = %q (%d bytes)", h, len(h))
	}
	if got := contact.Summary(contact.Contact{DisplayName: rlo + "Ba" + zwsp + "nk"}); got != "<U+202E>Ba<U+200B>nk" {
		t.Errorf("format characters are not visible in the summary: %q", got)
	}
	if contact.Summary(contact.Contact{DisplayName: "Bank"}) == contact.Summary(contact.Contact{DisplayName: "Ba" + zwsp + "nk"}) {
		t.Error("two different names give the same summary")
	}
}

// FuzzContactValidate: arbitrary JSON into Validate never panics, and an accepted
// payload always decodes (and decodes to something that validates again, with a
// body and a summary that stay bounded).
func FuzzContactValidate(f *testing.F) {
	for _, c := range []contact.Contact{minimal(), phoneOnly(), full()} {
		b, err := json.Marshal(c.Payload())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"display_name":""}`))
	f.Add([]byte(`{"organization":{"name":""}}`))
	f.Add([]byte(`{"display_name":"A","birthday":"--02-29"}`))
	f.Add([]byte(`{"display_name":"A","birthday":"1981-02-29"}`))
	f.Add([]byte(`{"phones":[{"value":"1","primary":true},{"value":"2"}],"emails":[],"raw":{"a":{"b":{"c":{}}}}}`))
	f.Add([]byte(`{"display_name":"A","snapshot":{"name":"s","xid":2.0},"recovery":{"relation":"uncommitted"},"deleted":{"source":"x"}}`))
	f.Add([]byte(`{"display_name":"A","snapshot":{"name":"s","xid":1e3}}`))
	f.Add([]byte(`{"display_name":"A","recovery":{},"deleted":{"source":"x"},"snapshot":{"name":"s","xid":2}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return
		}
		if err := contact.Validate(m); err != nil {
			if len(err.Error()) > 1<<12 {
				t.Errorf("error too long: %d bytes", len(err.Error()))
			}
			return
		}
		got, err := contact.Decode(contact.PayloadVersion, data)
		if err != nil {
			// only trailing data after the first value can make a validated object fail to decode
			if len(strings.TrimSpace(string(data[dec.InputOffset():]))) == 0 {
				t.Fatalf("an accepted payload does not decode: %v", err)
			}
			return
		}
		again, err := contact.Decode(contact.PayloadVersion, data)
		if err != nil || !reflect.DeepEqual(got, again) {
			t.Fatalf("Decode is not deterministic: %v", err)
		}
		// provenance survives Decode: a record whose payload carries recovery, snapshot or deleted (even empty) is never live
		prov := false
		for _, k := range []string{"recovery", "snapshot", "deleted"} {
			if _, ok := m[k].(map[string]any); ok {
				prov = true
			}
		}
		if got.Live() == prov {
			t.Fatalf("Live() = %v for a payload whose provenance presence is %v", got.Live(), prov)
		}
		if err := contact.Validate(got.Payload()); err != nil {
			t.Fatalf("the decoded contact does not produce a valid payload: %v", err)
		}
		if s := contact.Summary(got); len(s) > 512 || strings.ContainsAny(s, "\n\r\x00") || !utf8.ValidString(s) {
			t.Fatalf("Summary %q breaks its contract", s)
		}
		if b := contact.Body(got); !utf8.ValidString(b) && utf8.ValidString(string(data)) {
			t.Fatalf("Body %q is not valid UTF-8 although the payload was", b)
		}
	})
}

// v1Fixture is a stored v1 payload as the writer canonicalizes it, written out by
// hand so it does not move when the builder changes. It carries fields a future
// version might add (future_field, one inside a phone).
const v1Fixture = `{
  "accounts": [{"name": "alex@gmail.com", "type": "com.google"}],
  "addresses": [{"city": "Springfield", "formatted": "1 Main St, Springfield", "label": "home", "street": "1 Main St"}],
  "birthday": "--02-29",
  "deleted": {"source": "raw_contacts.deleted"},
  "display_name": "Alex Smith",
  "emails": [{"kind": "email", "value": "alex@example.com"}],
  "future_field": {"anything": [1, 2, 3]},
  "ims": [{"label": "Signal", "value": "alex_im"}],
  "names": {"family": "Smith", "given": "Alex", "phonetic": "Aleks"},
  "organization": {"department": "R&D", "name": "Acme", "title": "Engineer"},
  "phones": [{"future_in_phone": 1, "kind": "phone", "label": "mobile", "primary": true, "value": "+15551234567"}, {"value": "555-0100"}],
  "photo": {"artifact_id": "art-3", "present": true},
  "raw": {"x_times_contacted": 4, "ratio": 0.5, "tags": ["a", "b"], "nested": {"ok": true}},
  "recovery": {"relation": "uncommitted", "via": "wal", "wal": {"frame": 3, "committed": false}},
  "snapshot": {"name": "snap-1", "xid": 12},
  "source_id": "17",
  "source_ids": ["17", "204"],
  "starred": false,
  "urls": [{"value": "https://example.com/alex"}]
}`

func TestRecordTypesDecodeOlderVersions(t *testing.T) {
	t.Run("the version constant is the registered payload version", func(t *testing.T) {
		ty, ok := records.LookupType(contact.Type)
		if !ok {
			t.Fatal("contact is not a registered type")
		}
		if ty.PayloadVersion != contact.PayloadVersion {
			t.Errorf("registered payload version %d, package constant %d", ty.PayloadVersion, contact.PayloadVersion)
		}
	})

	t.Run("round trip equals the input", func(t *testing.T) {
		for name, c := range map[string]contact.Contact{"minimal": minimal(), "phone only": phoneOnly(), "full": full()} {
			b, err := json.Marshal(c.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := contact.Decode(contact.PayloadVersion, b)
			if err != nil {
				t.Fatalf("%s: Decode = %v", name, err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Errorf("%s: round trip\n got  %+v\n want %+v", name, got, c)
			}
			if !reflect.DeepEqual(got.Payload(), c.Payload()) {
				t.Errorf("%s: the decoded contact builds another payload", name)
			}
		}
	})

	t.Run("v1 fixture bytes decode", func(t *testing.T) {
		got, err := contact.Decode(1, []byte(v1Fixture))
		if err != nil {
			t.Fatalf("Decode(v1 fixture) = %v", err)
		}
		want := contact.Contact{
			DisplayName:  "Alex Smith",
			Names:        &contact.Names{Given: "Alex", Family: "Smith", Phonetic: "Aleks"},
			Organization: &contact.Organization{Name: "Acme", Title: "Engineer", Department: "R&D"},
			Phones:       []contact.Value{{Value: "+15551234567", Label: "mobile", Kind: "phone", Primary: ptr(true)}, {Value: "555-0100"}},
			Emails:       []contact.Value{{Value: "alex@example.com", Kind: "email"}},
			URLs:         []contact.Value{{Value: "https://example.com/alex"}},
			IMs:          []contact.Value{{Value: "alex_im", Label: "Signal"}},
			Addresses:    []contact.Address{{Formatted: "1 Main St, Springfield", Street: "1 Main St", City: "Springfield", Label: "home"}},
			Accounts:     []contact.Account{{Type: "com.google", Name: "alex@gmail.com"}},
			Birthday:     "--02-29", Starred: ptr(false), PhotoPresent: ptr(true), PhotoArtifactID: "art-3",
			SourceID: "17", SourceIDs: []string{"17", "204"},
			Deleted:  map[string]any{"source": "raw_contacts.deleted"},
			Recovery: map[string]any{"relation": "uncommitted", "via": "wal", "wal": map[string]any{"frame": json.Number("3"), "committed": false}},
			Snapshot: map[string]any{"name": "snap-1", "xid": json.Number("12")},
			Raw:      map[string]any{"x_times_contacted": json.Number("4"), "ratio": json.Number("0.5"), "tags": []any{"a", "b"}, "nested": map[string]any{"ok": true}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Decode(v1 fixture)\n got  %+v\n want %+v", got, want)
		}
		var p map[string]any
		dec := json.NewDecoder(strings.NewReader(v1Fixture))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if err := contact.Validate(p); err != nil {
			t.Errorf("Validate(v1 fixture) = %v", err)
		}
	})

	t.Run("a future-added unknown field is ignored", func(t *testing.T) {
		got, err := contact.Decode(1, []byte(`{"display_name":"A","new_in_v1_1":{"deep":[1,{"x":null}]},"another":"x"}`))
		if err != nil {
			t.Fatalf("Decode = %v", err)
		}
		if !reflect.DeepEqual(got, contact.Contact{DisplayName: "A"}) {
			t.Errorf("Decode = %+v", got)
		}
	})

	t.Run("other payload versions are unsupported", func(t *testing.T) {
		for _, v := range []int{0, 2, 3, -1, 1 << 30} {
			_, err := contact.Decode(v, []byte(v1Fixture))
			if !errors.Is(err, contact.ErrUnsupportedPayloadVersion) {
				t.Errorf("Decode(%d) = %v, want ErrUnsupportedPayloadVersion", v, err)
			}
		}
		if _, err := contact.Decode(1, []byte(v1Fixture)); errors.Is(err, contact.ErrUnsupportedPayloadVersion) {
			t.Error("v1 reported as unsupported")
		}
	})

	t.Run("damaged payloads are errors, never a panic and never an unsupported version", func(t *testing.T) {
		const marker = "SECRET-VALUE-9931"
		for name, b := range map[string]string{
			"empty":             ``,
			"not json":          `{"display_name": `,
			"null":              `null`,
			"array":             `[]`,
			"string":            `"x"`,
			"trailing value":    `{"display_name":"A"} {}`,
			"trailing garbage":  `{"display_name":"A"} x`,
			"nothing to name":   `{"birthday":"1980-01-01"}`,
			"empty display":     `{"display_name":""}`,
			"wrong type":        `{"display_name":"A","starred":"` + marker + `"}`,
			"bad birthday":      `{"display_name":"A","birthday":"` + marker + `"}`,
			"phone no value":    `{"phones":[{"label":"` + marker + `"}]}`,
			"exponent snapshot": `{"display_name":"A","snapshot":{"name":"s","xid":1e3}}`,
			"decimal snapshot":  `{"display_name":"A","snapshot":{"name":"s","xid":2.0}}`,
		} {
			_, err := contact.Decode(1, []byte(b))
			if err == nil {
				t.Errorf("%s: no error", name)
				continue
			}
			if errors.Is(err, contact.ErrUnsupportedPayloadVersion) {
				t.Errorf("%s: reported as an unsupported version", name)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s: error repeats a payload value: %v", name, err)
			}
		}
	})

	t.Run("deeply nested input does not panic", func(t *testing.T) {
		deep := strings.Repeat(`{"a":`, 2000) + `1` + strings.Repeat(`}`, 2000)
		if _, err := contact.Decode(1, []byte(`{"display_name":"A","raw":`+deep+`}`)); err == nil {
			t.Error("a payload nested 2000 deep was accepted")
		}
	})
}

// TestDecodeKeepsRecoveryAndSnapshot: a typed reader can never present a recovered
// or snapshot-derived contact (or one the application deleted) as a live one, so
// Decode carries the provenance objects and Payload writes them back.
func TestDecodeKeepsRecoveryAndSnapshot(t *testing.T) {
	const in = `{"display_name":"A",` +
		`"recovery":{"relation":"uncommitted","via":"wal","wal":{"frame":3,"salt1":7,"committed":false},"notes":["a","b"]},` +
		`"snapshot":{"name":"snap-1","xid":12},"deleted":{"source":"raw_contacts.deleted"}}`
	got, err := contact.Decode(1, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	wantRec := map[string]any{
		"relation": "uncommitted", "via": "wal", "notes": []any{"a", "b"},
		"wal": map[string]any{"frame": json.Number("3"), "salt1": json.Number("7"), "committed": false},
	}
	if !reflect.DeepEqual(got.Recovery, wantRec) {
		t.Errorf("Recovery = %#v, want %#v", got.Recovery, wantRec)
	}
	if want := map[string]any{"name": "snap-1", "xid": json.Number("12")}; !reflect.DeepEqual(got.Snapshot, want) {
		t.Errorf("Snapshot = %#v, want %#v", got.Snapshot, want)
	}
	if want := map[string]any{"source": "raw_contacts.deleted"}; !reflect.DeepEqual(got.Deleted, want) {
		t.Errorf("Deleted = %#v, want %#v", got.Deleted, want)
	}
	p := got.Payload()
	if !reflect.DeepEqual(p["recovery"], wantRec) || !reflect.DeepEqual(p["snapshot"], map[string]any{"name": "snap-1", "xid": json.Number("12")}) ||
		!reflect.DeepEqual(p["deleted"], map[string]any{"source": "raw_contacts.deleted"}) {
		t.Errorf("Payload dropped provenance: %v", p)
	}
	if err := contact.Validate(p); err != nil {
		t.Errorf("the re-built payload does not validate: %v", err)
	}
	live, err := contact.Decode(1, []byte(`{"display_name":"A"}`))
	if err != nil || live.Recovery != nil || live.Snapshot != nil || live.Deleted != nil {
		t.Errorf("a live contact decoded with provenance: %+v, %v", live, err)
	}
	for _, k := range []string{"recovery", "snapshot", "deleted"} {
		if _, has := live.Payload()[k]; has {
			t.Errorf("a live contact built a %s key", k)
		}
	}
}

func makeStrings(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = "id"
	}
	return out
}

// TestDecodeKeepsRawNumbersExact: a number in raw that does not fit an int64 or
// float64 survives Decode and Payload unchanged.
func TestDecodeKeepsRawNumbersExact(t *testing.T) {
	const in = `{"display_name":"A","raw":{"big":18446744073709551615,"dec":1.50,"n":{"x":[9007199254740993]}}}`
	c, err := contact.Decode(1, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.Raw["big"] != json.Number("18446744073709551615") || c.Raw["dec"] != json.Number("1.50") {
		t.Errorf("Raw = %#v", c.Raw)
	}
	b, err := json.Marshal(c.Payload())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"18446744073709551615", "1.50", "9007199254740993"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("payload %s lost %s", b, want)
		}
	}
	if err := contact.Validate(c.Payload()); err != nil {
		t.Errorf("rebuilt payload: %v", err)
	}
}
