package ioscalendar

import (
	"errors"
	"math"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The join states (the plan's ios.calendar vocabulary): a recovered row joins nothing (4I3-P4, B2); a Get join whose
// key names no row is "no-<Table>-row", or "zero-key" when the key is stored as 0 (B13); a Match join by owner states
// only "not-joined-recovered" (an empty match is no children, F15); a cell that holds no usable value is "undecided".
const (
	notJoinedRecovered = "not-joined-recovered"
	stateOutOfRange    = "out-of-range"
	labelUnknown       = "unknown" // a stored code, never labelled (Q1)
)

func noRow(table string) string { return "no-" + table + "-row" }

// stateZeroKey is the state of a Get join whose key is stored as integer 0 and finds no row (spec B13): the store
// writes 0 for "no key", so it is not stated as a key naming a missing row. A row whose rowid is 0 joins normally.
const stateZeroKey = "zero-key"

// missState is the state of a Get join whose key id names no row: "zero-key" for 0 (B13), else "no-<Table>-row".
func missState(table string, id int64) string {
	if id == 0 {
		return stateZeroKey
	}
	return noRow(table)
}

// matchStateKeys are the raw state keys of the four Match joins by owner: a recovered row (nil env) joins none of them
// and states each as "not-joined-recovered" (4I3-P4, B2, F15). A fresh slice per call.
func matchStateKeys() []string {
	return []string{"participants_state", "recurrence_state", "exception_dates_state", "alarms_state"}
}

// joinState is the answer of a Get join: never "no row" for what is really unknown.
type joinState int

const (
	jFound     joinState = iota // the join was made
	jNoRow                      // a clean miss: no row has the key
	jUndecided                  // a missing, guarded or damaged table, an undecidable key
	jRecovered                  // a recovered row: no join is made (B2)
)

// item is one CalendarItem row being mapped: its cells, the join context and environment, and the text facts that
// several readers add to.
type item struct {
	m         parse.MapContext
	e         *env
	r         cells
	flags     textFlags
	truncated []string
}

// get is the Get join of table at id (and the row when found). A recovered row (nil env) joins nothing. A table the
// database lacks, a guarded one, or one whose pages on the search path are damaged (the row may exist) is undecided,
// never a miss; every other error (the budget, a cancel) is fatal.
func (x *item) get(table string, id int64) (parse.Row, joinState, error) {
	if x.e == nil {
		return nil, jRecovered, nil
	}
	row, found, err := x.m.Get(table, id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable), err != nil && dataDamage(err):
		return nil, jUndecided, nil
	case err != nil:
		return nil, jFound, err
	case !found:
		return nil, jNoRow, nil
	}
	return row, jFound, nil
}

// getBy reads the key column of the item and joins table by it, stating the outcome in raw: a NULL or missing key
// joins nothing and states nothing; a key that cannot be read, or a join that cannot be decided, names the table in
// raw.join_undecided; a recovered row states stateKey "not-joined-recovered"; a key naming no row states stateKey
// "no-<Table>-row", or "zero-key" when the key is 0 (B13). The row is returned only when found.
func (x *item) getBy(keyCol, table, stateKey string) (parse.Row, int64, error) {
	id, ok, undecided := x.r.key(keyCol)
	switch {
	case (ok || undecided) && x.e == nil:
		x.r.raw[stateKey] = notJoinedRecovered
		return nil, 0, nil
	case undecided:
		undecidedJoin(x.r.raw, table) // an undecidable key: never "no row"
		return nil, 0, nil
	case !ok:
		return nil, 0, nil
	}
	row, st, err := x.get(table, id)
	switch {
	case err != nil:
		return nil, 0, err
	case st == jUndecided:
		undecidedJoin(x.r.raw, table)
	case st == jNoRow:
		x.r.raw[stateKey] = missState(table, id)
	}
	return row, id, nil
}

// joined reads the cells of a joined row; an unreadable or wrongly typed cell is listed in unknown_columns as
// "<table>.<col>", and raw facts go to raw (the item's raw unless a child entry is given).
func (x *item) joined(table string, row parse.Row, raw map[string]any, names ...string) cells {
	return cells{row: row, c: rowrun.Resolve(x.m, table, names...), raw: raw, unknown: x.r.unknown, prefix: table + "."}
}

// locationOf joins the item's Location (spec 8.2): title is the text (a free text, C4), address the structured
// address (likewise, its facts under "location_address"), the coordinates only when finite and in range (spec 9).
func (x *item) locationOf() (*calendar.Location, error) {
	row, _, err := x.getBy("location_id", tableLocation, "location_state")
	if err != nil || row == nil {
		return nil, err
	}
	c := x.joined(tableLocation, row, x.r.raw, "title", "address", "latitude", "longitude")
	var loc calendar.Location
	if b, ok := c.text("title"); ok {
		loc.Text = freeText(x.r.raw, x.flags, &x.truncated, "location", b, maxTitle)
	}
	if b, ok := c.text("address"); ok {
		loc.Address = freeText(x.r.raw, x.flags, &x.truncated, "location_address", b, maxTitle)
	}
	loc.Latitude = coordinate(c, "latitude", 90)
	loc.Longitude = coordinate(c, "longitude", 180)
	if loc == (calendar.Location{}) {
		return nil, nil
	}
	return &loc, nil
}

// coordinate reads a coordinate (spec 9): a finite number within +-limit is the value; any other number (NaN and the
// infinities included) is not stated, with the value in raw.<col> and raw.<col>_state "out-of-range"; a cell of
// another class or an unreadable one is listed in unknown_columns and kept as stored when it has a value.
func coordinate(c cells, col string, limit float64) *float64 {
	i := c.c.At(col)
	if nanCell(c.row, i) {
		flag(c.unknown, c.row, c.prefix+col, i, rowrun.CellOther) // a stored NaN, as everywhere (m1)
		c.raw[col] = "NaN"
		c.raw[col+"_state"] = stateOutOfRange
		return nil
	}
	n, k := cellx.NumberCell(c.row, i)
	switch k {
	case rowrun.CellValue:
		if f, ok := n.Float(); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Abs(f) <= limit {
			return &f
		}
		c.raw[col] = n.Raw()
		c.raw[col+"_state"] = stateOutOfRange
	case rowrun.CellOther, rowrun.CellUnknown:
		flag(c.unknown, c.row, c.prefix+col, i, k)
		if v, ok := storedOther(c.row, i); ok {
			c.raw[col] = v
		}
	}
	return nil
}

// calendarOf joins the item's Calendar and its Store (spec 8.2): {id, title, account: {name: Store.name}}, Store.type
// in raw.store_type as stored; a store_id naming no Store row is raw.store_state "no-Store-row". The title is a free
// text (calendar_title in text_flags and truncated_fields); the Store name is an identifier (calendar_account_state
// "too-long" with its length and SHA-256 over 1 KiB).
func (x *item) calendarOf() (*calendar.CalendarRef, error) {
	row, id, err := x.getBy("calendar_id", tableCalendar, "calendar_state")
	if err != nil || row == nil {
		return nil, err
	}
	c := x.joined(tableCalendar, row, x.r.raw, "title", "store_id")
	ref := calendar.CalendarRef{ID: strconv.FormatInt(id, 10)}
	if b, ok := c.text("title"); ok && len(b) > 0 {
		ref.Title = freeText(x.r.raw, x.flags, &x.truncated, "calendar_title", b, maxTitle) // never a silent cut (I1)
	}
	store, _, err := x.getByOf(c, "store_id", tableStore, "store_state")
	if err != nil {
		return nil, err
	}
	if store != nil {
		sc := x.joined(tableStore, store, x.r.raw, "name", "type")
		if b, ok := sc.text("name"); ok && len(b) > 0 {
			// the account is an identifier: never cut, over 1 KiB stated by its length and SHA-256 (I1)
			if name, ok := ident(x.r.raw, "calendar_account", b, maxID); ok {
				ref.Account = &calendar.Account{Name: name}
			}
		}
		if v, over, ok := sc.stored("type"); ok {
			x.r.raw["store_type"] = v
		} else if over != nil {
			sc.keep(x.r.raw, "store_type", "type") // its length and hash
		}
	}
	return &ref, nil
}

// getByOf is getBy with the key read from the joined row c (a key of a key: Calendar.store_id), the state stated in
// the item's raw under stateKey.
func (x *item) getByOf(c cells, keyCol, table, stateKey string) (parse.Row, int64, error) {
	sub := &item{m: x.m, e: x.e, r: cells{row: c.row, c: c.c, raw: x.r.raw, unknown: x.r.unknown, prefix: c.prefix}}
	return sub.getBy(keyCol, table, stateKey)
}

// person is one Participant row read as a person (spec 8.2): address = Participant.email, else Identity.address;
// name = Identity.display_name. Its states go to entry: address_state ("too-long" with address_len and
// address_sha256, F27; "undecided" with the stored email when the email cannot be read), name_state "undecided" (the
// stored display_name kept), identity_state ("no-Identity-row", "zero-key" for an identity_id of 0 naming no row (B13),
// or "undecided" when the identity key or join cannot be decided). undecided is true when the address or the name is unknown: such a participant is never one with "neither
// address nor name".
type person struct {
	calendar.Person
	undecided bool
}

func (x *item) personOf(row parse.Row, entry map[string]any, field string) (person, error) {
	c := x.joined(tableParticipant, row, entry, "email", "identity_id", "role", "type", "status")
	var p person
	addrStated := false
	col := c.c.At("email")
	switch b, k := textCellOf(row, col); k {
	case rowrun.CellValue:
		if len(b) > 0 {
			addrStated = true
			if s, ok := cleanWithin(b, maxID); ok {
				p.Address = s
			} else {
				entry["address_state"] = stateTooLong
				entry["address_len"] = int64(len(b))
				entry["address_sha256"] = sha256Hex(b)
			}
		}
	case rowrun.CellOther, rowrun.CellUnknown:
		flag(c.unknown, row, c.prefix+"email", col, k)
		c.keep(entry, "email", "email")
		entry["address_state"] = stateUndecided
		addrStated, p.undecided = true, true
	}
	iid, ok, undecided := c.key("identity_id")
	switch {
	case undecided:
		undecidedJoin(x.r.raw, tableIdentity)
		entry["identity_state"] = stateUndecided
		p.undecided = true
		return p, nil
	case !ok:
		return p, nil
	}
	irow, st, err := x.get(tableIdentity, iid)
	switch {
	case err != nil:
		return p, err
	case st == jUndecided:
		undecidedJoin(x.r.raw, tableIdentity)
		entry["identity_state"] = stateUndecided
		p.undecided = true
		return p, nil
	case st == jNoRow:
		entry["identity_state"] = missState(tableIdentity, iid)
		return p, nil
	}
	ic := x.joined(tableIdentity, irow, entry, "display_name", "address")
	if !addrStated {
		acol := ic.c.At("address")
		switch b, k := textCellOf(irow, acol); k {
		case rowrun.CellValue:
			if s, ok := cleanWithin(b, maxID); !ok {
				entry["address_state"] = stateTooLong
				entry["address_len"] = int64(len(b))
				entry["address_sha256"] = sha256Hex(b)
			} else {
				p.Address = s
			}
		case rowrun.CellOther, rowrun.CellUnknown:
			flag(ic.unknown, irow, ic.prefix+"address", acol, k)
			ic.keep(entry, "address", "address")
			entry["address_state"] = stateUndecided
			p.undecided = true
		}
	}
	ncol := ic.c.At("display_name")
	switch b, k := textCellOf(irow, ncol); k {
	case rowrun.CellValue:
		if len(b) > 0 {
			cl := common.CleanText(b, maxTitle)
			if len(cl.Flags) > 0 {
				x.flags[field] = true
			}
			p.Name = cl.Text
		}
	case rowrun.CellOther, rowrun.CellUnknown:
		flag(ic.unknown, irow, ic.prefix+"display_name", ncol, k)
		ic.keep(entry, "display_name", "display_name")
		entry["name_state"] = stateUndecided
		p.undecided = true
	}
	return p, nil
}

// organizerOf joins the item's organizer (spec 8.2): Participant by organizer_id (Get), then Identity by identity_id
// (Get). The state of the organizer_id link is raw.organizer_state ("no-Participant-row", "zero-key" (B13),
// "not-joined-recovered"); that of the Identity link raw.organizer_identity_state ("no-Identity-row", "zero-key",
// "undecided"), so the two links are never merged (Task 12 fix 2, I2); those of its address and name are
// raw.organizer_<key> (address_state, address_len, address_sha256, email, name_state, display_name).
func (x *item) organizerOf() (*calendar.Person, error) {
	row, _, err := x.getBy("organizer_id", tableParticipant, "organizer_state")
	if err != nil || row == nil {
		return nil, err
	}
	entry := map[string]any{}
	p, err := x.personOf(row, entry, "organizer")
	if err != nil {
		return nil, err
	}
	for k, v := range entry {
		switch k {
		case "identity_state":
			// the Identity link has its own key (m2; Task 12 fix 2, I2): organizer_state states the organizer_id link only
			x.r.raw["organizer_identity_state"] = v
		case "role", "type", "status":
		default:
			x.r.raw["organizer_"+k] = v
		}
	}
	if p.Person == (calendar.Person{}) {
		return nil, nil
	}
	return &p.Person, nil
}

// matchRows returns the rows of table whose owner column is the item's rowid, in row-id order. undecided is true when
// the answer is not trusted (spec 8.2, 9): a table the database lacks or that cannot be read, a lossy or over-cap join
// index, a guarded table, an undecidable key; the join is then named in raw.join_undecided and states nothing else.
// Any other error (the budget, a cancel) fails the job.
func (x *item) matchRows(table, col string, id int64) ([]parse.Row, bool, error) {
	rows, undecided, err := rowrun.Match(x.m, table, col, id)
	switch {
	case err != nil && (errors.Is(err, sqlitedb.ErrNoSuchTable) || errors.Is(err, sqlitedb.ErrIndexLimit) || dataDamage(err)):
		undecided = true
	case err != nil:
		return nil, false, err
	}
	if undecided {
		undecidedJoin(x.r.raw, table)
		return nil, true, nil
	}
	slices.SortStableFunc(rows, func(a, b parse.Row) int {
		p, _ := a.Rowid()
		q, _ := b.Rowid()
		switch {
		case p < q:
			return -1
		case p > q:
			return 1
		}
		return 0
	})
	return rows, false, nil
}

// participantsOf joins the item's Participant rows by item_id (spec 8.2, Q1): each is an attendee with its address
// and name (personOf) and role, type and status "unknown" when a code is stored, the stored codes in raw.attendees,
// strictly parallel to attendees. A participant with neither address nor name is not an attendee: unless its address
// or name is undecided it is counted in raw.attendees_unnamed; its entry, when it states anything (codes aside), goes
// to raw.attendees_unnamed_entries (at most maxList, with _total and _truncated). F29: the organizer participant is
// also an attendee when its item_id names the item.
func (x *item) participantsOf(id int64) ([]calendar.Attendee, *int64, bool, error) {
	rows, undecided, err := x.matchRows(tableParticipant, "item_id", id)
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
		entry := map[string]any{}
		p, err := x.personOf(row, entry, "attendees")
		if err != nil {
			return nil, nil, false, err
		}
		a := calendar.Attendee{Address: p.Address, Name: p.Name}
		c := x.joined(tableParticipant, row, entry, "role", "type", "status")
		a.Role, a.Type, a.Status = code(c, entry, "role"), code(c, entry, "type"), code(c, entry, "status")
		if a.Address == "" && a.Name == "" {
			if !p.undecided {
				unnamed++
			}
			if statesMore(entry) {
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
		x.r.raw["attendees"] = rawList
	}
	if unnamed > 0 {
		x.r.raw["attendees_unnamed"] = unnamed
	}
	putList(x.r.raw, "attendees_unnamed_entries", unnamedList, unnamedEntries)
	total, cut := capCount(named)
	return list, total, cut, nil
}

// statesMore says an unnamed participant's entry states more than its codes (an address, a name or an identity
// state), so it is kept beside the count.
func statesMore(entry map[string]any) bool {
	for k := range entry {
		switch k {
		case "role", "type", "status":
		default:
			return true
		}
	}
	return false
}

// code reads a stored code (Q1): never labelled; a stored INTEGER is kept in entry and the label is "unknown"; a cell
// of another class or an unreadable one is also listed in unknown_columns and kept as stored when it has a value;
// NULL states nothing.
func code(c cells, entry map[string]any, col string) string {
	i := c.c.At(col)
	v, k := intCellOf(c.row, i)
	switch k {
	case rowrun.CellNull, rowrun.CellMissing:
		return ""
	case rowrun.CellValue:
		entry[col] = v
		return labelUnknown
	}
	flag(c.unknown, c.row, c.prefix+col, i, k)
	c.keep(entry, col, col)
	return labelUnknown
}

// recurrenceCols are the Recurrence columns raw.recurrence states as stored (spec 8.2). A fresh slice per call.
func recurrenceCols() []string {
	return []string{"frequency", "interval", "count", "end_date", "specifier"}
}

// recurrenceOf joins the item's Recurrence rows by owner_id (spec 6.1, 8.2): stored_rules counts every row (only when
// there is one), raw.recurrence holds each row's stored columns (at most maxList, with _total and _truncated). The
// rule is never expanded and never interpreted.
func (x *item) recurrenceOf(id int64) (*calendar.Recurrence, error) {
	rows, undecided, err := x.matchRows(tableRecurrence, "owner_id", id)
	if err != nil || undecided || len(rows) == 0 {
		return nil, err
	}
	var list []any
	for _, row := range rows[:min(len(rows), maxList)] {
		entry := map[string]any{}
		c := x.joined(tableRecurrence, row, entry, recurrenceCols()...)
		for _, n := range recurrenceCols() {
			c.keep(entry, n, n)
		}
		list = append(list, entry)
	}
	putList(x.r.raw, "recurrence", list, len(rows))
	n := int64(len(rows))
	return &calendar.Recurrence{StoredRules: &n}, nil
}

// exceptionDatesOf joins the item's ExceptionDate rows by owner_id (spec 6.2): raw.exception_dates holds each row's
// stored date (at most maxList, with _total and _truncated), never read as a time.
func (x *item) exceptionDatesOf(id int64) error {
	rows, undecided, err := x.matchRows(tableExceptions, "owner_id", id)
	if err != nil || undecided {
		return err
	}
	var list []any
	for _, row := range rows[:min(len(rows), maxList)] {
		entry := map[string]any{}
		c := x.joined(tableExceptions, row, entry, "date")
		if _, k := numberCellOf(row, c.c.At("date")); k == rowrun.CellNull {
			list = append(list, nil) // a stored NULL is stated as such
			continue
		}
		c.keep(entry, "date", "date")
		switch v, ok := entry["date"]; {
		case ok:
			list = append(list, v)
		case len(entry) > 0:
			list = append(list, entry) // a BLOB (date_blob_b64) or an over-long value (date_state): scalars only, B8
		default:
			// an unknown date (listed in unknown_columns as ExceptionDate.date) keeps its place: the list stays
			// parallel to the rows, the entry a map of scalars as a BLOB's is (B8, fix round 1 m3)
			list = append(list, map[string]any{"date_state": stateUndecided})
		}
	}
	putList(x.r.raw, "exception_dates", list, len(rows))
	return nil
}

// alarmCols are the Alarm columns raw.alarms states as stored (spec 8.2). A fresh slice per call.
func alarmCols() []string { return []string{"trigger_interval", "trigger_date", "type"} }

// alarmsOf joins the item's Alarm rows by calendaritem_owner_id (spec 8.2): raw only, because the sign convention of
// trigger_interval is not established, so no reminders; at most maxList, with _total and _truncated.
func (x *item) alarmsOf(id int64) error {
	rows, undecided, err := x.matchRows(tableAlarm, "calendaritem_owner_id", id)
	if err != nil || undecided {
		return err
	}
	var list []any
	for _, row := range rows[:min(len(rows), maxList)] {
		entry := map[string]any{}
		c := x.joined(tableAlarm, row, entry, alarmCols()...)
		for _, n := range alarmCols() {
			c.keep(entry, n, n)
		}
		list = append(list, entry)
	}
	putList(x.r.raw, "alarms", list, len(rows))
	return nil
}

// children makes the four Match joins by owner (spec 8.2): with a nil env (the typed recovery pass) none is made and
// each state key is "not-joined-recovered" (4I3-P4, B2); a row without a rowid makes each join undecided.
func (x *item) children(row parse.Row, ev *calendar.Event) error {
	if x.e == nil {
		for _, k := range matchStateKeys() {
			x.r.raw[k] = notJoinedRecovered
		}
		return nil
	}
	id, ok := row.Rowid()
	if !ok {
		for _, t := range []string{tableParticipant, tableRecurrence, tableExceptions, tableAlarm} {
			undecidedJoin(x.r.raw, t)
		}
		return nil
	}
	var err error
	if ev.Attendees, ev.AttendeesTotal, ev.AttendeesTruncated, err = x.participantsOf(id); err != nil {
		return err
	}
	if ev.Recurrence, err = x.recurrenceOf(id); err != nil {
		return err
	}
	if err := x.exceptionDatesOf(id); err != nil {
		return err
	}
	return x.alarmsOf(id)
}
