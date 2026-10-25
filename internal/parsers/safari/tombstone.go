package safari

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

func tombstoneSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "start_time", Required: true, Affinity: "real"},
		{Name: "end_time", Required: true, Affinity: "real"},
		{Name: "url", Affinity: "text"},
		{Name: "generation", Affinity: "integer"},
	}
}

func tombstonesMapping() parse.TableMapping { return tombstonesCounting(nil) }

// tombstonesCounting is the history_tombstones mapping; when without is not nil it counts the rows that have
// no usable url (4F-R13), which Parse states in the note tombstones_without_url. The mapping itself keeps no
// state: with a nil counter it is pure.
func tombstonesCounting(without *int64) parse.TableMapping {
	return parse.TableMapping{Table: "history_tombstones", Columns: tombstoneSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return mapTombstone(m, row, without)
	}}
}

// mapTombstone maps a history_tombstones row. It is the application's own deletion marker: the record is
// Deleted with no Recovery, has no Record.Time (the instant of the deleted visit is unknown) and states the
// deleted range as Times (4F-R13).
func mapTombstone(m parse.MapContext, row parse.Row, without *int64) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "history_tombstones", "start_time", "end_time", "url", "generation")
	urlb, k := rowrun.TextCell(row, c.At("url"))
	if k != rowrun.CellValue || len(urlb) == 0 {
		if without != nil {
			*without++
		}
		return nil, nil
	}
	raw, unknown := map[string]any{}, map[string]any{}
	v := web.Visit{Browser: browser, Deleted: map[string]any{"source": "history_tombstones"}}
	v.URL = cleanText(raw, "url", urlb, maxURL)
	rec := records.Record{Type: web.VisitType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range, Deleted: true}
	for _, e := range []struct{ col, kind string }{{"start_time", "deleted_range_start"}, {"end_time", "deleted_range_end"}} {
		f, fk := floatCell(row, c.At(e.col))
		if fk != rowrun.CellValue {
			rowrun.Flag(unknown, row, e.col, c.At(e.col), fk)
			continue
		}
		in := cocoa(f)
		in.Record(raw, e.col)
		if in.Valid() {
			rec.Times = append(rec.Times, records.NamedTime{Kind: e.kind, Time: records.Time{T: in.T}})
		}
	}
	if g, gk := rowrun.IntCell(row, c.At("generation")); gk == rowrun.CellValue {
		raw["generation"] = g
	} else {
		rowrun.Flag(unknown, row, "generation", c.At("generation"), gk)
	}
	return []records.Record{finish(rec, v, raw, unknown, nil, row, "")}, nil
}
