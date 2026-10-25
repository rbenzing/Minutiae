package chromium

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// Identity of the parser.
const (
	Name    = "chromium.history"
	Version = "1.0.0"
)

// Parser is the chromium.history parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims stay empty until plan 4G implements typed recovery.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Chromium-family browser history (History database)",
		Platforms: []string{parse.PlatformAndroid},
		Emits: []parse.Emit{
			{Type: web.VisitType, PayloadVersion: web.PayloadVersion},
			{Type: web.SearchType, PayloadVersion: web.PayloadVersion},
			{Type: web.DownloadType, PayloadVersion: web.PayloadVersion},
		},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{
				"android:**/app_chrome/*/History",
				"android:**/app_webview/**/History",
			}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Probe: the urls and visits tables with the columns the mappings need (urls.id must be the rowid alias); the
// search and download tables are optional, and downloads needs its chain table.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, []rowrun.Req{
		{Table: "urls", Need: []string{"url", "last_visit_time"}, Alias: "id"},
		{Table: "visits", Need: []string{"url", "visit_time", "transition"}},
		{Table: "keyword_search_terms", Need: []string{"keyword_id", "url_id", "term"}, Optional: true},
		{Table: "downloads", Need: []string{"id", "target_path", "start_time"}, Optional: true, With: []string{"downloads_url_chains"}},
	})
}

// Mappings in step order: urls, visits, keyword_search_terms, downloads, meta.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{urlsMapping(), visitsMapping(), searchMapping(), downloadsMapping(), metaMapping()}
}

// Parse runs the mappings through rowrun.Run: urls identifies the file, visits is required, the rest is optional.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	ms := p.Mappings()
	steps := make([]rowrun.Step, len(ms))
	for i, mp := range ms {
		steps[i] = rowrun.Step{Mapping: mp, Optional: i >= 2}
	}
	steps[0].Identifying = true
	_, err := rowrun.Run(ctx, in, out, steps)
	return err
}

// ClaimableTables returns the tables whose mapping is total (4F-R3): every row gives one record or a row error.
func ClaimableTables() []string { return []string{"visits", "keyword_search_terms", "downloads"} }

func names(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}
