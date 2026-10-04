package records_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
)

func registerTemp(t *testing.T, ty records.Type) {
	t.Helper()
	if err := records.RegisterType(ty); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { records.UnregisterType(ty.Name) })
}

// TestValidatorPanicBecomesTypedError: a panicking Type.Validate never takes the
// process down; each panic is an ErrInvalidPayload naming the type.
func TestValidatorPanicBecomesTypedError(t *testing.T) {
	for name, fn := range map[string]func(map[string]any) error{
		"string":        func(map[string]any) error { panic("boom") },
		"error value":   func(map[string]any) error { panic(errors.New("kaboom")) },
		"nil map write": func(map[string]any) error { put(nil); return nil },
		"index":         func(map[string]any) error { _ = at(nil, 3); return nil },
		"type assert":   func(p map[string]any) error { _ = p["k"].(int); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			ty := "panicky_" + strings.ReplaceAll(name, " ", "_")
			registerTemp(t, records.Type{Name: ty, PayloadVersion: 1, Validate: fn})
			r := base()
			r.Type = ty
			_, err := records.Prepare(r, testArt)
			if !errors.Is(err, records.ErrInvalidPayload) || !errors.Is(err, records.ErrInvalidRecord) {
				t.Fatalf("err = %v, want ErrInvalidPayload", err)
			}
			if !strings.Contains(err.Error(), ty) || !strings.Contains(err.Error(), "panic") {
				t.Errorf("err = %q, want it to name the type and the panic", err)
			}
		})
	}
}

// TestValidatorGetsADeepCopy: whatever the validator does to the map it is given
// (nested maps and slices included), the caller's payload is untouched and the
// stored payload is the canonical form of what the caller passed in.
func TestValidatorGetsADeepCopy(t *testing.T) {
	registerTemp(t, records.Type{Name: "mutating", PayloadVersion: 1, Validate: func(p map[string]any) error {
		p["added"] = true
		p["inner"].(map[string]any)["x"] = "changed"
		p["list"].([]any)[0] = "changed"
		delete(p, "k")
		return nil
	}})
	payload := map[string]any{
		"k":     "v",
		"inner": map[string]any{"x": "orig"},
		"list":  []any{"a", map[string]any{"y": 1}},
		"n":     json.Number("12"),
	}
	r := base()
	r.Type, r.Payload = "mutating", payload
	p, err := records.Prepare(r, testArt)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 4 || payload["k"] != "v" || payload["inner"].(map[string]any)["x"] != "orig" || payload["list"].([]any)[0] != "a" {
		t.Errorf("the caller's payload was changed by the validator: %v", payload)
	}
	want := `{"inner":{"x":"orig"},"k":"v","list":["a",{"y":1}],"n":12}`
	if got := p.Row().Payload; got != want {
		t.Errorf("stored payload = %s, want %s", got, want)
	}
}

// TestErrorsClipCallerStrings: caller-supplied strings never reach an error
// message unclipped or unquoted.
func TestErrorsClipCallerStrings(t *testing.T) {
	huge := strings.Repeat("A", 100000) + "\n\x1b[31m"
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		msg := err.Error()
		if len(msg) > 1024 {
			t.Errorf("error is %d bytes", len(msg))
		}
		if !utf8.ValidString(msg) || strings.ContainsAny(msg, "\n\x1b") {
			t.Errorf("error holds raw control characters: %q", msg)
		}
	}
	t.Run("type", func(t *testing.T) {
		r := base()
		r.Type = huge
		_, err := records.Prepare(r, testArt)
		check(t, err)
	})
	t.Run("artifact id", func(t *testing.T) {
		r := base()
		r.ArtifactID = huge
		_, err := records.Prepare(r, testArt)
		check(t, err)
	})
	t.Run("expected artifact id", func(t *testing.T) {
		art := testArt
		art.ID = huge
		_, err := records.Prepare(base(), art)
		check(t, err)
	})
	t.Run("payload number", func(t *testing.T) {
		r := base()
		r.Payload = map[string]any{"k": json.Number(strings.Repeat("9", 100000) + "x")}
		_, err := records.Prepare(r, testArt)
		check(t, err)
	})
	t.Run("clip is at most 128 bytes", func(t *testing.T) {
		r := base()
		r.Type = strings.Repeat("é", 200)
		_, err := records.Prepare(r, testArt)
		check(t, err)
		if n := strings.Count(err.Error(), "é"); n > 64 {
			t.Errorf("%d clipped characters kept, want at most 64 (128 bytes)", n)
		}
	})
}

func at(s []int, i int) int { return s[i] }

func put(m map[string]int) { m["a"] = 1 }
