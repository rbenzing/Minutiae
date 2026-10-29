// Package androidcalllog parses the Android call log (the calls table of calllog.db,
// or of contacts2.db on older releases) as call records.
package androidcalllog

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// Identity of the parser.
const (
	Name    = "android.calllog"
	Version = "1.0.6"
)

// Parser is the android.calllog parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims is the table the mapping maps: a promise that typed recovery (plan 4G) can map
// the rows of calls that were deleted or superseded (the mapping is source-agnostic).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android call log (contacts provider)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: call.Type, PayloadVersion: call.PayloadVersion}},
		Claims:    []parse.TableClaim{{Role: "db", Table: "calls"}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{
				"android:**/com.android.providers.contacts/databases/calllog.db",
				"android:**/com.android.providers.contacts/databases/contacts2.db",
			}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Probe: a contacts2.db without a calls table is a benign skip, a calls table
// without the columns the mapping needs is unsupported-schema.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, []rowrun.Req{{Table: "calls", Need: []string{"number", "date", "duration", "type"}}})
}

// Mappings are the per-table mappings.
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{callsMapping()} }

// Parse runs the mappings through rowrun.Run.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	ms := p.Mappings()
	steps := make([]rowrun.Step, len(ms))
	for i, mp := range ms {
		steps[i] = rowrun.Step{Mapping: mp, Identifying: true}
	}
	_, err := rowrun.Run(ctx, in, out, steps)
	return err
}
