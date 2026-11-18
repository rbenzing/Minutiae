package ioswhatsapp

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "ios.whatsapp.chats"
	Version = "1.0.0"
)

// Parser is the ios.whatsapp.chats parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Errors Parse returns, wrapped with %w (plan 4I2, "Notes and warnings").
var (
	// ErrUnsupportedModel: the Core Data model does not name the entities the parser needs; the reason follows. Probe
	// reports the same finding as unsupported-schema when no companion defers it to Parse (PF9 b).
	ErrUnsupportedModel = errors.New("ios.whatsapp.chats: the Core Data model does not name the WhatsApp entities")
	// ErrNotWhatsAppPath: the primary's logical path is none of the places the globs name.
	ErrNotWhatsAppPath = errors.New("ios.whatsapp.chats: not a WhatsApp app group path")
	// ErrNoWhatsAppTables: a database at a backup domain of WhatsApp lacks the identifying table.
	ErrNoWhatsAppTables = errors.New("ios.whatsapp.chats: the identifying table is absent at the WhatsApp app group path")
)

const (
	tableMessage = "ZWAMESSAGE"
	tableChat    = "ZWACHATSESSION"
	entMessage   = "WAMessage"
	entChat      = "WAChatSession"
)

// Meta describes the parser: the roles and globs of plan 4I2 "Globs and places". It claims no table until the claims
// are earned (plan 4I2 task 6).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS WhatsApp chats (ChatStorage.sqlite)",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{
				"ios:" + domainConsumer + "/" + dbName,
				"ios:" + domainBusiness + "/" + dbName,
				appGroupRoot + "*/" + dbName,
			}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
			{Role: "container_meta", Globs: []string{"./.com.apple.mobile_container_manager.metadata.plist"}},
		},
	}
}

// probeReqs are the tables the mapping reads (PF3): ZWAMESSAGE identifies the store and needs ZWACHATSESSION beside it
// (spec 6.11 signature); the other tables are optional and their absence is stated by the mapping, never read as "no
// row". Z_PRIMARYKEY is optional here so that its absence reaches readModel and gets the 4I2-R3 reason, not rowrun's
// generic "table absent".
func probeReqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableMessage, Need: []string{"Z_PK", "ZCHATSESSION", "ZISFROMME", "ZMESSAGEDATE", "ZMESSAGETYPE"}, Alias: "Z_PK", With: []string{tableChat}},
		{Table: tableChat, Need: []string{"ZCONTACTJID"}, Alias: "Z_PK"},
		{Table: zPrimaryKey, Need: []string{"Z_ENT", "Z_NAME"}, Optional: true},
		{Table: "ZWAMEDIAITEM", Need: []string{"ZMESSAGE"}, Alias: "Z_PK", Optional: true},
		{Table: "ZWAGROUPMEMBER", Need: []string{"ZMEMBERJID"}, Alias: "Z_PK", Optional: true},
		{Table: "ZWAMESSAGEINFO", Alias: "Z_PK", Optional: true},
	}
}

const (
	reasonNotSQLite = "not an SQLite database" // rowrun.Probe's reason for a file that is no database
	notSQLiteAtDom  = "not a SQLite file at the WhatsApp app group path"
	noTablesAtDom   = "no WhatsApp ChatStorage tables at the WhatsApp app group path"
)

// Probe decides from the main file only (4I2-R2, PF9): at a backup domain the app is named, so a file that is no
// database or lacks the identifying table is unsupported-schema, not benign; at an app group it is not-applicable (a
// same-named file of another app). Then, without a wal or journal companion, the Core Data model is read from the main
// file and a *modelError is unsupported-schema (4I2-R3); with a companion every schema finding is deferred to Parse, as
// rowrun does. The model read is bounded by the probe's read limit: an error that wraps parse.ErrProbeLimit is
// returned, never Applicable.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	p := classifyPlace(in.Primary.Logical)
	if p.Place == placeOther {
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "not a WhatsApp app group path"}, nil
	}
	got, err := rowrun.Probe(ctx, in, probeReqs())
	if err != nil {
		return parse.Applicability{}, err
	}
	if got.Status == parse.NotApplicable && p.Place == placeDomain {
		reason := noTablesAtDom
		if got.Reason == reasonNotSQLite {
			reason = notSQLiteAtDom
		}
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reason}, nil
	}
	_, wal := in.Artifacts["wal"]
	_, journal := in.Artifacts["journal"]
	if got.Status != parse.Applicable || wal || journal {
		return got, nil
	}
	_, err = readModel(ctx, sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget, entMessage, entChat)
	var me *modelError
	switch {
	case errors.As(err, &me):
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: me.Reason}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	return got, nil
}

// Mappings is empty until the ZWAMESSAGE mapping (plan 4I2 task 4).
func (Parser) Mappings() []parse.TableMapping { return nil }

// Parse is the skeleton of the parser: the place, the app label and the model notes. The mapping follows in later
// tasks; with no step rowrun.Run only opens the files and forwards the library's warnings.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	p := classifyPlace(in.Primary.Logical)
	if p.Place == placeOther {
		return fmt.Errorf("%w: %s", ErrNotWhatsAppPath, in.Primary.Logical)
	}
	l := labelOf(in, p)
	l.note(out, p)
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	present, err := hasTable(ctx, files, in.Budget, tableMessage)
	if err != nil {
		return err
	}
	switch {
	case present:
		m, err := readModel(ctx, files, in.Budget, entMessage, entChat)
		var me *modelError
		if errors.As(err, &me) {
			return fmt.Errorf("%w: %s", ErrUnsupportedModel, me.Reason)
		} else if err != nil {
			return err
		}
		out.Note("entity."+entMessage, strconv.FormatInt(m.ByName[entMessage].Ent, 10))
		out.Note("entity."+entChat, strconv.FormatInt(m.ByName[entChat].Ent, 10))
	case p.Place == placeDomain:
		return ErrNoWhatsAppTables
	}
	_, err = rowrun.Run(ctx, in, out, nil)
	return err
}

// filesOf is the database and its companions as the host hands them (restated from the other WhatsApp parsers).
func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	for _, c := range []struct {
		role string
		set  func(a parse.Artifact)
	}{
		{"wal", func(a parse.Artifact) { f.WAL, f.WALSize = a.R, a.Size }},
		{"journal", func(a parse.Artifact) { f.Journal, f.JournalSize = a.R, a.Size }},
	} {
		if a, ok := in.Artifacts[c.role]; ok {
			if a.R == nil {
				return sqlitedb.Files{}, fmt.Errorf("%s: the %s artifact has no reader", Name, c.role)
			}
			c.set(a)
		}
	}
	return f, nil
}

// hasTable says whether the schema holds a table called name. A table that exists but cannot be read counts as present:
// the mapping that reads it reports why.
func hasTable(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, name string) (bool, error) {
	db, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		return false, err
	}
	defer db.Release()
	_, err = db.Table(ctx, name, nil, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return false, nil
	case err != nil && !dataDamage(err):
		return false, err
	}
	return true, nil
}
