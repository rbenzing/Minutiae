package chromium

import (
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

func searchSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "keyword_id", Required: true, Affinity: "integer"},
		{Name: "url_id", Required: true, Affinity: "integer"},
		{Name: "term", Required: true, Affinity: "text"},
		{Name: "normalized_term", Affinity: "text"},
	}
}

func searchMapping() parse.TableMapping {
	return parse.TableMapping{Table: "keyword_search_terms", Columns: searchSpecs(), Map: mapSearch}
}

// stampLabel adds the package and, for a record type with no profile slot of its own (a search or a download), the
// profile to raw (4F-C1).
func stampLabel(raw map[string]any, profile, pkg string) {
	stamp(raw, pkg)
	if profile != "" {
		raw["profile"] = profile
	}
}

// mapSearch gives one web_search per row (4F-R10). The row has no time of its own: the time and url come from the
// urls row named by url_id and are stated as that URL's last visit. The engine is never stated.
func mapSearch(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "keyword_search_terms", names(searchSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}

	tb, k := rowrun.TextCell(row, c.At("term"))
	if k != rowrun.CellValue || len(tb) == 0 {
		return nil, rowrun.Unmappable("the search term is empty or holds no usable text")
	}
	browser, profile, pkg := labelOf(m.Input().Primary.Logical)
	s := web.Search{Term: capped(raw, "term", tb, maxText), Browser: browser, Raw: raw}
	if nb, nk := rowrun.TextCell(row, c.At("normalized_term")); nk == rowrun.CellValue && len(nb) > 0 {
		s.NormalizedTerm = capped(raw, "normalized_term", nb, maxText)
	} else if nk != rowrun.CellValue && nk != rowrun.CellNull {
		rowrun.Flag(unknown, row, "normalized_term", c.At("normalized_term"), nk)
	}
	if kv, kk := rowrun.IntCell(row, c.At("keyword_id")); kk == rowrun.CellValue {
		s.EngineID = strconv.FormatInt(kv, 10)
	} else {
		rowrun.Flag(unknown, row, "keyword_id", c.At("keyword_id"), kk)
	}

	rec := records.Record{Type: web.SearchType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	uid, uk := rowrun.IntCell(row, c.At("url_id"))
	if uk == rowrun.CellValue {
		raw["url_id"] = uid
		ur, state, err := urlOf(m, uid)
		if err != nil {
			return nil, err
		}
		switch state {
		case urlAbsent:
			raw["url_state"] = "no-urls-row"
		case urlUndecided:
			raw["join_undecided"] = []any{"urls"}
		default:
			if u, ok := ur.usableURL(raw); ok {
				s.URL = u
			}
			if lv, lk := rowrun.IntCell(ur.row, ur.cols.At("last_visit_time")); lk == rowrun.CellValue {
				in := webkit(lv)
				in.Record(raw, "last_visit_time")
				if in.Valid() {
					rec.Time = &records.Time{T: in.T}
					s.TimeSource = "urls.last_visit_time"
				}
			} else {
				rowrun.Flag(unknown, ur.row, "last_visit_time", ur.cols.At("last_visit_time"), lk)
			}
		}
	} else {
		rowrun.Flag(unknown, row, "url_id", c.At("url_id"), uk)
	}
	stampLabel(raw, profile, pkg)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Body = s.Term
	rec.Summary = web.SearchSummary(s)
	rec.Payload = s.Payload()
	return []records.Record{rec}, nil
}
