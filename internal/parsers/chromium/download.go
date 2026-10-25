package chromium

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func downloadSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "id", Required: true, Affinity: "integer"},
		{Name: "target_path", Required: true, Affinity: "text"},
		{Name: "start_time", Required: true, Affinity: "integer"},
		{Name: "current_path", Affinity: "text"},
		{Name: "guid", Affinity: "text"},
		{Name: "received_bytes", Affinity: "integer"},
		{Name: "total_bytes", Affinity: "integer"},
		{Name: "state", Affinity: "integer"},
		{Name: "danger_type", Affinity: "integer"},
		{Name: "interrupt_reason", Affinity: "integer"},
		{Name: "hash", Affinity: "blob"},
		{Name: "end_time", Affinity: "integer"},
		{Name: "opened", Affinity: "integer"},
		{Name: "last_access_time", Affinity: "integer"},
		{Name: "transient", Affinity: "integer"},
		{Name: "referrer", Affinity: "text"},
		{Name: "site_url", Affinity: "text"},
		{Name: "tab_url", Affinity: "text"},
		{Name: "tab_referrer_url", Affinity: "text"},
		{Name: "http_method", Affinity: "text"},
		{Name: "mime_type", Affinity: "text"},
		{Name: "original_mime_type", Affinity: "text"},
	}
}

func downloadsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "downloads", Columns: downloadSpecs(), Map: mapDownload}
}

// mapDownload is filled in by Task 4 of plan 4F.
func mapDownload(_ parse.MapContext, _ parse.Row) ([]records.Record, error) { return nil, nil }
