package androidcalendar

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
)

// Identity of the parser.
const (
	Name    = "android.calendar"
	Version = "1.0.0"
)

// The errors of Parse, wrapped with %w.
var (
	// ErrUnsupportedVersion says the live view's user version is outside the supported band (B4, 4I3-P6): the job
	// fails and no record is emitted, at either place.
	ErrUnsupportedVersion = errors.New("android.calendar: calendar.db user version outside the supported band 400-699")
	// ErrNoEventsTable says the database at the calendar provider path has no Events table (after a Probe that
	// deferred the schema to Parse because a companion was present).
	ErrNoEventsTable = errors.New("android.calendar: no Events table at the calendar provider path")
	// ErrSchemaIncomplete says the live view's schema could not be read whole and no Events table was found in what
	// was read: whether Events exists is unknown, never "absent".
	ErrSchemaIncomplete = errors.New("android.calendar: the schema could not be read whole and no Events table was found in what was read")
)

// The supported band of calendar.db user versions (B4, 4I3-P6): 403, 502, 600 and 601 have fixtures; a version
// outside the band is a schema this parser was never checked against.
const minUserVersion, maxUserVersion = 400, 699

// Parser is the android.calendar parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// ClaimableTables are the tables whose mapping is total: every Events row gives one record (spec 10), so typed recovery
// (plan 4G) can map the events that were deleted or superseded. A recovered row joins no table (B2). The join tables,
// the derived Instances cache, Colors and the meta tables are never claimed.
func ClaimableTables() []string { return []string{tableEvents} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// Meta describes the parser. The one primary glob ends in calendar.db, so a -wal, -journal or -shm file is never a
// primary (4I-R21); it reaches the job only as the companion of its primary. The claims are ClaimableTables.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android calendar (calendar provider)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: calendar.Type, PayloadVersion: calendar.PayloadVersion}},
		Claims:    claimsOf(ClaimableTables()),
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{"android:**/com.android.providers.calendar/databases/calendar.db"}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings is the typed pass's mapping: Events with no Parse context, so a recovered row joins no table and says so
// (4I3-P4, B2).
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{eventMappingWith(nil)} }

// The reasons of Probe (byte-exact, plan 4I3 "Probe reasons").
const (
	reasonNotSQLite       = "not a SQLite file at the calendar provider path"
	reasonCompanionOnly   = "the main file is not a database but a -wal or -journal companion exists"
	reasonNoEvents        = "no Events table at the calendar provider path"
	reasonEncrypted       = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNotSQLiteOther  = "not an SQLite database"
	reasonSchemaUnreadAll = "the schema could not be read whole and no Events table was found in what was read"
	reasonNoEventsOther   = "table Events is absent"
)

func reasonVersion(v int32) string {
	return "calendar.db user version " + strconv.FormatInt(int64(v), 10) + " is outside the supported band 400-699"
}

func inBand(v int32) bool { return v >= minUserVersion && v <= maxUserVersion }

// reqs is what Probe needs of the schema (spec 7.2): Events identifying, every other table optional with its columns
// checked when present; where a join key is the rowid it must be the alias.
func reqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: "Events", Need: []string{"_id", "calendar_id", "dtstart", "allDay", "deleted"}, Alias: "_id"},
		{Table: "Calendars", Alias: "_id", Optional: true},
		{Table: "Attendees", Need: []string{"event_id"}, Optional: true},
		{Table: "Reminders", Need: []string{"event_id", "minutes", "method"}, Optional: true},
		{Table: "CalendarAlerts", Need: []string{"event_id", "state"}, Optional: true},
		{Table: "ExtendedProperties", Need: []string{"event_id", "name", "value"}, Optional: true},
		{Table: "EventsRawTimes", Need: []string{"event_id"}, Optional: true},
		{Table: "CalendarMetaData", Optional: true},
		{Table: "CalendarCache", Need: []string{"key", "value"}, Optional: true},
	}
}

// Probe decides from the main file only (4I3-P5, P6, B4). At the provider path every refusal is loud
// (unsupported-schema, or encrypted): the path names a calendar store this parser cannot read, not another kind of
// file. Anywhere else (a signature claim) every refusal is not-applicable, except an Events table that lacks a required
// column (rowrun's rule). With a -wal or -journal companion the main file's schema and user version prove nothing,
// so both are checked by Parse; Probe then defers as soon as the main file is not refused as encrypted or not SQLite.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	provider := classifyPlace(in.Primary.Logical) == placeProvider
	refuse := func(reason string) parse.Applicability {
		if provider {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reason}
		}
		return parse.Applicability{Status: parse.NotApplicable, Reason: reason}
	}
	_, wal := in.Artifacts["wal"]
	_, jr := in.Artifacts["journal"]
	companion := wal || jr
	main := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	db, err := sqlitedb.Open(ctx, main, in.Budget)
	if cerr := ctx.Err(); cerr != nil { // a cancel or a deadline is the caller's, never a deferral (Task 4 ruling e)
		if err == nil {
			db.Release()
		}
		return parse.Applicability{}, cerr
	}
	switch {
	case errors.Is(err, sqlitedb.ErrEncrypted): // before ErrNotSQLite, which it also wraps
		if provider {
			return parse.Applicability{Status: parse.Encrypted, Reason: reasonEncrypted}, nil
		}
		return refuse(reasonEncrypted), nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		switch {
		case !provider:
			return refuse(reasonNotSQLiteOther), nil
		case companion:
			return refuse(reasonCompanionOnly), nil
		}
		return refuse(reasonNotSQLite), nil
	case err != nil && companion:
		// a SQLite main file that cannot be opened whole (budget, I/O, damage) proves nothing beside a companion
		return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	version := db.Info().Stored.UserVersion
	db.Release()
	if companion {
		return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
	}
	if !inBand(version) {
		return refuse(reasonVersion(version)), nil
	}
	rep, err := census.Take(ctx, main, in.Budget, nil)
	if err != nil {
		return parse.Applicability{}, err
	}
	if !hasEvents(rep.Objects) {
		switch {
		case rep.SchemaSkipped > 0:
			return refuse(reasonSchemaUnreadAll), nil
		case provider:
			return refuse(reasonNoEvents), nil
		}
		return refuse(reasonNoEventsOther), nil
	}
	got, err := rowrun.Probe(ctx, in, reqs())
	if err != nil {
		return got, err
	}
	if provider && got.Status == parse.NotApplicable {
		got.Status = parse.UnsupportedSchema // the path names the provider's store: a missing table is not another kind of file
	}
	return got, nil
}

// hasEvents says the census saw a table named Events, matched as the engine matches names: ASCII case-insensitive
// only, so a name that folds to Events only under Unicode rules ("Eventſ") is not Events (coordinator ruling C2). A
// virtual Events counts: it exists, and rowrun.Probe states that it cannot be read.
func hasEvents(objs []census.Object) bool {
	for _, o := range objs {
		if o.Type == "table" && asciiEqualFold(o.Name, tableEvents) {
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
			return sqlitedb.Files{}, errors.New("android.calendar: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.calendar: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

// Parse checks the live view (its user version, B4, 4I3-P6; at the provider path that Events exists; Events' required
// columns and rowid alias; the join tables) and states the store (writeNotes) before any mapping runs, then maps
// Events and writes the count notes after it. The step is Identifying
// elsewhere (a database of another kind is not applicable there) and reads through the join guard. Records pass
// through a counting emitter: the mapping cannot warn, so after rowrun.Run Parse warns once per undecided flag
// column and once per table with an undecided join (4I3-P9, B9).
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	g, version, err := checkLiveView(ctx, in, files, out)
	if err != nil {
		return err
	}
	e := &env{}
	if g != nil {
		e.Guard = *g
		if err := writeNotes(ctx, files, in.Budget, out, version); err != nil {
			return fmt.Errorf("android.calendar: %w", err)
		}
	}
	ce := newCountingEmitter(out)
	step := rowrun.Step{Mapping: eventMappingWith(e), Identifying: classifyPlace(in.Primary.Logical) == placeOther}
	if _, err := rowrun.Run(ctx, in, ce, []rowrun.Step{step}); err != nil {
		return fmt.Errorf("android.calendar: %w", err)
	}
	if err := ce.warn(ctx); err != nil {
		return err
	}
	writeCountNotes(out, ce.c)
	return nil
}

// requiredEvents are the identifying columns of Events (spec 7.2).
func requiredEvents() []string {
	var out []string
	for _, c := range eventSpecs() {
		if c.Required {
			out = append(out, c.Name)
		}
	}
	return out
}

// checkLiveView opens the live view once: its user version must be in the band; an Events table must have its
// required columns and _id as the rowid alias (Probe deferred these checks beside a companion), and then the join
// tables are checked (the guard). At the provider path Events must exist: it is absent only when the schema was read
// whole and the census saw no table of that name; otherwise its presence is unknown (ErrSchemaIncomplete), and a
// census that saw one makes any failure to open it an error, never ErrNoEventsTable. Elsewhere a live view without
// Events is left to the identifying step (nil guard). The user version is returned for the note database_version.
func checkLiveView(ctx context.Context, in *parse.Input, files sqlitedb.Files, out parse.Emitter) (*joinGuard, int32, error) {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, 0, fmt.Errorf("android.calendar: %w", err)
	}
	defer db.Release()
	v := db.Info().Header.UserVersion
	if !inBand(v) {
		return nil, v, fmt.Errorf("%w (user version %d)", ErrUnsupportedVersion, v)
	}
	provider := classifyPlace(in.Primary.Logical) == placeProvider
	t, err := db.Table(ctx, tableEvents, requiredEvents(), nil)
	switch {
	case err == nil:
		if i := t.Col("_id"); i < 0 || !t.Cols()[i].RowidAlias {
			return nil, v, fmt.Errorf("android.calendar: %w", &sqlitedb.UnsupportedSchemaError{Table: tableEvents, Reason: "column _id is not the rowid alias"})
		}
		partial := func() (bool, error) {
			rep, err := census.Take(ctx, files, in.Budget, nil)
			return rep.SchemaSkipped > 0, err
		}
		g, err := checkJoinSchema(ctx, db, out, partial)
		return g, v, err
	case !provider && errors.Is(err, sqlitedb.ErrNoSuchTable):
		return nil, v, nil // a database of another kind: the identifying step states it
	case !provider:
		return nil, v, fmt.Errorf("android.calendar: Events: %w", err)
	}
	rep, cerr := census.Take(ctx, files, in.Budget, nil)
	if cerr != nil {
		return nil, v, fmt.Errorf("android.calendar: Events: %w (the schema census failed: %w)", err, cerr)
	}
	return nil, v, eventsAbsence(err, rep)
}

// eventsAbsence reads err, the failure to open Events at the provider path, against the schema census: a census that
// saw Events makes any failure an error that wraps it, never "absent" (Task 4 ruling f); no Events in a schema read in
// part is ErrSchemaIncomplete (unknown, never absent); no Events in a schema read whole and no such table is
// ErrNoEventsTable; any other failure is returned wrapped.
func eventsAbsence(err error, rep census.Report) error {
	switch {
	case hasEvents(rep.Objects):
		return fmt.Errorf("android.calendar: Events: %w", err)
	case rep.SchemaSkipped > 0:
		return fmt.Errorf("%w: %w", ErrSchemaIncomplete, err)
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return ErrNoEventsTable
	}
	return fmt.Errorf("android.calendar: Events: %w", err)
}
