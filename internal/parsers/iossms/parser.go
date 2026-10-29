package iossms

import (
	"context"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "ios.sms"
	Version = "1.0.0"
)

// Parser is the ios.sms parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims stay empty until plan 4G implements typed recovery.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS Messages (sms.db)",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"ios:*/Library/SMS/sms.db"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// probeReqs are the tables the mapping reads. Only message identifies the file; every other table is
// optional (a variant without it leaves participants, thread or attachments unknown, never empty) but a
// present one must have the columns and the rowid alias its joins need.
func probeReqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableMessage, Need: []string{"ROWID", "guid", "date", "is_from_me", "handle_id"}, Alias: "ROWID"},
		{Table: "handle", Need: []string{"ROWID", "id"}, Alias: "ROWID", Optional: true},
		{Table: "chat", Need: []string{"ROWID"}, Alias: "ROWID", Optional: true},
		{Table: "chat_message_join", Need: []string{"chat_id", "message_id"}, Optional: true},
		{Table: "chat_handle_join", Need: []string{"chat_id", "handle_id"}, Optional: true},
		{Table: "attachment", Need: []string{"ROWID", "filename"}, Alias: "ROWID", Optional: true},
		{Table: "message_attachment_join", Need: []string{"message_id", "attachment_id"}, Optional: true},
		{Table: "chat_recoverable_message_join", Need: []string{"chat_id", "message_id", "delete_date"}, Optional: true},
	}
}

// Probe decides from the main file's schema (rowrun.Probe, D4).
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, probeReqs())
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{messageMapping()}
}

// Parse maps the message table through rowrun.Run; message is the identifying table. The messages whose text
// column and attributedBody disagree (4E-R14) are counted in the note attributed_body_differs.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	cnt := &counting{Emitter: out}
	_, err := rowrun.Run(ctx, in, cnt, []rowrun.Step{{Mapping: messageMapping(), Identifying: true}})
	if cnt.differs > 0 {
		out.Note("attributed_body_differs", strconv.FormatInt(cnt.differs, 10))
	}
	return err
}

// counting counts the emitted records that carry raw.attributed_body_differs.
type counting struct {
	parse.Emitter
	differs int64
}

func (c *counting) Emit(ctx context.Context, r records.Record) error {
	if raw, ok := r.Payload["raw"].(map[string]any); ok && raw["attributed_body_differs"] == true {
		c.differs++
	}
	return c.Emitter.Emit(ctx, r)
}
