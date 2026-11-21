package androidgooglemessages

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
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "android.googlemessages"
	Version = "1.0.0"
)

// The errors of Parse, wrapped with %w.
var (
	// ErrNoSignature says the database at the Messages app path lacks the bugle_db signature (after a Probe that
	// deferred the schema to Parse because a companion was present).
	ErrNoSignature = errors.New("android.googlemessages: no bugle_db signature at the Messages app path")
	// ErrSchemaIncomplete says the live view's schema could not be read whole and the signature was not found in what
	// was read: whether it exists is unknown, never "absent".
	ErrSchemaIncomplete = errors.New("android.googlemessages: the schema could not be read whole and no bugle_db signature was found in what was read")
)

// The tables of the signature and of the mapping (spec 5.3).
const (
	tableMessages      = "messages"
	tableParts         = "parts"
	tableParticipants  = "participants"
	tableConversations = "conversations"
	tableMembers       = "conversation_participants"
)

// Parser is the android.googlemessages parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. The two primary globs end in the literal bugle_db, so a -wal, -journal or -shm file is
// never a primary (4I-R21); it reaches the job only as the companion of its primary. Only the two packages are
// discovered by path: another package's bugle_db is reached through --scan or --artifact, by signature. There are no
// claims until the typed pass (Task 8).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android Google Messages and AOSP Messaging (bugle_db)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{
				"android:**/" + pkgGoogle + "/databases/" + dbFileName,
				"android:**/" + pkgAOSP + "/databases/" + dbFileName,
			}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings are the typed pass's mappings (4I5-P6): the messages mapping with no per-job state, so it joins nothing.
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{messagesMapping(nil)} }

// The reasons of Probe (byte-exact, the plan's "Probe reasons").
const (
	reasonNotSQLite       = "not a SQLite file at the Messages app path"
	reasonCompanionOnly   = "the main file is not a database but a -wal or -journal companion exists"
	reasonEncrypted       = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNotSQLiteOther  = "not an SQLite database"
	reasonNotBugle        = "not a bugle_db"
	reasonNoSignature     = "no bugle_db signature at the Messages app path: missing "
	reasonSchemaUnreadAll = "the schema could not be read whole and no bugle_db signature was found in what was read"
)

// destinationColumns are the participants columns of which the signature needs at least one (spec 5.3: rowrun.Req
// cannot say "one of").
var destinationColumns = []string{"normalized_destination", "send_destination", "display_destination"}

// reqs is what Probe needs of the schema (spec 5.3): the four tables of the signature with the columns the mapping
// cannot work without and the rowid alias of each _id, and conversation_participants optional but checked when
// present.
func reqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableMessages, Need: []string{"_id", "conversation_id", "sender_id", "received_timestamp", "message_protocol", "message_status"}, Alias: "_id"},
		{Table: tableParts, Need: []string{"_id", "message_id", "text", "content_type", "uri"}, Alias: "_id"},
		{Table: tableParticipants, Need: []string{"_id", "sub_id"}, Alias: "_id"},
		{Table: tableConversations, Need: []string{"_id"}, Alias: "_id"},
		{Table: tableMembers, Need: []string{"conversation_id", "participant_id"}, Optional: true},
	}
}

// mappedColumns are the columns the mapping reads of a table (spec 6), signature columns included: B4 states the ones
// outside the signature that a store lacks.
func mappedColumns(table string) []string {
	switch table {
	case tableMessages:
		return []string{
			"_id", "conversation_id", "sender_id", "self_id", "sent_timestamp", "received_timestamp", "message_protocol", "message_status",
			"seen", "read", "sms_message_uri", "sms_priority", "sms_message_size", "mms_subject", "mms_transaction_id",
			"mms_content_location", "mms_expiry", "raw_status", "retry_start_timestamp",
		}
	case tableParts:
		return []string{"_id", "message_id", "text", "uri", "content_type", "width", "height", "timestamp", "conversation_id"}
	case tableParticipants:
		return []string{
			"_id", "sub_id", "sim_slot_id", "normalized_destination", "send_destination", "display_destination", "full_name",
			"contact_id", "blocked", "subscription_name",
		}
	case tableConversations:
		return []string{"_id", "name", "sms_thread_id", "archive_status", "participant_count", "sort_timestamp", "last_read_timestamp", "IS_ENTERPRISE"}
	case tableMembers:
		return []string{"conversation_id", "participant_id"}
	}
	return nil
}

// unreadableError says a table of the signature exists (or the schema shows damage around it) and cannot be read: the
// signature is undecided, never absent.
type unreadableError struct {
	Table string
	Err   error
}

func (e *unreadableError) Error() string {
	return "table " + e.Table + " could not be read: " + e.Err.Error()
}
func (e *unreadableError) Unwrap() error { return e.Err }

// signatureOf checks the signature on an opened view and says what is missing: "" when it is present, else
// "table <t>", "column <t>.<c>" (the first of reqs() order) or the one-of text of the destination columns. A table that
// cannot be read is an *unreadableError (undecided, not missing); a budget, context or I/O error is returned. Optional
// tables are not part of the signature.
func signatureOf(ctx context.Context, db *sqlitedb.DB) (missing string, err error) {
	for _, rq := range reqs() {
		if rq.Optional {
			continue
		}
		t, err := db.Table(ctx, rq.Table, rq.Need, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable):
			return "table " + rq.Table, nil
		case errors.As(err, &us) && len(us.Missing) > 0:
			return "column " + rq.Table + "." + firstMissing(rq.Need, us.Missing), nil
		case dataDamage(err):
			return "", &unreadableError{Table: rq.Table, Err: err}
		case err != nil:
			return "", err
		}
		if rq.Table == tableParticipants && !hasDestination(t) {
			qualified := make([]string, len(destinationColumns))
			for i, c := range destinationColumns {
				qualified[i] = tableParticipants + "." + c
			}
			return "one of " + strings.Join(qualified, ", "), nil
		}
	}
	return "", nil
}

// firstMissing is the first of need (the order of reqs()) that the table lacks.
func firstMissing(need, missing []string) string {
	for _, n := range need {
		for _, m := range missing {
			if asciiEqualFold(n, m) {
				return n
			}
		}
	}
	return missing[0]
}

// hasDestination says the participants table has at least one of destinationColumns (read here, never passed on: the table is
// package state and must stay read-only, P5).
func hasDestination(t *sqlitedb.Table) bool {
	for _, n := range destinationColumns {
		if t.Col(n) >= 0 {
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

// schemaPartial says the schema table of the files could not be read whole.
func schemaPartial(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget) (bool, error) {
	rep, err := census.Take(ctx, f, b, nil)
	if err != nil {
		return false, err
	}
	return rep.SchemaSkipped > 0, nil
}

// Probe decides from the main file only (spec 5.4). At the app path every refusal is loud (unsupported-schema, or
// encrypted): the path names a store this parser cannot read, not another kind of file. Anywhere else (a signature
// claim) every refusal is not-applicable, except a column or rowid alias rowrun requires (rowrun's rule). With a -wal
// or -journal companion the main file's schema proves nothing, so all of it is checked by Parse; Probe then defers as
// soon as the main file is not refused as encrypted or not SQLite.
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	pl, _ := classifyPlace(in.Primary.Logical)
	app := pl == placeApp
	refuse := func(reason string) parse.Applicability {
		if app {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reason}
		}
		return parse.Applicability{Status: parse.NotApplicable, Reason: reason}
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
		if app {
			return parse.Applicability{Status: parse.Encrypted, Reason: reasonEncrypted}, nil
		}
		return refuse(reasonEncrypted), nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		switch {
		case !app:
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
	if companion {
		db.Release()
		return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
	}
	missing, err := signatureOf(ctx, db)
	db.Release()
	var ue *unreadableError
	switch {
	case errors.As(err, &ue):
		if partial, perr := schemaPartial(ctx, main, in.Budget); perr != nil {
			return parse.Applicability{}, perr
		} else if partial {
			return refuse(reasonSchemaUnreadAll), nil
		}
		return refuse("table " + ue.Table + " could not be read"), nil
	case err != nil:
		return parse.Applicability{}, err
	case missing != "":
		if strings.HasPrefix(missing, "table ") { // a table not found in a schema read in part is unknown, not absent
			if partial, perr := schemaPartial(ctx, main, in.Budget); perr != nil {
				return parse.Applicability{}, perr
			} else if partial {
				return refuse(reasonSchemaUnreadAll), nil
			}
		}
		if app {
			return refuse(reasonNoSignature + missing), nil
		}
		return refuse(reasonNotBugle), nil
	}
	got, err := rowrun.Probe(ctx, in, reqs())
	if err != nil {
		return got, err
	}
	if app && got.Status == parse.NotApplicable {
		got.Status = parse.UnsupportedSchema // the path names the app's store: a missing table is not another kind of file
	}
	return got, nil
}

// filesOf is the bundle of the input: the database and its companions.
func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	if in == nil || in.Primary.R == nil {
		return sqlitedb.Files{}, errors.New("android.googlemessages: the primary artifact has no reader")
	}
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.googlemessages: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.googlemessages: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

// Parse checks the live view (the signature, the rowid aliases of the mapped tables, the join tables, the mapped
// columns the store lacks) and states the store (writeNotes) before any mapping runs, so the host's note cap keeps those
// notes. Away from the app path a database without the signature is of another kind: Parse maps nothing and states it
// in rowrun's not-applicable form. The messages mapping runs after them.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	pl, app := classifyPlace(in.Primary.Logical)
	lv, err := checkLiveView(ctx, in, files, out, pl)
	if err != nil {
		return err
	}
	if lv.NotApplicable != "" {
		out.Note("not_applicable", lv.NotApplicable)
		return out.Warn(ctx, "sqlite:schema", "not applicable at parse time: "+lv.NotApplicable)
	}
	if lv.Version != 1 && lv.Version != 2 {
		reason := reasonUncited + ": user_version " + strconv.FormatInt(int64(lv.Version), 10) + " is not a cited AOSP version; read by the AOSP core signature"
		if err := out.Warn(ctx, "sqlite:schema", reason); err != nil {
			return err
		}
	}
	if err := writeNotes(ctx, files, in.Budget, out, lv, app, databaseUser(in.Primary.Logical)); err != nil {
		return fmt.Errorf("android.googlemessages: %w", err)
	}
	cnt := &counts{}
	ce := newCountingEmitter(out, cnt)
	steps := []rowrun.Step{{Mapping: messagesMapping(&env{Guard: lv.Guard, Counts: cnt}), Identifying: true}}
	if _, err := rowrun.Run(ctx, in, ce, steps); err != nil {
		return fmt.Errorf("android.googlemessages: %w", err)
	}
	return ce.warn(ctx)
}

// reasonUncited is the reason prefix of the warning about a user version no cited AOSP source names (spec 5.4).
const reasonUncited = "googlemessages-schema-uncited"

// liveView is what checkLiveView found: the join guard, the live user version, the journal mode, whether the census
// could read the messages table whole (set by writeNotes), and the mapped columns each table lacks.
type liveView struct {
	Guard              *joinGuard
	Version            int32
	JournalMode        string
	MessagesUnreadable bool
	Missing            map[string][]string // table -> the mapped columns the store lacks, in mappedColumns order
	// NotApplicable is why a live view away from the app path is not applicable ("" when it is): no signature
	// ("not a bugle_db"), or a signature that is undecided, stated as undecided.
	NotApplicable string
}

// The values of the note journal_mode (plan 4I5-P10): header bytes 18 and 19, no PRAGMA is evaluated.
const (
	journalWAL       = "wal"
	journalRollback  = "rollback"
	journalUndecided = "undecided"
)

func journalModeOf(writeVersion, readVersion uint8) string {
	switch {
	case writeVersion == 2 && readVersion == 2:
		return journalWAL
	case writeVersion == 1 && readVersion == 1:
		return journalRollback
	}
	return journalUndecided
}

// checkLiveView opens the live view once. At the app path a signature that is absent is ErrNoSignature, and one that is
// unknown in a schema read in part ErrSchemaIncomplete; elsewhere a live view without the signature is of another kind
// (NotApplicable "not a bugle_db") and an undecided signature is not applicable with its undecided reason, never the
// decided absence. Then every mapped table must have its rowid alias (messages, parts), the mapped columns each table
// lacks are listed (B4), and the join tables are checked (the guard).
func checkLiveView(ctx context.Context, in *parse.Input, files sqlitedb.Files, out parse.Emitter, pl place) (*liveView, error) {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, fmt.Errorf("android.googlemessages: %w", err)
	}
	defer db.Release()
	app := pl == placeApp
	var part *bool
	partial := func() (bool, error) {
		if part == nil {
			p, err := schemaPartial(ctx, files, in.Budget)
			if err != nil {
				return false, err
			}
			part = &p
		}
		return *part, nil
	}
	missing, err := signatureOf(ctx, db)
	var ue *unreadableError
	switch {
	case errors.As(err, &ue):
		p, perr := partial()
		switch {
		case perr != nil:
			return nil, fmt.Errorf("android.googlemessages: %w", perr)
		case p && app:
			return nil, ErrSchemaIncomplete
		case p:
			return &liveView{NotApplicable: reasonSchemaUnreadAll}, nil
		case app:
			return nil, fmt.Errorf("android.googlemessages: %w", ue)
		}
		return &liveView{NotApplicable: "table " + ue.Table + " could not be read"}, nil
	case err != nil:
		return nil, fmt.Errorf("android.googlemessages: %w", err)
	case missing != "":
		if strings.HasPrefix(missing, "table ") { // a table not found in a schema read in part is unknown, not absent
			p, perr := partial()
			switch {
			case perr != nil:
				return nil, fmt.Errorf("android.googlemessages: %w", perr)
			case p && app:
				return nil, fmt.Errorf("%w: missing %s", ErrSchemaIncomplete, missing)
			case p:
				return &liveView{NotApplicable: reasonSchemaUnreadAll}, nil
			}
		}
		if app {
			return nil, fmt.Errorf("%w: missing %s", ErrNoSignature, missing)
		}
		return &liveView{NotApplicable: reasonNotBugle}, nil
	}
	st := db.Info().Stored
	lv := &liveView{Version: db.Info().Header.UserVersion, JournalMode: journalModeOf(st.WriteVersion, st.ReadVersion), Missing: map[string][]string{}}
	for _, rq := range reqs() {
		switch rq.Table {
		case tableMessages, tableParts:
		default:
			continue // a join table: the guard checks it
		}
		t, err := db.Table(ctx, rq.Table, rq.Need, nil)
		if err != nil {
			return nil, fmt.Errorf("android.googlemessages: %s: %w", rq.Table, err)
		}
		if i := t.Col(rq.Alias); i < 0 || !t.Cols()[i].RowidAlias {
			return nil, fmt.Errorf("android.googlemessages: %w", &sqlitedb.UnsupportedSchemaError{Table: rq.Table, Reason: "column " + rq.Alias + " is not the rowid alias"})
		}
	}
	for _, table := range []string{tableMessages, tableParts, tableParticipants, tableConversations, tableMembers} {
		t, err := db.Table(ctx, table, nil, nil)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable), dataDamage(err):
			continue // an absent or unreadable join table is stated by the guard
		case err != nil:
			return nil, fmt.Errorf("android.googlemessages: %s: %w", table, err)
		}
		for _, c := range mappedColumns(table) {
			if t.Col(c) < 0 {
				lv.Missing[table] = append(lv.Missing[table], c)
			}
		}
	}
	g, err := checkJoinSchema(ctx, db, out, partial)
	if err != nil {
		return nil, err
	}
	lv.Guard = g
	return lv, nil
}
