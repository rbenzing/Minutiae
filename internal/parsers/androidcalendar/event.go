package androidcalendar

import (
	"errors"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The tables, the source token and the join states of the mapping (spec 7.3, the plan's state vocabularies).
const (
	tableEvents      = "Events"
	tableCalendars   = "Calendars"
	sourceAndroid    = "android_calendar"
	stateNoCalendars = "no-calendars-row"
	stateNoEventsRow = "no-events-row"
	stateNotText     = "not-text"
)

// env is what Parse hands the mapping: the join guard. It is nil in Mappings(), the typed recovery pass (4I3-P4,
// B2): a recovered row then joins no table, and each join-derived state is "not-joined-recovered".
type env struct{ Guard joinGuard }

// eventSpecs is the Events column list: the five identifying columns are Required (spec 7.2), every other is
// optional (C3), so the 403 and 502 tables read with fewer fields.
func eventSpecs() []parse.ColumnSpec {
	req := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Required: true, Affinity: aff} }
	opt := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Affinity: aff} }
	specs := []parse.ColumnSpec{
		req("_id", "integer"), req("calendar_id", "integer"), req("dtstart", "integer"), req("allDay", "integer"), req("deleted", "integer"),
		opt("_sync_id", "text"), opt("dirty", "integer"), opt("mutators", "text"), opt("lastSynced", "integer"),
		opt("title", "text"), opt("eventLocation", "text"), opt("description", "text"), opt("eventColor", "integer"),
		opt("eventColor_index", "text"), opt("eventStatus", "integer"), opt("selfAttendeeStatus", "integer"),
		opt("dtend", "integer"), opt("eventTimezone", "text"), opt("duration", "text"), opt("accessLevel", "integer"),
		opt("availability", "integer"), opt("hasAlarm", "integer"), opt("hasExtendedProperties", "integer"),
		opt("rrule", "text"), opt("rdate", "text"), opt("exrule", "text"), opt("exdate", "text"),
		opt("original_id", "integer"), opt("original_sync_id", "text"), opt("originalInstanceTime", "integer"),
		opt("originalAllDay", "integer"), opt("lastDate", "integer"), opt("hasAttendeeData", "integer"),
		opt("guestsCanModify", "integer"), opt("guestsCanInviteOthers", "integer"), opt("guestsCanSeeGuests", "integer"),
		opt("organizer", "numeric"), opt("isOrganizer", "integer"), opt("eventEndTimezone", "text"),
		opt("customAppPackage", "text"), opt("customAppUri", "text"), opt("uid2445", "text"),
	}
	for i := 1; i <= 10; i++ {
		specs = append(specs, opt("sync_data"+strconv.Itoa(i), "text"))
	}
	return specs
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// eventMappingWith maps Events with the environment e (nil: the typed recovery pass): a closure over an immutable
// value, so the mapper stays a pure function of the row and its context. With a guard, the joins read through it.
func eventMappingWith(e *env) parse.TableMapping {
	names := specNames(eventSpecs())
	return parse.TableMapping{Table: tableEvents, Columns: eventSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		if e != nil && len(e.Guard.bad) > 0 {
			m = guardedCtx{MapContext: m, g: &e.Guard}
		}
		return mapEvent(m, row, e, names)
	}}
}

// The raw-only columns kept as stored (the plan's "As stored" list).
var asStored = []string{
	"eventColor", "hasAlarm", "hasExtendedProperties", "hasAttendeeData", "guestsCanModify", "guestsCanInviteOthers",
	"guestsCanSeeGuests", "isOrganizer", "eventColor_index", "customAppPackage", "customAppUri",
}

// mapEvent turns one Events row into its calendar_event record (spec 7.3; B7; 4I3-P4).
func mapEvent(m parse.MapContext, row parse.Row, e *env, names []string) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	raw, unknown := map[string]any{}, map[string]any{}
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl // the decoder's row-level damage (fix round 1, M2)
	}
	r := cells{row: row, c: rowrun.Resolve(m, tableEvents, names...), raw: raw, unknown: unknown, prefix: unknownPrefix}
	ev := calendar.Event{Source: sourceAndroid}
	if id, ok := row.Rowid(); ok {
		ev.SourceID = strconv.FormatInt(id, 10)
	}
	et := readTimes(row, r.c, raw, unknown)
	ev.Timing, ev.StartDate, ev.EndDate = et.Timing, et.StartDate, et.EndDate
	ev.StartTimezone = r.ident("eventTimezone", maxZone)
	ev.EndTimezone = r.ident("eventEndTimezone", maxZone)
	readDuration(r, &ev)

	flags, truncated := textFlags{}, []string(nil)
	if b, ok := r.text("title"); ok {
		ev.Title = freeText(raw, flags, &truncated, "title", b, maxTitle)
	}
	if b, ok := r.text("eventLocation"); ok {
		if t := freeText(raw, flags, &truncated, "location", b, maxTitle); t != "" {
			ev.Location = &calendar.Location{Text: t}
		}
	}
	body := readBody(r, &ev)

	ev.Status = readCode(r, "eventStatus", true)
	ev.Availability = readCode(r, "availability", true)
	ev.Privacy = readCode(r, "accessLevel", true)
	ev.SelfStatus = readCode(r, "selfAttendeeStatus", false)
	readOrganizer(r, &ev)
	readRecurrence(r, &ev)
	if err := readException(m, r, e, &ev); err != nil {
		return nil, err
	}
	ev.UID = r.ident("uid2445", maxID)
	ev.SyncID = r.ident("_sync_id", maxID)

	calDeleted, err := readCalendar(m, r, e, &ev, flags, &truncated)
	if err != nil {
		return nil, err
	}
	evDeleted := readFlag(row, r.c.At("deleted"), unknownPrefix+"deleted", unknown, true)
	lastSynced := readFlag(row, r.c.At("lastSynced"), unknownPrefix+"lastSynced", unknown, false)
	readDirty(r)
	if v, ok := r.stored("mutators"); ok {
		raw["mutators"] = v
	}
	if lastSynced.set() {
		raw["last_synced"] = int64(1)
	}
	d := deletionOf(evDeleted, lastSynced, calDeleted)
	ev.Deleted = d.record(raw)

	readRawOnly(r)
	if err := readChildren(m, row, e, r, flags, &ev); err != nil {
		return nil, err
	}
	flags.write(raw)
	slices.Sort(truncated)
	ev.TruncatedFields = truncated
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	ev.Raw = raw

	rec := records.Record{
		Type: calendar.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
		Range: &place.Range, Time: et.Start, TimeEnd: et.End, Body: body, Deleted: d.Deleted,
	}
	if et.OriginalStart != nil {
		rec.Times = []records.NamedTime{{Kind: calendar.OriginalStart, Time: *et.OriginalStart}}
	}
	rec.Summary = calendar.Summary(ev, et.Start)
	rec.Payload = ev.Payload()
	return []records.Record{rec}, nil
}

// readDuration keeps the stored duration text (spec 4.1, at most 64 bytes, never cut). A longer text is not in the
// payload: its length and SHA-256 are in raw and raw.duration_state is "unparsed" in every case, also with no start
// or an unknown timing (coordinator ruling on the oracle, Task 4). A cell of another class or an unreadable one is
// listed in unknown_columns in every case, a readable one kept as stored in raw.duration (spec 5.4, C1).
func readDuration(r cells, ev *calendar.Event) {
	col := r.c.At("duration")
	b, k := textCellOf(r.row, col)
	switch k {
	case rowrun.CellOther, rowrun.CellUnknown:
		// listed and kept as stored whether or not an end depends on it (fix round 1, I1; the key is readTimes' own)
		rowrun.Flag(r.unknown, r.row, r.prefix+"duration", col, k)
		if v, ok := r.stored("duration"); ok {
			r.raw["duration"] = v
		}
		return
	case rowrun.CellValue:
	default:
		return
	}
	if len(b) == 0 {
		return
	}
	s, ok := cleanWithin(b, maxDurationLen) // the limit holds for the text the payload would carry
	if !ok {
		r.raw["duration_state"] = durUnparsed
		r.raw["duration_len"] = int64(len(b))
		r.raw["duration_sha256"] = sha256Hex(b)
		return
	}
	ev.Duration = s
}

// readBody cleans the description at 256 KiB into the record Body and the body_* facts (spec 4.2, C4, C6).
func readBody(r cells, ev *calendar.Event) string {
	b, ok := r.text("description")
	if !ok {
		return ""
	}
	c := common.CleanText(b, maxBody)
	ev.BodyFlags = c.Flags
	if c.Truncated {
		t, n := true, int64(c.TotalBytes)
		ev.BodyTruncated, ev.BodyTotalBytes = &t, &n
	}
	ev.BodyRawB64, ev.BodyRawSHA256 = c.RawB64, c.RawSHA256
	if c.RawSHA256 != "" {
		n := int64(c.RawLen)
		ev.BodyRawLen = &n
	}
	return c.Text
}

// readCode maps a CalendarContract code (spec 2, 7.3): a defined value is its label, any other value (or a cell of
// another class) "unknown" with the stored value in raw; an unreadable cell is "unknown" and listed in
// unknown_columns; NULL is absent. keepRaw keeps a defined code in raw too.
func readCode(r cells, col string, keepRaw bool) string {
	i := r.c.At(col)
	v, k := intCellOf(r.row, i)
	switch k {
	case rowrun.CellNull, rowrun.CellMissing:
		return ""
	case rowrun.CellValue:
		l, ok := lookupEnum(col, v)
		if !ok || keepRaw {
			r.raw[col] = v
		}
		if ok {
			return l
		}
		return labelUnknown
	}
	rowrun.Flag(r.unknown, r.row, r.prefix+col, i, k)
	if s, ok := storedOther(r.row, i); ok {
		r.raw[col] = s
	}
	return labelUnknown
}

// readOrganizer maps organizer (spec 7.3): Events.organizer has NUMERIC affinity, so only a TEXT cell is the address
// that was written; a number (or a cell of another class) is kept as stored with organizer_state "not-text", and no
// organizer is stated, because the text that was written cannot be recovered from it.
func readOrganizer(r cells, ev *calendar.Event) {
	col := r.c.At("organizer")
	b, k := textCellOf(r.row, col)
	switch k {
	case rowrun.CellValue:
		if s, _ := ident(r.raw, "organizer", b, maxID); s != "" {
			ev.Organizer = &calendar.Person{Address: s}
		}
	case rowrun.CellOther:
		if v, ok := r.stored("organizer"); ok {
			r.raw["organizer"] = v
			r.raw["organizer_state"] = stateNotText
		} else {
			rowrun.Flag(r.unknown, r.row, r.prefix+"organizer", col, k)
		}
	case rowrun.CellUnknown:
		rowrun.Flag(r.unknown, r.row, r.prefix+"organizer", col, k)
	}
}

// readRecurrence keeps the four rule texts as stored, non-empty only, each never cut (spec 6.1, 9). The rule is
// never expanded.
func readRecurrence(r cells, ev *calendar.Event) {
	rec := calendar.Recurrence{
		RRule: r.ident("rrule", maxRule), RDate: r.ident("rdate", maxRule),
		ExRule: r.ident("exrule", maxRule), ExDate: r.ident("exdate", maxRule),
	}
	if rec != (calendar.Recurrence{}) {
		ev.Recurrence = &rec
	}
}

// readException links an exception row to its series (spec 6.2): original_id as a decimal string, checked against
// the live Events rows (a dangling id is raw.original_state "no-events-row"; a recovered row joins nothing), and
// original_sync_id as stored.
func readException(m parse.MapContext, r cells, e *env, ev *calendar.Event) error {
	var x calendar.ExceptionOf
	col := r.c.At("original_id")
	oid, k := intCellOf(r.row, col)
	switch k {
	case rowrun.CellValue:
		x.EventID = strconv.FormatInt(oid, 10)
		switch st, err := getRow(m, e, tableEvents, oid); {
		case err != nil:
			return err
		case st == jRecovered:
			r.raw["original_state"] = notJoinedRecovered
		case st == jNoRow:
			r.raw["original_state"] = stateNoEventsRow
		case st == jUndecided:
			undecidedJoin(r.raw, tableEvents)
		}
	case rowrun.CellOther, rowrun.CellUnknown:
		rowrun.Flag(r.unknown, r.row, r.prefix+"original_id", col, k)
	}
	x.SyncID = r.ident("original_sync_id", maxID)
	if x != (calendar.ExceptionOf{}) {
		ev.ExceptionOf = &x
	}
	return nil
}

// joinState is the answer of a Get join: never "no row" for what is really unknown.
type joinState int

const (
	jFound     joinState = iota // the join was made
	jNoRow                      // a clean miss: no row has the key
	jUndecided                  // a missing, guarded or damaged table, an undecidable key
	jRecovered                  // a recovered row: no join is made (B2)
)

// getRowAt returns the state of the Get join of table at rowid (and the row when found). A recovered row (nil env)
// joins nothing. A table the database lacks, or whose pages on the search path are damaged (dataDamage: the row may
// exist), is undecided, not a miss, as a child join is (spec 9); every other error (the budget, a cancel) is fatal.
func getRowAt(m parse.MapContext, e *env, table string, id int64) (parse.Row, joinState, error) {
	if e == nil {
		return nil, jRecovered, nil
	}
	row, found, err := m.Get(table, id)
	var us *sqlitedb.UnsupportedSchemaError
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable), errors.As(err, &us), errors.Is(err, sqlitedb.ErrKeyUndecidable), err != nil && dataDamage(err):
		return nil, jUndecided, nil
	case err != nil:
		return nil, jFound, err
	case !found:
		return nil, jNoRow, nil
	}
	return row, jFound, nil
}

// getRow is getRowAt for a join that needs only its state.
func getRow(m parse.MapContext, e *env, table string, id int64) (joinState, error) {
	_, st, err := getRowAt(m, e, table, id)
	return st, err
}

// calendarRaw are the Calendars columns raw.calendar states as stored (spec 7.3).
func calendarRaw() []string {
	return []string{"name", "calendar_timezone", "visible", "sync_events", "calendar_access_level", "isPrimary", "deleted", "dirty"}
}

// readCalendar joins the row's calendar (spec 7.3) and returns its deletion flag (B7): a decided Calendars.deleted,
// "undecided" when the calendar's state cannot be decided (an undecidable key or join, an unreadable or another value),
// "absent" when no calendar row has the key, "not-joined-recovered" for a recovered row.
func readCalendar(m parse.MapContext, r cells, e *env, ev *calendar.Event, flags textFlags, truncated *[]string) (flag, error) {
	if e == nil {
		r.raw["calendar_state"] = notJoinedRecovered
		return flag{State: notJoinedRecovered}, nil
	}
	col := r.c.At("calendar_id")
	cid, k := intCellOf(r.row, col)
	if k != rowrun.CellValue {
		rowrun.Flag(r.unknown, r.row, r.prefix+"calendar_id", col, k)
		undecidedJoin(r.raw, tableCalendars) // an undecidable key: never "no row"
		return flag{State: flagUndecided}, nil
	}
	crow, st, err := getRowAt(m, e, tableCalendars, cid)
	switch {
	case err != nil:
		return flag{}, err
	case st == jUndecided:
		undecidedJoin(r.raw, tableCalendars)
		return flag{State: flagUndecided}, nil
	case st == jNoRow:
		r.raw["calendar_state"] = stateNoCalendars
		return flag{State: flagAbsent}, nil
	}
	cr := cells{
		row: crow, c: rowrun.Resolve(m, tableCalendars, append([]string{"calendar_displayName", "account_type", "account_name", "ownerAccount"}, calendarRaw()...)...),
		raw: map[string]any{}, unknown: r.unknown, prefix: tableCalendars + ".",
	}
	ref := calendar.CalendarRef{ID: strconv.FormatInt(cid, 10)}
	if b, ok := cr.text("calendar_displayName"); ok && len(b) > 0 {
		// a free text: cut and named (calendar_title in text_flags and truncated_fields), never silently (task 9 fix I1)
		ref.Title = freeText(r.raw, flags, truncated, "calendar_title", b, maxTitle)
	}
	var acct calendar.Account
	if b, ok := cr.text("account_type"); ok {
		acct.Type = validText(b)
	}
	if b, ok := cr.text("account_name"); ok {
		acct.Name = validText(b)
	}
	if acct != (calendar.Account{}) {
		ref.Account = &acct
	}
	if b, ok := cr.text("ownerAccount"); ok {
		ref.Owner = validText(b)
	}
	ev.Calendar = &ref
	for _, n := range calendarRaw() {
		if v, ok := cr.stored(n); ok {
			cr.raw[n] = v
		}
	}
	if len(cr.raw) > 0 {
		r.raw["calendar"] = cr.raw
	}
	return readFlag(crow, cr.c.At("deleted"), tableCalendars+".deleted", r.unknown, true), nil
}

// readDirty states the unsynced-change flag (spec 6.3): 0 or 1 as stored in raw.dirty (live either way); another
// INTEGER also in raw.dirty with dirty_state "undecided"; a cell of another class or an unreadable one undecided; a
// NULL or a missing column states nothing.
func readDirty(r cells) {
	col := r.c.At("dirty")
	v, k := intCellOf(r.row, col)
	switch k {
	case rowrun.CellNull, rowrun.CellMissing:
		return
	case rowrun.CellValue:
		r.raw["dirty"] = v
		if v == 0 || v == 1 {
			return
		}
		r.unknown[r.prefix+"dirty"] = "out-of-set"
	default:
		rowrun.Flag(r.unknown, r.row, r.prefix+"dirty", col, k)
	}
	r.raw["dirty_state"] = flagUndecided
}

// readRawOnly keeps the raw-only Events columns (spec 7.3): the "As stored" list, lastDate as an instant with its
// status, and sync_data1..10 (a text over 1 KiB cut there, with its length and SHA-256).
func readRawOnly(r cells) {
	for _, n := range asStored {
		if v, ok := r.stored(n); ok {
			r.raw[n] = v
		}
	}
	if _, k := intCellOf(r.row, r.c.At("lastDate")); k != rowrun.CellNull && k != rowrun.CellMissing {
		readMillis(r.row, r.c, "lastDate", r.raw, r.unknown)
	}
	for i := 1; i <= 10; i++ {
		n := "sync_data" + strconv.Itoa(i)
		col := r.c.At(n)
		if b, k := textCellOf(r.row, col); k == rowrun.CellValue && len(b) > maxSyncData {
			r.raw[n] = common.CleanText(b, maxSyncData).Text
			r.raw[n+"_len"] = int64(len(b))
			r.raw[n+"_sha256"] = sha256Hex(b)
			continue
		}
		if v, ok := r.stored(n); ok {
			r.raw[n] = v
		}
	}
}
