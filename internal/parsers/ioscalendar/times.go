package ioscalendar

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// The raw states of the times (the plan's state vocabularies).
const (
	stateNotMidnight    = "all-day-not-midnight"
	stateNoStart        = "no-start"
	stateNoneStored     = "none-stored"
	stateEndBeforeStart = "end-before-start"
	stateMismatch       = "timing-mismatch"
	stateAllDayUndec    = "all-day-undecided"
	stateTZAbsent       = "start-tz-absent"
	stateTZTooLong      = "start-tz-too-long"
	stateTZUnreadable   = "start-tz-unreadable"
)

// What readTimes reads (from memory, spec 8.2): the all_day flags, the floating marker of start_tz and end_tz, the
// cap of a zone name (spec 4.1) and the table prefix of unknown_columns.
const (
	allDayFlag, timedFlag = 1, 0
	floatingZone          = "_float"
	maxZone               = 256
	unknownPrefix         = "CalendarItem."
)

// The Record.Times kinds of the item's stamps (spec 4.2, 8.2).
const (
	kindCreated  = "created"
	kindModified = "modified"
)

// eventTimes are the timing and the times of one CalendarItem row, read as stored (spec 5): the start and end of a
// timed row are utc instants; those of a floating row the stored wall-clock readings (local-unknown); those of an
// all-day row dates at a UTC midnight, stated as local-unknown readings with StartDate and EndDate. iOS has no
// original_start: orig_date is raw only, its basis unestablished (spec 6.2).
type eventTimes struct {
	Timing             string
	Start, End         *records.Time
	StartDate, EndDate string
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

// numberCellOf is cellx.NumberCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func numberCellOf(row parse.Row, col int) (cellx.Number, rowrun.CellKind) {
	n, k := cellx.NumberCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return n, k
}

// cocoaCell is one Cocoa-seconds column: its instant when it holds an INTEGER or a REAL (stated in raw with its
// status by Instant.Record), a stored NULL, a missing column, or an unreadable or wrongly typed cell (unknown_columns,
// nothing in raw: never a NULL).
type cocoaCell struct {
	inst cellx.Instant
	kind rowrun.CellKind
}

// ok says the cell holds a valid time.
func (c cocoaCell) ok() bool { return c.kind == rowrun.CellValue && c.inst.Valid() }

// readCocoa reads a Cocoa-seconds column through cellx.CocoaSeconds, never CocoaAuto (spec 5.4: a calendar has no
// nanosecond generation, so a nanosecond-magnitude value is out of range, never rescaled). A NULL is stated in raw as
// null when nullStated, else not at all.
func readCocoa(row parse.Row, c rowrun.Cols, name string, nullStated bool, raw, unknown map[string]any) cocoaCell {
	col := c.At(name)
	n, k := numberCellOf(row, col)
	cc := cocoaCell{kind: k}
	switch k {
	case rowrun.CellValue:
		cc.inst = cellx.CocoaSeconds(n)
		cc.inst.Record(raw, name)
	case rowrun.CellNull:
		if nullStated {
			raw[name] = nil
		}
	default:
		rowrun.Flag(unknown, row, unknownPrefix+name, col, k)
	}
	return cc
}

// readTimes reads the timing and the times of a CalendarItem row (spec 5, 8.2, B1, B5, B6). The class of the start is
// all_day 1 → all-day; all_day 0 → the class of start_tz (zoneClass); anything else unknown timing with
// raw.timing_state and no time. The end of an all-day record is all-day; any other end takes the class of end_tz, and
// a class that differs from the start's gives no end (B6). No readable start gives no end (B5, before every other end
// rule). An unreadable start or end leaves what depends on it undecided: no time and no state that claims an absence.
func readTimes(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) eventTimes {
	et := eventTimes{Timing: readTiming(row, c, raw, unknown)}
	start := readCocoa(row, c, "start_date", true, raw, unknown)
	end := readCocoa(row, c, "end_date", true, raw, unknown)
	if et.Timing == calendar.TimingUnknown {
		return et
	}
	if start.ok() {
		et.Start, et.StartDate = readingOf(et.Timing, start.inst)
		if et.Start == nil {
			raw["start_state"] = stateNotMidnight
		}
	}
	if et.Start == nil {
		raw["end_state"] = stateNoStart // B5, before any other end rule
		return et
	}
	endClass := calendar.TimingAllDay
	if et.Timing != calendar.TimingAllDay {
		endClass, _ = zoneClass(row, c, "end_tz", unknown)
	}
	switch {
	case endClass != et.Timing:
		raw["end_state"] = stateMismatch // B6: also an end_tz whose class cannot be decided (Task 7 ruling)
		return et
	case end.kind == rowrun.CellNull || end.kind == rowrun.CellMissing:
		raw["end_state"] = stateNoneStored
		return et
	case !end.ok():
		return et // unreadable, or stored but not a time (its raw status says why)
	case end.inst.T.Before(start.inst.T):
		raw["end_state"] = stateEndBeforeStart
		return et
	}
	et.End, et.EndDate = readingOf(et.Timing, end.inst)
	if et.End == nil {
		raw["end_state"] = stateNotMidnight // B1: a 23:59:59 end included
	}
	return et
}

// readingOf is the record time of a valid Cocoa instant for a decided timing (spec 5.1, 5.2): a utc instant when
// timed; the Cocoa reading as if UTC, local-unknown, when floating; the date at a UTC midnight, local-unknown, when
// all-day (nil off midnight: no zone could repair it, so none is tried).
func readingOf(timing string, inst cellx.Instant) (*records.Time, string) {
	switch timing {
	case calendar.TimingTimed:
		return &records.Time{T: inst.T, Basis: records.BasisUTC}, ""
	case calendar.TimingFloating:
		return &records.Time{T: inst.T, Basis: records.BasisLocalUnknown}, ""
	}
	if d, ok := calendar.DateOfMidnight(inst.T); ok {
		return &records.Time{T: inst.T, Basis: records.BasisLocalUnknown}, d
	}
	return nil, ""
}

// readTiming reads the timing class of the start (spec 5.1, 8.2, the plan's ios.calendar vocabulary).
func readTiming(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) string {
	col := c.At("all_day")
	v, k := intCellOf(row, col)
	switch {
	case k == rowrun.CellValue && v == allDayFlag:
		return calendar.TimingAllDay
	case k == rowrun.CellValue && v == timedFlag:
		class, state := zoneClass(row, c, "start_tz", unknown)
		if class == calendar.TimingUnknown {
			raw["timing_state"] = state
		}
		return class
	case k == rowrun.CellValue:
		raw["all_day"] = v
	case k == rowrun.CellNull:
		raw["all_day"] = nil
	default:
		rowrun.Flag(unknown, row, unknownPrefix+"all_day", col, k)
		if v, ok := storedOther(row, col); ok {
			raw["all_day"] = v // C1: a readable value of another class is kept as stored
		}
	}
	raw["timing_state"] = stateAllDayUndec
	return calendar.TimingUnknown
}

// zoneClass is the timing class a zone column names with all_day 0 (spec 8.2): floating for the marker _float, timed
// for any other non-empty TEXT of at most maxZone bytes as cleaned (an invalid byte grows to the three bytes of
// U+FFFD), else unknown with the start-tz state: absent (NULL, empty, a missing column), too-long, or unreadable (a
// cell of another class or one the reader could not read, listed in unknown_columns). The text is measured, never
// copied: a hostile cell over the cap costs nothing.
func zoneClass(row parse.Row, c rowrun.Cols, name string, unknown map[string]any) (class, state string) {
	col := c.At(name)
	b, k := textCellOf(row, col)
	switch {
	case k == rowrun.CellOther || k == rowrun.CellUnknown:
		rowrun.Flag(unknown, row, unknownPrefix+name, col, k)
		return calendar.TimingUnknown, stateTZUnreadable
	case k != rowrun.CellValue || len(b) == 0:
		return calendar.TimingUnknown, stateTZAbsent
	case string(b) == floatingZone:
		return calendar.TimingFloating, ""
	case !cleanFits(b, maxZone):
		return calendar.TimingUnknown, stateTZTooLong
	}
	return calendar.TimingTimed, ""
}

// cleanFits says validText(b) is at most limit bytes, without building it: a stored text over the limit is refused
// at once (validText never shrinks its input), and one within it is measured rune by rune.
func cleanFits(b []byte, limit int) bool {
	if len(b) > limit {
		return false
	}
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			n += utf8.RuneLen(utf8.RuneError)
		} else {
			n += size
		}
		b = b[size:]
	}
	return n <= limit
}

// readStamps reads the item's creation and modification stamps (spec 4.2, 8.2): Cocoa seconds, utc instants, with
// raw.<col>_status when the value is no time; a NULL states nothing.
func readStamps(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) []records.NamedTime {
	var out []records.NamedTime
	for _, s := range [...]struct{ col, kind string }{{"creation_date", kindCreated}, {"last_modified", kindModified}} {
		if cc := readCocoa(row, c, s.col, false, raw, unknown); cc.ok() {
			out = append(out, records.NamedTime{Kind: s.kind, Time: records.Time{T: cc.inst.T, Basis: records.BasisUTC}})
		}
	}
	return out
}

// storedOther is a readable cell of another storage class as raw keeps it (C1): TEXT as its string with each byte
// that is not valid UTF-8 replaced by U+FFFD, a finite REAL as its number, a BLOB as {"blob_b64": ...}. It is false
// for an unreadable cell and for a NaN or an infinity (no stored value JSON can hold).
func storedOther(row parse.Row, col int) (any, bool) {
	if col < 0 || !rowrun.StateOf(row, col).Known() || nanCell(row, col) {
		return nil, false
	}
	if b, ok := row.Text(col); ok {
		return validText(b), true
	}
	if f, ok := row.Float(col); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f, true
	}
	if b, ok := row.Blob(col); ok {
		return map[string]any{"blob_b64": base64(b)}, true
	}
	return nil, false
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
