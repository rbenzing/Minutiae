package chromium

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
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

// mapSearch is filled in by Task 4 of plan 4F.
func mapSearch(_ parse.MapContext, _ parse.Row) ([]records.Record, error) { return nil, nil }
