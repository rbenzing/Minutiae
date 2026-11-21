package ioswacontacts

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// Identity of the parser.
const (
	Name    = "ios.whatsapp.contacts"
	Version = "1.0.2"
)

// Parser is the ios.whatsapp.contacts parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Errors Parse returns, wrapped with %w (plan 4I2, "Notes and warnings").
var (
	// ErrNotWhatsAppPath: the primary's logical path is none of the places the globs name.
	ErrNotWhatsAppPath = errors.New("ios.whatsapp.contacts: not a WhatsApp app group path")
	// ErrNoWhatsAppTables: a database at a backup domain of WhatsApp lacks the identifying table.
	ErrNoWhatsAppTables = errors.New("ios.whatsapp.contacts: the identifying table is absent at the WhatsApp app group path")
)

// ClaimableTables are the tables whose mapping is total (4I2-R10): every ZWAADDRESSBOOKCONTACT row gives one record or one
// counted warning, so typed recovery can map the rows that were deleted or superseded. The mapping reads no other table.
func ClaimableTables() []string { return []string{tableContact} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// Meta describes the parser: the roles and globs of plan 4I2 "Globs and places", and the claims of ClaimableTables.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS WhatsApp contacts (ContactsV2.sqlite)",
		Platforms: []string{parse.PlatformIOS},
		Claims:    claimsOf(ClaimableTables()),
		Emits:     []parse.Emit{{Type: contact.Type, PayloadVersion: contact.PayloadVersion}},
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

// probeReqs are the tables and columns the mapping cannot work without (spec 6.11 signature): the identifying table with its
// rowid key and the JID column.
func probeReqs() []rowrun.Req {
	return []rowrun.Req{{Table: tableContact, Need: []string{"Z_PK", colWhatsAppID}}}
}

const (
	reasonNotSQLite = "not an SQLite database" // rowrun.Probe's reason for a file that is no database
	notSQLiteAtDom  = "not a SQLite file at the WhatsApp app group path"
	noTablesAtDom   = "no WhatsApp ContactsV2 tables at the WhatsApp app group path"
)

// Probe decides from the main file only (4I2-R2, PF9): at a backup domain the app is named, so a file that is no database or
// lacks the identifying table is unsupported-schema, not benign; at an app group it is not-applicable (a same-named file of
// another app) and the identifying table present makes the rest required. With a wal or journal companion every schema finding
// is deferred to Parse, as rowrun does. A contacts store has no Core Data model check: no entity number drives a mapping (4I2-P3).
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
	return got, nil
}

// Mappings is the typed pass's mapping: ZWAADDRESSBOOKCONTACT with no Parse context, so a recovered row states the label its
// place allows (PF5, 4I2-P8).
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{contactMappingWith(nil)}
}

// Parse reports the place and the app label as notes, then maps ZWAADDRESSBOOKCONTACT through rowrun.Run, which forwards the
// reader's warnings after the row warnings (PF13a) inside its own cap of 1000 and states one warnings_dropped total. The step
// is Identifying at an app group (a same-named database of another app is benign there); at a backup domain the absence of the
// table is ErrNoWhatsAppTables. The mapping is total: no other table is read, so there is no join to guard and no warning
// after Run.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	p := classifyPlace(in.Primary.Logical)
	if p.Place == placeOther {
		return fmt.Errorf("%w: %s", ErrNotWhatsAppPath, in.Primary.Logical)
	}
	for _, role := range []string{"db", "wal", "journal"} { // what rowrun.Run writes, kept when the label check below fails first
		a := in.Primary
		if role != "db" {
			var ok bool
			if a, ok = in.Artifacts[role]; !ok {
				continue
			}
		}
		if a.Incomplete {
			out.Note("incomplete."+role, "true")
		}
	}
	l, err := labelOf(in, p)
	if err != nil {
		return err
	}
	l.note(out, p)
	step := rowrun.Step{Mapping: contactMappingWith(&env{Place: p, Label: l}), Identifying: p.Place == placeAppGroup}
	if _, err := rowrun.Run(ctx, in, out, []rowrun.Step{step}); err != nil {
		if p.Place == placeDomain && errors.Is(err, sqlitedb.ErrNoSuchTable) {
			return fmt.Errorf("%w: %w", ErrNoWhatsAppTables, err)
		}
		return err
	}
	return nil
}
