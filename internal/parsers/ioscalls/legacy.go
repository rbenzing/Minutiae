package ioscalls

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// legacySpecs are the columns of the call table the mapping reads, in the order of the DDL snapshot.
func legacySpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "ROWID", Required: true, Affinity: "integer"},
		{Name: "address", Affinity: "text"},
		{Name: "date", Required: true, Affinity: "integer"},
		{Name: "duration", Affinity: "integer"},
		{Name: "flags", Affinity: "integer"},
		{Name: "id", Affinity: "integer"},
		{Name: "name", Affinity: "text"},
		{Name: "country_code", Affinity: "text"},
		{Name: "network_code", Affinity: "text"},
		{Name: "read", Affinity: "integer"},
		{Name: "assisted", Affinity: "integer"},
		{Name: "face_time_data", Affinity: "blob"},
		{Name: "originalAddress", Affinity: "text"},
	}
}

func legacyMapping() parse.TableMapping {
	return parse.TableMapping{Table: tableLegacy, Columns: legacySpecs(), Map: mapLegacy}
}

// mapLegacy maps a row of the older call_history.db. The bits of call.flags are not interpreted (4E-R21): the
// direction and the outcome are unknown and the stored value is kept in raw.flags.
func mapLegacy(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, tableLegacy, names(legacySpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	cl := call.Call{Direction: call.DirUnknown, Outcome: call.OutcomeUnknown}

	ints(row, c, []string{"flags", "id", "assisted"}, raw, unknown)
	cl.Address = address(row, c, "address", raw, unknown)
	if b, ok := textCell(row, c, "name", unknown); ok {
		cl.NameCached = cleanField(raw, "name_cached", b)
	}
	if b, ok := textCell(row, c, "country_code", unknown); ok {
		cl.CountryISO = cleanField(raw, "country_iso", b)
	}
	for _, col := range []string{"network_code", "originalAddress"} {
		if b, ok := textCell(row, c, col, unknown); ok {
			raw[col] = cleanField(raw, col, b)
		}
	}
	faceTimeBytes(row, c, raw, unknown)
	cl.Read = readFlag(row, c, "read", raw, unknown)

	dur, durOK := duration(row, c, "duration", false, raw, unknown)
	if durOK {
		cl.DurationS = &dur
	}
	rec := newRecord(place)
	if t, ok := instant(row, c, "date", cellx.UnixSeconds, raw, unknown); ok {
		rec.Time = &records.Time{T: t}
		rec.TimeEnd = endTime(t, dur, durOK)
	}
	return []records.Record{finish(rec, cl, raw, unknown, row)}, nil
}

// faceTimeBytes states the length of the face_time_data BLOB (its content is not read).
func faceTimeBytes(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) {
	i := c.At("face_time_data")
	switch k := blobKind(row, i); k {
	case rowrun.CellValue:
		b, _ := row.Blob(i)
		raw["face_time_data_bytes"] = int64(len(b))
	default:
		rowrun.Flag(unknown, row, "face_time_data", i, k)
	}
}

// blobKind classifies a BLOB column like rowrun.TextCell classifies a text one.
func blobKind(row parse.Row, col int) rowrun.CellKind {
	switch {
	case col < 0:
		return rowrun.CellMissing
	case !rowrun.StateOf(row, col).Known():
		return rowrun.CellUnknown
	case row.IsNull(col):
		return rowrun.CellNull
	}
	if _, ok := row.Blob(col); ok {
		return rowrun.CellValue
	}
	return rowrun.CellOther
}
