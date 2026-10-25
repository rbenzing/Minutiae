package safari

import (
	"context"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// Identity of the parser.
const (
	Name    = "safari.history"
	Version = "1.0.0"
)

// Parser is the safari.history parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims stay empty until plan 4G implements typed recovery.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Safari browsing history (iOS History.db)",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: web.VisitType, PayloadVersion: web.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"ios:*/Library/Safari/History.db"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Probe: history_items and history_visits identify a Safari database (a database without them is a benign
// skip), and the columns the mappings need must exist or the schema is unsupported. history_items.id must
// be the rowid alias: the visit join is by rowid (4F-R30). history_tombstones is optional.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, []rowrun.Req{
		{Table: "history_items", Need: []string{"id", "url"}, Alias: "id"},
		{Table: "history_visits", Need: []string{"history_item", "visit_time"}},
		{Table: "history_tombstones", Need: []string{"start_time", "end_time"}, Optional: true},
	})
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{itemsMapping(), visitsMapping(), tombstonesMapping()}
}

// Parse runs the mappings through rowrun.Run; history_tombstones is optional. The tombstones that have no
// usable url are counted in the note tombstones_without_url.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	var without int64
	steps := []rowrun.Step{
		{Mapping: itemsMapping(), Identifying: true},
		{Mapping: visitsMapping()},
		{Mapping: tombstonesCounting(&without), Optional: true},
	}
	_, err := rowrun.Run(ctx, in, out, steps)
	if without > 0 {
		out.Note("tombstones_without_url", strconv.FormatInt(without, 10))
	}
	return err
}

// ClaimableTables returns the tables whose mapping is total (every row gives one record or a RowError):
// history_visits (4F-R3). The other tables emit only on a decided condition and must not be claimed.
func ClaimableTables() []string { return []string{"history_visits"} }
