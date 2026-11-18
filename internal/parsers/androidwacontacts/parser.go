package androidwacontacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// Identity of the parser.
const (
	Name    = "android.whatsapp.contacts"
	Version = "1.0.0"
)

// ErrNotLiveDatabase is returned by Parse for a database that is not in the app's live databases directory (4I-R3).
var ErrNotLiveDatabase = errors.New("android.whatsapp.contacts: not the app's live databases directory")

// Parser is the android.whatsapp.contacts parser. It holds no state.
type Parser struct{}

var _ parse.RowMapper = Parser{}

// ClaimableTables are the tables whose mapping is total: every row gives one record or a counted warning (spec 6.6),
// so typed recovery (plan 4G) can map the rows of contacts that were deleted or superseded.
func ClaimableTables() []string { return []string{"wa_contacts"} }

func claimsOf(tables []string) []parse.TableClaim {
	out := make([]parse.TableClaim, len(tables))
	for i, t := range tables {
		out[i] = parse.TableClaim{Role: "db", Table: t}
	}
	return out
}

// Meta describes the parser. The primary globs end in a database or backup name, never in a bare "*", so a -wal,
// -journal or -shm file is never a primary (4I-R21); it reaches the job only as the companion of its primary.
func (Parser) Meta() parse.Meta {
	return parse.Meta{
		Name: Name, Version: Version, Title: "Android WhatsApp contacts (wa.db)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: contact.Type, PayloadVersion: contact.PayloadVersion}},
		Claims:    claimsOf(ClaimableTables()),
		Inputs: []parse.InputSpec{
			{Role: "db", Required: true, Globs: []string{
				"android:**/com.whatsapp/databases/wa.db",
				"android:**/com.whatsapp.w4b/databases/wa.db",
				"android:**/WhatsApp/Databases/wa.db",
				"android:**/WhatsApp/Databases/wa.db.crypt*",
				"android:**/WhatsApp Business/Databases/wa.db",
				"android:**/WhatsApp Business/Databases/wa.db.crypt*",
			}},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "journal", Companions: []string{"-journal"}},
		},
	}
}

// Mappings are the per-table mappings.
func (Parser) Mappings() []parse.TableMapping { return []parse.TableMapping{waContactsMapping()} }

// The reasons of Probe.
const (
	reasonEncryptedBackup = "WhatsApp encrypted backup (crypt12/14/15): sub-project 10"
	reasonPlainBackup     = "backup-copy: plain SQLite at a WhatsApp backup path, not parsed (sub-project 10)"
	reasonOtherDir        = "backup-copy: not the app's live databases directory"
	reasonNotSQLiteBackup = "not a SQLite file at a WhatsApp backup path"
	reasonNotSQLiteLive   = "not a SQLite file at the WhatsApp databases path"
	reasonNoTable         = "no wa_contacts table at the WhatsApp databases path"
	rowrunNotSQLite       = "not an SQLite database" // rowrun.Probe's reason for a file that is not SQLite
)

const sqliteMagic = "SQLite format 3\x00"

// Probe decides from the main file only (4I-R3). A database outside the app's live databases directory is never
// applicable and never a benign skip. At the live path a file that is not SQLite, or a database without a wa_contacts
// table, is unsupported-schema, never not-applicable: the path names a WhatsApp store we cannot read, not another kind
// of file (unknown is never absent).
func (Parser) Probe(ctx context.Context, in *parse.Input) (parse.Applicability, error) {
	if in == nil || in.Primary.R == nil {
		return parse.Applicability{}, rowrun.ErrNilInput
	}
	dp := classifyDB(in.Primary.Logical)
	if dp.Place != placeLive {
		return probeNonLive(ctx, in, dp)
	}
	got, err := rowrun.Probe(ctx, in, []rowrun.Req{{Table: "wa_contacts", Need: []string{"jid"}}})
	if err != nil {
		return got, err
	}
	if got.Status == parse.NotApplicable {
		got.Status = parse.UnsupportedSchema
		if got.Reason == rowrunNotSQLite {
			got.Reason = reasonNotSQLiteLive
		} else {
			got.Reason = reasonNoTable
		}
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

// unmappedTables are the tables of wa.db that spec 6.6 lists as not mapped; the ones present are in the note
// unmapped_tables. A fresh slice per call: the package holds no mutable state.
func unmappedTables() []string {
	return []string{"wa_biz_profiles", "wa_block_list", "wa_group_admin_settings", "wa_vnames"}
}

// Parse notes the app and the Android user, and the unmapped tables present, before any mapping (so the host's note
// cap keeps them), then runs the mapping. A database that is not in the app's live databases directory is
// ErrNotLiveDatabase and emits nothing (4I-R3).
func (Parser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if in == nil || in.Primary.R == nil {
		return rowrun.ErrNilInput
	}
	dp := classifyDB(in.Primary.Logical)
	if dp.Place != placeLive {
		return ErrNotLiveDatabase
	}
	out.Note("app", dp.App)
	out.Note("database_user", strconv.Itoa(dp.User))
	files := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok && a.R != nil {
		files.WAL, files.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok && a.R != nil {
		files.Journal, files.JournalSize = a.R, a.Size
	}
	rep, err := census.Take(ctx, files, in.Budget, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	var present []string
	for _, o := range rep.Objects {
		if o.Type == "table" && slices.Contains(unmappedTables(), strings.ToLower(o.Name)) {
			present = append(present, strings.ToLower(o.Name))
		}
	}
	slices.Sort(present)
	if len(present) > 0 {
		out.Note("unmapped_tables", strings.Join(slices.Compact(present), ","))
	}
	if _, err := rowrun.Run(ctx, in, out, []rowrun.Step{{Mapping: waContactsMapping()}}); err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	return nil
}
