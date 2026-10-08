package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func TestRegistryCoreTypes(t *testing.T) {
	want := []string{
		"message", "call", "contact", "calendar_event", "location", "web_visit",
		"web_search", "download", "file", "account", "app_event", "media", "note",
		"wifi_network", "event",
	}
	if len(want) != 15 {
		t.Fatalf("test lists %d core types, want 15", len(want))
	}
	for _, n := range want {
		ty, ok := records.LookupType(n)
		if !ok {
			t.Errorf("core type %q is not registered", n)
			continue
		}
		if ty.Name != n || ty.PayloadVersion < 1 || ty.Validate != nil {
			t.Errorf("core type %q = %+v: want PayloadVersion >= 1 and no validator", n, ty)
		}
	}
	if _, ok := records.LookupType("nonesuch"); ok {
		t.Error("LookupType found a type that was never registered")
	}
}

func TestRegisterTypeRules(t *testing.T) {
	for _, bad := range []string{"", "a", "A", "1abc", "has space", "has-dash", "Upper", "x" + strings.Repeat("a", 32), "é_type"} {
		err := records.RegisterType(records.Type{Name: bad, PayloadVersion: 1})
		if !errors.Is(err, records.ErrInvalidField) || !errors.Is(err, records.ErrInvalidRecord) {
			t.Errorf("RegisterType(%q) = %v, want ErrInvalidField", bad, err)
		}
	}
	// 32 characters is the longest name
	longest := "x" + strings.Repeat("a", 31)
	if err := records.RegisterType(records.Type{Name: longest, PayloadVersion: 1}); err != nil {
		t.Fatalf("a 32 character name: %v", err)
	}
	t.Cleanup(func() { records.UnregisterType(longest) })

	if err := records.RegisterType(records.Type{Name: "message", PayloadVersion: 1}); !errors.Is(err, records.ErrInvalidField) {
		t.Errorf("duplicate of a core type = %v, want ErrInvalidField", err)
	}
	for _, v := range []int{0, -1} {
		if err := records.RegisterType(records.Type{Name: "zero_version", PayloadVersion: v}); !errors.Is(err, records.ErrInvalidField) {
			t.Errorf("PayloadVersion %d = %v, want ErrInvalidField", v, err)
		}
	}
	if _, ok := records.LookupType("zero_version"); ok {
		t.Error("a rejected registration is visible")
	}

	// a validator is invoked, and its error wraps ErrInvalidPayload
	calls := 0
	boom := errors.New("missing field x")
	records.UnregisterType("strict_type")
	if err := records.RegisterType(records.Type{Name: "strict_type", PayloadVersion: 3, Validate: func(p map[string]any) error {
		calls++
		if _, ok := p["x"]; !ok {
			return boom
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType("strict_type") })
	art := records.ArtifactInfo{ID: "a1", Size: 10}
	_, err := records.Prepare(records.Record{Type: "strict_type", ArtifactID: "a1", Payload: map[string]any{}}, art)
	if calls != 1 || !errors.Is(err, records.ErrInvalidPayload) || !errors.Is(err, records.ErrInvalidRecord) || !errors.Is(err, boom) {
		t.Fatalf("validator calls = %d, err = %v: want 1 call and ErrInvalidPayload wrapping the validator's error", calls, err)
	}
	p, err := records.Prepare(records.Record{Type: "strict_type", ArtifactID: "a1", Payload: map[string]any{"x": 1}}, art)
	if err != nil || calls != 2 {
		t.Fatalf("valid payload: err = %v, calls = %d", err, calls)
	}
	if p.Row().PayloadV != 3 {
		t.Errorf("payload_v = %d, want the registered 3", p.Row().PayloadV)
	}
}

func TestTypesListsCoreSortedWithValidatorFlag(t *testing.T) {
	core := []string{
		"message", "call", "contact", "calendar_event", "location", "web_visit",
		"web_search", "download", "file", "account", "app_event", "media", "note",
		"wifi_network", "event",
	}
	if err := records.RegisterType(records.Type{Name: "zz_types_plain", PayloadVersion: 3}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType("zz_types_plain") })
	if err := records.RegisterType(records.Type{Name: "zz_types_strict", PayloadVersion: 2, Validate: func(map[string]any) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType("zz_types_strict") })
	if err := records.RegisterType(records.Type{Name: "zz_types_set", PayloadVersion: 5}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType("zz_types_set") })
	records.SetValidator("zz_types_set", func(map[string]any) error { return nil })

	got := records.Types()
	byName := map[string]records.TypeInfo{}
	for i, ti := range got {
		byName[ti.Name] = ti
		if i > 0 && got[i-1].Name >= ti.Name {
			t.Errorf("Types is not sorted by name: %q before %q", got[i-1].Name, ti.Name)
		}
	}
	for _, n := range core {
		ti, ok := byName[n]
		if !ok {
			t.Errorf("core type %q is missing from Types", n)
			continue
		}
		if ti.PayloadVersion < 1 || ti.HasValidator {
			t.Errorf("core type %q = %+v: want PayloadVersion >= 1 and HasValidator false", n, ti)
		}
	}
	for name, want := range map[string]records.TypeInfo{
		"zz_types_plain":  {Name: "zz_types_plain", PayloadVersion: 3, HasValidator: false},
		"zz_types_strict": {Name: "zz_types_strict", PayloadVersion: 2, HasValidator: true},
		"zz_types_set":    {Name: "zz_types_set", PayloadVersion: 5, HasValidator: true},
	} {
		if byName[name] != want {
			t.Errorf("Types()[%s] = %+v, want %+v", name, byName[name], want)
		}
	}
	// the list is a snapshot: changing it changes nothing in the registry
	want := got[0]
	got[0].HasValidator = !got[0].HasValidator
	if again := records.Types(); again[0] != want {
		t.Errorf("Types()[0] = %+v after the caller changed its copy, want %+v", again[0], want)
	}
}
