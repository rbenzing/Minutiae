package dbrecover

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// valueBudget is what is left of a row's payload allowance, in bytes of
// canonical JSON.
type valueBudget struct{ left int64 }

func newValueBudget(n int64) *valueBudget { return &valueBudget{left: max(n, 0)} }

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// encodedSize is the size of m as canonical JSON (no HTML escaping), so JSON
// escaping and base64 growth are counted.
func encodedSize(m map[string]any) int64 {
	var c countWriter
	enc := json.NewEncoder(&c)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return math.MaxInt64
	}
	return c.n - 1 // Encode appends a newline
}

func kindName(k sqlitefile.Kind) string {
	switch k {
	case sqlitefile.KindInt:
		return "integer"
	case sqlitefile.KindFloat:
		return "real"
	case sqlitefile.KindText:
		return "text"
	case sqlitefile.KindBlob:
		return "blob"
	}
	return "null"
}

// charge subtracts the encoded size of m from the budget, if it fits.
func (b *valueBudget) charge(m map[string]any) bool {
	n := encodedSize(m)
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

// encodeValue is the payload form of column i. A value whose encoded size does
// not fit the budget becomes {i, type, len, omitted: "payload-budget"}: the
// column is never dropped silently and never exceeds the allowance.
func encodeValue(i int, v sqlitefile.Value, b *valueBudget) map[string]any {
	m := map[string]any{"i": int64(i), "type": kindName(v.Kind)}
	isBytes := v.Kind == sqlitefile.KindText || v.Kind == sqlitefile.KindBlob
	if isBytes {
		m["len"] = v.Len
	}
	if v.Serial != 0 && v.Serial <= math.MaxInt64 {
		m["serial"] = int64(v.Serial)
	}
	if v.Kind == sqlitefile.KindText {
		m["enc"] = v.Enc.String()
	}
	if v.Clipped {
		m["clipped"] = true
	}
	switch {
	case v.Unread:
		m["unread"] = true
		if v.Omitted {
			m["omitted"] = true
		}
	case v.Omitted:
		m["omitted"] = true
	default:
		// a text value costs at least half its stored bytes (UTF-16 to UTF-8),
		// so a hopeless one is refused before it is expanded
		if isBytes && int64(len(v.Bytes))/2 > b.left {
			return b.degrade(i, v)
		}
		fillValue(m, v)
	}
	if !b.charge(m) {
		return b.degrade(i, v)
	}
	return m
}

func fillValue(m map[string]any, v sqlitefile.Value) {
	switch v.Kind {
	case sqlitefile.KindInt:
		m["value"] = v.Int
	case sqlitefile.KindFloat:
		m["float_bits"] = fmt.Sprintf("%016x", math.Float64bits(v.Float))
		if !math.IsNaN(v.Float) && !math.IsInf(v.Float, 0) {
			m["value"] = strconv.FormatFloat(v.Float, 'g', -1, 64)
		}
	case sqlitefile.KindText:
		if s, ok := v.Text(); ok && !strings.ContainsRune(s, 0) {
			m["value"] = s
		} else {
			m["value_b64"] = base64.StdEncoding.EncodeToString(v.Bytes)
			m["text_valid"] = false
		}
	case sqlitefile.KindBlob:
		m["value_b64"] = base64.StdEncoding.EncodeToString(v.Bytes)
	}
}

func (b *valueBudget) degrade(i int, v sqlitefile.Value) map[string]any {
	m := map[string]any{"i": int64(i), "type": kindName(v.Kind), "omitted": "payload-budget"}
	if v.Kind == sqlitefile.KindText || v.Kind == sqlitefile.KindBlob {
		m["len"] = v.Len
	}
	b.left -= min(encodedSize(m), b.left)
	return m
}

// encodeName stores a table or index name exactly: as text under key when it
// is valid UTF-8 without NUL and short enough for the validator, else as
// base64 of its bytes under key+"_b64" (with key+"_valid" false when it is not
// valid text, so a name that is only long is not called invalid).
func encodeName(key string, name string, out map[string]any) {
	valid := utf8.ValidString(name) && !strings.ContainsRune(name, 0)
	if valid && len(name) <= maxNameBytes {
		out[key] = name
		return
	}
	out[key+"_b64"] = base64.StdEncoding.EncodeToString([]byte(name))
	if !valid {
		out[key+"_valid"] = false
	}
}
