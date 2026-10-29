package ioscalls

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// coreSpecs are the columns of ZCALLRECORD the mapping reads, in the order of the DDL snapshot (the schema
// note lists the optional ones in this order). Z_ENT, Z_OPT and ZDEVICE_ID are left unmapped.
func coreSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "Z_PK", Required: true, Affinity: "integer"},
		{Name: "ZANSWERED", Affinity: "integer"},
		{Name: "ZCALLTYPE", Affinity: "integer"},
		{Name: "ZDISCONNECTED_CAUSE", Affinity: "integer"},
		{Name: "ZFACE_TIME_DATA", Affinity: "integer"},
		{Name: "ZNUMBER_AVAILABILITY", Affinity: "integer"},
		{Name: "ZORIGINATED", Affinity: "integer"},
		{Name: "ZREAD", Affinity: "integer"},
		{Name: "ZJUNKCONFIDENCE", Affinity: "integer"},
		{Name: "ZDATE", Required: true, Affinity: "real"},
		{Name: "ZDURATION", Affinity: "real"},
		{Name: "ZADDRESS", Affinity: "text"},
		{Name: "ZISO_COUNTRY_CODE", Affinity: "text"},
		{Name: "ZLOCATION", Affinity: "text"},
		{Name: "ZNAME", Affinity: "text"},
		{Name: "ZSERVICE_PROVIDER", Affinity: "text"},
		{Name: "ZUNIQUE_ID", Affinity: "text"},
	}
}

func coreMapping() parse.TableMapping {
	return parse.TableMapping{Table: tableCore, Columns: coreSpecs(), Map: mapCore}
}

// coreRawInts are the integer columns stated in raw as stored.
func coreRawInts() []string {
	return []string{"ZORIGINATED", "ZANSWERED", "ZCALLTYPE", "ZDISCONNECTED_CAUSE", "ZNUMBER_AVAILABILITY", "ZFACE_TIME_DATA", "ZJUNKCONFIDENCE"}
}

// coreDirection is the direction and outcome of ruling 4E-R22: ZORIGINATED 1/0 is out/in; in+ZANSWERED 1 is
// answered, in+0 missed, out+1 answered; everything else is unknown, out+0 included (4E-R35: it also covers busy,
// failed and not answered).
func coreDirection(v map[string]int64) (direction, outcome string) {
	direction, outcome = call.DirUnknown, call.OutcomeUnknown
	orig, ok := v["ZORIGINATED"]
	switch {
	case ok && orig == 1:
		direction = call.DirOut
	case ok && orig == 0:
		direction = call.DirIn
	}
	ans, ok := v["ZANSWERED"]
	switch {
	case !ok:
	case direction == call.DirIn && ans == 1, direction == call.DirOut && ans == 1:
		outcome = call.OutcomeAnswered
	case direction == call.DirIn && ans == 0:
		outcome = call.OutcomeMissed
	}
	return direction, outcome
}

// coreKind maps ZCALLTYPE (from memory, 4E-R22).
func coreKind(v int64) string {
	switch v {
	case 1:
		return call.KindVoice
	case 8:
		return call.KindFaceTimeVideo
	case 16:
		return call.KindFaceTimeAudio
	}
	return call.KindUnknown
}

func mapCore(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, tableCore, names(coreSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	var cl call.Call

	v := ints(row, c, coreRawInts(), raw, unknown)
	cl.Direction, cl.Outcome = coreDirection(v)
	if t, ok := v["ZCALLTYPE"]; ok {
		cl.Kind = coreKind(t)
	}
	// 4E-R35: out+ZANSWERED 0 is unknown; the duration that would decide it is kept
	ans, haveAns := v["ZANSWERED"]
	keepDuration := cl.Direction == call.DirOut && haveAns && ans == 0

	cl.Address = address(row, c, "ZADDRESS", raw, unknown)
	if b, ok := textCell(row, c, "ZNAME", unknown); ok {
		cl.NameCached = namedField(raw, "name_cached", b)
	}
	if b, ok := textCell(row, c, "ZISO_COUNTRY_CODE", unknown); ok {
		cl.CountryISO = cleanField(raw, "country_iso", b)
	}
	if b, ok := textCell(row, c, "ZLOCATION", unknown); ok {
		cl.GeoDescription = namedField(raw, "geo_description", b)
	}
	if b, ok := textCell(row, c, "ZSERVICE_PROVIDER", unknown); ok {
		cl.Service = cleanField(raw, "service", b)
		raw["ZSERVICE_PROVIDER"] = cl.Service
	}
	if b, ok := textCell(row, c, "ZUNIQUE_ID", unknown); ok {
		raw["ZUNIQUE_ID"] = cleanField(raw, "ZUNIQUE_ID", b)
	}
	cl.Read = readFlag(row, c, "ZREAD", raw, unknown)

	dur, durOK := duration(row, c, "ZDURATION", keepDuration, raw, unknown)
	if durOK {
		cl.DurationS = &dur
	}
	rec := newRecord(place)
	if t, ok := instant(row, c, "ZDATE", cellx.CocoaSeconds, raw, unknown); ok {
		rec.Time = &records.Time{T: t}
		rec.TimeEnd = endTime(t, dur, durOK)
	}
	return []records.Record{finish(rec, cl, raw, unknown, row)}, nil
}
