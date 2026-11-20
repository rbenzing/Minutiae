package androidcalendar

import (
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The child tables the mapping joins by event_id (spec 7.3), their list cap (spec 4.1, 9: the telephony list cap),
// the cap of an extended property's value and the child states.
const (
	tableAttendees    = "Attendees"
	tableReminders    = "Reminders"
	tableAlerts       = "CalendarAlerts"
	tableProperties   = "ExtendedProperties"
	tableRawTimes     = "EventsRawTimes"
	maxList           = 1000
	maxProp           = 4 << 10
	stateNegativeMins = "negative-minutes"
)

// childStateKeys are the raw state keys of the child joins: a recovered row (nil env) joins none of them and states
// each as "not-joined-recovered" (4I3-P4, B2). A fresh slice per call: the package holds no mutable state.
func childStateKeys() []string {
	return []string{"attendees_state", "reminders_state", "alerts_state", "extended_properties_state", "raw_times_state"}
}

// childColumns are the columns each child table's join reads (or names, event_id), for the note
// unmapped_columns.<table> (fix round 1, M7).
func childColumns(table string) []string {
	switch table {
	case tableAttendees:
		return []string{"event_id", "attendeeEmail", "attendeeName", "attendeeRelationship", "attendeeType", "attendeeStatus", "attendeeIdentity", "attendeeIdNamespace"}
	case tableReminders:
		return []string{"event_id", "minutes", "method"}
	case tableAlerts:
		return append([]string{"event_id"}, alertCols()...)
	case tableProperties:
		return []string{"event_id", "name", "value"}
	case tableRawTimes:
		return append([]string{"event_id"}, rawTimesCols()...)
	}
	return nil
}

// childRows returns the rows of table whose event_id is id, in row-id order. undecided is true when the answer is not
// trusted (spec 7.3, 9): a table the database lacks or that cannot be read, a lossy or over-cap join index, a guarded
// table, an undecidable key; the join is then named in raw.join_undecided and states nothing else. Any other error
// (the budget, a cancel) fails the job.
func childRows(m parse.MapContext, table string, id int64, raw map[string]any) ([]parse.Row, bool, error) {
	rows, undecided, err := rowrun.Match(m, table, "event_id", id)
	switch {
	case err != nil && (errors.Is(err, sqlitedb.ErrNoSuchTable) || errors.Is(err, sqlitedb.ErrIndexLimit) || dataDamage(err)):
		undecided = true
	case err != nil:
		return nil, false, err
	}
	if undecided {
		undecidedJoin(raw, table)
		return nil, true, nil
	}
	slices.SortStableFunc(rows, func(a, b parse.Row) int {
		x, _ := a.Rowid()
		y, _ := b.Rowid()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	return rows, false, nil
}

// childCells reads the cells of one child row; an unreadable or wrongly typed cell is listed in unknown_columns as
// "<table>.<col>".
func childCells(m parse.MapContext, r cells, table string, row parse.Row, names ...string) cells {
	return cells{row: row, c: rowrun.Resolve(m, table, names...), raw: map[string]any{}, unknown: r.unknown, prefix: table + "."}
}

// capCount is the _total and _truncated pair of a list of n items cut at maxList (C6): nil and false within the cap.
func capCount(n int) (*int64, bool) {
	if n <= maxList {
		return nil, false
	}
	t := int64(n)
	return &t, true
}

// putEntry keeps a stored value v in a child entry under key. A child entry is a map inside a raw list (level 4), so
// it holds scalars only (B8): a BLOB, which raw elsewhere states as {"blob_b64": ...}, is flattened to the sibling
// scalar <key>_blob_b64 (fix round 1, C1).
func putEntry(entry map[string]any, key string, v any) {
	if b, ok := v.(map[string]any); ok {
		if s, ok := b["blob_b64"].(string); ok {
			entry[key+"_blob_b64"] = s
		}
		return
	}
	entry[key] = v
}

// storeCell keeps the stored value of column name of c in entry under the same name (putEntry), if it has one.
func storeCell(c cells, entry map[string]any, name string) {
	if v, ok := c.stored(name); ok {
		putEntry(entry, name, v)
	}
}

// childCode maps a stored child code of col by the contract's column enum (spec 2): the stored value is kept in entry
// and its label returned, "unknown" for a value the contract does not define; a wrongly typed or unreadable cell is
// also listed in unknown_columns; NULL states nothing.
func childCode(c cells, col, enum string, entry map[string]any) string {
	i := c.c.At(col)
	v, k := intCellOf(c.row, i)
	switch k {
	case rowrun.CellNull, rowrun.CellMissing:
		return ""
	case rowrun.CellValue:
		entry[col] = v
		if l, ok := lookupEnum(enum, v); ok {
			return l
		}
		return labelUnknown
	}
	rowrun.Flag(c.unknown, c.row, c.prefix+col, i, k)
	if s, ok := storedOther(c.row, i); ok {
		putEntry(entry, col, s)
	}
	return labelUnknown
}

// readAddress reads attendeeEmail into a and entry: TEXT within 1 KiB is the address; longer is not kept (an
// identifier is never cut) and entry states address_state "too-long", address_len and address_sha256 (F27); a cell of
// another class or an unreadable one keeps its stored value in entry (when it has one), is listed in unknown_columns
// and states address_state "undecided" (fix round 1, I3). It reports whether the address is undecided: such a row is
// not one with "neither address nor name".
func readAddress(c cells, a *calendar.Attendee, entry map[string]any) bool {
	col := c.c.At("attendeeEmail")
	b, k := textCellOf(c.row, col)
	switch k {
	case rowrun.CellValue:
		switch {
		case len(b) == 0:
		case len(b) > maxID:
			entry["address_state"] = stateTooLong
			entry["address_len"] = int64(len(b))
			entry["address_sha256"] = sha256Hex(b)
		default:
			a.Address = validText(b)
		}
	case rowrun.CellOther, rowrun.CellUnknown:
		rowrun.Flag(c.unknown, c.row, c.prefix+"attendeeEmail", col, k)
		storeCell(c, entry, "attendeeEmail")
		entry["address_state"] = stateUndecided
		return true
	}
	return false
}

// readName reads attendeeName into a: TEXT is the name, cleaned at 4 KiB (an altered name puts "attendees" in
// raw.text_flags); a cell of another class or an unreadable one is listed in unknown_columns, keeps its stored value in
// entry (when it has one) and states name_state "undecided", as readAddress does (coordinator ruling h). It reports
// whether the name is undecided: such a row is not one with "neither address nor name".
func readName(c cells, a *calendar.Attendee, entry map[string]any, flags textFlags) bool {
	col := c.c.At("attendeeName")
	b, k := textCellOf(c.row, col)
	switch k {
	case rowrun.CellValue:
		if len(b) > 0 {
			cl := common.CleanText(b, maxTitle)
			if len(cl.Flags) > 0 {
				flags["attendees"] = true
			}
			a.Name = cl.Text
		}
	case rowrun.CellOther, rowrun.CellUnknown:
		rowrun.Flag(c.unknown, c.row, c.prefix+"attendeeName", col, k)
		storeCell(c, entry, "attendeeName")
		entry["name_state"] = stateUndecided
		return true
	}
	return false
}

// attendeesOf joins the event's Attendees (spec 7.3): attendeeEmail is the address (readAddress), attendeeName the
// name (readName), the codes labelled by the contract.
// raw.attendees holds each attendee's stored codes, attendeeIdentity, attendeeIdNamespace and address and name states,
// strictly parallel to attendees (spec 7.3 "aligned by order"; fix round 1, I2). A row with no name and no address is
// not an attendee: with neither (an over-long address counts as none, F27; an undecided address or name is not
// "neither", ruling h) it is counted in raw.attendees_unnamed; its entry, when it states an address (over-long or
// undecided) or an undecided name, goes to raw.attendees_unnamed_entries (at most maxList, with
// _total and _truncated). It returns the attendees (at most maxList), their total and whether they were cut.
func attendeesOf(m parse.MapContext, id int64, r cells, flags textFlags) ([]calendar.Attendee, *int64, bool, error) {
	rows, undecided, err := childRows(m, tableAttendees, id, r.raw)
	if err != nil || undecided {
		return nil, nil, false, err
	}
	var (
		list                  []calendar.Attendee
		rawList, unnamedList  []any
		named, unnamedEntries int
		unnamed               int64
	)
	for _, row := range rows {
		c := childCells(m, r, tableAttendees, row, "attendeeEmail", "attendeeName", "attendeeRelationship", "attendeeType",
			"attendeeStatus", "attendeeIdentity", "attendeeIdNamespace")
		var a calendar.Attendee
		entry := map[string]any{}
		addrUndecided := readAddress(c, &a, entry)
		nameUndecided := readName(c, &a, entry, flags)
		a.Role = childCode(c, "attendeeRelationship", "attendeeRelationship", entry)
		a.Type = childCode(c, "attendeeType", "attendeeType", entry)
		a.Status = childCode(c, "attendeeStatus", "attendeeStatus", entry)
		storeCell(c, entry, "attendeeIdentity")
		storeCell(c, entry, "attendeeIdNamespace")
		if a.Address == "" && a.Name == "" {
			if !addrUndecided && !nameUndecided {
				unnamed++
			}
			if _, stated := entry["address_state"]; stated || nameUndecided {
				unnamedEntries++
				if len(unnamedList) < maxList {
					unnamedList = append(unnamedList, entry)
				}
			}
			continue
		}
		named++
		if len(list) < maxList {
			list = append(list, a)
			rawList = append(rawList, entry)
		}
	}
	if len(rawList) > 0 {
		r.raw["attendees"] = rawList
	}
	if unnamed > 0 {
		r.raw["attendees_unnamed"] = unnamed
	}
	putList(r.raw, "attendees_unnamed_entries", unnamedList, unnamedEntries)
	total, cut := capCount(named)
	return list, total, cut, nil
}

// remindersOf joins the event's Reminders (spec 7.3): before_s = minutes x 60 for minutes >= 0; -1 (the provider's
// default) gives no before_s; another negative keeps the reminder with its method only and states raw.reminders_state
// "negative-minutes"; minutes whose seconds overflow are listed as "out-of-range". A reminder with neither field is
// not stated. raw.reminders holds minutes and method as stored, aligned with reminders.
func remindersOf(m parse.MapContext, id int64, r cells) ([]calendar.Reminder, *int64, bool, error) {
	rows, undecided, err := childRows(m, tableReminders, id, r.raw)
	if err != nil || undecided {
		return nil, nil, false, err
	}
	var (
		out     []calendar.Reminder
		rawList []any
		n       int
	)
	for _, row := range rows {
		c := childCells(m, r, tableReminders, row, "minutes", "method")
		var rem calendar.Reminder
		col := c.c.At("minutes")
		switch v, k := intCellOf(row, col); {
		case k == rowrun.CellValue && v >= 0 && v <= math.MaxInt64/60:
			s := v * 60
			rem.BeforeS = &s
		case k == rowrun.CellValue && v >= 0:
			r.unknown[c.prefix+"minutes"] = "out-of-range"
		case k == rowrun.CellValue && v != -1:
			r.raw["reminders_state"] = stateNegativeMins
		case k == rowrun.CellOther || k == rowrun.CellUnknown:
			rowrun.Flag(r.unknown, row, c.prefix+"minutes", col, k)
		}
		entry := map[string]any{}
		storeCell(c, entry, "minutes")
		rem.Method = childCode(c, "method", "Reminders.method", entry)
		if rem.BeforeS == nil && rem.Method == "" {
			continue
		}
		n++
		if len(out) < maxList {
			out = append(out, rem)
			rawList = append(rawList, entry)
		}
	}
	if len(rawList) > 0 {
		r.raw["reminders"] = rawList
	}
	total, cut := capCount(n)
	return out, total, cut, nil
}

// alertCols are the CalendarAlerts columns raw.alerts states as stored (spec 7.3). A fresh slice per call.
func alertCols() []string {
	return []string{"begin", "end", "alarmTime", "creationTime", "receivedTime", "notifyTime", "state"}
}

// rawTimesCols are the RFC 2445 strings of EventsRawTimes raw.raw_times states as stored. A fresh slice per call.
func rawTimesCols() []string {
	return []string{"dtstart2445", "dtend2445", "originalInstanceTime2445", "lastDate2445"}
}

// rawChildren keeps the raw-only child tables (spec 7.3, Q8): raw.alerts (each column as stored, a defined state with
// its state_label; a fired alert is not a record), raw.extended_properties (the name as an identifier, never cut; the
// value cleaned and cut at 4 KiB with value_flags), each at most maxList with _total and _truncated (the entries of the
// first maxList rows only), and raw.raw_times (the first EventsRawTimes row's four RFC 2445 strings as stored; more
// than one row states raw_times_total, fix round 1, M5). Every entry holds scalars only (putEntry).
func rawChildren(m parse.MapContext, id int64, r cells) error {
	if err := alertsOf(m, id, r); err != nil {
		return err
	}
	if err := propertiesOf(m, id, r); err != nil {
		return err
	}
	rows, undecided, err := childRows(m, tableRawTimes, id, r.raw)
	if err != nil || undecided || len(rows) == 0 {
		return err
	}
	c := childCells(m, r, tableRawTimes, rows[0], rawTimesCols()...)
	entry := map[string]any{}
	for _, n := range rawTimesCols() {
		storeCell(c, entry, n)
	}
	if len(entry) > 0 {
		r.raw["raw_times"] = entry
	}
	if len(rows) > 1 { // the provider keeps one row per event (UNIQUE event_id): more is stated, never dropped silently
		r.raw["raw_times_total"] = int64(len(rows))
	}
	return nil
}

func alertsOf(m parse.MapContext, id int64, r cells) error {
	rows, undecided, err := childRows(m, tableAlerts, id, r.raw)
	if err != nil || undecided {
		return err
	}
	var list []any
	for _, row := range rows[:min(len(rows), maxList)] {
		c := childCells(m, r, tableAlerts, row, alertCols()...)
		entry := map[string]any{}
		for _, n := range alertCols() {
			storeCell(c, entry, n)
		}
		if v, ok := entry["state"].(int64); ok {
			if l, ok := lookupEnum("CalendarAlerts.state", v); ok {
				entry["state_label"] = l
			}
		}
		list = append(list, entry)
	}
	putList(r.raw, "alerts", list, len(rows))
	return nil
}

func propertiesOf(m parse.MapContext, id int64, r cells) error {
	rows, undecided, err := childRows(m, tableProperties, id, r.raw)
	if err != nil || undecided {
		return err
	}
	var list []any
	for _, row := range rows[:min(len(rows), maxList)] {
		c := childCells(m, r, tableProperties, row, "name", "value")
		entry := map[string]any{}
		if b, k := textCellOf(row, c.c.At("name")); k == rowrun.CellValue {
			if s, ok := ident(entry, "name", b, maxID); ok {
				entry["name"] = s
			}
		} else {
			storeCell(c, entry, "name")
		}
		if b, k := textCellOf(row, c.c.At("value")); k == rowrun.CellValue {
			cl := common.CleanText(b, maxProp)
			entry["value"] = cl.Text
			if len(cl.Flags) > 0 {
				entry["value_flags"] = strings.Join(cl.Flags, ",")
			}
		} else {
			storeCell(c, entry, "value")
		}
		list = append(list, entry)
	}
	putList(r.raw, "extended_properties", list, len(rows))
	return nil
}

// putList states a raw list cut at maxList with its _total and _truncated when n exceeds the cap (C6).
func putList(raw map[string]any, key string, list []any, n int) {
	if len(list) == 0 {
		return
	}
	raw[key] = list
	if total, cut := capCount(n); cut {
		raw[key+"_total"] = *total
		raw[key+"_truncated"] = true
	}
}

// readChildren states the event's children (spec 7.3): with a nil env (the typed recovery pass) no join is made and
// every child state key is "not-joined-recovered" (4I3-P4, B2).
func readChildren(m parse.MapContext, row parse.Row, e *env, r cells, flags textFlags, ev *calendar.Event) error {
	if e == nil {
		for _, k := range childStateKeys() {
			r.raw[k] = notJoinedRecovered
		}
		return nil
	}
	id, ok := row.Rowid()
	if !ok {
		for _, t := range []string{tableAttendees, tableAlerts, tableRawTimes, tableProperties, tableReminders} {
			undecidedJoin(r.raw, t)
		}
		return nil
	}
	var err error
	if ev.Attendees, ev.AttendeesTotal, ev.AttendeesTruncated, err = attendeesOf(m, id, r, flags); err != nil {
		return err
	}
	if ev.Reminders, ev.RemindersTotal, ev.RemindersTruncated, err = remindersOf(m, id, r); err != nil {
		return err
	}
	return rawChildren(m, id, r)
}
