package dbrecover

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func validPayload() map[string]any {
	return map[string]any{
		"entry_kind":  "table",
		"table":       "messages",
		"table_basis": "schema",
		"rowid":       int64(7),
		"relation":    "absent-from-live",
		"columns": []any{
			map[string]any{"i": int64(0), "type": "integer", "value": int64(5)},
			map[string]any{"i": int64(1), "type": "text", "value": "hi", "text_valid": true},
			map[string]any{"i": int64(2), "type": "blob", "value_b64": "AAEC"},
			map[string]any{"i": int64(3), "type": "null"},
		},
		"file":   "db",
		"origin": "freelist",
		"page":   int64(3),
		"cell":   int64(0),
		"notes":  []any{"x"},
	}
}

func TestDBRowTypeIsRegistered(t *testing.T) {
	ty, ok := records.LookupType(TypeName)
	if !ok {
		t.Fatalf("type %q is not registered", TypeName)
	}
	if ty.PayloadVersion != PayloadVersion || ty.Validate == nil {
		t.Fatalf("registered type = %+v", ty)
	}
}

func TestDBRowValidatorMatrix(t *testing.T) {
	if err := validateDBRow(validPayload()); err != nil {
		t.Fatalf("minimal valid payload: %v", err)
	}
	slack := validPayload()
	delete(slack, "cell")
	slack["carved"] = map[string]any{"span_kind": "gap", "span_offset": int64(100), "span_length": int64(20)}
	if err := validateDBRow(slack); err != nil {
		t.Fatalf("slack-shaped payload: %v", err)
	}
	num := validPayload()
	num["page"] = json.Number("4")
	if err := validateDBRow(num); err != nil {
		t.Fatalf("json.Number page: %v", err)
	}

	bad := map[string]func(p map[string]any){
		"missing entry_kind":   func(p map[string]any) { delete(p, "entry_kind") },
		"missing columns":      func(p map[string]any) { delete(p, "columns") },
		"missing origin":       func(p map[string]any) { delete(p, "origin") },
		"mistyped table":       func(p map[string]any) { p["table"] = int64(1) },
		"mistyped columns":     func(p map[string]any) { p["columns"] = "x" },
		"unknown key":          func(p map[string]any) { p["surprise"] = true },
		"bad entry_kind":       func(p map[string]any) { p["entry_kind"] = "view" },
		"bad basis":            func(p map[string]any) { p["table_basis"] = "magic" },
		"bad relation":         func(p map[string]any) { p["relation"] = "live" },
		"bad file":             func(p map[string]any) { p["file"] = "shm" },
		"page zero":            func(p map[string]any) { p["page"] = int64(0) },
		"cell negative":        func(p map[string]any) { p["cell"] = int64(-1) },
		"cell absent no carve": func(p map[string]any) { delete(p, "cell") },
		"bad rowid":            func(p map[string]any) { p["rowid"] = "7" },
		"bad sibling_pairing":  func(p map[string]any) { p["sibling_pairing"] = "guess" },
		"notes not strings":    func(p map[string]any) { p["notes"] = []any{int64(1)} },
		"carved missing key":   func(p map[string]any) { p["carved"] = map[string]any{"span_kind": "gap"} },
		"float payload":        func(p map[string]any) { p["page"] = 3.5 },
		"text column no value": func(p map[string]any) {
			p["columns"] = []any{map[string]any{"i": int64(0), "type": "text"}}
		},
		"bad column type": func(p map[string]any) {
			p["columns"] = []any{map[string]any{"i": int64(0), "type": "decimal", "value": "1"}}
		},
		"unknown column key": func(p map[string]any) {
			p["columns"] = []any{map[string]any{"i": int64(0), "type": "null", "zzz": true}}
		},
		"bad base64": func(p map[string]any) {
			p["columns"] = []any{map[string]any{"i": int64(0), "type": "blob", "value_b64": "***"}}
		},
		"bad table_b64": func(p map[string]any) { p["table_b64"] = "***" },
		"column not object": func(p map[string]any) {
			p["columns"] = []any{"x"}
		},
		"bad omitted": func(p map[string]any) {
			p["columns"] = []any{map[string]any{"i": int64(0), "type": "text", "omitted": "other"}}
		},
	}
	for name, mut := range bad {
		p := validPayload()
		mut(p)
		err := validateDBRow(p)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, records.ErrInvalidPayload) {
			t.Errorf("%s: error %v does not wrap ErrInvalidPayload", name, err)
		}
	}
	// a non-null column accepted through omitted/unread
	ok := validPayload()
	ok["columns"] = []any{
		map[string]any{"i": int64(0), "type": "text", "omitted": "payload-budget"},
		map[string]any{"i": int64(1), "type": "blob", "unread": true},
		map[string]any{"i": int64(2), "type": "real", "float_bits": "3ff0000000000000", "value": "1"},
	}
	if err := validateDBRow(ok); err != nil {
		t.Errorf("omitted/unread columns: %v", err)
	}
}

func TestDBRowValidatorHostile(t *testing.T) {
	if err := validateDBRow(nil); err == nil || !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("nil map: %v", err)
	}
	deep := any("x")
	for range 64 {
		deep = []any{deep}
	}
	p := validPayload()
	p["notes"] = deep
	if err := validateDBRow(p); err == nil {
		t.Error("deep nesting accepted")
	}
	p = validPayload()
	cols := make([]any, 100000)
	for i := range cols {
		cols[i] = map[string]any{"i": int64(i), "type": "null"}
	}
	p["columns"] = cols
	if err := validateDBRow(p); err == nil || !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("100k columns: %v", err)
	}
	p = validPayload()
	p["columns"] = []any{map[any]any{1: 2}, map[string]any{"i": "x"}, nil}
	if err := validateDBRow(p); err == nil {
		t.Error("non-string keys accepted")
	}
	p = validPayload()
	p["table"] = strings.Repeat("a", 1<<20)
	if err := validateDBRow(p); err == nil {
		t.Error("oversized table name accepted")
	}
}

func FuzzValidateDBRow(f *testing.F) {
	f.Add("table", int64(1), int64(0), "text", "v")
	f.Add("x", int64(0), int64(-1), "blob", "***")
	f.Fuzz(func(_ *testing.T, kind string, page, cell int64, ctype, val string) {
		p := validPayload()
		p["entry_kind"] = kind
		p["page"] = page
		p["cell"] = cell
		p["columns"] = []any{map[string]any{"i": page, "type": ctype, "value": val, "value_b64": val}}
		_ = validateDBRow(p)
	})
}

// FuzzValidateDBRowJSON explores whole payload shapes: the input is decoded as
// JSON (numbers kept exact) and validated; the validator must never panic.
func FuzzValidateDBRowJSON(f *testing.F) {
	for _, p := range []map[string]any{validPayload(), realPayload(0x7ff8_0000_dead_beef)} {
		b, err := json.Marshal(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	f.Add(`{"entry_kind":"index","columns":[{"i":0,"type":"real","float_bits":"zz"}]}`)
	f.Fuzz(func(_ *testing.T, in string) {
		dec := json.NewDecoder(strings.NewReader(in))
		dec.UseNumber()
		var p map[string]any
		if dec.Decode(&p) == nil {
			_ = validateDBRow(p)
		}
	})
}
