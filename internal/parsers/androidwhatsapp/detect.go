package androidwhatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The errors of Parse, wrapped with %w.
var (
	// ErrNeitherTable says the database holds neither the legacy nor the modern message table.
	ErrNeitherTable = errors.New("android.whatsapp.msgstore: neither messages nor message table")
	// ErrNotLiveDatabase says Parse was called on a database that is not in the app's live databases directory (4I-R3).
	ErrNotLiveDatabase = errors.New("android.whatsapp.msgstore: not the app's live databases directory")
	// ErrSchemaUndecidable says the schema table could not be read whole and neither message table was seen in what
	// was read: whether the store is legacy, modern or neither is unknown, never "neither".
	ErrSchemaUndecidable = errors.New("android.whatsapp.msgstore: the schema is damaged and no message table was found in what could be read")
)

type generation int

const (
	genNone generation = iota
	genLegacy
	genModern
	genTransition
)

func (g generation) String() string {
	switch g {
	case genLegacy:
		return "legacy"
	case genModern:
		return "modern"
	case genTransition:
		return "transition"
	}
	return "none"
}

// The tables the detector counts. The legacy sentinel is a TEXT key_remote_jid of '-1' (research 3.1).
const (
	tblMessages    = "messages"
	tblMessage     = "message"
	tblAddOn       = "message_add_on"
	tblCallLog     = "call_log"
	tblDeletedJobs = "deleted_chat_job"
	sentinelJID    = "-1"
	maxUnmapped    = 512 // bytes of the unmapped_tables note
)

func censusSpecs() []census.CountSpec {
	return []census.CountSpec{
		{Table: tblMessages, Column: "key_remote_jid", Equals: sentinelJID},
		{Table: tblMessage},
		{Table: tblAddOn},
		{Table: tblCallLog},
		{Table: tblDeletedJobs},
	}
}

// detection is what the census of the live view says about the store (spec 6.2).
type detection struct {
	Gen                     generation
	Census                  census.Report
	MessagesRows            int64 // the full-scan count, the sentinel excluded; meaningful when MessagesKnown
	MessageRows             int64 // meaningful when MessageKnown
	Sentinel                int64
	MessagesKnown           bool // the table is a base table and its scan was whole
	MessageKnown            bool
	ViewSource              string // "messages", "other", "absent" or "unknown" (transition only)
	ViewsNamedLikeTables    []string
	messages, message       census.Count
	hasMessages, hasMessage bool // a base table of that name was seen
}

// filesOf is the bundle of the input: the database and its companions.
func filesOf(in *parse.Input) (sqlitedb.Files, error) {
	if in == nil || in.Primary.R == nil {
		return sqlitedb.Files{}, errors.New("android.whatsapp.msgstore: the primary artifact has no reader")
	}
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.whatsapp.msgstore: the wal artifact has no reader")
		}
		f.WAL, f.WALSize = a.R, a.Size
	}
	if a, ok := in.Artifacts["journal"]; ok {
		if a.R == nil {
			return sqlitedb.Files{}, errors.New("android.whatsapp.msgstore: the journal artifact has no reader")
		}
		f.Journal, f.JournalSize = a.R, a.Size
	}
	return f, nil
}

func isBaseTable(o census.Object, name string) bool {
	return o.Type == "table" && strings.EqualFold(o.Name, name) && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(o.SQL)), "CREATE VIRTUAL")
}

func hasTable(objs []census.Object, name string) bool {
	for _, o := range objs {
		if isBaseTable(o, name) {
			return true
		}
	}
	return false
}

func (d detection) count(table string) (census.Count, bool) {
	for _, c := range d.Census.Counts {
		if strings.EqualFold(c.Table, table) {
			return c, true
		}
	}
	return census.Count{}, false
}

// decide fills the generation from the counts. Presence is only what the schema table showed: a damaged schema that
// lost entries leaves a missing table unknown, never absent.
func (d *detection) decide() error {
	d.messages, _ = d.count(tblMessages)
	d.message, _ = d.count(tblMessage)
	d.hasMessages, d.hasMessage = d.messages.Present, d.message.Present
	switch {
	case d.hasMessages && d.hasMessage:
		d.Gen = genTransition
	case d.hasMessages:
		d.Gen = genLegacy
	case d.hasMessage:
		d.Gen = genModern
	default:
		if d.messages.Unreadable == census.SchemaDamagedNote || d.message.Unreadable == census.SchemaDamagedNote {
			return ErrSchemaUndecidable
		}
		return ErrNeitherTable
	}
	d.MessagesKnown = d.hasMessages && d.messages.Unreadable == ""
	d.MessageKnown = d.hasMessage && d.message.Unreadable == ""
	if d.MessagesKnown {
		d.Sentinel = d.messages.Matched
		d.MessagesRows = d.messages.Rows - d.messages.Matched
	}
	if d.MessageKnown {
		d.MessageRows = d.message.Rows
	}
	for _, o := range d.Census.Objects {
		if o.Type == "view" && (strings.EqualFold(o.Name, tblMessages) || strings.EqualFold(o.Name, tblMessage)) {
			d.ViewsNamedLikeTables = append(d.ViewsNamedLikeTables, o.Name)
		}
	}
	d.ViewSource = "absent"
	for _, o := range d.Census.Objects {
		if o.Type == "view" && strings.EqualFold(o.Name, "message_view") {
			d.ViewSource = viewSource(o)
			break
		}
	}
	return nil
}

// viewSource says whether the SQL of message_view reads the legacy table: the token "FROM messages" (ASCII
// case-insensitive, white space normalised, the table name whole). A cut SQL without the token is unknown, not "other".
func viewSource(o census.Object) string {
	s := strings.ToLower(strings.Join(strings.FieldsFunc(o.SQL, func(r rune) bool { return r == ' ' || (r >= '\t' && r <= '\r') }), " "))
	const tok = "from messages"
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			break
		}
		end := i + j + len(tok)
		if end == len(s) || !isIdentByte(s[end]) {
			return "messages"
		}
		i = end
	}
	if o.SQLTruncated {
		return "unknown"
	}
	return "other"
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// detect reads the census of the live view (WAL applied, a hot journal rolled back) and decides the generation. A
// table that exists only in a committed WAL frame counts; an uncommitted frame never does.
func detect(ctx context.Context, in *parse.Input) (detection, error) {
	files, err := filesOf(in)
	if err != nil {
		return detection{}, err
	}
	rep, err := census.Take(ctx, files, in.Budget, censusSpecs())
	if err != nil {
		return detection{}, fmt.Errorf("android.whatsapp.msgstore: %w", err)
	}
	d := detection{Census: rep}
	if err := d.decide(); err != nil {
		return d, err
	}
	return d, nil
}

// steps are the tables to map, in the order of R1: the legacy table, then the modern one, each only when the census
// saw it as a table. Both are Optional so that a table that the census saw but the run cannot open is a note, not a
// silent skip: rowrun states it.
func (d detection) steps(g *joinGuard, js *jobSet) []rowrun.Step {
	var steps []rowrun.Step
	if d.hasMessages {
		steps = append(steps, rowrun.Step{Mapping: withGuard(messagesMappingWith(js), g), Optional: true})
	}
	if d.hasMessage {
		steps = append(steps, rowrun.Step{Mapping: withGuard(messageMappingWith(js), g), Optional: true})
	}
	if hasTable(d.Census.Objects, tblAddOn) {
		steps = append(steps, rowrun.Step{Mapping: withGuard(addOnMappingWith(js), g), Optional: true})
	}
	return steps
}

// unmappedTable says whether spec 6.5 lists the table as unmapped: message_orphaned_edit, receipt_*, message_thumbnail,
// media_hash_thumbnail, group_participant*, missed_call_logs, newsletter* and props, and the legacy receipts table.
func unmappedTable(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "message_orphaned_edit", "message_thumbnail", "media_hash_thumbnail", "missed_call_logs", "props", "receipts":
		return true
	}
	return strings.HasPrefix(n, "receipt_") || strings.HasPrefix(n, "group_participant") || strings.HasPrefix(n, "newsletter")
}

// note writes the detector's notes (4I-R11). They are written before rowrun.Run, so the host's note cap, where the
// first write wins, keeps them.
func (d detection) note(out parse.Emitter, p dbPath) {
	out.Note("generation", d.Gen.String())
	if d.MessagesKnown {
		out.Note("rows.messages", strconv.FormatInt(d.MessagesRows, 10))
		out.Note("sentinel_rows", strconv.FormatInt(d.Sentinel, 10))
	}
	if d.MessageKnown {
		out.Note("rows.message", strconv.FormatInt(d.MessageRows, 10))
	}
	if d.Gen == genTransition {
		out.Note("message_view_source", d.ViewSource)
	}
	if len(d.ViewsNamedLikeTables) > 0 {
		out.Note("views_named_like_tables", strings.Join(d.ViewsNamedLikeTables, ","))
	}
	for _, t := range []string{tblMessages, tblMessage, tblAddOn, tblCallLog} {
		if c, ok := d.count(t); ok && c.Unreadable != "" {
			out.Note(t, "unreadable")
		}
	}
	damaged := d.Census.SchemaSkipped > 0
	if d.Gen != genLegacy && !damaged && !hasTable(d.Census.Objects, "jid_map") {
		out.Note("jid_map", "absent")
	}
	if d.hasMessages && !damaged && !hasTable(d.Census.Objects, "chat_list") {
		out.Note("table.chat_list", "absent") // a legacy row then has a thread id and no title (4I-R13)
	}
	if list := d.unmapped(); list != "" {
		out.Note("unmapped_tables", list)
	}
	out.Note("app", p.App)
	out.Note("database_user", strconv.Itoa(p.User))
	if c, ok := d.count(tblDeletedJobs); ok && c.Present && c.Unreadable == "" {
		out.Note("deleted_chat_jobs", strconv.FormatInt(c.Rows, 10))
	}
	for _, o := range d.Census.Objects {
		if o.Type == "view" && (o.Name == "available_message_view" || o.Name == "legacy_available_messages_view") {
			sum := sha256.Sum256([]byte(o.SQL))
			v := "sha256=" + hex.EncodeToString(sum[:]) + ";bytes=" + strconv.Itoa(len(o.SQL))
			if o.SQLTruncated {
				v += ";truncated=true"
			}
			out.Note("view_sql."+o.Name, v)
		}
	}
}

// unmapped is the comma list of the present tables that spec 6.5 lists as unmapped, in schema order, cut at an entry
// boundary near maxUnmapped bytes with a trailing ",…".
func (d detection) unmapped() string {
	var b strings.Builder
	for _, o := range d.Census.Objects {
		if o.Type != "table" || !unmappedTable(o.Name) {
			continue
		}
		if b.Len()+1+len(o.Name) > maxUnmapped {
			b.WriteString(",…")
			return b.String()
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(o.Name)
	}
	return b.String()
}

// warn raises the schema warnings: both generations populated (R1) and a message_view that disagrees with where the
// rows are. Neither stops the parse.
func (d detection) warn(ctx context.Context, out parse.Emitter) error {
	if d.MessagesKnown && d.MessageKnown && d.MessagesRows > 0 && d.MessageRows > 0 {
		reason := fmt.Sprintf("whatsapp-both-generations: messages=%d message=%d", d.MessagesRows, d.MessageRows)
		if err := out.Warn(ctx, "sqlite:schema", reason); err != nil {
			return err
		}
	}
	if d.Gen != genTransition || !d.MessagesKnown || !d.MessageKnown || (d.ViewSource != "messages" && d.ViewSource != "other") {
		return nil
	}
	rows := "none"
	switch {
	case d.MessagesRows > 0 && d.MessageRows > 0:
		rows = "both"
	case d.MessagesRows > 0:
		rows = "messages"
	case d.MessageRows > 0:
		rows = "message"
	}
	if (d.ViewSource == "messages" && rows == "message") || (d.ViewSource == "other" && rows == "messages") {
		reason := "whatsapp-message-view-disagrees: message_view reads " + d.ViewSource + " but rows are in " + rows
		return out.Warn(ctx, "sqlite:schema", reason)
	}
	return nil
}
