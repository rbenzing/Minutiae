package ioscalls

import (
	"context"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// Identity of the parser.
const (
	Name    = "ios.calls"
	Version = "1.0.3"
)

// Parser is the ios.calls parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims are exactly ClaimableTables: a promise that typed recovery (plan 4G) can map the
// rows of ZCALLRECORD and call that were deleted or superseded (both mappings give one record per row, 4h-R6).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS call history",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: call.Type, PayloadVersion: call.PayloadVersion}},
		Claims:    claimsOf(ClaimableTables()),
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{
				"ios:*/Library/CallHistoryDB/CallHistory.storedata",
				"ios:*/Library/CallHistory/call_history.db",
			}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

func coreReq() rowrun.Req {
	return rowrun.Req{Table: tableCore, Need: []string{"Z_PK", "ZDATE"}, Alias: "Z_PK"}
}

func legacyReq() rowrun.Req {
	return rowrun.Req{Table: tableLegacy, Need: []string{"ROWID", "date"}, Alias: "ROWID"}
}

// Probe (ruling 4E-R30): the file is applicable when either table set is complete, unsupported-schema when a
// table that is present lacks what its mapping needs (the other table being complete does not rescue it),
// not-applicable when neither table exists, and encrypted for an encrypted file. With a -wal or -journal
// companion the schema decision is deferred to Parse (rowrun.Probe).
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	var got [2]parse.Applicability
	for i, rq := range []rowrun.Req{coreReq(), legacyReq()} {
		a, err := rowrun.Probe(ctx, in, []rowrun.Req{rq})
		if err != nil {
			return parse.Applicability{}, err
		}
		got[i] = a
	}
	for _, st := range []string{parse.Encrypted, parse.UnsupportedSchema, parse.Applicable} {
		for _, a := range got {
			if a.Status == st {
				return a, nil
			}
		}
	}
	// neither table: say so for both, unless the file is not a database at all
	if strings.HasPrefix(got[0].Reason, "table ") && strings.HasPrefix(got[1].Reason, "table ") {
		return parse.Applicability{Status: parse.NotApplicable, Reason: "neither table " + tableCore + " nor table " + tableLegacy + " is present"}, nil
	}
	return got[0], nil
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{coreMapping(), legacyMapping()}
}

// Parse runs both mappings as optional steps through rowrun.Run (ruling 4E-R30). When neither table exists
// (possible after a deferred Probe) it ends as the identifying parsers do: one analysis.warning, the note
// not_applicable, no record.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	ms := p.Mappings()
	steps := make([]rowrun.Step, len(ms))
	for i, mp := range ms {
		steps[i] = rowrun.Step{Mapping: mp, Optional: true}
	}
	rep, err := rowrun.Run(ctx, in, out, steps)
	if err != nil {
		return err
	}
	if len(rep.Tables) == len(steps) {
		absent := 0
		for _, t := range rep.Tables {
			if t.Absent {
				absent++
			}
		}
		if absent == len(steps) {
			why := "neither table " + tableCore + " nor table " + tableLegacy + " exists"
			out.Note("not_applicable", why)
			return out.Warn(ctx, "sqlite:schema", "not applicable at parse time: "+why)
		}
	}
	return nil
}

// ClaimableTables returns the tables whose mapping is total (every row gives one record or a RowError): ZCALLRECORD
// and call, one record per row each (4h-R6). A call joins nothing live, so a recovered row needs no other table.
func ClaimableTables() []string { return []string{tableCore, tableLegacy} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}
