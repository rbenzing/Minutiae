package dbrecover

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// valueBudget is the part of a row's payload allowance that is left after the
// degraded form of every column has been reserved (J35): the extra bytes a
// column may spend to be encoded in full. It is a hard cap on the row.
type valueBudget struct {
	left int64
	err  error // the first size that could not be measured (errPayloadSize); the row then fails closed
}

func newValueBudget(n int64) *valueBudget { return &valueBudget{left: max(n, 0)} }

// errPayloadSize: a payload could not be measured, so its size is unknown. The
// row is refused; no sentinel size ever enters a sum (J113).
var errPayloadSize = errors.New("dbrecover: a payload could not be measured")

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// encodedSize is the size of m as canonical JSON (no HTML escaping), so JSON
// escaping and base64 growth are counted.
func encodedSize(m map[string]any) (int64, error) {
	var c countWriter
	enc := json.NewEncoder(&c)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return 0, fmt.Errorf("%w: %w", errPayloadSize, err)
	}
	return c.n - 1, nil // Encode appends a newline
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

// degraded is the form of column i that carries no value: it is the reserve of
// every column, so degrading never spends unreserved budget.
func degraded(i int, v sqlitefile.Value) map[string]any {
	m := map[string]any{"i": int64(i), "type": kindName(v.Kind), "omitted": "payload-budget"}
	if v.Kind == sqlitefile.KindText || v.Kind == sqlitefile.KindBlob {
		m["len"] = v.Len
	}
	if v.Omitted && !v.Unread && v.DefaultUndecided != sqlitefile.DefaultReasonNone {
		m["default_undecided"] = v.DefaultUndecided.String() // the reason is evidence and tiny
	}
	return m
}

// minEncodedBytes is a lower bound of the encoded size of a text or blob
// value: a text costs at least half its stored bytes (UTF-16 to UTF-8), a
// blob its base64 size 4*ceil(n/3). A hopeless value is refused before it is
// expanded.
func minEncodedBytes(v sqlitefile.Value) int64 {
	n := int64(len(v.Bytes))
	if v.Kind == sqlitefile.KindBlob {
		return 4 * ((n + 2) / 3)
	}
	return n / 2
}

// encodeValue is the payload form of column i. The column is encoded in full
// only when its extra size over the (already reserved) degraded form fits what
// is left; otherwise it becomes {i, type, len, omitted: "payload-budget"}: never
// dropped silently and never over the allowance.
func encodeValue(i int, v sqlitefile.Value, b *valueBudget) map[string]any {
	deg := degraded(i, v)
	degSize, err := encodedSize(deg)
	if err != nil {
		b.err = errors.Join(b.err, err)
		return deg
	}
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
		if v.DefaultUndecided != sqlitefile.DefaultReasonNone {
			m["default_undecided"] = v.DefaultUndecided.String()
		}
	default:
		if isBytes && minEncodedBytes(v) > b.left+degSize {
			return deg
		}
		fillValue(m, v)
	}
	full, err := encodedSize(m)
	if err != nil {
		b.err = errors.Join(b.err, err)
		return deg
	}
	extra := max(full-degSize, 0)
	if extra > b.left {
		return deg
	}
	b.left -= extra
	return m
}

// encodeColumns encodes a whole row. Before any column is encoded, the
// degraded form of every column plus its separator is reserved, so the encoded
// columns never exceed max(allowance, that reservation): the reservation is
// the floor of a row with this many columns.
func encodeColumns(vals []sqlitefile.Value, allowance int64) ([]any, error) {
	var reserved int64
	for i, v := range vals {
		n, err := encodedSize(degraded(i, v))
		if err != nil {
			return nil, err
		}
		reserved += n + 1
	}
	b := newValueBudget(allowance - reserved)
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = encodeValue(i, v, b)
	}
	return out, b.err
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
