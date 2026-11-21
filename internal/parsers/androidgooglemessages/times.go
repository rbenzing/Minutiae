package androidgooglemessages

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// The values of raw.time_basis (4I5-P2, B5).
const (
	basisReceived  = "received"
	basisSent      = "sent"
	basisAbsent    = "absent"
	basisInvalid   = "invalid"
	basisUndecided = "undecided"
)

// timeCell is one timestamp column as timesOf reads it.
type timeCell struct {
	in      rowrun.Instant
	kind    rowrun.CellKind
	nonZero bool // a stored integer other than the 0 sentinel
}

// readTime reads a millisecond column through rowrun.MillisFrom1973 (the 1e11 floor): the stored value goes to raw with its
// _status when it is not a time, and a cell that is not a decided integer is listed in unknown.
func readTime(row parse.Row, c rowrun.Cols, col string, raw, unknown map[string]any) timeCell {
	v, k := intCellOf(row, c.At(col))
	rowrun.Flag(unknown, row, col, c.At(col), k)
	t := timeCell{kind: k}
	if k == rowrun.CellValue {
		t.in = rowrun.MillisFrom1973(v)
		t.in.Record(raw, col)
		t.nonZero = v != 0
	}
	return t
}

// timesOf is the time of a message (spec 6.6, B5, 4I5-P2): Record.Time is received_timestamp when it is a time; else, when
// it is a stored 0 (or NULL, or the column is missing: no value), sent_timestamp when that is a time; else no Time. A
// received_timestamp that is non-zero but not a time (below 1e11, negative, beyond 2100) gives no Time and no fallback
// (time_basis invalid), and an unreadable one gives none either (undecided): neither is a decided zero. 0 is the sentinel
// (no time, never 1970). Times.received is the received time of an incoming message and Times.sent the sent time of
// any; direction is the decided direction. raw.time_basis states which column decided.
func timesOf(row parse.Row, c rowrun.Cols, raw, unknown map[string]any, direction string) (*records.Time, []records.NamedTime) {
	rc := readTime(row, c, "received_timestamp", raw, unknown)
	sc := readTime(row, c, "sent_timestamp", raw, unknown)
	var tm *records.Time
	var basis string
	switch {
	case rc.kind == rowrun.CellValue && rc.in.Valid():
		tm, basis = &records.Time{T: rc.in.T}, basisReceived
	case rc.kind == rowrun.CellValue && rc.nonZero:
		basis = basisInvalid
	case rc.kind == rowrun.CellOther || rc.kind == rowrun.CellUnknown:
		basis = basisUndecided
	default: // a stored 0, NULL or no column: B5 takes the sent time
		switch {
		case sc.kind == rowrun.CellValue && sc.in.Valid():
			tm, basis = &records.Time{T: sc.in.T}, basisSent
		case sc.kind == rowrun.CellValue && sc.nonZero:
			basis = basisInvalid
		case sc.kind == rowrun.CellOther || sc.kind == rowrun.CellUnknown:
			basis = basisUndecided
		default:
			basis = basisAbsent
		}
	}
	raw["time_basis"] = basis
	var times []records.NamedTime
	if rc.kind == rowrun.CellValue && rc.in.Valid() && direction == "in" {
		times = append(times, records.NamedTime{Kind: "received", Time: records.Time{T: rc.in.T}})
	}
	if sc.kind == rowrun.CellValue && sc.in.Valid() {
		times = append(times, records.NamedTime{Kind: "sent", Time: records.Time{T: sc.in.T}})
	}
	return tm, times
}
