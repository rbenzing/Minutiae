package androidwhatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Identity of the parser.
const (
	Name    = "android.whatsapp.msgstore"
	Version = "1.0.0"
)

// Parser is the android.whatsapp.msgstore parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// Meta describes the parser. The primary globs end in a database or backup name, never in a bare "*", so a -wal,
// -journal or -shm file is never a primary (4I-R21); it reaches the job only as the companion of its primary.
// There are no claims until the typed recovery of plan 4I task 14.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android WhatsApp messages and calls (msgstore.db)",
		Platforms: []string{parse.PlatformAndroid},
		Emits: []parse.Emit{
			{Type: message.Type, PayloadVersion: message.PayloadVersion},
			{Type: call.Type, PayloadVersion: call.PayloadVersion},
		},
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{
				"android:**/com.whatsapp/databases/msgstore.db",
				"android:**/com.whatsapp.w4b/databases/msgstore.db",
				"android:**/WhatsApp/Databases/msgstore*.db",
				"android:**/WhatsApp/Databases/msgstore*.db.crypt*",
				"android:**/WhatsApp Business/Databases/msgstore*.db",
				"android:**/WhatsApp Business/Databases/msgstore*.db.crypt*",
			}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings are the per-table mappings, in the order Parse runs them: the legacy table first, then the modern one (R1).
func (Parser) Mappings() []parse.TableMapping {
	return []parse.TableMapping{messagesMapping(), messageMapping(), addOnMapping(), callLogMapping()}
}

// The reasons of Probe.
const (
	reasonEncryptedBackup = "WhatsApp encrypted backup (crypt12/14/15): sub-project 10"
	reasonPlainBackup     = "backup-copy: plain SQLite at a WhatsApp backup path, not parsed (sub-project 10)"
	reasonOtherDir        = "backup-copy: not the app's live databases directory"
	reasonNotSQLiteBackup = "not a SQLite file at a WhatsApp backup path"
	reasonNotSQLiteLive   = "not a SQLite file at the WhatsApp databases path"
	reasonCompanionOnly   = "the main file is not a database but a -wal or -journal companion exists"
	reasonEncryptedLive   = "the file looks encrypted"
	reasonDeferred        = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNeither         = "neither messages nor message table"
	reasonSchemaDamaged   = "the schema could not be read whole and no message table was found in what was read"
)

const sqliteMagic = "SQLite format 3\x00"

// Probe decides from the main file only (4I-R3). A database that is not in the app's live databases directory is
// never applicable and never a benign skip: it is reported unparsed (encrypted, a backup copy, not SQLite). A file
// that is not SQLite at the live path is unsupported-schema, never not-applicable: the path names a WhatsApp store we
// cannot read, not another kind of file (unknown is never absent).
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	dp := classifyDB(in.Primary.Logical)
	if dp.Place != placeLive {
		return probeNonLive(ctx, in, dp)
	}
	_, wal := in.Artifacts["wal"]
	_, jr := in.Artifacts["journal"]
	companion := wal || jr
	rep, err := census.Take(ctx, sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget, nil)
	switch {
	case errors.Is(err, sqlitedb.ErrEncrypted):
		return parse.Applicability{Status: parse.Encrypted, Reason: reasonEncryptedLive}, nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		if companion {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonCompanionOnly}, nil
		}
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonNotSQLiteLive}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	hasLegacy, hasModern := hasTable(rep.Objects, tblMessages), hasTable(rep.Objects, tblMessage)
	if !hasLegacy && !hasModern {
		switch {
		case companion:
			return parse.Applicability{Status: parse.Applicable, Reason: reasonDeferred}, nil
		case rep.SchemaSkipped > 0:
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonSchemaDamaged}, nil
		}
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonNeither}, nil
	}
	got, err := rowrun.Probe(ctx, in, reqsFor(hasLegacy, hasModern))
	if err != nil {
		return got, err
	}
	if got.Status == parse.NotApplicable {
		// the path anchors on the WhatsApp databases directory: a missing table is a store we cannot read, not another kind of file
		got.Status = parse.UnsupportedSchema
	}
	return got, nil
}

// probeNonLive answers for a database at a backup path or anywhere else a live glob matched (4I-R3).
func probeNonLive(ctx context.Context, in *parse.Input, dp dbPath) (parse.Applicability, error) {
	var head [len(sqliteMagic)]byte
	n, err := in.Primary.R.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return parse.Applicability{}, err
	}
	plain := func() parse.Applicability {
		if dp.Place == placeBackup {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonPlainBackup}
		}
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonOtherDir}
	}
	if n == len(head) && string(head[:]) == sqliteMagic {
		return plain(), nil
	}
	db, err := sqlitedb.Open(ctx, sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	switch {
	case errors.Is(err, sqlitedb.ErrEncrypted):
		return parse.Applicability{Status: parse.Encrypted, Reason: reasonEncryptedBackup}, nil
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		if dp.Place == placeBackup {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonNotSQLiteBackup}, nil
		}
		return parse.Applicability{Status: parse.UnsupportedSchema, Reason: reasonOtherDir}, nil
	case err != nil:
		return parse.Applicability{}, err
	}
	db.Release()
	return plain(), nil
}

// reqsFor is what Probe needs of the schema (spec 6.4.1): the identifying tables of each generation present, and every
// join table optional but checked when present. Where a join key is the rowid it must be the alias.
func reqsFor(legacy, modern bool) []rowrun.Req {
	var reqs []rowrun.Req
	if modern {
		reqs = append(reqs,
			rowrun.Req{Table: "message", Need: []string{"chat_row_id", "from_me", "key_id", "timestamp", "message_type"}, Alias: "_id"},
			rowrun.Req{Table: "chat", Need: []string{"jid_row_id"}, Alias: "_id"},
			rowrun.Req{Table: "jid", Need: []string{"raw_string"}, Alias: "_id"},
		)
		for _, t := range []string{
			"message_media", "message_location", "message_quoted", "message_revoked", "message_system", "message_system_value_change",
			"message_edit_info", "message_ephemeral", "message_view_once_media", "message_forwarded", "message_call_log", "message_system_number_change",
		} {
			reqs = append(reqs, rowrun.Req{Table: t, Need: []string{"message_row_id"}, Alias: "message_row_id", Optional: true})
		}
		reqs = append(reqs,
			rowrun.Req{Table: "message_vcard", Need: []string{"message_row_id"}, Optional: true},
			rowrun.Req{Table: "jid_map", Need: []string{"jid_row_id"}, Alias: "lid_row_id", Optional: true},
			rowrun.Req{Table: "message_add_on", Need: []string{"parent_message_row_id", "message_add_on_type"}, Alias: "_id", Optional: true},
			rowrun.Req{Table: "message_add_on_reaction", Alias: "message_add_on_row_id", Optional: true},
			rowrun.Req{Table: "call_log", Alias: "_id", Optional: true},
			rowrun.Req{Table: "call_log_participant_v2", Need: []string{"call_log_row_id"}, Optional: true},
			rowrun.Req{Table: "deleted_chat_job", Need: []string{"chat_row_id"}, Optional: true},
		)
	}
	if legacy {
		reqs = append(reqs,
			rowrun.Req{Table: "messages", Need: []string{"key_remote_jid", "key_from_me", "key_id", "timestamp", "media_wa_type", "status"}, Alias: "_id"},
			rowrun.Req{Table: "chat_list", Need: []string{"key_remote_jid"}, Optional: true},
			rowrun.Req{Table: "messages_quotes", Alias: "_id", Optional: true},
			rowrun.Req{Table: "messages_vcards", Need: []string{"message_row_id"}, Optional: true},
		)
	}
	return reqs
}

// Parse decides the generation from the census of the live view, writes the detector's notes and warnings before any
// mapping (so the host's note cap keeps them), and runs the mappings. A database that is not in the app's live
// databases directory is ErrNotLiveDatabase and emits nothing (4I-R3).
func (p Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return parseWith(ctx, in, out, maxJobs)
}

// parseWith is Parse with the cap of deleted_chat_job rows as a parameter (a test seam).
func parseWith(ctx context.Context, in *parse.Input, out parse.Emitter, maxRows int) error {
	if in == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	dp := classifyDB(in.Primary.Logical)
	if dp.Place != placeLive {
		return ErrNotLiveDatabase
	}
	d, err := detect(ctx, in)
	if err != nil {
		return err
	}
	d.note(out, dp)
	if err := d.warn(ctx, out); err != nil {
		return err
	}
	g, err := checkJoinSchema(ctx, in, d, out)
	if err != nil {
		return fmt.Errorf("android.whatsapp.msgstore: %w", err)
	}
	js, err := loadJobs(ctx, in, maxRows, g)
	if err != nil {
		return fmt.Errorf("android.whatsapp.msgstore: %w", err)
	}
	d.judge(js)
	if err := js.emit(ctx, out); err != nil {
		return err
	}
	_, err = rowrun.Run(ctx, in, out, d.steps(g, js))
	if err != nil {
		return fmt.Errorf("android.whatsapp.msgstore: %w", err)
	}
	if !js.Complete {
		return ErrPurgeJobsIncomplete // after every record was emitted: the job is incomplete with its records kept
	}
	return nil
}
