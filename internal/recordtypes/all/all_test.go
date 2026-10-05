package all_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/all"

	// the packages again, directly: a second registration would panic at init
	_ "github.com/rbenzing/minutiae/internal/recordtypes/call"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/contact"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/message"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/web"
)

var sixTypes = []string{"message", "call", "contact", "web_visit", "web_search", "download"}

// TestAllSixTypesHaveValidators: importing all installs exactly the six
// validators, and no other core type gets one. (That the validators are set
// exactly once even when several packages import all is shown by this binary
// starting at all: a second SetValidator panics at init.)
func TestAllSixTypesHaveValidators(t *testing.T) {
	var with []string
	infos := map[string]records.TypeInfo{}
	for _, ti := range records.Types() {
		infos[ti.Name] = ti
		if ti.HasValidator {
			with = append(with, ti.Name)
		}
	}
	want := slices.Clone(sixTypes)
	slices.Sort(want)
	slices.Sort(with)
	if !slices.Equal(with, want) {
		t.Errorf("types with a validator = %v, want %v", with, want)
	}
	for _, name := range sixTypes {
		if ti, ok := infos[name]; !ok || ti.PayloadVersion != 1 {
			t.Errorf("%s: registered %+v (%v)", name, ti, ok)
		}
	}
	for _, name := range []string{"calendar_event", "location", "file", "event", "note"} {
		if _, ok := infos[name]; !ok {
			t.Errorf("%s is not a registered core type", name)
		}
		if infos[name].HasValidator {
			t.Errorf("%s has a validator", name)
		}
	}
}

// TestValidPayloadPassesEveryValidator: the plain data of recordstest.ValidPayload
// is accepted by the real writer with the real validators, which proves the two
// stay in step.
func TestValidPayloadPassesEveryValidator(t *testing.T) {
	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "x.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "validpayload-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range sixTypes {
		p := recordstest.ValidPayload(typ)
		if len(p) == 0 {
			t.Errorf("ValidPayload(%q) is empty", typ)
		}
		if err := w.Add(ctx, records.Record{Type: typ, ArtifactID: a.ID, Summary: typ, Payload: p}); err != nil {
			t.Errorf("the writer refused ValidPayload(%q): %v", typ, err)
		}
		// the writer really validates: an empty payload of the same type is refused
		if err := w.Add(ctx, records.Record{Type: typ, ArtifactID: a.ID, Summary: typ, Payload: map[string]any{}}); !errors.Is(err, records.ErrInvalidPayload) {
			t.Errorf("Add(empty %s payload) = %v, want ErrInvalidPayload", typ, err)
		}
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(sixTypes)) || res.Rejected != len(sixTypes) {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

// TestValidPayloadIsFreshEachCall: a test may add keys to a payload, or change a
// nested value, without touching what the next caller gets.
func TestValidPayloadIsFreshEachCall(t *testing.T) {
	for _, typ := range sixTypes {
		a := recordstest.ValidPayload(typ)
		a["added"] = "x"
		for k, v := range a {
			if l, ok := v.([]any); ok {
				a[k] = append(l, "appended")
			}
		}
		delete(a, "display_name")
		b := recordstest.ValidPayload(typ)
		if _, ok := b["added"]; ok {
			t.Errorf("%s: a later call returned the earlier caller's map", typ)
		}
		if l, ok := b["participants"].([]any); ok && len(l) != 0 {
			t.Errorf("%s: a later call returned the earlier caller's slice: %v", typ, l)
		}
		if typ == "contact" && b["display_name"] != "x" {
			t.Errorf("contact: a later call lost display_name")
		}
	}
	// any other type gets an empty map, not nil, and again a fresh one
	for _, typ := range []string{"note", "event", "", "nope"} {
		p := recordstest.ValidPayload(typ)
		if p == nil || len(p) != 0 {
			t.Errorf("ValidPayload(%q) = %#v, want an empty map", typ, p)
		}
		p["x"] = 1
		if len(recordstest.ValidPayload(typ)) != 0 {
			t.Errorf("ValidPayload(%q) shares its map", typ)
		}
	}
}

// TestRecordsGeneratorStaysValidUnderValidators: the generator's mixed records
// (free-form keys plus ValidPayload for the validated types) ingest cleanly in a
// binary that links every validator.
func TestRecordsGeneratorStaysValidUnderValidators(t *testing.T) {
	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "gen.db", make([]byte, recordstest.ArtifactSize))
	recs := recordstest.Records(a.ID, 300, 7)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Type] = true
		if _, ok := r.Payload["seed"]; !ok {
			t.Fatalf("a record lost its free-form keys: %v", r.Payload)
		}
	}
	for _, typ := range []string{"message", "call", "contact", "web_visit"} {
		if !seen[typ] {
			t.Fatalf("the generator made no %s record in 300 (the test would prove nothing)", typ)
		}
	}
	res := recordstest.Ingest(t, cs, records.Parser{Name: "gen-test", Version: "1"}, []string{a.ID}, recs)
	if res.Records != int64(len(recs)) || res.Rejected != 0 {
		t.Errorf("ingest = %+v, want %d records and none rejected", res, len(recs))
	}
}
