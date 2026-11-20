package ioscalendar

import (
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// The table and the source token of the mapping (spec 4.1, 8.2).
const (
	tableItem = "CalendarItem"
	sourceIOS = "ios_calendar"
)

// env is what Parse hands the mapping. It is empty until the joins arrive (plan 4I3 Task 9 adds the join guard); a
// nil env will mean the typed recovery pass (4I3-P4, B2).
type env struct{}

// itemSpecs is the CalendarItem column list this mapping reads: the five identifying columns are Required (spec 8.1),
// every other is optional (C3).
func itemSpecs() []parse.ColumnSpec {
	req := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Required: true, Affinity: aff} }
	opt := func(n, aff string) parse.ColumnSpec { return parse.ColumnSpec{Name: n, Affinity: aff} }
	return []parse.ColumnSpec{
		req("ROWID", "integer"), req("start_date", "real"), req("start_tz", "text"), req("all_day", "integer"), req("calendar_id", "integer"),
		opt("end_date", "real"), opt("end_tz", "text"), opt("entity_type", "integer"), opt("creation_date", "real"), opt("last_modified", "real"),
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// eventMappingWith maps CalendarItem with the environment e. This is the interim mapping of plan 4I3 Task 8 (F12): it
// applies the row rule of the item kinds, the timing and times and the zone names only, so that the records can be
// checked; Task 9 extends it with the joins and the rest of spec 8.2.
func eventMappingWith(e *env) parse.TableMapping {
	names := specNames(itemSpecs())
	return parse.TableMapping{Table: tableItem, Columns: itemSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return mapItem(m, row, e, names)
	}}
}

// mapItem turns one CalendarItem row into its calendar_event record, or refuses a non-event row (rowrun.Unmappable,
// a counted warning).
func mapItem(m parse.MapContext, row parse.Row, _ *env, names []string) ([]records.Record, error) {
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
	ev := calendar.Event{Source: sourceIOS}
	if id, ok := row.Rowid(); ok {
		ev.SourceID = strconv.FormatInt(id, 10)
	}
	et := readTimes(row, c, raw, unknown)
	ev.Timing, ev.StartDate, ev.EndDate = et.Timing, et.StartDate, et.EndDate
	ev.StartTimezone = zoneName(row, c, "start_tz", raw, unknown)
	ev.EndTimezone = zoneName(row, c, "end_tz", raw, unknown)
	stamps := readStamps(row, c, raw, unknown)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	ev.Raw = raw
	rec := records.Record{
		Type: calendar.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
		Range: &place.Range, Time: et.Start, TimeEnd: et.End, Times: stamps,
	}
	rec.Summary = calendar.Summary(ev, et.Start)
	rec.Payload = ev.Payload()
	return []records.Record{rec}, nil
}

// zoneName is a stored zone name as stored, never applied (spec 4.1, 8.2): the floating marker is raw only
// (raw.<col> = "_float"); a name over maxZone bytes as cleaned is not stated (its length and hash arrive with the
// identifier rule of Task 9); an empty, NULL or missing cell states nothing; an unreadable one is listed.
func zoneName(row parse.Row, c rowrun.Cols, name string, raw, unknown map[string]any) string {
	col := c.At(name)
	b, k := textCellOf(row, col)
	switch {
	case k == rowrun.CellOther || k == rowrun.CellUnknown:
		rowrun.Flag(unknown, row, unknownPrefix+name, col, k)
		return ""
	case k != rowrun.CellValue || len(b) == 0 || !cleanFits(b, maxZone):
		return ""
	case string(b) == floatingZone:
		raw[name] = floatingZone
		return ""
	}
	return validText(b)
}
