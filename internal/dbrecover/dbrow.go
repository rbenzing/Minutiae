package dbrecover

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/records"
)

// TypeName is the record type of one recovered database row.
const TypeName = "db_row"

// PayloadVersion is the version of the db_row payload layout.
const PayloadVersion = 1

// Validator bounds. SQLite allows at most 32767 columns per row.
const (
	maxColumns   = 32767
	maxNameBytes = 4096
	maxDepth     = 8
	maxNodes     = 1 << 16
)

func init() {
	if err := records.RegisterType(records.Type{Name: TypeName, PayloadVersion: PayloadVersion, Validate: validateDBRow}); err != nil {
		panic(err)
	}
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: db_row: %s", records.ErrInvalidPayload, fmt.Sprintf(format, a...))
}

// fieldKind is the payload type a key must have.
type fieldKind int

const (
	kName    fieldKind = iota // string capped at maxNameBytes
	kB64                      // standard base64 string
	kBool                     // boolean
	kCount                    // integer >= 0
	kAny                      // any plain value (bounded walk)
	kStrings                  // list of strings
)

var topKeys = map[string]fieldKind{
	"table": kName, "table_b64": kB64, "table_valid": kBool,
	"index": kName, "index_b64": kB64, "index_valid": kBool,
	"origin": kName, "wal": kAny, "journal": kAny, "overflow": kAny, "overflow_head": kAny,
	"truncated": kBool, "notes": kStrings, "method_confidence": kCount, "confidence_cap": kCount,
	"sibling_artifacts": kStrings, "sibling_unused": kAny, "mixed_inputs": kBool,
	"input_incomplete": kBool, "values_unread": kCount, "values_omitted": kCount,
	"values_clipped": kCount, "body_clipped": kBool,
}

var colKeys = map[string]fieldKind{
	"serial": kCount, "len": kCount, "enc": kName, "value": kAny, "value_b64": kB64,
	"float_bits": kName, "text_valid": kBool, "unread": kBool, "clipped": kBool,
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func oneOf(p map[string]any, key string, allowed ...string) error {
	v, ok := p[key]
	if !ok {
		return bad("missing %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return bad("%q must be a string", key)
	}
	for _, a := range allowed {
		if s == a {
			return nil
		}
	}
	return bad("%q has the unknown value %q", key, clip(s))
}

// asInt returns v as an int64 (an int64 or an integral json.Number).
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// walk checks that v holds only the payload Go types, nested at most maxDepth
// deep and holding at most *budget nodes.
func walk(v any, depth int, budget *int) error {
	*budget--
	if *budget < 0 {
		return bad("value too large")
	}
	if depth > maxDepth {
		return bad("value nested deeper than %d", maxDepth)
	}
	switch x := v.(type) {
	case nil, bool, string, int64, json.Number:
	case []any:
		for _, e := range x {
			if err := walk(e, depth+1, budget); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range x {
			if err := walk(e, depth+1, budget); err != nil {
				return err
			}
		}
	default:
		return bad("unsupported value type %T", v)
	}
	return nil
}

func checkField(key string, kind fieldKind, v any) error {
	switch kind {
	case kName:
		s, ok := v.(string)
		if !ok {
			return bad("%q must be a string", key)
		}
		if len(s) > maxNameBytes {
			return bad("%q is longer than %d bytes", key, maxNameBytes)
		}
	case kB64:
		s, ok := v.(string)
		if !ok {
			return bad("%q must be a base64 string", key)
		}
		if _, err := base64.StdEncoding.DecodeString(s); err != nil {
			return bad("%q is not valid base64", key)
		}
	case kBool:
		if _, ok := v.(bool); !ok {
			return bad("%q must be a boolean", key)
		}
	case kCount:
		if n, ok := asInt(v); !ok || n < 0 {
			return bad("%q must be an integer >= 0", key)
		}
	case kAny:
		budget := maxNodes
		return walk(v, 0, &budget)
	case kStrings:
		l, ok := v.([]any)
		if !ok || len(l) > maxNodes {
			return bad("%q must be a list of strings (at most %d)", key, maxNodes)
		}
		for _, e := range l {
			if _, ok := e.(string); !ok {
				return bad("%q must be a list of strings", key)
			}
		}
	}
	return nil
}

func validateColumn(c any, idx int) error {
	m, ok := c.(map[string]any)
	if !ok {
		return bad("column %d is not an object", idx)
	}
	for k := range m {
		if _, known := colKeys[k]; !known && k != "i" && k != "type" && k != "omitted" {
			return bad("column %d has the unknown key %q", idx, clip(k))
		}
	}
	if n, ok := asInt(m["i"]); !ok || n < 0 {
		return bad("column %d: \"i\" must be an integer >= 0", idx)
	}
	if err := oneOf(m, "type", "null", "integer", "real", "text", "blob"); err != nil {
		return fmt.Errorf("column %d: %w", idx, err)
	}
	for k, v := range m {
		if kind, known := colKeys[k]; known {
			if err := checkField(k, kind, v); err != nil {
				return fmt.Errorf("column %d: %w", idx, err)
			}
		}
	}
	omitted := false
	if v, has := m["omitted"]; has {
		switch o := v.(type) {
		case bool:
			omitted = o
		case string:
			if o != "payload-budget" {
				return bad("column %d: \"omitted\" has the unknown value %q", idx, clip(o))
			}
			omitted = true
		default:
			return bad("column %d: \"omitted\" must be a boolean or a reason", idx)
		}
	}
	unread, _ := m["unread"].(bool)
	_, hasV := m["value"]
	_, hasB := m["value_b64"]
	if m["type"] != "null" && !hasV && !hasB && !omitted && !unread {
		return bad("column %d: a %s column needs a value, value_b64, omitted or unread", idx, m["type"])
	}
	return nil
}

// validateDBRow checks a db_row payload: the complete key set, the types and
// closed value sets of every field, and the shape of the columns. It never
// panics and is bounded in depth, columns and size.
func validateDBRow(payload map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = bad("validator failed on a hostile payload: %v", r)
		}
	}()
	if payload == nil {
		return bad("payload is nil")
	}
	for _, e := range []struct {
		key     string
		allowed []string
	}{
		{"entry_kind", []string{"table", "index"}},
		{"table_basis", []string{"schema", "fit", "guess", "none"}},
		{"relation", []string{"absent-from-live", "superseded-version", "uncommitted", "unknown"}},
		{"file", []string{"db", "wal", "journal"}},
	} {
		if err := oneOf(payload, e.key, e.allowed...); err != nil {
			return err
		}
	}
	if _, ok := payload["sibling_pairing"]; ok {
		if err := oneOf(payload, "sibling_pairing", "by-name", "explicit", "none"); err != nil {
			return err
		}
	}
	for k, v := range payload {
		switch k {
		case "entry_kind", "table_basis", "relation", "file", "sibling_pairing", "columns", "carved":
		case "rowid":
			if _, isInt := asInt(v); v != nil && !isInt {
				return bad("\"rowid\" must be an integer or null")
			}
		case "page":
			if n, ok := asInt(v); !ok || n < 1 {
				return bad("\"page\" must be an integer >= 1")
			}
		case "cell":
			if n, ok := asInt(v); !ok || n < 0 || n > math.MaxInt32 {
				return bad("\"cell\" must be an integer >= 0")
			}
		default:
			kind, known := topKeys[k]
			if !known {
				return bad("unknown key %q", clip(k))
			}
			if err := checkField(k, kind, v); err != nil {
				return err
			}
		}
	}
	for _, k := range []string{"page", "origin"} {
		if _, ok := payload[k]; !ok {
			return bad("missing %q", k)
		}
	}
	carved, hasCarved := payload["carved"]
	if hasCarved {
		m, ok := carved.(map[string]any)
		if !ok || len(m) != 3 {
			return bad("\"carved\" must be an object of span_kind, span_offset, span_length")
		}
		if s, ok := m["span_kind"].(string); !ok || len(s) > maxNameBytes {
			return bad("\"carved\": span_kind must be a string")
		}
		for _, k := range []string{"span_offset", "span_length"} {
			if n, ok := asInt(m[k]); !ok || n < 0 {
				return bad("\"carved\": %s must be an integer >= 0", k)
			}
		}
	}
	if _, ok := payload["cell"]; !ok && !hasCarved {
		return bad("\"cell\" may be absent only when \"carved\" is present")
	}
	cols, ok := payload["columns"].([]any)
	if !ok {
		return bad("\"columns\" must be a list")
	}
	if len(cols) > maxColumns {
		return bad("%d columns exceed the limit of %d", len(cols), maxColumns)
	}
	for i, c := range cols {
		if err := validateColumn(c, i); err != nil {
			return err
		}
	}
	return nil
}
