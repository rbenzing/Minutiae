package dbrecover

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func requireRefused(t *testing.T, name string, p map[string]any) {
	t.Helper()
	if err := validateDBRow(p); err == nil || !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("%s: err = %v, want ErrInvalidPayload", name, err)
	}
}

func requireAccepted(t *testing.T, name string, p map[string]any) {
	t.Helper()
	if err := validateDBRow(p); err != nil {
		t.Errorf("%s: refused: %v", name, err)
	}
}

// TestTableLabelRules (J28): a table row names its table exactly when its
// basis is schema, fit or guess; basis none names none; never both forms.
func TestTableLabelRules(t *testing.T) {
	with := func(basis string, mut func(p map[string]any)) map[string]any {
		p := validPayload()
		p["table_basis"] = basis
		delete(p, "table")
		mut(p)
		return p
	}
	for _, b := range []string{"schema", "fit", "guess"} {
		requireAccepted(t, b+" with table", with(b, func(p map[string]any) { p["table"] = "t" }))
		requireAccepted(t, b+" with table_b64", with(b, func(p map[string]any) { p["table_b64"] = "AAEC"; p["table_valid"] = false }))
		requireRefused(t, b+" with neither", with(b, func(p map[string]any) {}))
		requireRefused(t, b+" with both", with(b, func(p map[string]any) { p["table"] = "t"; p["table_b64"] = "AAEC" }))
	}
	requireAccepted(t, "none with neither", with("none", func(p map[string]any) {}))
	requireRefused(t, "none with table", with("none", func(p map[string]any) { p["table"] = "t" }))
	requireRefused(t, "none with table_b64", with("none", func(p map[string]any) { p["table_b64"] = "AAEC" }))
	requireRefused(t, "none with both", with("none", func(p map[string]any) { p["table"] = "t"; p["table_b64"] = "AAEC" }))
}

// TestFloat64RefusedInOpaqueValues (J30): a float64 anywhere inside the walked
// values (wal, journal, overflow and the other opaque keys) is refused.
func TestFloat64RefusedInOpaqueValues(t *testing.T) {
	for _, k := range []string{"wal", "journal", "overflow", "overflow_head", "sibling_unused"} {
		p := validPayload()
		p[k] = map[string]any{"x": []any{1.5}}
		requireRefused(t, k+" float64", p)
		p = validPayload()
		p[k] = map[string]any{"x": []any{int64(1), "s", true, nil}}
		requireAccepted(t, k+" plain", p)
	}
}

// TestColumnIndexBound (J30): SQLite allows 32767 columns, indexes 0..32766.
func TestColumnIndexBound(t *testing.T) {
	col := func(i int64) map[string]any {
		p := validPayload()
		p["columns"] = []any{map[string]any{"i": i, "type": "null"}}
		return p
	}
	requireAccepted(t, "i=32766", col(32766))
	requireRefused(t, "i=32767", col(32767))
	requireRefused(t, "i=32768", col(32768))
	requireRefused(t, "i=-1", col(-1))
}

// TestValueRulesPerType (J30, J31): each storage class carries only the keys it
// may; a text column holds value_b64 only with text_valid false.
func TestValueRulesPerType(t *testing.T) {
	one := func(m map[string]any) map[string]any {
		p := validPayload()
		m["i"] = int64(0)
		p["columns"] = []any{m}
		return p
	}
	for name, m := range map[string]map[string]any{
		"null with value":         {"type": "null", "value": int64(1)},
		"null with value_b64":     {"type": "null", "value_b64": "AA=="},
		"null with float_bits":    {"type": "null", "float_bits": "0000000000000000"},
		"blob with value":         {"type": "blob", "value": "x"},
		"blob with both":          {"type": "blob", "value": "x", "value_b64": "AA=="},
		"blob with float_bits":    {"type": "blob", "value_b64": "AA==", "float_bits": "0000000000000000"},
		"integer with float_bits": {"type": "integer", "value": int64(1), "float_bits": "0000000000000000"},
		"text with float_bits":    {"type": "text", "value": "x", "float_bits": "0000000000000000"},
		"integer with value_b64":  {"type": "integer", "value": int64(1), "value_b64": "AA=="},
		"text valid with b64":     {"type": "text", "value_b64": "AA==", "text_valid": true},
		"text b64 no flag":        {"type": "text", "value_b64": "AA=="},
		"text both forms":         {"type": "text", "value": "x", "value_b64": "AA==", "text_valid": false},
		"text invalid with value": {"type": "text", "value": "x", "text_valid": false},
		"real with value_b64":     {"type": "real", "float_bits": "0000000000000000", "value_b64": "AA=="},
	} {
		requireRefused(t, name, one(m))
	}
	for name, m := range map[string]map[string]any{
		"null":                {"type": "null"},
		"blob":                {"type": "blob", "value_b64": "AA=="},
		"empty blob":          {"type": "blob", "value_b64": ""},
		"text invalid":        {"type": "text", "value_b64": "/w==", "text_valid": false},
		"text valid":          {"type": "text", "value": "x", "text_valid": true},
		"text invalid + enc":  {"type": "text", "value_b64": "/w==", "text_valid": false, "enc": "UTF-16le"},
		"text omitted":        {"type": "text", "omitted": true},
		"unread integer":      {"type": "integer", "unread": true},
		"null with len flags": {"type": "null", "clipped": false},
	} {
		requireAccepted(t, name, one(m))
	}
}
