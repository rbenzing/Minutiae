package ioscalendar

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// Identity of the parser.
const (
	Name    = "ios.calendar"
	Version = "1.0.4"
)

// The errors of Parse, wrapped with %w.
var (
	// ErrNoCalendarItemTable says the database at the system calendar path has no CalendarItem table (after a Probe
	// that deferred the schema to Parse because a companion was present): the path names the store, so this is a
	// layout this parser cannot read (the older Event layout lands here), never a database of another kind.
	ErrNoCalendarItemTable = errors.New("ios.calendar: no CalendarItem table at the system calendar path")
	// ErrSchemaIncomplete says the live view's schema could not be read whole and no CalendarItem table was found in
	// what was read: whether CalendarItem exists is unknown, never "absent".
	ErrSchemaIncomplete = errors.New("ios.calendar: the schema could not be read whole and no CalendarItem table was found in what was read")
)

// Parser is the ios.calendar parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// ClaimableTables are the tables whose mapping is total: every CalendarItem row gives one record or one counted
// non-event warning (spec 10, 8.2), so typed recovery (plan 4G) can map the items that were deleted or superseded. A
// recovered row joins no table (B2). The join tables, the occurrence cache and the change-tracking tables are never
// claimed.
func ClaimableTables() []string { return []string{tableItem} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// Meta describes the parser. The one primary glob is the system calendar path of HomeDomain, which a backup and an
// extraction share (4I3-P5, B4); it ends in Calendar.sqlitedb, so a -wal, -journal or -shm file is never a primary
// (4I-R21): it reaches the job only as the companion of its primary. The claims are ClaimableTables.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS calendar (Calendar.sqlitedb)",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: calendar.Type, PayloadVersion: calendar.PayloadVersion}},
		Claims:    claimsOf(ClaimableTables()),
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{systemPath}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings is the typed pass's mapping: CalendarItem with no Parse context, so a recovered row joins no table and says
// so (4I3-P4, B2).
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{eventMappingWith(nil)} }

// The reasons of Probe (byte-exact, plan 4I3 "Probe reasons").
const (
	reasonNotSQLite       = "not a SQLite file at the system calendar path"
	reasonNoItem          = "no CalendarItem table at the system calendar path"
	reasonEncrypted       = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonSchemaUnreadAll = "the schema could not be read whole and no CalendarItem table was found in what was read"
)

// reqs is what Probe needs of the schema (spec 8.1): CalendarItem identifying with ROWID as the rowid alias; the
// join tables optional. Whether a join table read by key (Get) is keyed by its rowid is checked after rowrun.Probe
// (probeGetKeys), because a table that declares no column named ROWID is keyed by its implicit rowid, which rowrun's
// alias check does not accept (the real store's Identity, plan 4I3 Task 12).
func reqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableItem, Need: []string{"ROWID", "start_date", "start_tz", "all_day", "calendar_id"}, Alias: "ROWID"},
		{Table: "Calendar", Optional: true},
		{Table: "Store", Optional: true},
		{Table: "Location", Optional: true},
		{Table: "Participant", Optional: true},
		{Table: "Identity", Optional: true},
		{Table: "Alarm", Optional: true},
		{Table: "Recurrence", Optional: true},
		{Table: "ExceptionDate", Optional: true},
	}
}

// probe is rowrun.Probe with reqs, then, when it found the schema applicable from the main file, the key check of
// each present join table read by key (probeGetKeys). Beside a -wal or -journal companion the main file's schema
// proves nothing, so rowrun's outcome holds and Parse's guard decides the keys on the live view (Task 12 fix 2, I1).
func probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	got, err := rowrun.Probe(ctx, in, reqs())
	if err != nil || got.Status != parse.Applicable {
		return got, err
	}
	_, wal := in.Artifacts["wal"]
	_, jr := in.Artifacts["journal"]
	if wal || jr {
		return got, nil
	}
	return probeGetKeys(ctx, in, got)
}

// probeGetKeys refuses (unsupported-schema) a present join table read by key that is not keyed by its rowid
// (rowidKeyed): a Get would attach another row with no marker. The main file only: Probe calls it with no companion.
func probeGetKeys(ctx context.Context, in *parse.Input, ok parse.Applicability) (parse.Applicability, error) {
	db, err := sqlitedb.Open(ctx, sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	if err != nil {
		return parse.Applicability{}, err
	}
	defer db.Release()
	for _, jt := range joinTables() {
		if jt.Alias == "" {
			continue
		}
		t, err := db.Table(ctx, jt.Table, nil, nil)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			continue
		case err != nil:
			return parse.Applicability{}, err
		}
		switch why := rowidKeyed(t, jt.Alias); {
		case why == "":
		case t.WithoutRowid():
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "table " + t.Name() + " is WITHOUT ROWID: it has no rowid to join by"}, nil
		default: // rowrun's wording for the same finding
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "column " + jt.Alias + " of table " + t.Name() + " is not the rowid alias"}, nil
		}
	}
	return ok, nil
}

// Probe decides from the main file only (4I3-P5, B4, spec 8.1). At the system path the path names the store, so every
// refusal is loud: encrypted for a file that looks encrypted, else unsupported-schema (not a SQLite file, no
// CalendarItem table, a schema not read whole with no CalendarItem in what was read, and rowrun's not-applicable
// findings). With a -wal or -journal companion the main file's schema proves nothing, so Parse checks it; Probe then
// defers as soon as the main file is not refused as encrypted or not SQLite. Anywhere else rowrun's outcome holds
// unchanged.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	if classifyPlace(in.Primary.Logical) != placeSystem {
		return probe(ctx, in)
	}
	loud := func(reason string) parse.Applicability {
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reason}
	}
	_, wal := in.Artifacts["wal"]
	_, jr := in.Artifacts["journal"]
	companion := wal || jr
	main := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	db, err := sqlitedb.Open(ctx, main, in.Budget)
	if cerr := ctx.Err(); cerr != nil { // a cancel or a deadline is the caller's, never a deferral
		if err == nil {
			db.Release()
		}
		return parse.Applicability{}, cerr
	}
	switch {
	case errors.Is(err, sqlitedb.ErrEncrypted): // before ErrNotSQLite, which it also wraps
		return parse.Applicability{Status: parse.Encrypted, Reason: reasonEncrypted}, nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		return loud(reasonNotSQLite), nil
	case err != nil && companion:
		// a SQLite main file that cannot be opened whole (budget, I/O, damage) proves nothing beside a companion
		return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	db.Release()
	if companion {
		return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
	}
	rep, err := census.Take(ctx, main, in.Budget, nil)
	if err != nil {
		return parse.Applicability{}, err
	}
	if !hasItemTable(rep.Objects) {
		if rep.SchemaSkipped > 0 {
			return loud(reasonSchemaUnreadAll), nil
		}
		return loud(reasonNoItem), nil
	}
	got, err := probe(ctx, in)
	if err != nil {
		return got, err
	}
	if got.Status == parse.NotApplicable {
		got.Status = parse.UnsupportedSchema // the path names the system store: a missing table is not another kind of file
	}
	return got, nil
}

// hasItemTable says the census saw a table named CalendarItem, matched as the engine matches names: ASCII
// case-insensitive only (a name that folds to CalendarItem only under Unicode rules is not CalendarItem). A virtual
// CalendarItem counts: it exists, and rowrun.Probe states that it cannot be read.
func hasItemTable(objs []census.Object) bool {
	for _, o := range objs {
		if o.Type == "table" && asciiEqualFold(o.Name, tableItem) {
			return true
		}
	}
	return false
}

// asciiEqualFold compares two names folding the ASCII letters only, as SQLite does.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// filesOf is the bundle of the input: the database and its companions.
func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("ios.calendar: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("ios.calendar: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

// Parse checks the live view before any mapping runs (checkLiveView: the item kinds and their notes, CalendarItem's
// rowid alias, the join tables) and states the store (writeNotes), then maps CalendarItem, a step that is Identifying
// elsewhere (a database of another kind is not applicable there), reading through the join guard. Records pass
// through a counting emitter: the mapping cannot warn, so after rowrun.Run Parse warns once per table with an
// undecided join (4I3-P9, B9); rowrun.Run warns for at most 1,000 rows, and Parse states the rest of the non-event
// rows in one warning and in the note warnings_dropped (spec 8.2, 9, 4I-R9).
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	g, err := checkLiveView(ctx, in, files, out)
	if err != nil {
		return err
	}
	e := &env{}
	if g != nil {
		e.Guard = *g
		if err := writeNotes(ctx, files, in.Budget, out); err != nil {
			return fmt.Errorf("ios.calendar: %w", err)
		}
	}
	ce := newCountingEmitter(out)
	step := rowrun.Step{Mapping: eventMappingWith(e), Identifying: classifyPlace(in.Primary.Logical) == placeOther}
	rep, err := rowrun.Run(ctx, in, ce, []rowrun.Step{step})
	if werr := ce.warnJoins(ctx); err == nil {
		err = werr
	}
	if werr := ce.stateDropped(ctx, rep); err == nil {
		err = werr
	}
	if err != nil {
		return fmt.Errorf("ios.calendar: %w", err)
	}
	return nil
}

// requiredItem are the identifying columns of CalendarItem (spec 8.1).
func requiredItem() []string {
	var out []string
	for _, c := range itemSpecs() {
		if c.Required {
			out = append(out, c.Name)
		}
	}
	return out
}

// checkLiveView opens the live view once. It counts the item kinds of CalendarItem and states them (4I3-P10); at the
// system path any failure to read CalendarItem is read against the schema census (itemAbsence); elsewhere a live view
// without CalendarItem is left to the identifying step (nil guard) and any other failure fails the job. A CalendarItem
// must have its required columns and ROWID as the rowid alias (Probe deferred these checks beside a companion; the
// source ids and every join depend on it); then the join tables are checked (the guard).
func checkLiveView(ctx context.Context, in *parse.Input, files sqlitedb.Files, out parse.Emitter) (*joinGuard, error) {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, fmt.Errorf("ios.calendar: %w", err)
	}
	defer db.Release()
	k, err := countKinds(ctx, db)
	switch {
	case err == nil:
	case classifyPlace(in.Primary.Logical) == placeSystem:
		rep, cerr := census.Take(ctx, files, in.Budget, nil)
		if cerr != nil {
			return nil, fmt.Errorf("ios.calendar: %s: %w (the schema census failed: %w)", tableItem, err, cerr)
		}
		return nil, itemAbsence(err, rep)
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return nil, nil // a database of another kind: the identifying step states it
	default:
		return nil, fmt.Errorf("ios.calendar: %s: %w", tableItem, err)
	}
	k.note(out)
	t, err := db.Table(ctx, tableItem, requiredItem(), nil)
	if err != nil {
		return nil, fmt.Errorf("ios.calendar: %s: %w", tableItem, err)
	}
	if i := t.Col("ROWID"); i < 0 || !t.Cols()[i].RowidAlias {
		return nil, fmt.Errorf("ios.calendar: %w", &sqlitedb.UnsupportedSchemaError{Table: tableItem, Reason: "column ROWID is not the rowid alias"})
	}
	partial := func() (bool, error) {
		rep, err := census.Take(ctx, files, in.Budget, nil)
		return rep.SchemaSkipped > 0, err
	}
	return checkJoinSchema(ctx, db, out, partial)
}

// itemAbsence reads err, a failure to read CalendarItem at the system path, against the schema census: a census that
// saw CalendarItem makes any failure an error that wraps it, never "absent"; no CalendarItem in a schema read in part
// is ErrSchemaIncomplete (unknown, never absent); no CalendarItem in a schema read whole and no such table is
// ErrNoCalendarItemTable; any other failure is returned wrapped.
func itemAbsence(err error, rep census.Report) error {
	switch {
	case hasItemTable(rep.Objects):
		return fmt.Errorf("ios.calendar: %s: %w", tableItem, err)
	case rep.SchemaSkipped > 0:
		return fmt.Errorf("%w: %w", ErrSchemaIncomplete, err)
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return ErrNoCalendarItemTable
	}
	return fmt.Errorf("ios.calendar: %s: %w", tableItem, err)
}
