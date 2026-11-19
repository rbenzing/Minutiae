package androidcalendar

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

// Meta describes the parser. The one primary glob ends in calendar.db, so a -wal, -journal or -shm file is never a
// primary (4I-R21); it reaches the job only as the companion of its primary. No table is claimed until the Events
// mapping is total (plan 4I3 Task 6).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android calendar (calendar provider)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: calendar.Type, PayloadVersion: calendar.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{"android:**/com.android.providers.calendar/databases/calendar.db"}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings are the per-table mappings (none until the Events mapping, plan 4I3 Task 4).
func (Parser) Mappings() []parse.TableMapping { return nil }

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

// hasEvents says the census saw a table named Events (the engine's name match is ASCII case-insensitive). A virtual
// Events counts: it exists, and rowrun.Probe states that it cannot be read.
func hasEvents(objs []census.Object) bool {
	for _, o := range objs {
		if o.Type == "table" && strings.EqualFold(o.Name, "Events") {
			return true
		}
	}
	return false
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

// Parse checks the live view's user version (B4, 4I3-P6) and, at the provider path, that Events exists, before any
// mapping runs; both refusals fail the job with no record.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	if err := checkLiveView(ctx, in, files); err != nil {
		return err
	}
	if _, err := rowrun.Run(ctx, in, out, nil); err != nil {
		return fmt.Errorf("android.calendar: %w", err)
	}
	return nil
}

// checkLiveView opens the live view once: its user version must be in the band, and at the provider path it must
// hold a readable Events table. An Events that the schema does not show is absent only when the schema was read
// whole; otherwise its presence is unknown (ErrSchemaIncomplete). Every other failure to read Events is returned.
func checkLiveView(ctx context.Context, in *parse.Input, files sqlitedb.Files) error {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return fmt.Errorf("android.calendar: %w", err)
	}
	defer db.Release()
	if v := db.Info().Header.UserVersion; !inBand(v) {
		return fmt.Errorf("%w (user version %d)", ErrUnsupportedVersion, v)
	}
	if classifyPlace(in.Primary.Logical) != placeProvider {
		return nil // elsewhere a database without Events is of another kind (the identifying step, plan 4I3 Task 4)
	}
	_, err = db.Table(ctx, "Events", nil, nil)
	if err == nil {
		return nil
	}
	rep, cerr := census.Take(ctx, files, in.Budget, nil)
	switch {
	case cerr != nil:
		return fmt.Errorf("android.calendar: Events: %w (the schema census failed: %w)", err, cerr)
	case !hasEvents(rep.Objects) && rep.SchemaSkipped > 0:
		return fmt.Errorf("%w: %w", ErrSchemaIncomplete, err)
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return ErrNoEventsTable
	}
	return fmt.Errorf("android.calendar: Events: %w", err)
}
