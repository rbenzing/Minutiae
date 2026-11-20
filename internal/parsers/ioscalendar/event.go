package ioscalendar

import (
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The table, the source token and the deletion state of the mapping (spec 4.1, 6.4, 8.2).
const (
	tableItem         = "CalendarItem"
	sourceIOS         = "ios_calendar"
	stateNotEvaluated = "not-evaluated" // raw.deletion_state on every iOS record: no deletion marker is established
)

// env is what Parse hands the mapping: the join guard. It is nil in Mappings(), the typed recovery pass (4I3-P4, B2):
// a recovered row then joins no table, and each join-derived state is "not-joined-recovered".
type env struct{ Guard joinGuard }

// itemSpecs is the CalendarItem column list this mapping reads (spec 8.2): the five identifying columns are Required
// (spec 8.1), every other is optional (C3); a column the store has beyond these is rowrun's note unmapped_columns.
func itemSpecs() []parse.ColumnSpec {
	req := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Required: true, Affinity: aff} }
	opt := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Affinity: aff} }
	return []parse.ColumnSpec{
		req("ROWID", "integer"), req("start_date", "real"), req("start_tz", "text"), req("all_day", "integer"), req("calendar_id", "integer"),
		opt("end_date", "real"), opt("end_tz", "text"), opt("entity_type", "integer"), opt("creation_date", "real"), opt("last_modified", "real"),
		opt("summary", "text"), opt("description", "text"), opt("location_id", "integer"), opt("orig_item_id", "integer"),
		opt("orig_date", "real"), opt("organizer_id", "integer"), opt("status", "integer"), opt("availability", "integer"),
		opt("privacy_level", "integer"), opt("url", "text"), opt("external_id", "text"), opt("unique_identifier", "text"),
		opt("external_rep", "blob"), opt("hidden", "integer"), opt("has_recurrences", "integer"), opt("has_attendees", "integer"),
		opt("invitation_status", "integer"), opt("sequence_num", "integer"), opt("birthday_id", "integer"),
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// eventMappingWith maps CalendarItem with the environment e (nil: the typed recovery pass): a closure over an
// immutable value, so the mapper stays a pure function of the row and its context. With a guard, the joins read
// through it.
func eventMappingWith(e *env) parse.TableMapping {
	names := specNames(itemSpecs())
	return parse.TableMapping{Table: tableItem, Columns: itemSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		if e != nil && len(e.Guard.bad) > 0 {
			m = guardedCtx{MapContext: m, g: &e.Guard}
		}
		return mapItem(m, row, e, names)
	}}
}

// asStored are the raw-only CalendarItem columns kept as stored (spec 8.2, the plan's "As stored" list). A fresh slice
// per call.
func asStored() []string {
	return []string{"hidden", "has_recurrences", "has_attendees", "invitation_status", "sequence_num", "birthday_id"}
}

// mapItem turns one CalendarItem row into its calendar_event record (spec 8.2, 6.2, 6.4, Q1), or refuses a non-event
// row (rowrun.Unmappable, a counted warning).
func mapItem(m parse.MapContext, row parse.Row, e *env, names []string) ([]records.Record, error) {
	c := rowrun.Resolve(m, tableItem, names...)
	raw, unknown := map[string]any{}, map[string]any{}
	if err := itemKind(row, c, raw, unknown); err != nil {
		return nil, err
	}
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl // the decoder's row-level damage
	}
	raw["deletion_state"] = stateNotEvaluated // spec 6.4
	x := &item{m: m, e: e, r: cells{row: row, c: c, raw: raw, unknown: unknown, prefix: unknownPrefix}, flags: textFlags{}}
	ev := calendar.Event{Source: sourceIOS}
	if id, ok := row.Rowid(); ok {
		ev.SourceID = strconv.FormatInt(id, 10)
	}
	et := readTimes(row, c, raw, unknown)
	ev.Timing, ev.StartDate, ev.EndDate = et.Timing, et.StartDate, et.EndDate
	ev.StartTimezone = zoneName(x.r, "start_tz")
	ev.EndTimezone = zoneName(x.r, "end_tz")
	stamps := readStamps(row, c, raw, unknown)

	if b, ok := x.r.text("summary"); ok {
		ev.Title = freeText(raw, x.flags, &x.truncated, "title", b, maxTitle)
	}
	body := readBody(x.r, &ev)
	ev.Status = storedCode(x.r, "status")
	ev.Availability = storedCode(x.r, "availability")
	ev.Privacy = storedCode(x.r, "privacy_level")
	ev.URL = x.r.ident("url", maxID)
	ev.UID = x.r.ident("unique_identifier", maxID)
	ev.SyncID = x.r.ident("external_id", maxID)
	readExternalRep(x.r)
	readException(x.r, &ev)
	for _, n := range asStored() {
		x.r.keepTop(n)
	}

	if ev.Location, err = x.locationOf(); err != nil {
		return nil, err
	}
	if ev.Calendar, err = x.calendarOf(); err != nil {
		return nil, err
	}
	if ev.Organizer, err = x.organizerOf(); err != nil {
		return nil, err
	}
	if err := x.children(row, &ev); err != nil {
		return nil, err
	}
	x.flags.write(raw)
	slices.Sort(x.truncated)
	ev.TruncatedFields = slices.Compact(x.truncated)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	ev.Raw = raw
	rec := records.Record{
		Type: calendar.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
		Range: &place.Range, Time: et.Start, TimeEnd: et.End, Times: stamps, Body: body,
	}
	rec.Summary = calendar.Summary(ev, et.Start)
	rec.Payload = ev.Payload()
	return []records.Record{rec}, nil
}

// zoneName is a stored zone name as stored, never applied (spec 4.1, 8.2): the floating marker is raw only
// (raw.<col> = "_float"); a name over maxZone bytes as cleaned is not stated, with raw.<col>_state "too-long", its
// length and SHA-256 (ident, spec 9); an empty, NULL or missing cell states nothing; an unreadable one is listed.
func zoneName(r cells, name string) string {
	b, ok := r.text(name)
	switch {
	case !ok || len(b) == 0:
		return ""
	case string(b) == floatingZone:
		r.raw[name] = floatingZone
		return ""
	}
	s, _ := ident(r.raw, name, b, maxZone)
	return s
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

// storedCode reads a coded column (Q1, coordinator ruling 1): no iOS code is mapped, so a stored value is "unknown"
// with the value in raw.<col> as stored; a cell of another class or an unreadable one is also listed in
// unknown_columns; NULL and a missing column are absent.
func storedCode(r cells, col string) string {
	i := r.c.At(col)
	switch _, k := intCellOf(r.row, i); k {
	case rowrun.CellNull, rowrun.CellMissing:
		return ""
	case rowrun.CellOther, rowrun.CellUnknown:
		rowrun.Flag(r.unknown, r.row, r.prefix+col, i, k)
	}
	r.keepTop(col)
	return labelUnknown
}

// readExternalRep states the stored external_rep by its length and SHA-256 only (spec 8.2): never decoded, never
// kept. A TEXT is hashed as its bytes; a number is a value of another class and an unreadable cell is unknown, each
// listed in unknown_columns; NULL states nothing.
func readExternalRep(r cells) {
	col := r.c.At("external_rep")
	switch {
	case col < 0:
		return
	case !rowrun.StateOf(r.row, col).Known():
		rowrun.Flag(r.unknown, r.row, r.prefix+"external_rep", col, rowrun.CellUnknown)
		return
	case nanCell(r.row, col):
		rowrun.Flag(r.unknown, r.row, r.prefix+"external_rep", col, rowrun.CellOther)
		return
	case r.row.IsNull(col):
		return
	}
	b, ok := r.row.Blob(col)
	if !ok {
		b, ok = r.row.Text(col)
	}
	if !ok {
		rowrun.Flag(r.unknown, r.row, r.prefix+"external_rep", col, rowrun.CellOther)
		return
	}
	r.raw["external_rep_len"] = int64(len(b))
	r.raw["external_rep_sha256"] = sha256Hex(b)
}

// readException links an exception row to its series (spec 6.2): orig_item_id as a decimal string (0 or NULL is
// none); orig_date is raw only as stored, with raw.orig_date_basis "unestablished" (its basis convention is not
// known, so no original_start).
func readException(r cells, ev *calendar.Event) {
	col := r.c.At("orig_item_id")
	switch oid, k := intCellOf(r.row, col); {
	case k == rowrun.CellValue && oid != 0:
		ev.ExceptionOf = &calendar.ExceptionOf{EventID: strconv.FormatInt(oid, 10)}
	case k == rowrun.CellOther || k == rowrun.CellUnknown:
		rowrun.Flag(r.unknown, r.row, r.prefix+"orig_item_id", col, k)
	}
	r.keepTop("orig_date")
	if _, stated := r.raw["orig_date"]; stated {
		r.raw["orig_date_basis"] = "unestablished"
	}
}
