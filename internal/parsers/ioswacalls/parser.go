package ioswacalls

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
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// Identity of the parser.
const (
	Name    = "ios.whatsapp.calls"
	Version = "1.0.0"
)

// Parser is the ios.whatsapp.calls parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Errors Parse returns, wrapped with %w (plan 4I2, "Notes and warnings").
var (
	// ErrNotWhatsAppPath: the primary's logical path is none of the places the globs name.
	ErrNotWhatsAppPath = errors.New("ios.whatsapp.calls: not a WhatsApp app group path")
	// ErrNoWhatsAppTables: a database at a backup domain of WhatsApp lacks the identifying table.
	ErrNoWhatsAppTables = errors.New("ios.whatsapp.calls: the identifying table is absent at the WhatsApp app group path")
	// ErrAggregatesIncomplete: the links between the calls and their aggregates could not be read whole (a damaged table, the
	// memory limit or more distinct aggregates than the cap). It is returned after rowrun.Run, so the records stay and the job is
	// incomplete; the calls whose direction could not be decided say so.
	ErrAggregatesIncomplete = errors.New("ios.whatsapp.calls: the aggregate links could not be read whole")
)

// ClaimableTables are the tables whose mapping is total (4I2-R10): every ZWACDCALLEVENT row gives one record or one counted
// warning, so typed recovery can map the rows that were deleted or superseded. ZWAAGGREGATECALLEVENT is a join of that mapping,
// not a mapping of its own: a recovered aggregate alone is no call.
func ClaimableTables() []string { return []string{tableCall} }

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
		Name: Name, Version: Version, Title: "iOS WhatsApp calls (CallHistory.sqlite)",
		Platforms: []string{parse.PlatformIOS},
		Claims:    claimsOf(ClaimableTables()),
		Emits:     []parse.Emit{{Type: call.Type, PayloadVersion: call.PayloadVersion}},
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

// probeReqs are the tables the mapping reads (PF3): ZWACDCALLEVENT identifies the store and needs ZWAAGGREGATECALLEVENT beside
// it (spec 6.11 signature). Z1CALLEVENTS is not needed: a store with another name for it states no-aggregate-column per call.
func probeReqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableCall, Need: []string{"Z_PK", "ZDATE", "ZDURATION", colOutcome}, Alias: "Z_PK", With: []string{tableAgg}},
		{Table: tableAgg, Alias: "Z_PK"},
	}
}

const (
	reasonNotSQLite = "not an SQLite database" // rowrun.Probe's reason for a file that is no database
	notSQLiteAtDom  = "not a SQLite file at the WhatsApp app group path"
	noTablesAtDom   = "no WhatsApp CallHistory tables at the WhatsApp app group path"
)

// Probe decides from the main file only (4I2-R2, PF9): at a backup domain the app is named, so a file that is no database or
// lacks the identifying table is unsupported-schema, not benign; at an app group it is not-applicable (a same-named file of
// another app) and the identifying table present makes the rest required. With a wal or journal companion every schema finding
// is deferred to Parse, as rowrun does. A call history has no Core Data model check: no entity number drives a mapping (4I2-P3).
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

// Mappings is the typed pass's mapping: ZWACDCALLEVENT with no Parse context, so a recovered row states the label its place
// allows and no direction (4I2-P8, PF5, Review Focus 4).
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{callMappingWith(nil)}
}

// Parse reports the place, the app label and the notes, reads the aggregate links once, then maps ZWACDCALLEVENT. The step is
// Identifying at an app group (a same-named database of another app is benign there) and reads through the join guard. Records
// pass through a counting emitter: the mapping cannot warn, so after rowrun.Run, and so after its reader warnings, Parse warns
// once per table whose join was undecided (4I2-P7) and, last, when the aggregate set is incomplete, which also ends the job with
// ErrAggregatesIncomplete (the records are kept).
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return parseWith(ctx, in, out, maxAggregates)
}

// parseWith is Parse with the cap on distinct aggregate keys as a parameter (the test seam of the flood).
func parseWith(ctx context.Context, in *parse.Input, out parse.Emitter, maxKeys int) error {
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
	pf, err := preflight(ctx, in, out, p, maxKeys)
	if err != nil {
		return err
	}
	var as *aggSet
	if pf != nil {
		as = pf.as
		defer in.Budget.Free(as.charged)
	}
	ce := &countingEmitter{Emitter: out, counts: map[string]int64{}, seen: map[string]bool{}}
	e := &env{Place: p, Label: l, Aggs: as}
	var g *joinGuard
	if pf != nil {
		g = pf.g
	}
	step := rowrun.Step{Mapping: withGuard(callMappingWith(e), g), Identifying: p.Place == placeAppGroup}
	_, err = rowrun.Run(ctx, in, ce, []rowrun.Step{step})
	if pf != nil {
		if werr := ce.forward(ctx, pf.pending); err == nil {
			err = werr
		}
	}
	if werr := ce.warn(ctx); err == nil {
		err = werr
	}
	if as != nil && !as.Complete {
		if werr := out.Warn(ctx, "sqlite:table="+tableCall, fmt.Sprintf("whatsapp-ios-aggregate-set-incomplete: %d rows, %d read (%s)", as.Total, as.Read, as.Why)); err == nil && werr != nil {
			err = werr
		}
		if err == nil {
			err = ErrAggregatesIncomplete
		}
	}
	return err
}

// maxPreflightWarnings caps the reader warnings preflight forwards (rowrun's own cap).
const maxPreflightWarnings = 1000

// readerWarning is a warning of the SQLite reader, as Parse and rowrun.Run state it.
type readerWarning struct{ Locator, Reason string }

// preResult is what preflight learned before rowrun.Run: the join guard, the aggregate set (charged to the budget: the caller frees
// it) and the reader warnings of the scan that rowrun.Run raises again when it reads the same table, held back and stated after
// Run only when Run did not state them (a damaged page is evidence once, whichever read found it).
type preResult struct {
	g       *joinGuard
	as      *aggSet
	pending []readerWarning
}

// preflight opens the database once, decides whether ZWACDCALLEVENT is present and, when it is, notes the unmapped tables,
// guards the joins and reads the aggregate links. It forwards the reader's warnings raised after the open (the pages the lookups
// read, which Run never reads), holds back those of the scan of the call table, and on an error all of them: the damage that
// made the read fail is evidence and rowrun.Run never runs. A nil result means Run decides (an app group without the table).
func preflight(ctx context.Context, in *parse.Input, out parse.Emitter, p dbPlace, maxKeys int) (res *preResult, err error) {
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
	scanFrom, scanTo := -1, -1
	defer func() {
		all := db.Warnings()
		forward := all // on an error all of them
		if err == nil {
			// on success what the lookups raised (Run never reads those pages) and, held back, what the scan of the call table
			// raised (Run reads that table again and raises it itself, unless it fails before the scan)
			from := min(n0, len(all))
			if scanFrom < 0 || res == nil {
				forward = all[from:]
			} else {
				a, b := min(scanFrom, len(all)), min(scanTo, len(all))
				forward = slices.Concat(all[from:a], all[b:])
				for _, w := range all[a:b] {
					res.pending = append(res.pending, readerWarning{fmt.Sprintf("sqlite:file=%s;page=%d;offset=%d", w.File, w.Page, w.Offset), w.Code + ": " + w.Msg})
				}
			}
		}
		dropped := 0
		for i, w := range forward {
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
	_, terr := db.Table(ctx, tableCall, nil, nil)
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
	names, terr := db.Tables(ctx)
	if terr != nil {
		return nil, terr
	}
	if list := unmappedTables(names); list != "" {
		out.Note("unmapped_tables", list)
	}
	g, err := checkJoinSchema(ctx, db, out)
	if err != nil {
		return nil, err
	}
	scanFrom = len(db.Warnings())
	as, err := loadAggregates(ctx, db, in.Budget, maxKeys)
	scanTo = len(db.Warnings())
	if err != nil {
		if as != nil {
			in.Budget.Free(as.charged)
		}
		return nil, err
	}
	if !as.NoColumn {
		out.Note("aggregates", strconv.Itoa(len(as.ByAgg)))
	}
	res = &preResult{g: g, as: as}
	return res, nil
}

// maxUnmapped is the size of the unmapped_tables note.
const maxUnmapped = 512

// unmappedTableNames are the tables of CallHistory the parser does not map, in the order of the plan ("Notes and warnings"):
// their content is not read (the participant table in particular: no counterparty is stated, 4I2-R4), and the note says they
// exist (unknown is never absent).
var unmappedTableNames = []string{"ZWACDCALLEVENTPARTICIPANT", "ZWAJOINABLECALLEVENT", "ZWAUPCOMINGCALLEVENT"}

// unmappedTables is the comma list of the present tables among unmappedTableNames, cut at an entry boundary near maxUnmapped
// bytes with a trailing ",…".
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
