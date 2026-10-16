package androidcalllog

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

const (
	maxText     = 256 << 10 // bytes of any stored text kept
	maxDuration = 1_000_000_000
	maxExact    = 1 << 53 // every integer up to this magnitude is exactly a float64
)

func callSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "number", Required: true, Affinity: "text"},
		{Name: "date", Required: true, Affinity: "integer"},
		{Name: "duration", Required: true, Affinity: "integer"},
		{Name: "type", Required: true, Affinity: "integer"},
		{Name: "presentation", Affinity: "integer"},
		{Name: "name", Affinity: "text"},
		{Name: "countryiso", Affinity: "text"},
		{Name: "geocoded_location", Affinity: "text"},
		{Name: "subscription_id", Affinity: "text"},
		{Name: "features", Affinity: "integer"},
		{Name: "is_read", Affinity: "integer"},
		{Name: "new", Affinity: "integer"},
	}
}

func callsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "calls", Columns: callSpecs(), Map: mapCall}
}

// callType maps calls.type (spec 9.2). A type or duration that is not a known integer gives
// unknown, never a guessed direction or outcome.
func callType(typ int64, typeKnown bool, dur int64, durKnown bool) (direction, outcome string) {
	if !typeKnown {
		return call.DirUnknown, call.OutcomeUnknown
	}
	switch typ {
	case 1, 7:
		return call.DirIn, call.OutcomeAnswered
	case 2:
		switch {
		case !durKnown || dur < 0:
			return call.DirOut, call.OutcomeUnknown
		case dur > 0:
			return call.DirOut, call.OutcomeAnswered
		}
		return call.DirOut, call.OutcomeCancelled
	case 3:
		return call.DirIn, call.OutcomeMissed
	case 4:
		return call.DirIn, call.OutcomeVoicemail
	case 5:
		return call.DirIn, call.OutcomeRejected
	case 6:
		return call.DirIn, call.OutcomeBlocked
	}
	return call.DirUnknown, call.OutcomeUnknown
}

func presentation(v int64) string {
	switch v {
	case 1:
		return call.PresentationAllowed
	case 2:
		return call.PresentationRestricted
	case 4:
		return call.PresentationPayphone
	}
	return call.PresentationUnknown
}

// markText lists field in raw.text_flags (sorted): the text of that field was altered
// (invalid UTF-8 or NUL became U+FFFD, or it was cut). The original bytes stay in the
// artifact; nothing is altered without a flag (spec 7.6).
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// cleanField is common.CleanText for a free-text field that has no flag slot of its own
// in the typed payload; an altered text is listed in raw.text_flags.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}

// parseSmall parses decimal text of at most 32 bytes; longer text is refused before any
// conversion (D15), so a hostile cell costs nothing.
func parseSmall(s string) (int64, error) {
	if len(s) > 32 {
		return 0, strconv.ErrRange
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

func cleanText(row parse.Row, c rowrun.Cols, name, field string, raw, unknown map[string]any) string {
	b, k := rowrun.TextCell(row, c.At(name))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, name, c.At(name), k)
		return ""
	}
	return cleanField(raw, field, b)
}

func callNames() []string {
	specs := callSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func mapCall(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "calls", callNames()...)
	raw := map[string]any{}
	unknown := map[string]any{}
	cl := call.Call{Raw: raw}

	typ, tk := rowrun.IntCell(row, c.At("type"))
	rowrun.Flag(unknown, row, "type", c.At("type"), tk)
	dur, dk := rowrun.IntCell(row, c.At("duration"))
	rowrun.Flag(unknown, row, "duration", c.At("duration"), dk)
	cl.Direction, cl.Outcome = callType(typ, tk == rowrun.CellValue, dur, dk == rowrun.CellValue)

	if nb, nk := rowrun.TextCell(row, c.At("number")); nk == rowrun.CellValue && len(nb) > 0 {
		t := common.CleanText(nb, maxText)
		cl.Address = t.Text
		if len(t.Flags) > 0 {
			fl := make([]any, len(t.Flags))
			for i, f := range t.Flags {
				fl[i] = f
			}
			raw["number_flags"] = fl
			if t.RawB64 != "" {
				raw["number_raw_b64"] = t.RawB64
			}
		}
	} else {
		rowrun.Flag(unknown, row, "number", c.At("number"), nk)
	}
	if pv, pk := rowrun.IntCell(row, c.At("presentation")); pk == rowrun.CellValue {
		cl.AddressPresentation = presentation(pv)
	} else {
		rowrun.Flag(unknown, row, "presentation", c.At("presentation"), pk)
	}
	cl.NameCached = cleanText(row, c, "name", "name_cached", raw, unknown)
	cl.CountryISO = cleanText(row, c, "countryiso", "country_iso", raw, unknown)
	cl.GeoDescription = cleanText(row, c, "geocoded_location", "geo_description", raw, unknown)
	exact := dk == rowrun.CellValue && dur >= -maxExact && dur <= maxExact
	if dk == rowrun.CellValue && !exact {
		// T62: beyond 2^53 an integer is not a float64; stating its rounding would invent a value.
		unknown["duration"] = "precision"
	}
	if exact && dur >= 0 {
		f := float64(dur)
		cl.DurationS = &f
	}
	if fv, fk := rowrun.IntCell(row, c.At("features")); fk == rowrun.CellValue {
		if fv&1 != 0 {
			cl.Kind = call.KindVideo
		} else {
			cl.Kind = call.KindVoice
		}
	} else {
		rowrun.Flag(unknown, row, "features", c.At("features"), fk)
	}
	sidInt, sk := rowrun.IntCell(row, c.At("subscription_id"))
	switch sk {
	case rowrun.CellValue:
		raw["subscription_id"] = sidInt
		if sidInt >= 0 {
			cl.Subscription = &sidInt
		}
	case rowrun.CellOther:
		if tb, tk := rowrun.TextCell(row, c.At("subscription_id")); tk == rowrun.CellValue {
			txt := cleanField(raw, "subscription_id", tb)
			raw["subscription_id"] = txt
			if n, perr := parseSmall(txt); perr == nil && n >= 0 {
				cl.Subscription = &n
			}
		} else {
			// T61: a REAL or BLOB is neither a subscription nor an absence.
			rowrun.Flag(unknown, row, "subscription_id", c.At("subscription_id"), sk)
		}
	default:
		rowrun.Flag(unknown, row, "subscription_id", c.At("subscription_id"), sk)
	}
	if v, k := rowrun.IntCell(row, c.At("is_read")); k == rowrun.CellValue && (v == 0 || v == 1) {
		b := v == 1
		cl.Read = &b
	}
	if v, k := rowrun.IntCell(row, c.At("new")); k == rowrun.CellValue && (v == 0 || v == 1) {
		b := v == 1
		cl.New = &b
	}
	for _, name := range []string{"type", "duration", "presentation", "features", "is_read", "new"} {
		if v, k := rowrun.IntCell(row, c.At(name)); k == rowrun.CellValue {
			raw[name] = v
		} else {
			rowrun.Flag(unknown, row, name, c.At(name), k)
		}
	}

	// SourcePath is the remote path of the file that holds the cell (ruling T48).
	rec := records.Record{Type: call.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	date, datek := rowrun.IntCell(row, c.At("date"))
	rowrun.Flag(unknown, row, "date", c.At("date"), datek)
	if datek == rowrun.CellValue {
		in := rowrun.Millis(date)
		in.Record(raw, "date")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
			if dk == rowrun.CellValue && dur > 0 && dur <= maxDuration {
				if end := in.T.Add(time.Duration(dur) * time.Second); end.Before(ts.Max()) {
					rec.TimeEnd = &records.Time{T: end}
				}
			}
		}
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Summary = call.Summary(cl.Direction, cl.Outcome, cl.Address)
	rec.Payload = cl.Payload()
	return []records.Record{rec}, nil
}
