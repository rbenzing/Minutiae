package safari

import (
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// rawIntVisit are the columns of a visit stated in raw as stored integers, uninterpreted (4F-R24).
func rawIntVisit() []string {
	return []string{"http_non_get", "synthesized", "origin", "generation", "attributes", "score"}
}

func visitSpecs() []parse.ColumnSpec {
	specs := []parse.ColumnSpec{
		{Name: "history_item", Required: true, Affinity: "integer"},
		{Name: "visit_time", Required: true, Affinity: "real"},
		{Name: "title", Affinity: "text"},
		{Name: "load_successful", Affinity: "integer"},
		{Name: "redirect_source", Affinity: "integer"},
		{Name: "redirect_destination", Affinity: "integer"},
	}
	for _, n := range rawIntVisit() {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "integer"})
	}
	return specs
}

func visitsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "history_visits", Columns: visitSpecs(), Map: mapVisit}
}

func visitNames() []string {
	specs := visitSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// mapVisit maps a history_visits row, joined to its history_items row by rowid. A visit whose item is
// absent, undecidable or without a usable URL is a RowError, never a record with an invented URL (4F-R4).
func mapVisit(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "history_visits", visitNames()...)
	key, k := rowrun.IntCell(row, c.At("history_item"))
	if k != rowrun.CellValue {
		return nil, rowrun.Unmappable("history_visits.history_item holds no integer key")
	}
	item, ic, err := getItem(m, key)
	if err != nil {
		return nil, err
	}
	urlb, ok := itemURL(item, ic)
	if !ok {
		return nil, rowrun.Unmappable("the history_items row of history_item has no usable url")
	}

	raw, unknown, joined := map[string]any{}, map[string]any{}, map[string]any{}
	v := web.Visit{Browser: browser}
	v.URL = cleanText(raw, "url", urlb, maxURL)
	title := ""
	if tb, tk := rowrun.TextCell(row, c.At("title")); tk == rowrun.CellValue {
		title = cleanText(raw, "title", tb, maxText)
		v.Title = title
	} else {
		rowrun.Flag(unknown, row, "title", c.At("title"), tk)
	}
	if id, ok := row.Rowid(); ok {
		v.VisitID = strconv.FormatInt(id, 10)
	}
	itemFacts(item, ic, &v, raw, joined, "history_items.")

	rec := records.Record{Type: web.VisitType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if f, fk := floatCell(row, c.At("visit_time")); fk == rowrun.CellValue {
		in := cocoa(f)
		in.Record(raw, "visit_time")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
		}
	} else {
		rowrun.Flag(unknown, row, "visit_time", c.At("visit_time"), fk)
	}
	if n, lk := rowrun.IntCell(row, c.At("load_successful")); lk == rowrun.CellValue {
		if n == 0 || n == 1 {
			b := n == 1
			v.LoadSuccessful = &b
		} else {
			unknown["load_successful"] = "not-boolean"
		}
	} else {
		rowrun.Flag(unknown, row, "load_successful", c.At("load_successful"), lk)
	}
	for _, r := range []struct {
		col string
		dst *string
	}{{"redirect_source", &v.RedirectSource}, {"redirect_destination", &v.RedirectDestination}} {
		if n, rk := rowrun.IntCell(row, c.At(r.col)); rk == rowrun.CellValue {
			*r.dst = strconv.FormatInt(n, 10)
			raw[r.col] = n
		} else {
			rowrun.Flag(unknown, row, r.col, c.At(r.col), rk)
		}
	}
	for _, col := range rawIntVisit() {
		if n, rk := rowrun.IntCell(row, c.At(col)); rk == rowrun.CellValue {
			raw[col] = n
		} else {
			rowrun.Flag(unknown, row, col, c.At(col), rk)
		}
	}
	return []records.Record{finish(rec, v, raw, unknown, joined, row, title)}, nil
}
