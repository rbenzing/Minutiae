package iosaddressbook

import (
	"context"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// Identity of the parser.
const (
	Name    = "ios.addressbook"
	Version = "1.0.1"
)

// Parser is the ios.addressbook parser. The zero value is the parser; it holds no state.
type Parser struct {
	photoCap int // test seam: the cap of the photo-presence set, 0 for the default
}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. Claims are exactly ClaimableTables: a promise that typed recovery (plan 4G) can map
// the claimed table's recovered rows, each to one person or a counted unmapped reason (4h-R6).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS address book",
		Platforms: []string{parse.PlatformIOS},
		Claims:    claimsOf(ClaimableTables()),
		Emits:     []parse.Emit{{Type: contact.Type, PayloadVersion: contact.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"ios:*/Library/AddressBook/AddressBook.sqlitedb"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
			{Role: "images", Globs: []string{"./AddressBookImages.sqlitedb"}},
			{Role: "images_wal", Globs: []string{"./AddressBookImages.sqlitedb-wal"}},
		},
	}
}

// Probe: ABPerson is the identifying table, its ROWID the alias every join goes through. Every join table is
// optional (an older or stripped database still names its people), but when one exists the columns the joins
// read exist, or the join would be wrong.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	return rowrun.Probe(ctx, in, []rowrun.Req{
		{Table: tablePerson, Need: []string{"ROWID"}, Alias: "ROWID"},
		{Table: "ABMultiValue", Need: []string{"record_id", "property", "label", "value"}, Alias: "UID", Optional: true},
		{Table: "ABMultiValueLabel", Need: []string{"value"}, Optional: true},
		{Table: "ABMultiValueEntry", Need: []string{"parent_id", "key", "value"}, Optional: true},
		{Table: "ABMultiValueEntryKey", Need: []string{"value"}, Optional: true},
		{Table: "ABStore", Need: []string{"ROWID"}, Alias: "ROWID", Optional: true},
		{Table: "ABGroup", Need: []string{"ROWID"}, Alias: "ROWID", Optional: true},
		{Table: "ABGroupMembers", Need: []string{"group_id", "member_type", "member_id"}, Optional: true},
	})
}

// Mappings are the per-table mappings, in the order Parse runs them.
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{personMapping()} }

// Parse reads the photo presence from the images database BEFORE the rows are mapped (ruling 4E-R25), then runs
// the ABPerson mapping through rowrun.Run with an emitter that adds photo.present. The note "images" says what
// the images database gave: absent, read, or why it decided less.
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	set, why, err := p.photos(ctx, in)
	if err != nil {
		return err
	}
	defer in.Budget.Free(set.charged)
	if why == "" {
		why = "read"
	}
	out.Note("images", why)
	em := out
	if set.known {
		em = photoEmitter{Emitter: out, set: set, known: true}
	}
	ms := p.Mappings()
	_, err = rowrun.Run(ctx, in, em, []rowrun.Step{{Mapping: ms[0], Identifying: true}})
	return err
}

// photos reads the photo-presence set with the parser's cap.
func (p Parser) photos(ctx context.Context, in *parse.Input) (photoSet, string, error) {
	if p.photoCap > 0 {
		return photoIndexLimit(ctx, in, p.photoCap)
	}
	return photoIndex(ctx, in)
}

// ClaimableTables returns the tables whose mapping is total (every row gives one record or an Unmappable counted
// warning): ABPerson (4h-R6). A person that identifies nobody, or one without a rowid, is unmapped, never invented.
func ClaimableTables() []string { return []string{"ABPerson"} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}
