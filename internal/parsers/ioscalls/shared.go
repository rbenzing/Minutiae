package ioscalls

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

const (
	tableCore   = "ZCALLRECORD"
	tableLegacy = "call"

	maxText     = 256 << 10     // bytes of any stored text kept
	maxDuration = 1_000_000_000 // seconds: a longer call has a duration but no end time
)

// names lists the column names of specs.
func names(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// markText lists field in raw.text_flags (sorted): its text was altered (invalid UTF-8 or NUL became U+FFFD,
// or it was cut). The original bytes stay in the artifact; nothing is altered without a flag.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	if slices.Contains(cur, any(field)) {
		return
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// keepOriginal states what CleanText altered: the original bytes (base64, or their hash and length when
// large) and the full length of a text that was cut.
func keepOriginal(raw map[string]any, field string, c common.Cleaned) {
	if c.RawB64 != "" {
		raw[field+"_raw_b64"] = c.RawB64
	}
	if c.RawSHA256 != "" {
		raw[field+"_raw_sha256"] = c.RawSHA256
		raw[field+"_raw_len"] = int64(c.RawLen)
	}
	if c.Truncated {
		raw[field+"_total_bytes"] = int64(c.TotalBytes)
	}
}

// cleanField is common.CleanText for a free-text field that has no flag slot in the typed payload: an
// altered text is listed in raw.text_flags and its original is kept under raw.<field>_raw_b64.
func cleanField(raw map[string]any, field string, b []byte) string {
	c := common.CleanText(b, maxText)
	if len(c.Flags) > 0 || c.Truncated {
		markText(raw, field)
	}
	keepOriginal(raw, field, c)
	return c.Text
}

// textCell reads a TEXT column. A column that holds another storage class or cannot be read is named in
// unknown, never read as empty.
func textCell(row parse.Row, c rowrun.Cols, col string, unknown map[string]any) ([]byte, bool) {
	b, k := rowrun.TextCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, c.At(col), k)
		return nil, false
	}
	return b, true
}

// namedField is cleanField for a text that may be empty: an empty value is absent with raw.<field>_state "empty",
// as an empty address is (raw.address_state), so an empty text is stated and not mistaken for a NULL one.
func namedField(raw map[string]any, field string, b []byte) string {
	if len(b) == 0 {
		raw[field+"_state"] = "empty"
		return ""
	}
	return cleanField(raw, field, b)
}

// ints reads the integer columns and states each value read in raw under its column name; the others are
// named in unknown. It returns the values read.
func ints(row parse.Row, c rowrun.Cols, cols []string, raw, unknown map[string]any) map[string]int64 {
	got := map[string]int64{}
	for _, col := range cols {
		if v, k := rowrun.IntCell(row, c.At(col)); k == rowrun.CellValue {
			raw[col] = v
			got[col] = v
		} else {
			rowrun.Flag(unknown, row, col, c.At(col), k)
		}
	}
	return got
}

// readFlag reads the 0/1 read column: any other integer is stated in raw, not guessed.
func readFlag(row parse.Row, c rowrun.Cols, col string, raw, unknown map[string]any) *bool {
	v, k := rowrun.IntCell(row, c.At(col))
	switch {
	case k != rowrun.CellValue:
		rowrun.Flag(unknown, row, col, c.At(col), k)
	case v == 0 || v == 1:
		b := v == 1
		return &b
	default:
		raw[col] = v
	}
	return nil
}

// address reads the address column: TEXT or BLOB through CleanText. NULL is absent; an empty value is absent
// with raw.address_state "empty"; a BLOB is marked raw.address_storage "blob"; an altered address lists its
// flags in raw.address_flags and keeps the original under raw.address_raw_b64.
func address(row parse.Row, c rowrun.Cols, col string, raw, unknown map[string]any) string {
	i := c.At(col)
	b, k := rowrun.TextCell(row, i)
	blob := false
	if k == rowrun.CellOther {
		if bb, ok := row.Blob(i); ok {
			b, k, blob = bb, rowrun.CellValue, true
		}
	}
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, i, k)
		return ""
	}
	if blob {
		raw["address_storage"] = "blob"
	}
	if len(b) == 0 {
		raw["address_state"] = "empty"
		return ""
	}
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		fl := make([]any, len(cl.Flags))
		for i, f := range cl.Flags {
			fl[i] = f
		}
		raw["address_flags"] = fl
	}
	keepOriginal(raw, "address", cl)
	return cl.Text
}

// duration reads a duration column through cellx. A finite value >= 0 is returned; a negative one is stated in
// raw.<col> with raw.<col>_status "negative"; a non-finite REAL by its IEEE bits (raw.<col>_ieee, status
// "invalid", no raw.<col> key: ruling P-4-1); an INTEGER beyond 2^53 is not a float64, so it is unknown
// ("precision") and stays in raw. keep also states a returned value in raw.<col>.
func duration(row parse.Row, c rowrun.Cols, col string, keep bool, raw, unknown map[string]any) (float64, bool) {
	n, k := cellx.NumberCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, c.At(col), k)
		return 0, false
	}
	if !n.IsInt && (math.IsNaN(n.F) || math.IsInf(n.F, 0)) {
		raw[col+"_ieee"] = strconv.FormatUint(math.Float64bits(n.F), 16)
		raw[col+"_status"] = ts.StatusInvalid.String()
		return 0, false
	}
	if (n.IsInt && n.I < 0) || (!n.IsInt && n.F < 0) {
		raw[col] = n.Raw()
		raw[col+"_status"] = "negative"
		return 0, false
	}
	f, ok := n.Float()
	if !ok {
		unknown[col] = "precision"
		raw[col] = n.Raw()
		return 0, false
	}
	if f == 0 {
		f = 0 // never -0
	}
	if keep {
		if n.IsInt {
			raw[col] = n.I
		} else {
			raw[col] = f
		}
	}
	return f, true
}

// instant reads a date column through conv and states it in raw (value, status, and for a non-finite REAL its
// IEEE bits). The time is returned only for a valid instant.
func instant(row parse.Row, c rowrun.Cols, col string, conv func(cellx.Number) cellx.Instant, raw, unknown map[string]any) (time.Time, bool) {
	n, k := cellx.NumberCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, c.At(col), k)
		return time.Time{}, false
	}
	in := conv(n)
	in.Record(raw, col)
	return in.T, in.Valid()
}

// endTime is start + duration for 0 < duration <= 1e9 seconds, and only while the result is a representable
// time; otherwise there is no end.
func endTime(start time.Time, dur float64, durOK bool) *records.Time {
	if !durOK || !(dur > 0 && dur <= maxDuration) {
		return nil
	}
	end := start.Add(time.Duration(math.Round(dur * 1e9)))
	if !end.Before(ts.Max()) {
		return nil
	}
	return &records.Time{T: end}
}

// finish completes a record: the flags, the damage of the row, the typed payload and the summary.
func finish(rec records.Record, cl call.Call, raw, unknown map[string]any, row parse.Row) records.Record {
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	if len(raw) > 0 {
		cl.Raw = raw
	}
	rec.Summary = call.Summary(cl.Direction, cl.Outcome, cl.Address)
	rec.Payload = cl.Payload()
	return rec
}

// newRecord is the record skeleton at the row's place; SourcePath is the remote path of the file that holds
// the cell (ruling T48).
func newRecord(place rowrun.Place) records.Record {
	return records.Record{Type: call.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
}
