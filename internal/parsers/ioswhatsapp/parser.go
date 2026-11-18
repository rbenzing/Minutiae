package ioswhatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
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

// Mappings is the typed pass's mapping: ZWAMESSAGE with no Parse context, so a recovered row states the label its
// place allows (4I2-P8, PF5).
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{messageMappingWith(nil)}
}

// Parse reports the place, the app label and the model notes, then maps ZWAMESSAGE. The step is Identifying at an app
// group (a same-named database of another app is benign there) and reads through the join guard. Records pass through a
// counting emitter: the mapping cannot warn, so after rowrun.Run, and so after its reader warnings, Parse warns once per
// table whose join was undecided (4I2-P7).
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	p := classifyPlace(in.Primary.Logical)
	if p.Place == placeOther {
		return fmt.Errorf("%w: %s", ErrNotWhatsAppPath, in.Primary.Logical)
	}
	for _, role := range []string{"db", "wal", "journal"} { // what rowrun.Run writes, kept when a check below fails first
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
	g, err := preflight(ctx, in, out, p)
	if err != nil {
		return err
	}
	ce := &countingEmitter{Emitter: out, counts: map[string]int64{}}
	step := rowrun.Step{Mapping: withGuard(messageMappingWith(&env{Place: p, Label: l}), g), Identifying: p.Place == placeAppGroup}
	_, err = rowrun.Run(ctx, in, ce, []rowrun.Step{step})
	if werr := ce.warn(ctx); err == nil {
		err = werr
	}
	return err
}

// countingEmitter counts the records whose raw.join_undecided names a table (4I2-P7).
type countingEmitter struct {
	parse.Emitter
	counts map[string]int64
}

func (c *countingEmitter) Emit(ctx context.Context, r records.Record) error {
	raw, _ := r.Payload["raw"].(map[string]any)
	if list, ok := raw["join_undecided"].([]any); ok {
		for _, t := range list {
			if name, ok := t.(string); ok {
				c.counts[name]++
			}
		}
	}
	return c.Emitter.Emit(ctx, r)
}

// warn raises one warning per table with an undecided join, in table order: locator sqlite:table=<t>.
func (c *countingEmitter) warn(ctx context.Context) error {
	tables := make([]string, 0, len(c.counts))
	for t := range c.counts {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	for _, t := range tables {
		if err := c.Warn(ctx, "sqlite:table="+t, fmt.Sprintf("%s: %s: %d records", reasonJoinUndecided, t, c.counts[t])); err != nil {
			return err
		}
	}
	return nil
}

// maxPreflightWarnings caps the reader warnings preflight forwards (rowrun's own cap).
const maxPreflightWarnings = 1000

// preflight opens the database once, decides whether ZWAMESSAGE is present and, when it is, reads the Core Data model
// and notes the entity numbers. It forwards the reader's warnings raised after the open (the pages of Z_PRIMARYKEY
// it read, which Run never reads, even when it still delivered a row), and on an error all of them: the damage that
// made the model unreadable is evidence and rowrun.Run never runs.
func preflight(ctx context.Context, in *parse.Input, out parse.Emitter, p dbPlace) (g *joinGuard, err error) {
	files, err := filesOf(in)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, err
	}
	defer db.Release()
	n0 := len(db.Warnings()) // the open's own schema warnings: rowrun.Run's open raises them again
	defer func() {
		from := n0 // on success only what the lookup and the model read raised, which Run never raises (review N1)
		if err != nil {
			from = 0
		}
		dropped := 0
		for i, w := range db.Warnings()[from:] {
			if i >= maxPreflightWarnings {
				dropped++
				continue
			}
			loc := fmt.Sprintf("sqlite:file=%s;page=%d;offset=%d", w.File, w.Page, w.Offset)
			if werr := out.Warn(ctx, loc, w.Code+": "+w.Msg); werr != nil {
				return
			}
		}
		if dropped > 0 {
			out.Note("warnings_dropped", strconv.Itoa(dropped))
		}
	}()
	_, terr := db.Table(ctx, tableMessage, nil, nil)
	switch {
	case errors.Is(terr, sqlitedb.ErrNoSuchTable):
		if p.Place == placeDomain {
			return nil, ErrNoWhatsAppTables
		}
		return nil, nil
	case terr != nil && !dataDamage(terr):
		return nil, terr
	}
	// present (a table that exists but cannot be read counts as present: the mapping that reads it reports why)
	m, err := readModelDB(ctx, db, entMessage, entChat)
	var me *modelError
	if errors.As(err, &me) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedModel, me.Reason)
	} else if err != nil {
		return nil, err
	}
	out.Note("entity."+entMessage, strconv.FormatInt(m.ByName[entMessage].Ent, 10))
	out.Note("entity."+entChat, strconv.FormatInt(m.ByName[entChat].Ent, 10))
	names, terr := db.Tables(ctx)
	if terr != nil {
		return nil, terr
	}
	if list := unmappedTables(names); list != "" {
		out.Note("unmapped_tables", list)
	}
	return checkJoinSchema(ctx, db, out)
}

// maxUnmapped is the size of the unmapped_tables note.
const maxUnmapped = 512

// unmappedTableNames are the tables of ChatStorage the parser does not map, in the order of the plan ("Notes and
// warnings"): their content is not read, and the note says they exist (unknown is never absent).
var unmappedTableNames = []string{
	"ZWAPROFILEPUSHNAME", "ZWAGROUPMEMBERSCHANGE", "ZWAGROUPINFO", "ZWAMESSAGEDATAITEM", "ZWAVCARDMENTION",
	"ZWACHATPROPERTIES", "ZWACHATPUSHCONFIG", "ZWABLACKLISTITEM", "ZWAZ1PAYMENTTRANSACTION",
}

// unmappedTables is the comma list of the present tables among unmappedTableNames, cut at an entry boundary near
// maxUnmapped bytes with a trailing ",…".
func unmappedTables(present []string) string {
	var b strings.Builder
	for _, want := range unmappedTableNames {
		if !slices.ContainsFunc(present, func(n string) bool { return strings.EqualFold(n, want) }) {
			continue
		}
		if b.Len()+1+len(want) > maxUnmapped {
			b.WriteString(",…")
			return b.String()
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(want)
	}
	return b.String()
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
