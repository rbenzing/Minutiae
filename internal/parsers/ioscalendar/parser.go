package ioscalendar

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// Identity of the parser.
const (
	Name    = "ios.calendar"
	Version = "1.0.0"
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

// Meta describes the parser. The one primary glob is the system calendar path of HomeDomain, which a backup and an
// extraction share (4I3-P5, B4); it ends in Calendar.sqlitedb, so a -wal, -journal or -shm file is never a primary
// (4I-R21): it reaches the job only as the companion of its primary.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "iOS calendar (Calendar.sqlitedb)",
		Platforms: []string{parse.PlatformIOS},
		Emits:     []parse.Emit{{Type: calendar.Type, PayloadVersion: calendar.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{systemPath}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings is the typed pass's mapping; it arrives with the full CalendarItem mapping (plan 4I3 Task 9).
func (Parser) Mappings() []parse.TableMapping { return nil }

// The reasons of Probe (byte-exact, plan 4I3 "Probe reasons").
const (
	reasonNotSQLite       = "not a SQLite file at the system calendar path"
	reasonNoItem          = "no CalendarItem table at the system calendar path"
	reasonEncrypted       = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonSchemaUnreadAll = "the schema could not be read whole and no CalendarItem table was found in what was read"
)

// reqs is what Probe needs of the schema (spec 8.1): CalendarItem identifying with ROWID as the rowid alias; the
// join tables optional, each with ROWID as the alias where the mapping reads it by key (Get).
func reqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableItem, Need: []string{"ROWID", "start_date", "start_tz", "all_day", "calendar_id"}, Alias: "ROWID"},
		{Table: "Calendar", Alias: "ROWID", Optional: true},
		{Table: "Store", Alias: "ROWID", Optional: true},
		{Table: "Location", Alias: "ROWID", Optional: true},
		{Table: "Participant", Alias: "ROWID", Optional: true},
		{Table: "Identity", Alias: "ROWID", Optional: true},
		{Table: "Alarm", Optional: true},
		{Table: "Recurrence", Optional: true},
		{Table: "ExceptionDate", Optional: true},
	}
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
		return rowrun.Probe(ctx, in, reqs())
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
	got, err := rowrun.Probe(ctx, in, reqs())
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

// Parse opens the live view once to count the item kinds of CalendarItem and state them (4I3-P10); at the system
// path a live view without CalendarItem fails the job (ErrNoCalendarItemTable only when the schema was read whole,
// else ErrSchemaIncomplete). It then maps CalendarItem, a step that is Identifying elsewhere (a database of another
// kind is not applicable there). rowrun.Run warns for at most 1,000 rows; Parse states the rest of the non-event rows
// in one warning and in the note warnings_dropped (spec 8.2, 9, 4I-R9).
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	if err := stateKinds(ctx, in, files, out); err != nil {
		return err
	}
	cw := &rowWarnCounter{Emitter: out}
	step := rowrun.Step{Mapping: eventMappingWith(&env{}), Identifying: classifyPlace(in.Primary.Logical) == placeOther}
	rep, err := rowrun.Run(ctx, in, cw, []rowrun.Step{step})
	if werr := cw.stateDropped(ctx, rep); err == nil {
		err = werr
	}
	if err != nil {
		return fmt.Errorf("ios.calendar: %w", err)
	}
	return nil
}

// stateKinds counts the item kinds of the live view's CalendarItem and writes their notes. At the system path any
// failure is read against the schema census (itemAbsence); elsewhere a live view without CalendarItem is left to the
// identifying step and any other failure fails the job.
func stateKinds(ctx context.Context, in *parse.Input, files sqlitedb.Files, out parse.Emitter) error {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return fmt.Errorf("ios.calendar: %w", err)
	}
	k, err := countKinds(ctx, db)
	db.Release()
	switch {
	case err == nil:
		k.note(out)
		return nil
	case classifyPlace(in.Primary.Logical) == placeSystem:
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return nil // a database of another kind: the identifying step states it
	default:
		return fmt.Errorf("ios.calendar: %s: %w", tableItem, err)
	}
	rep, cerr := census.Take(ctx, files, in.Budget, nil)
	if cerr != nil {
		return fmt.Errorf("ios.calendar: %s: %w (the schema census failed: %w)", tableItem, err, cerr)
	}
	return itemAbsence(err, rep)
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

// rowWarnCounter counts the row warnings rowrun.Run states (locator sqlite:table=...) and holds back its own note
// warnings_dropped (the reader warnings past its cap), so that Parse writes one total.
type rowWarnCounter struct {
	parse.Emitter
	rowWarned  int64
	runDropped int64
}

// Warn counts a row warning and passes every warning on.
func (c *rowWarnCounter) Warn(ctx context.Context, locator, reason string) error {
	if strings.HasPrefix(locator, "sqlite:table=") {
		c.rowWarned++
	}
	return c.Emitter.Warn(ctx, locator, reason)
}

// Note holds back rowrun.Run's warnings_dropped.
func (c *rowWarnCounter) Note(key, value string) {
	if key == "warnings_dropped" {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			c.runDropped += n
			return
		}
	}
	c.Emitter.Note(key, value)
}

// stateDropped states the CalendarItem rows rowrun.Run did not warn for (its cap of 1,000 warnings per run): one
// warning naming their number (spec 8.2, 9), and the note warnings_dropped with every warning dropped.
func (c *rowWarnCounter) stateDropped(ctx context.Context, rep rowrun.Report) error {
	var unmapped int64
	for _, tr := range rep.Tables {
		if tr.Table == tableItem {
			unmapped += tr.Unmapped
		}
	}
	rows := max(unmapped-c.rowWarned, 0)
	if total := rows + c.runDropped; total > 0 {
		c.Emitter.Note("warnings_dropped", strconv.FormatInt(total, 10))
	}
	if rows == 0 {
		return nil
	}
	return c.Emitter.Warn(ctx, "sqlite:table="+tableItem, strconv.FormatInt(rows, 10)+" more CalendarItem row warnings dropped after the cap of 1000")
}
