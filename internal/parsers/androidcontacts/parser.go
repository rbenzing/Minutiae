// Package androidcontacts parses the Android contacts provider database
// (contacts2.db): one contact record per raw_contacts row, joined to its typed data
// rows, its account (accounts, through raw_contacts.account_id) and the last update of
// its aggregate contact (contacts, through raw_contacts.contact_id), plus a tombstone
// record per deleted_contacts row.
//
// The provider keeps no modification time per raw contact. The only time it holds is the
// aggregate contact's last update, which the parser states under the time key
// contact.AggregateLastUpdated and never as Record.Time. A join that cannot be decided
// (a missing table, pages the decoder lost) is listed in raw.join_undecided; a dangling
// id is stated as raw.account_state or raw.aggregate_state; neither is shown as an
// absent account or time.
package androidcontacts

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// Identity of the parser.
const (
	Name    = "android.contacts"
	Version = "1.0.1"
)

// Parser is the android.contacts parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims stay empty until plan 4G implements typed recovery.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android contacts (contacts provider)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: contact.Type, PayloadVersion: contact.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"android:**/com.android.providers.contacts/databases/contacts2.db"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Probe: raw_contacts is the identifying table; when it exists, data and mimetypes must too
// (a contact without its data rows would be a contact that looks empty). accounts and contacts
// are optional (an older schema keeps the account inline and has no aggregate time), but when
// they exist their key is the rowid alias and the columns read exist, or the join would be wrong.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, []rowrun.Req{
		{Table: "raw_contacts", Need: []string{"display_name"}, With: []string{"data", "mimetypes"}},
		{Table: "data", Need: []string{"raw_contact_id", "mimetype_id"}, Optional: true},
		{Table: "mimetypes", Need: []string{"mimetype"}, Alias: "_id", Optional: true},
		{Table: "accounts", Need: []string{"account_name", "account_type"}, Alias: "_id", Optional: true},
		{Table: "contacts", Need: []string{"contact_last_updated_timestamp"}, Alias: "_id", Optional: true},
		{Table: "deleted_contacts", Need: []string{"contact_id"}, Optional: true},
	})
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{rawContactsMapping(), deletedContactsMapping()}
}

// Parse runs the mappings through rowrun.Run; deleted_contacts is optional.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	ms := p.Mappings()
	steps := make([]rowrun.Step, len(ms))
	for i, mp := range ms {
		steps[i] = rowrun.Step{Mapping: mp, Optional: mp.Table != "raw_contacts", Identifying: mp.Table == "raw_contacts"}
	}
	_, err := rowrun.Run(ctx, in, out, steps)
	return err
}
