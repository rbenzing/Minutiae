package androidtelegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "android.telegram"
	Version = "1.0.1"
)

// The errors of Parse, wrapped with %w (the plan's "Notes and warnings").
var (
	// ErrUnsupportedVersion says a cache4.db with the Telegram signature has a live user version outside the supported
	// band (B4, 4I4-P8): the job fails and no record is emitted, at either place.
	ErrUnsupportedVersion = errors.New("android.telegram: cache4.db user version outside the supported band 64-181")
	// ErrNoMessageTable says the database at the app path lacks the Telegram signature, or that its version lacks the
	// message table of its layout (after a Probe that deferred the schema to Parse because a companion was present).
	ErrNoMessageTable = errors.New("android.telegram: no Telegram message table at the app path")
	// ErrSchemaIncomplete says the live view's schema could not be read whole and the signature was not found in what
	// was read: whether it exists is unknown, never "absent".
	ErrSchemaIncomplete = errors.New("android.telegram: the schema could not be read whole and no Telegram message table was found in what was read")
)

// The supported band of cache4.db user versions (B4, 4I4-P8): 64, 84, 159 and 181 have fixtures; 160-180 are not tied
// to releases (spec 6.1, UNVERIFIED). firstV2Version is the version that moved messages to messages_v2 (spec 6.2:
// DMH:791-987).
const (
	minUserVersion, maxUserVersion = 64, 181
	firstV2Version                 = 84
)

// The tables of spec 6.3 the parser reads (enc_chats is secret.go's tableEncChats, F9).
const (
	tableMessagesV2 = "messages_v2"
	tableMessages   = "messages"
	tableUsers      = "users"
	tableChats      = "chats"
	tableContacts   = "contacts"
	tableUsersData  = "users_data"
)

// Parser is the android.telegram parser. It holds no state.
type Parser struct{}

// ClaimableTables are the tables whose mapping is total: every messages, messages_v2 and users row gives one record
// (spec 8.5, 8.7, 8.9), so typed recovery can map the rows that were deleted or superseded (spec 11, call 7). A
// recovered row joins no table (B2). media_v* and messages_topics (copies of message blobs), chats, contacts,
// users_data, dialogs, enc_chats (never, spec 9) and every other table are not claimed.
func ClaimableTables() []string { return []string{tableMessages, tableMessagesV2, tableUsers} }

// Meta describes the parser. The two primary globs end in cache4.db, so a -wal, -journal or -shm file is never a
// primary (4I-R21); it reaches the job only as the companion of its primary. Only org.telegram.messenger is discovered
// by path (Q9): another package's cache4.db is reached through --scan or --artifact, by signature. The claims are
// ClaimableTables (spec 11).
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android Telegram messages, calls and contacts (cache4.db)",
		Platforms: []string{parse.PlatformAndroid},
		Emits: []parse.Emit{
			{Type: message.Type, PayloadVersion: message.PayloadVersion},
			{Type: call.Type, PayloadVersion: call.PayloadVersion},
			{Type: contact.Type, PayloadVersion: contact.PayloadVersion},
		},
		Claims: claimsOf(ClaimableTables()),
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{
				"android:**/org.telegram.messenger/files/cache4.db",
				"android:**/org.telegram.messenger/files/account*/cache4.db",
			}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings are the typed pass's mappings (a nil env, 4I4-P6): the message tables, the old generation first, then users.
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{
		messageMappingWith(nil, tableMessages), messageMappingWith(nil, tableMessagesV2), userMappingWith(nil),
	}
}

// The reasons of Probe (byte-exact, the plan's "Probe reasons").
const (
	reasonNotSQLite       = "not a SQLite file at the Telegram app path"
	reasonCompanionOnly   = "the main file is not a database but a -wal or -journal companion exists"
	reasonNoMessageTable  = "no Telegram message table at the app path"
	reasonEncrypted       = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNotSQLiteOther  = "not an SQLite database"
	reasonNotTelegram     = "not a Telegram cache4.db"
	reasonSchemaUnreadAll = "the schema could not be read whole and no Telegram message table was found in what was read"
)

func reasonVersion(v int32) string {
	return "cache4.db user version " + strconv.FormatInt(int64(v), 10) + " is outside the supported band 64-181"
}

func reasonLayout(v int32, table string) string {
	return "cache4.db user version " + strconv.FormatInt(int64(v), 10) + " needs table " + table
}

func inBand(v int32) bool { return v >= minUserVersion && v <= maxUserVersion }

// layoutTable is the message table a user version needs (spec 8.1): messages before 84, messages_v2 from 84 on.
func layoutTable(v int32) string {
	if v < firstV2Version {
		return tableMessages
	}
	return tableMessagesV2
}

// signatureColumns are the message-table columns of the signature (spec 8.1); usersColumns those of users.
func signatureColumns() []string { return []string{"mid", "uid", "date", "data", "out"} }

func usersColumns() []string { return []string{"uid", "data"} }

// reqs is what Probe needs of the schema (spec 6.3, 8.1): every table optional but users, with its columns checked
// when present; where a join key is the rowid it must be the alias. messages_v2 is keyed (mid, uid) on the implicit
// rowid, so it has no alias.
func reqs() []rowrun.Req {
	return []rowrun.Req{
		{Table: tableMessagesV2, Need: signatureColumns(), Optional: true},
		{Table: tableMessages, Need: signatureColumns(), Alias: "mid", Optional: true},
		{Table: tableUsers, Need: usersColumns(), Alias: "uid"},
		{Table: tableChats, Need: []string{"uid", "data"}, Alias: "uid", Optional: true},
		{Table: tableContacts, Need: []string{"uid", "mutual"}, Alias: "uid", Optional: true},
		{Table: tableUsersData, Need: []string{"uid", "about"}, Alias: "uid", Optional: true},
		{Table: tableEncChats, Need: encChatsColumns(), Alias: "uid", Optional: true},
	}
}

// Probe decides from the main file only (4I4-P8, spec 8.1). At the app path every refusal is loud (unsupported-schema,
// or encrypted): the path names a Telegram store this parser cannot read, not another kind of file. Anywhere else (a
// signature claim) every refusal is not-applicable, except a column rowrun requires (rowrun's rule). The signature is
// checked before the band (4I3 final review I1), the layout after it. With a -wal or -journal companion the main
// file's schema and user version prove nothing, so all of it is checked by Parse; Probe then defers as soon as the
// main file is not refused as encrypted or not SQLite.
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
	version := db.Info().Stored.UserVersion
	rep, err := census.Take(ctx, main, in.Budget, nil)
	if err != nil {
		db.Release()
		return parse.Applicability{}, err
	}
	sig, err := readSignature(ctx, db, func() (bool, error) { return rep.SchemaSkipped > 0, nil })
	db.Release()
	switch {
	case err != nil:
		return parse.Applicability{}, err
	case sig.state == sigUndecided:
		return refuse(sig.why()), nil
	case sig.state == sigAbsent && app:
		return refuse(reasonNoMessageTable), nil
	case sig.state == sigAbsent:
		return refuse(reasonNotTelegram), nil
	}
	if !inBand(version) {
		return refuse(reasonVersion(version)), nil
	}
	if need := layoutTable(version); !hasTable(rep.Objects, need) {
		if rep.SchemaSkipped > 0 {
			return refuse(reasonSchemaUnreadAll), nil
		}
		return refuse(reasonLayout(version, need)), nil
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

// hasTable says the census saw a table of that name, matched as the engine matches names: ASCII case-insensitive only.
func hasTable(objs []census.Object, name string) bool {
	for _, o := range objs {
		if o.Type == "table" && asciiEqualFold(o.Name, name) {
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

// sigState is what the schema says of the Telegram signature (spec 8.1).
type sigState int

const (
	sigAbsent    sigState = iota // a part of the signature is absent, decided
	sigPresent                   // a message table and users hold the signature columns
	sigUndecided                 // neither: a table could not be read, or the schema was read only in part
)

// signature is the signature of one database: its state, which message tables hold the signature columns, and, when
// undecided, the damage that made it so (nil when only a schema read in part did).
type signature struct {
	state       sigState
	v1, v2      bool
	damage      error
	damageTable string
}

// why is the Probe reason of an undecided signature, naming the table only (rowrun's rule for reasons).
func (s signature) why() string {
	if s.damage != nil {
		return "table " + s.damageTable + " could not be read"
	}
	return reasonSchemaUnreadAll
}

// readSignature reads the signature: a message table (messages_v2 or messages) with the signature columns and users with
// uid and data (spec 8.1). A table that lacks a column is absent for the signature; a table not found is absent only when
// the schema was read whole; in a schema read in part a table not found or not readable is undecided without a table
// reason (partial is asked only for those); a table that exists and cannot be read in a schema read whole leaves its
// part undecided with its damage, never absent. Budget, context and I/O errors are returned.
func readSignature(ctx context.Context, db *sqlitedb.DB, partial func() (bool, error)) (signature, error) {
	var s signature
	var part *bool
	holds := func(table string, cols []string) (sigState, error) {
		_, err := db.Table(ctx, table, cols, nil)
		var us *sqlitedb.UnsupportedSchemaError
		switch {
		case err == nil:
			return sigPresent, nil
		case errors.As(err, &us) && len(us.Missing) > 0:
			return sigAbsent, nil
		case errors.Is(err, sqlitedb.ErrNoSuchTable), dataDamage(err):
			if part == nil {
				p, perr := partial()
				if perr != nil {
					return sigAbsent, perr
				}
				part = &p
			}
			switch {
			case *part:
				// in a schema read in part the engine reports a table it did not see as corrupt, not absent: either
				// way the table is unknown, stated as the schema read in part (review I1)
				return sigUndecided, nil
			case errors.Is(err, sqlitedb.ErrNoSuchTable):
				return sigAbsent, nil
			}
			if s.damage == nil {
				s.damage, s.damageTable = fmt.Errorf("table %s could not be read: %w", table, err), table
			}
			return sigUndecided, nil
		}
		return sigAbsent, err
	}
	v2, err := holds(tableMessagesV2, signatureColumns())
	if err != nil {
		return s, err
	}
	v1, err := holds(tableMessages, signatureColumns())
	if err != nil {
		return s, err
	}
	users, err := holds(tableUsers, usersColumns())
	if err != nil {
		return s, err
	}
	s.v1, s.v2 = v1 == sigPresent, v2 == sigPresent
	msgs := sigAbsent
	switch {
	case s.v1 || s.v2:
		msgs = sigPresent
	case v1 == sigUndecided || v2 == sigUndecided:
		msgs = sigUndecided
	}
	switch {
	case msgs == sigAbsent || users == sigAbsent:
		s.state = sigAbsent
	case msgs == sigPresent && users == sigPresent:
		s.state = sigPresent
	default:
		s.state = sigUndecided
	}
	return s, nil
}

// filesOf is the bundle of the input: the database and its companions.
func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.telegram: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.telegram: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

// Parse checks the live view (the signature, then its user version, B4, 4I4-P8; the layout table; the mapped tables'
// required columns and rowid aliases; the join tables) and states the store (writeNotes) before any mapping runs, so
// the host's note cap keeps those notes. Away from the app path a database without the signature is of another kind:
// Parse maps nothing and states it in rowrun's not-applicable form (F3). Then it maps both message tables (spec 8.3),
// messages before messages_v2, each optional, and users (spec 8.9), through one constructor table (built once per
// Parse, C2) and the counting emitter (4I4-P9), and after the run raises the counted warnings and writes the count notes.
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || out == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	files, err := filesOf(in)
	if err != nil {
		return err
	}
	lv, err := checkLiveView(ctx, in, files, out)
	if err != nil {
		return err
	}
	if lv.NotApplicable != "" {
		out.Note("not_applicable", lv.NotApplicable)
		return out.Warn(ctx, "sqlite:schema", "not applicable at parse time: "+lv.NotApplicable)
	}
	_, account := classifyPlace(in.Primary.Logical)
	if err := writeNotes(ctx, files, in.Budget, out, *lv, account, databaseUser(in.Primary.Logical)); err != nil {
		return fmt.Errorf("android.telegram: %w", err)
	}
	tab := newTable()
	e := &env{Account: account}
	if lv.Guard != nil {
		e.Guard = *lv.Guard
	}
	ce := newCountingEmitter(out)
	steps := []rowrun.Step{
		{Mapping: messageMapping(e, tab, tableMessages), Optional: true},
		{Mapping: messageMapping(e, tab, tableMessagesV2), Optional: true},
		{Mapping: userMapping(e, tab)},
	}
	rep, err := rowrun.Run(ctx, in, ce, steps)
	if err != nil {
		return fmt.Errorf("android.telegram: %w", err)
	}
	if err := warnBothGenerations(ctx, out, rep); err != nil {
		return err
	}
	if err := ce.warn(ctx); err != nil {
		return err
	}
	writeCountNotes(out, ce)
	return nil
}

// warnBothGenerations raises the one counted warning of spec 8.3 when both message tables held rows: both were read
// in full and each record is labelled by raw.source_table; nothing is merged and the outcome stays complete (4I R1).
func warnBothGenerations(ctx context.Context, out parse.Emitter, rep rowrun.Report) error {
	rows := map[string]int64{}
	for _, t := range rep.Tables {
		rows[t.Table] = t.Rows
	}
	if rows[tableMessages] == 0 || rows[tableMessagesV2] == 0 {
		return nil
	}
	return out.Warn(ctx, "sqlite:schema", reasonBothGenerations+": messages="+strconv.FormatInt(rows[tableMessages], 10)+
		" messages_v2="+strconv.FormatInt(rows[tableMessagesV2], 10))
}

// liveView is what checkLiveView found: the join guard, the live user version (the note database_version) and which
// message tables exist.
type liveView struct {
	Guard              *joinGuard
	Version            int32
	HasMessages, HasV2 bool
	// NotApplicable is why a live view away from the app path is not applicable ("" when it is): no signature
	// ("not a Telegram cache4.db"), or a signature that is undecided, stated as undecided (review I1).
	NotApplicable string
}

// checkLiveView opens the live view once (4I4-P8). At the app path the user version must be in the band first (the
// loud rule) and the signature must be present: absent is ErrNoMessageTable, unknown in a schema read in part
// ErrSchemaIncomplete. Elsewhere a live view without the signature is of another kind (NotApplicable "not a Telegram
// cache4.db"), an undecided signature is not applicable with its undecided reason (review I1), and
// the band applies only once the signature is present (4I3 final review I1). Then the version's layout table must exist
// (ErrNoMessageTable naming it), every mapped table present must have its required columns and rowid alias, and the
// join tables are checked (the guard).
func checkLiveView(ctx context.Context, in *parse.Input, files sqlitedb.Files, out parse.Emitter) (*liveView, error) {
	db, err := sqlitedb.Open(ctx, files, in.Budget)
	if err != nil {
		return nil, fmt.Errorf("android.telegram: %w", err)
	}
	defer db.Release()
	v := db.Info().Header.UserVersion
	pl, _ := classifyPlace(in.Primary.Logical)
	app := pl == placeApp
	if app && !inBand(v) {
		return nil, fmt.Errorf("%w (user version %d)", ErrUnsupportedVersion, v)
	}
	var part *bool
	partial := func() (bool, error) {
		if part == nil {
			rep, err := census.Take(ctx, files, in.Budget, nil)
			if err != nil {
				return false, err
			}
			p := rep.SchemaSkipped > 0
			part = &p
		}
		return *part, nil
	}
	sig, err := readSignature(ctx, db, partial)
	if err != nil {
		return nil, fmt.Errorf("android.telegram: %w", err)
	}
	switch {
	case sig.state == sigUndecided && app && sig.damage != nil:
		return nil, fmt.Errorf("android.telegram: %w", sig.damage)
	case sig.state == sigUndecided && app:
		return nil, ErrSchemaIncomplete
	case sig.state == sigAbsent && app:
		return nil, ErrNoMessageTable
	case sig.state == sigUndecided:
		// elsewhere an undecided signature is not applicable with its own reason, never the decided absence (review I1)
		return &liveView{NotApplicable: sig.why()}, nil
	case sig.state == sigAbsent:
		return &liveView{NotApplicable: reasonNotTelegram}, nil // a database of another kind
	}
	if !inBand(v) {
		return nil, fmt.Errorf("%w (user version %d)", ErrUnsupportedVersion, v)
	}
	need := layoutTable(v)
	if _, err := db.Table(ctx, need, nil, nil); errors.Is(err, sqlitedb.ErrNoSuchTable) {
		if p, perr := partial(); perr != nil {
			return nil, fmt.Errorf("android.telegram: %w", perr)
		} else if p {
			return nil, fmt.Errorf("%w: %s", ErrSchemaIncomplete, reasonLayout(v, need))
		}
		return nil, fmt.Errorf("%w: %s", ErrNoMessageTable, reasonLayout(v, need))
	}
	lv := &liveView{Version: v}
	for _, rq := range reqs() {
		switch rq.Table {
		case tableMessagesV2, tableMessages, tableUsers:
		default:
			continue // a join table: the guard checks it
		}
		t, err := db.Table(ctx, rq.Table, rq.Need, nil)
		switch {
		case errors.Is(err, sqlitedb.ErrNoSuchTable) && rq.Optional:
			continue
		case err != nil:
			return nil, fmt.Errorf("android.telegram: %s: %w", rq.Table, err)
		}
		if rq.Alias != "" {
			if i := t.Col(rq.Alias); i < 0 || !t.Cols()[i].RowidAlias {
				return nil, fmt.Errorf("android.telegram: %w", &sqlitedb.UnsupportedSchemaError{Table: rq.Table, Reason: "column " + rq.Alias + " is not the rowid alias"})
			}
		}
		switch rq.Table {
		case tableMessagesV2:
			lv.HasV2 = true
		case tableMessages:
			lv.HasMessages = true
		}
	}
	g, err := checkJoinSchema(ctx, db, out, partial)
	if err != nil {
		return nil, err
	}
	lv.Guard = g
	return lv, nil
}
