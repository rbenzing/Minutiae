package androidcalendar

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// The raw states of the times (the plan's state vocabularies).
const (
	stateUndecided      = "undecided"
	stateNotMidnight    = "all-day-not-midnight"
	stateNoStart        = "no-start"
	stateNoneStored     = "none-stored"
	stateEndBeforeStart = "end-before-start"
	stateNotUTC         = "not-utc"
	stateOutOfRange     = "out-of-range"
)

// The values of raw.time_end_basis (4I3-P7).
const (
	basisDtend    = "dtend"
	basisDuration = "dtstart+duration"
)

// What readTimes reads: the Events flags of CalendarContract (allDay 0 and 1), the provider's all-day zone, the unit
// of the duration sum, and the table prefix of unknown_columns.
const (
	allDayFlag, timedFlag = 1, 0
	utcZone               = "UTC"
	millisPerSecond       = 1000
	unknownPrefix         = "Events."
)

// eventTimes are the timing and the times of one Events row, read as stored (spec 5): the start and end of a timed
// row are utc instants; those of an all-day row are dates at a UTC midnight, stated as local-unknown readings with
// StartDate and EndDate; OriginalStart is the start of the occurrence an exception replaces.
type eventTimes struct {
	Timing             string
	Start, End         *records.Time
	StartDate, EndDate string
	OriginalStart      *records.Time
}

// nanCell says a cell is a stored REAL NaN: the reader (as the engine) reads it as NULL but keeps the serial type 7,
// so it is detectable here and is never read as absent (unknown is never absent, 4I2-A6).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// intCellOf is rowrun.IntCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func intCellOf(row parse.Row, col int) (int64, rowrun.CellKind) {
	v, k := rowrun.IntCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return v, k
}

// textCellOf is rowrun.TextCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func textCellOf(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	b, k := rowrun.TextCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return b, k
}

// millisCell is one millisecond column: its instant when it holds an integer (stated in raw with its status by
// Instant.Record), a stored NULL (raw null), a missing column (nothing), or an unreadable or wrongly typed cell
// (unknown_columns, nothing in raw: never a NULL).
type millisCell struct {
	inst rowrun.Instant
	kind rowrun.CellKind
}

// ok says the cell holds a valid time.
func (m millisCell) ok() bool { return m.kind == rowrun.CellValue && m.inst.Valid() }

func readMillis(row parse.Row, c rowrun.Cols, name string, raw, unknown map[string]any) millisCell {
	col := c.At(name)
	v, k := intCellOf(row, col)
	m := millisCell{kind: k}
	switch k {
	case rowrun.CellValue:
		m.inst = rowrun.Millis(v)
		m.inst.Record(raw, name)
	case rowrun.CellNull:
		raw[name] = nil
	default:
		flagCell(unknown, row, unknownPrefix+name, col, k)
	}
	return m
}

// readTimes reads the timing and the times of an Events row (spec 5, B5, 4I3-P7, 4I3-P8). allDay 0 is timed, 1
// all-day; any other value, a NULL or an unreadable cell is unknown timing with no time (raw.timing_state). A start
// or end that is unreadable leaves what depends on it undecided: no time and no state that would claim an absence.
func readTimes(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) eventTimes {
	et := eventTimes{Timing: readTiming(row, c, raw, unknown)}
	start := readMillis(row, c, "dtstart", raw, unknown)
	dtend := readMillis(row, c, "dtend", raw, unknown)
	if et.Timing == calendar.TimingAllDay {
		if tz, k := textCellOf(row, c.At("eventTimezone")); k == rowrun.CellValue && string(tz) != utcZone {
			raw["all_day_zone_state"] = stateNotUTC // the provider stores all-day events in UTC; the zone is never applied
		}
	}
	et.OriginalStart = readOriginalStart(row, c, raw, unknown)
	if et.Timing == calendar.TimingUnknown {
		return et
	}
	if start.ok() {
		if et.Timing == calendar.TimingTimed {
			et.Start = &records.Time{T: start.inst.T, Basis: records.BasisUTC}
		} else if d, ok := calendar.DateOfMidnight(start.inst.T); ok {
			et.Start, et.StartDate = &records.Time{T: start.inst.T, Basis: records.BasisLocalUnknown}, d
		} else {
			raw["start_state"] = stateNotMidnight
		}
	}
	if et.Start == nil {
		raw["end_state"] = stateNoStart // B5, before any other end rule
		return et
	}
	endMs, basis, decided := endOf(row, c, start.inst.Raw, dtend, raw, unknown)
	if !decided {
		return et
	}
	if basis == "" {
		raw["end_state"] = stateNoneStored
		return et
	}
	if endMs < start.inst.Raw {
		raw["end_state"] = stateEndBeforeStart
		return et
	}
	end := rowrun.Millis(endMs)
	if !end.Valid() {
		raw["end_state"] = stateOutOfRange // a computed end outside the decoder's range is no time (coordinator ruling D1)
		return et
	}
	if et.Timing == calendar.TimingTimed {
		et.End = &records.Time{T: end.T, Basis: records.BasisUTC}
	} else if d, ok := calendar.DateOfMidnight(end.T); ok {
		et.End, et.EndDate = &records.Time{T: end.T, Basis: records.BasisLocalUnknown}, d
	} else {
		raw["end_state"] = stateNotMidnight
		return et
	}
	raw["time_end_basis"] = basis
	return et
}

// readTiming reads allDay (spec 5.1, 7.3).
func readTiming(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) string {
	col := c.At("allDay")
	v, k := intCellOf(row, col)
	switch {
	case k == rowrun.CellValue && v == timedFlag:
		return calendar.TimingTimed
	case k == rowrun.CellValue && v == allDayFlag:
		return calendar.TimingAllDay
	case k == rowrun.CellValue:
		raw["allDay"] = v
	case k == rowrun.CellNull:
		raw["allDay"] = nil
	default:
		flagCell(unknown, row, unknownPrefix+"allDay", col, k)
		keepOther(raw, "allDay", row, col, putTop) // C1: a readable value of another class is kept as stored (ruling D6)
	}
	raw["timing_state"] = stateUndecided
	return calendar.TimingUnknown
}

// endOf applies the provider's end rule (spec 5.3): a stored, parseable duration gives dtstart + duration (and a
// stored dtend that disagrees is a stated conflict); else a stored dtend; else none (basis ""). decided is false when
// the end cannot be decided (an unreadable duration or dtend, a duration that does not parse): no end and no state
// that claims there is none.
func endOf(row parse.Row, c rowrun.Cols, startMs int64, dtend millisCell, raw, unknown map[string]any) (endMs int64, basis string, decided bool) {
	col := c.At("duration")
	dur, k := textCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		secs, state := parseDuration(dur)
		if state != "" {
			if state == durTooLong {
				state = durUnparsed // the vocabulary of raw.duration_state; the length and hash are the mapping's
			}
			raw["duration_state"] = state // no fallback to dtend (spec 5.3)
			return 0, "", false
		}
		endMs = startMs + secs*millisPerSecond
		if dtend.kind == rowrun.CellValue && dtend.inst.Raw != endMs { // both stored (spec 5.3), a sentinel dtend included
			raw["end_conflict"] = true // duration wins, as in the provider
		}
		return endMs, basisDuration, true
	case rowrun.CellOther, rowrun.CellUnknown:
		flagCell(unknown, row, unknownPrefix+"duration", col, k)
		return 0, "", false
	}
	switch {
	case dtend.ok():
		return dtend.inst.Raw, basisDtend, true
	case dtend.kind == rowrun.CellNull || dtend.kind == rowrun.CellMissing:
		return 0, "", true
	}
	return 0, "", false // dtend unreadable, or stored but not a time (its raw status says why)
}

// readOriginalStart reads the start of the occurrence an exception row replaces (spec 5.2, 6.2): originalAllDay 0
// is a utc instant, 1 the date rule of the start, anything else undecided.
func readOriginalStart(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) *records.Time {
	oit := readMillisIfStored(row, c, "originalInstanceTime", raw, unknown)
	if !oit.ok() && oit.kind != rowrun.CellValue {
		return nil
	}
	col := c.At("originalAllDay")
	oad, k := intCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		raw["originalAllDay"] = oad
	case rowrun.CellNull:
		raw["originalAllDay"] = nil
	case rowrun.CellOther, rowrun.CellUnknown:
		flagCell(unknown, row, unknownPrefix+"originalAllDay", col, k)
	}
	if !oit.ok() {
		return nil // stored but not a time: originalInstanceTime_status says why
	}
	switch {
	case k == rowrun.CellValue && oad == timedFlag:
		return &records.Time{T: oit.inst.T, Basis: records.BasisUTC}
	case k == rowrun.CellValue && oad == allDayFlag:
		if _, ok := calendar.DateOfMidnight(oit.inst.T); ok {
			return &records.Time{T: oit.inst.T, Basis: records.BasisLocalUnknown}
		}
		raw["original_start_state"] = stateNotMidnight
		return nil
	}
	raw["original_start_state"] = stateUndecided
	return nil
}

// readMillisIfStored is readMillis for a column that only an exception row fills: a NULL is not stated in raw.
func readMillisIfStored(row parse.Row, c rowrun.Cols, name string, raw, unknown map[string]any) millisCell {
	col := c.At(name)
	if _, k := intCellOf(row, col); k == rowrun.CellNull || k == rowrun.CellMissing {
		return millisCell{kind: k}
	}
	return readMillis(row, c, name, raw, unknown)
}

// storedOther is a readable cell of another storage class as raw keeps it (C1): TEXT as its string with each byte
// that is not valid UTF-8 replaced by U+FFFD, a finite REAL as its number, a BLOB as {"blob_b64": ...}. It is false
// for an unreadable cell and for a NaN or an infinity (no stored value JSON can hold). A TEXT or BLOB over maxStored
// is not kept: its stored bytes are returned as over (ok false), for putStored to state, never cut (final review I2).
func storedOther(row parse.Row, col int) (v any, over []byte, ok bool) {
	if col < 0 || !rowrun.StateOf(row, col).Known() || nanCell(row, col) {
		return nil, nil, false
	}
	if b, ok := row.Text(col); ok {
		if len(b) > maxStored {
			return nil, b, false
		}
		return validText(b), nil, true
	}
	if f, ok := row.Float(col); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, nil, true
	}
	if b, ok := row.Blob(col); ok {
		if len(b) > maxStored {
			return nil, b, false
		}
		return map[string]any{"blob_b64": base64(b)}, nil, true
	}
	return nil, nil, false
}

// validText is b as a string, each byte that does not start a valid UTF-8 sequence replaced by U+FFFD.
func validText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:n])
		}
		b = b[n:]
	}
	return sb.String()
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64 is standard padded base64 (encoding/base64 is not importable by a parser).
func base64(b []byte) string {
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := min(3, len(b)-i)
		for j := range n {
			v |= uint32(b[i+j]) << (16 - 8*j)
		}
		for j := range 4 {
			if j <= n {
				out = append(out, b64Alphabet[v>>(18-6*j)&0x3f])
			} else {
				out = append(out, '=')
			}
		}
	}
	return string(out)
}
