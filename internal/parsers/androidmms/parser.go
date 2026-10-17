// Package androidmms parses the Android telephony provider database (mmssms.db):
// SMS and MMS messages as message records. It is a set of pure per-table mappings
// (Mappings) driven by rowrun.Run; the same mappings serve the typed recovery pass
// of plan 4G.
package androidmms

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "android.mmssms"
	Version = "1.0.1"
)

// Parser is the android.mmssms parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims stay empty until plan 4G implements typed recovery.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android SMS and MMS (telephony provider)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"android:**/com.android.providers.telephony/databases/mmssms.db"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

func probeReqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: "sms", Need: []string{"address", "date", "type", "body"}},
		{Table: "pdu", Need: []string{"date", "msg_box"}, Optional: true, With: []string{"addr", "part"}},
		{Table: "addr", Need: []string{"msg_id", "address", "type"}, Optional: true},
		{Table: "part", Need: []string{"mid", "ct"}, Optional: true},
		{Table: "threads", Need: []string{"recipient_ids"}, Alias: "_id", Optional: true},
		{Table: "canonical_addresses", Need: []string{"address"}, Alias: "_id", Optional: true},
	}
}

// Probe decides from the main file's schema (rowrun.Probe, D4).
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, probeReqs())
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{smsMapping(), pduMapping()}
}

// Parse maps every table through rowrun.Run, which fails the job when the mapping
// context fails and warns for rows it cannot map.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	ms := p.Mappings()
	steps := make([]rowrun.Step, len(ms))
	for i, mp := range ms {
		steps[i] = rowrun.Step{Mapping: mp, Optional: mp.Table != "sms", Identifying: mp.Table == "sms"}
	}
	_, err := rowrun.Run(ctx, in, out, steps)
	return err
}
