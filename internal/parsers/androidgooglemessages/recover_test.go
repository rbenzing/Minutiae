package androidgooglemessages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// notJoinedRecovered is the state of every join a recovered messages row does not make (spec 8.2), pinned literally.
const notJoinedRecovered = "not-joined-recovered"

// typedEmitter keeps everything the typed pass hands over: the recovered records with their facts, the unmapped rows, the
// warnings and notes, and the count of live emits (which a typed pass never makes).
type typedEmitter struct {
	recs     []records.Record
	facts    []parse.RecoveredFacts
	unmapped []string
	warnings []string
	notes    map[string]string
	live     int
}

func (e *typedEmitter) Emit(context.Context, records.Record) error {
	e.live++
	return nil
}

func (e *typedEmitter) EmitRecovered(_ context.Context, r records.Record, f parse.RecoveredFacts) error {
	e.recs, e.facts = append(e.recs, r), append(e.facts, f)
	return nil
}

func (e *typedEmitter) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	e.unmapped = append(e.unmapped, f.Locator+" "+reason)
	return nil
}

func (e *typedEmitter) Warn(_ context.Context, locator, reason string) error {
	e.warnings = append(e.warnings, locator+" "+reason)
	return nil
}

func (e *typedEmitter) Note(k, v string)      { e.notes[k] = v }
func (e *typedEmitter) Progress(int64, int64) {}

// typedRun is the typed recovery pass of p over files at the app path, as the host runs it: recrun.Run with the parser's
// mappings and claims.
func typedRun(t *testing.T, p parse.RowMapper, files map[string][]byte) *typedEmitter {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Files: files}.Bundle(h, remoteApp)
	e := &typedEmitter{notes: map[string]string{}}
	if _, err := recrun.Run(t.Context(), b.Input(h, p, false), e, p.Mappings(), p.Meta().Claims); err != nil {
		t.Fatal(err)
	}
	if e.live != 0 {
		t.Errorf("the typed pass emitted %d live records", e.live)
	}
	if len(e.unmapped) != 0 {
		t.Errorf("the typed pass reported rows unmapped: %v", e.unmapped)
	}
	return e
}

// recoveredParts are the typed records of recovered parts rows by their raw.part_rowid, with their facts.
func recoveredParts(e *typedEmitter) map[int64][]int {
	out := map[int64][]int{}
	for i, r := range e.recs {
		if e.facts[i].Table != "parts" {
			continue
		}
		if id, ok := rawOf(r)["part_rowid"].(int64); ok {
			out[id] = append(out[id], i)
		}
	}
	return out
}

// recoveredRow is the first recovered row of table with that rowid in variant (the typed pass would deliver it).
func recoveredRow(t *testing.T, variant, table string, rowid int64) sqlitefile.RecoveredRow {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", variant).Bundle(h, remoteApp)
	rr, ok := parsertest.FirstClaimedRow(t, h, androidgooglemessages.Parser{}, b, table, func(r sqlitefile.RecoveredRow) bool {
		return r.Rowid != nil && *r.Rowid == rowid
	})
	if !ok {
		t.Fatalf("%s: no recovered %s row %d", variant, table, rowid)
	}
	return rr
}

// mapOn maps a recovered row through the typed pass of p against the live view of files (at the app path) and requires one
// record.
func mapOn(t *testing.T, p parse.RowMapper, files map[string][]byte, rr sqlitefile.RecoveredRow) records.Record {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Files: files}.Bundle(h, remoteApp)
	got := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered %s row: err %v, unmapped %q, %d records", rr.Table, got.Err, got.Unmapped, len(got.Records))
	}
	return got.Records[0]
}

// joinOpts are the departures of a joinStore from gm-deleted's M2.
type joinOpts struct {
	noAuto      bool           // messages._id INTEGER PRIMARY KEY without AUTOINCREMENT
	noSeqRow    bool           // sqlite_sequence without its messages row (inserted, then deleted)
	noSeqTable  bool           // no sqlite_sequence table at all
	plainPartic bool           // participants._id INT PRIMARY KEY (not the rowid alias), with a -wal companion
	members     int            // non-self members of conversation 1 (default 1: Ann)
	message     map[string]any // cells of M2 that replace the defaults
}

// joinStore is a Go-built bugle_db holding the live message M2 of gm-deleted (outgoing, conversation 1, received at T+2000,
// the self P1 and the member Ann P2) and sqlite_sequence, with the departures of o.
func joinStore(t *testing.T, o joinOpts) map[string][]byte {
	t.Helper()
	m2 := map[string]any{"sender_id": int64(1), "message_status": int64(1), "conversation_id": int64(1), "received_timestamp": tT + 2000}
	for k, v := range o.message {
		m2[k] = v
	}
	s := store{
		participants:  []mrow{selfRow(1, 1), person(2, "5550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		members:       memberRows(1, 2),
		messages:      []mrow{{2, m2}},
		plainIDs:      o.plainPartic,
	}
	for i := 1; i < o.members; i++ {
		id := int64(100 + i)
		s.participants = append(s.participants, person(id, "555"+strconv.FormatInt(1000000+id, 10)))
		s.members = append(s.members, memberRows(1, id)...)
	}
	s.ddlOf = func(table, ddl string) string {
		var out string
		switch {
		case table == "messages" && o.noAuto:
			out = strings.Replace(ddl, "_id INTEGER PRIMARY KEY AUTOINCREMENT", "_id INTEGER PRIMARY KEY", 1)
		case table == "participants" && o.plainPartic:
			out = strings.Replace(ddl, "_id INT,", "_id INT PRIMARY KEY,", 1)
		default:
			return ddl
		}
		if out == ddl {
			t.Fatalf("%s: the DDL was not rewritten: %s", table, ddl)
		}
		return out
	}
	b := s.builder(t)
	if !o.noSeqTable {
		seq := b.CreateTable("sqlite_sequence", "CREATE TABLE sqlite_sequence(name,seq)")
		seq.Insert(1, "conversations", int64(1))
		seq.Insert(2, "messages", int64(2))
		seq.Insert(3, "participants", int64(2))
		if o.noSeqRow {
			seq.Delete(2)
		}
	}
	if o.plainPartic {
		return withWAL(b.Bytes())
	}
	return dbOnly(b.Bytes())
}

// undecidedSchema is the parser whose mapping context answers every Match on sqlite_master as undecidable.
type undecidedSchema struct{ androidgooglemessages.Parser }

func (p undecidedSchema) Mappings() []parse.TableMapping {
	ms := p.Parser.Mappings()
	for i := range ms {
		inner := ms[i].Map
		ms[i].Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			return inner(undecidedSchemaCtx{m}, row)
		}
	}
	return ms
}

type undecidedSchemaCtx struct{ parse.MapContext }

func (c undecidedSchemaCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	if strings.EqualFold(table, "sqlite_master") {
		return nil, sqlitedb.ErrKeyUndecidable
	}
	return c.MapContext.Match(table, col, key)
}

// Spec 8.1: the claims are exactly the claimable (total) tables, messages and parts; participants, conversations and
// conversation_participants (join context) are never claimed.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := androidgooglemessages.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	want := []string{"messages", "parts"}
	if cl := androidgooglemessages.ClaimableTables(); !slices.Equal(got, cl) || !slices.Equal(got, want) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = %v", got, cl, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
	if err := parse.ValidateMeta(p.Meta()); err != nil {
		t.Errorf("Meta with its claims: %v", err)
	}
}

// Spec 8 and 4I2 PF13: the claimed tables map their recovered rows, checked against the oracle the generator computed with an
// independent page walker from the committed gm-deleted and gm-rebuilt files (every recovered row of a claimed table is a
// typed record whose fields equal the oracle's, digests included, and whose values are a version the scenario wrote).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range []string{"gm-deleted", "gm-rebuilt"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteApp)
			want := parsertest.LoadRecoverExpect(t, "testdata", v)
			if len(want.Records) == 0 || !slices.Equal(want.Claimed, androidgooglemessages.ClaimableTables()) {
				t.Fatalf("the %s recover oracle lists %d records for claims %v: the check proves nothing", v, len(want.Records), want.Claimed)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidgooglemessages.Parser{}, b, want)
		})
	}
}

// Spec 8.2 (4I3 B2): a recovered messages row is the spec 6 mapping of its own columns and joins nothing: gm-deleted's M50
// (an incoming SMS whose conversation, sender and self rows are live) has its channel, direction, status and Time, no Body,
// the five join states not-joined-recovered, participants_unknown and the thread by its id alone.
func TestGoogleMessagesRecoveredMessageJoinsNothing(t *testing.T) {
	r := mapOn(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-deleted"), recoveredRow(t, "gm-deleted", "messages", 50))
	if str(r, "channel") != "sms" || str(r, "direction") != "in" || str(r, "status") != "received" {
		t.Errorf("M50: channel %q direction %q status %q, want sms in received", str(r, "channel"), str(r, "direction"), str(r, "status"))
	}
	if r.Time == nil || r.Time.T.UnixMilli() != tT+50000 {
		t.Errorf("M50: Time %v, want T+50000", r.Time)
	}
	if r.Body != "" || r.Payload["text_source"] != nil {
		t.Errorf("M50: Body %q, text_source %v: a recovered message joins no part", r.Body, r.Payload["text_source"])
	}
	raw := rawOf(r)
	for _, k := range []string{"body_state", "sender_state", "self_row_state", "conversation_state", "members_state"} {
		if raw[k] != notJoinedRecovered {
			t.Errorf("M50: raw.%s = %#v, want %q", k, raw[k], notJoinedRecovered)
		}
	}
	if r.Payload["participants_unknown"] != true || jsonText(r.Payload["participants"]) != "[]" {
		t.Errorf("M50: participants %v (unknown %v), want none and unknown", r.Payload["participants"], r.Payload["participants_unknown"])
	}
	if th := threadMap(r); !reflect.DeepEqual(th, map[string]any{"id": "1"}) {
		t.Errorf("M50: thread %v, want {id 1} only", th)
	}
	for _, k := range []string{"sender_is_self", "from_address_basis", "self_sub_id", "members_total", "members_basis", "sms_thread_id", "parts", "join_undecided"} {
		if v, ok := raw[k]; ok {
			t.Errorf("M50: raw.%s = %v: a recovered message joins nothing", k, v)
		}
	}
	if raw["sender_id"] != int64(2) || raw["self_id"] != int64(1) || raw["conversation_id"] != int64(1) {
		t.Errorf("M50: raw sender_id %v self_id %v conversation_id %v, want its own cells 2, 1, 1", raw["sender_id"], raw["self_id"], raw["conversation_id"])
	}
}

// partJoinOf is the raw.part_join and raw.part_join_reason of a record.
func partJoinOf(r records.Record) (join, reason string) {
	j, _ := rawOf(r)["part_join"].(string)
	why, _ := rawOf(r)["part_join_reason"].(string)
	return j, why
}

// Spec 8.3 with C5 and C5a: a recovered part joins the live message of its message_id only when the live messages _id is
// INTEGER PRIMARY KEY AUTOINCREMENT, sqlite_sequence holds a messages row, the message is live and its conversation and
// received time equal the part's; the first failing condition is the reason. An undecidable schema answer is undecided for
// every part of the run, never a join.
func TestGoogleMessagesRecoveredPartJoinRule(t *testing.T) {
	res := typedRun(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-deleted"))
	parts := recoveredParts(res)
	for id, w := range map[int64][2]string{52: {"joined", ""}, 50: {"not-joined", "no-live-message"}, 51: {"not-joined", "no-live-message"}, 53: {"not-joined", "no-live-message"}} {
		if len(parts[id]) == 0 {
			t.Fatalf("gm-deleted: no typed record of recovered part %d", id)
		}
		for _, i := range parts[id] {
			r := res.recs[i]
			if j, why := partJoinOf(r); j != w[0] || why != w[1] {
				t.Errorf("part %d: part_join %q reason %q, want %q %q", id, j, why, w[0], w[1])
			}
			if id == 52 && rawOf(r)["joined_message_rowid"] != int64(2) {
				t.Errorf("part 52: joined_message_rowid %v, want 2", rawOf(r)["joined_message_rowid"])
			}
			if id != 52 && has(r, "joined_message_rowid") {
				t.Errorf("part %d: joined_message_rowid %v on a part that is not joined", id, rawOf(r)["joined_message_rowid"])
			}
		}
	}

	// Go-built stores holding M2 as gm-deleted does: only the condition named fails (spec 8.3, I3, I4, T8b, M1, M3)
	p52 := recoveredRow(t, "gm-deleted", "parts", 52)
	cols := parsertest.LoadRecoverExpect(t, "testdata", "gm-deleted").Columns["parts"]
	with := func(col string, v sqlitefile.Value) sqlitefile.RecoveredRow {
		i := slices.Index(cols, col)
		if i < 0 {
			t.Fatalf("the recover oracle lists no parts column %s: %v", col, cols)
		}
		rr := p52
		rr.Values = slices.Clone(p52.Values)
		rr.Values[i] = v
		return rr
	}
	text := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte("x"), Len: 1}
	null := sqlitefile.Value{Kind: sqlitefile.KindNull}
	corrupt := faultParser{get: map[string]error{"messages": fmt.Errorf("%w: fake", sqlitedb.ErrCorrupt)}}
	twice := faultParser{twiceMaster: "messages"}
	for _, c := range []struct {
		name         string
		p            parse.RowMapper
		o            joinOpts
		rr           sqlitefile.RecoveredRow
		join, reason string
	}{
		{"AUTOINCREMENT with its sqlite_sequence row", androidgooglemessages.Parser{}, joinOpts{}, p52, "joined", ""},
		{"INTEGER PRIMARY KEY without AUTOINCREMENT", androidgooglemessages.Parser{}, joinOpts{noAuto: true}, p52, "not-joined", "no-autoincrement"},
		{"AUTOINCREMENT with the sqlite_sequence row deleted", androidgooglemessages.Parser{}, joinOpts{noSeqRow: true}, p52, "not-joined", "no-sqlite-sequence"},
		{"no sqlite_sequence in the schema read (I3)", androidgooglemessages.Parser{}, joinOpts{noSeqTable: true}, p52, "not-joined", "undecided"},
		{"two table rows of messages in sqlite_master (M3)", twice, joinOpts{}, p52, "not-joined", "undecided"},
		{"the part's message_id TEXT", androidgooglemessages.Parser{}, joinOpts{}, with("message_id", text), "not-joined", "undecided"},
		{"the part's message_id NULL", androidgooglemessages.Parser{}, joinOpts{}, with("message_id", null), "not-joined", "undecided"},
		{"the part's conversation_id TEXT", androidgooglemessages.Parser{}, joinOpts{}, with("conversation_id", text), "not-joined", "undecided"},
		{"the part's conversation_id NULL", androidgooglemessages.Parser{}, joinOpts{}, with("conversation_id", null), "not-joined", "undecided"},
		{"the part's timestamp TEXT", androidgooglemessages.Parser{}, joinOpts{}, with("timestamp", text), "not-joined", "undecided"},
		{"the part's timestamp NULL", androidgooglemessages.Parser{}, joinOpts{}, with("timestamp", null), "not-joined", "undecided"},
		{"the live conversation_id TEXT", androidgooglemessages.Parser{}, joinOpts{message: map[string]any{"conversation_id": "x"}}, p52, "not-joined", "undecided"},
		{"the live received_timestamp TEXT", androidgooglemessages.Parser{}, joinOpts{message: map[string]any{"received_timestamp": "x"}}, p52, "not-joined", "undecided"},
		{"a damaged messages Get", corrupt, joinOpts{}, p52, "not-joined", "undecided"},
	} {
		r := mapOn(t, c.p, joinStore(t, c.o), c.rr)
		if j, why := partJoinOf(r); j != c.join || why != c.reason {
			t.Errorf("%s: part_join %q reason %q, want %q %q", c.name, j, why, c.join, c.reason)
		}
	}

	// M1: a budget failure of the basis lookup is the mapping's error, never a basis
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Files: joinStore(t, joinOpts{})}.Bundle(h, remoteApp)
	if got := parsertest.MapRecoveredRow(t, h, faultParser{matchErr: parse.ErrBudget}, b, p52); !errors.Is(got.Err, parse.ErrBudget) {
		t.Errorf("a budget failure of the sqlite_master lookup: err %v, unmapped %q, %d records, want the budget error", got.Err, got.Unmapped, len(got.Records))
	}

	// an undecidable schema answer: every part of the run is undecided
	und := typedRun(t, undecidedSchema{}, fixtureFiles(t, "gm-deleted"))
	n := 0
	for _, idx := range recoveredParts(und) {
		for _, i := range idx {
			n++
			if j, why := partJoinOf(und.recs[i]); j != "not-joined" || why != "undecided" {
				t.Errorf("%s: part_join %q reason %q with an undecidable sqlite_master, want not-joined undecided", und.facts[i].Locator, j, why)
			}
		}
	}
	if n == 0 {
		t.Error("the undecided run mapped no part: the check proves nothing")
	}

	for _, c := range []struct {
		sql  string
		want bool
	}{
		{"CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id INT)", true},
		{"create table messages (_id integer primary key autoincrement, conversation_id int)", true},
		{"CREATE TABLE messages (_id INTEGER PRIMARY KEY, x TEXT DEFAULT 'AUTOINCREMENT')", false},
		{"CREATE TABLE messages (x INT, \"_id\" INTEGER PRIMARY KEY AUTOINCREMENT)", true},
		{"CREATE TABLE messages (_id INTEGER, x TEXT CHECK (x <> 'a, _id PRIMARY KEY AUTOINCREMENT'))", false},
		{"CREATE TABLE messages (_ID INTEGER PRIMARY KEY DEFAULT 'AUTOINCREMENT')", false},
		// M2: a comment is no word of a definition and its comma does not split
		{"CREATE TABLE messages (_id INTEGER PRIMARY KEY -- AUTOINCREMENT\n, x INT)", false},
		{"CREATE TABLE messages (x INT /* AUTOINCREMENT */, _id INTEGER PRIMARY KEY)", false},
		{"CREATE TABLE messages /* ( */ (/* a, b */ _id INTEGER PRIMARY KEY AUTOINCREMENT, x INT)", true},
	} {
		if got := androidgooglemessages.AutoincrementID(c.sql); got != c.want {
			t.Errorf("autoincrementID(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
	// I2: the rowid alias of a join table, read from its sql
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{"CREATE TABLE participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, sub_id INT)", true},
		{"CREATE TABLE participants (_id integer primary key, sub_id INT)", true},
		{"CREATE TABLE participants (_id INT PRIMARY KEY, sub_id INT)", false},
		{"CREATE TABLE participants (_id INTEGER PRIMARY KEY DESC, sub_id INT)", false},
		{"CREATE TABLE participants (_id INTEGER PRIMARY KEY, sub_id INT) WITHOUT ROWID", false},
		{"CREATE TABLE participants (_id INTEGER, sub_id INT, PRIMARY KEY (_id))", false}, // not read: guarded, never trusted
	} {
		if got := androidgooglemessages.RowidAliasID(c.sql); got != c.want {
			t.Errorf("rowidAliasID(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

// noteSink keeps the notes of a mapping context.
type noteSink map[string]string

func (n noteSink) Note(k, v string) { n[k] = v }

// mapWithNotes maps a recovered parts row as MapRecoveredRow does, against the live view of files at the app path, and
// returns its one record with the notes the mapping made.
func mapWithNotes(t *testing.T, files map[string][]byte, rr sqlitefile.RecoveredRow) (records.Record, map[string]string) {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidgooglemessages.Parser{}
	in := parsertest.Fixture{Files: files}.Bundle(h, remoteApp).Input(h, p, false)
	f := sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}
	if a, ok := in.Artifacts["wal"]; ok {
		f.WAL, f.WALSize = a.R, a.Size
	}
	db, err := sqlitedb.Open(t.Context(), f, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tbl, err := db.Table(t.Context(), "parts", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := sqlitedb.FromRecovered(tbl, rr)
	if err != nil {
		t.Fatal(err)
	}
	defer row.Release()
	notes := noteSink{}
	mc := sqlitedb.NewContext(t.Context(), db, in, notes)
	defer mc.Close()
	recs, err := p.Mappings()[1].Map(mc, row)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recovered part: err %v, %d records", err, len(recs))
	}
	return recs[0], notes
}

// B1a (I1): a joined part copies the raw that qualifies its copied parties: an outgoing part's members_basis current and
// members_total, its sender_is_self; a conversation over 1,000 members gives members_cut and the cut is stated (the note
// members_cut.<conversation>, the typed pass's statement of the live pass's counted warning), never silent. Nothing of the
// thread, status or times is copied.
func TestGoogleMessagesJoinedPartCopiesItsPartyRaw(t *testing.T) {
	p52 := recoveredRow(t, "gm-deleted", "parts", 52)
	r, notes := mapWithNotes(t, joinStore(t, joinOpts{}), p52)
	raw := rawOf(r)
	if raw["members_basis"] != "current" || raw["members_total"] != int64(1) || raw["sender_is_self"] != true || raw["part_join"] != "joined" {
		t.Errorf("joined part 52: members_basis %v members_total %v sender_is_self %v part_join %v", raw["members_basis"], raw["members_total"], raw["sender_is_self"], raw["part_join"])
	}
	for _, k := range []string{"members_cut", "sms_thread_id", "group_state", "self_sub_id", "self_id", "sender_id", "label_message_status", "time_basis", "received_timestamp_status"} {
		if v, ok := raw[k]; ok {
			t.Errorf("joined part 52: raw.%s = %v: B1a copies only the raw of the parties", k, v)
		}
	}
	if len(notes) != 0 {
		t.Errorf("joined part 52 without a cut: notes %v", notes)
	}
	r, notes = mapWithNotes(t, joinStore(t, joinOpts{members: 1001}), p52)
	raw = rawOf(r)
	if raw["members_cut"] != true || raw["members_total"] != int64(1001) || len(rolesOf(r)) != 1000 {
		t.Errorf("a 1,001-member conversation: members_cut %v members_total %v %d participants", raw["members_cut"], raw["members_total"], len(rolesOf(r)))
	}
	if got := notes["members_cut.1"]; got != "googlemessages-members-cut: conversation 1: 1001 members, 1000 kept" {
		t.Errorf("the cut is not stated: note members_cut.1 = %q (notes %v)", got, notes)
	}
}

// I2: a join table whose _id is not the rowid alias (participants._id INT PRIMARY KEY, beside a -wal companion, so Probe
// deferred) is guarded in the typed pass: a joined part's party joins through it are undecided (join_undecided, participants
// unknown), never attached by rowid, and the guard is noted.
func TestGoogleMessagesTypedPassGuardsTheJoinTables(t *testing.T) {
	p52 := recoveredRow(t, "gm-deleted", "parts", 52)
	r, notes := mapWithNotes(t, joinStore(t, joinOpts{plainPartic: true}), p52)
	raw := rawOf(r)
	ju, _ := raw["join_undecided"].([]any)
	if raw["part_join"] != "joined" || !slices.Contains(ju, any("participants")) || r.Payload["participants_unknown"] != true || len(rolesOf(r)) != 0 {
		t.Errorf("guarded participants: part_join %v join_undecided %v participants %v (unknown %v)", raw["part_join"], raw["join_undecided"], rolesOf(r), r.Payload["participants_unknown"])
	}
	if notes["join_undecided.participants"] != "column _id is not the rowid alias" {
		t.Errorf("the guard is not noted: %v", notes)
	}
}

// rebuiltPart is gm-rebuilt's old part rowid as the raw history holds it, with the table named parts by the test. The host
// names no table for it (T8a): its freed page belongs to no live object and a 9-value parts record also fits messages
// strictly, so the fit is ambiguous; the typed pass never delivers it and it stays a generic row. The test names the table
// only to map the real rebuilt row through the parts mapping, where the C5a reasons are pinned.
func rebuiltPart(t *testing.T, rowid int64) sqlitefile.RecoveredRow {
	t.Helper()
	f := fixtureFiles(t, "gm-rebuilt")
	db, err := sqlitefile.Open(bytes.NewReader(f["db"]), int64(len(f["db"])), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := db.History()
	defer h.Release()
	var out sqlitefile.RecoveredRow
	found := false
	if _, err := h.Rows(t.Context(), func(r sqlitefile.RecoveredRow) bool {
		if r.Rowid == nil || *r.Rowid != rowid || len(r.Values) != 9 {
			return true
		}
		if r.Table != "" || r.TableBasis != sqlitefile.BasisNone {
			t.Errorf("gm-rebuilt part %d: the host names table %q (basis %s): the ambiguous fit names nothing", rowid, r.Table, r.TableBasis)
		}
		out, found = r, true
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("gm-rebuilt: no history row %d", rowid)
	}
	out.Values = slices.Clone(out.Values)
	for i := range out.Values {
		out.Values[i].Bytes = slices.Clone(out.Values[i].Bytes)
	}
	out.Table = "parts"
	return out
}

// Spec 8.3 condition 3 (C5a), T8a: after a rebuild that restarted the AUTOINCREMENT sequence, gm-rebuilt's old part 60
// names a live message _id 1 of another conversation and part 61 a live _id 2 of another time. The host never types them
// (an ambiguous fit names nothing: no typed record of a parts row in the whole typed pass); mapped through the parts
// mapping against the live view, neither joins and the reason is the condition that failed.
func TestGoogleMessagesRebuiltPartIsNotJoined(t *testing.T) {
	res := typedRun(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-rebuilt"))
	if n := len(recoveredParts(res)); n != 0 || res.notes["recovered_rows.parts"] != "0" {
		t.Errorf("gm-rebuilt: %d typed parts records, note recovered_rows.parts %q: the old parts must stay generic rows", n, res.notes["recovered_rows.parts"])
	}
	for id, why := range map[int64]string{60: "conversation-differs", 61: "timestamp-differs"} {
		r := mapOn(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-rebuilt"), rebuiltPart(t, id))
		if j, got := partJoinOf(r); j != "not-joined" || got != why {
			t.Errorf("part %d: part_join %q reason %q, want not-joined %q", id, j, got, why)
		}
		if str(r, "channel") != "unknown" || str(r, "direction") != "unknown" || r.Payload["participants_unknown"] != true {
			t.Errorf("part %d: channel %q direction %q participants_unknown %v: a part that is not joined copies nothing", id, str(r, "channel"), str(r, "direction"), r.Payload["participants_unknown"])
		}
	}
}

// canonical is a record without its artifact fields (artifact, source path, locator, range).
func canonical(t *testing.T, r records.Record) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": r.Type, "time": r.Time, "times": r.Times, "body": r.Body, "summary": r.Summary, "payload": r.Payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// B1 (spec 8.2): a recovered part that joins is its own record, never merged: part 52 of gm-deleted states its own text and
// copies only the channel, the direction and the participants of the live M2, with part_rowid and joined_message_rowid,
// no guid, no Time and no status. The live M2 is unchanged: canonically equal to gm-aosp-v2's M2.
func TestGoogleMessagesRecoveredJoinedPartIsItsOwnRecord(t *testing.T) {
	r := mapOn(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-deleted"), recoveredRow(t, "gm-deleted", "parts", 52))
	_, live := fixtureRecords(t, "gm-deleted")
	m2, ok := live[2]
	if !ok {
		t.Fatal("gm-deleted: M2 is not live")
	}
	raw := rawOf(r)
	if r.Body != "deleted second part" || r.Payload["text_source"] != "part" {
		t.Errorf("part 52: Body %q text_source %v, want its own text", r.Body, r.Payload["text_source"])
	}
	if str(r, "channel") != "sms" || str(r, "direction") != "out" {
		t.Errorf("part 52: channel %q direction %q, want M2's sms out", str(r, "channel"), str(r, "direction"))
	}
	if got, want := jsonText(r.Payload["participants"]), jsonText(m2.Payload["participants"]); got != want || !slices.Equal(rolesOf(r), []string{"member 5550000002"}) {
		t.Errorf("part 52: participants %s, want M2's %s = [member 5550000002]", got, want)
	}
	if raw["part_rowid"] != int64(52) || raw["joined_message_rowid"] != int64(2) || raw["part_join"] != "joined" {
		t.Errorf("part 52: part_rowid %v joined_message_rowid %v part_join %v", raw["part_rowid"], raw["joined_message_rowid"], raw["part_join"])
	}
	if r.Time != nil || len(r.Times) != 0 || r.Payload["status"] != nil || r.Payload["guid"] != nil || r.Payload["subscription"] != nil || r.Payload["read"] != nil {
		t.Errorf("part 52: Time %v Times %v status %v guid %v subscription %v read %v: B1 copies only channel, direction and participants",
			r.Time, r.Times, r.Payload["status"], r.Payload["guid"], r.Payload["subscription"], r.Payload["read"])
	}
	if th := threadMap(r); !reflect.DeepEqual(th, map[string]any{"id": "1"}) {
		t.Errorf("part 52: thread %v, want {id 1} from its own conversation_id", th)
	}
	_, base := fixtureRecords(t, "gm-aosp-v2")
	if got, want := canonical(t, m2), canonical(t, base[2]); got != want {
		t.Errorf("the live M2 of gm-deleted differs from gm-aosp-v2's M2 apart from artifact fields:\n got %s\nwant %s", got, want)
	}
}

// Review Focus 5 (spec 8.2, B1): the older image of M1's part 1 ("Hello from Ann (old)", superseded by an UPDATE in the WAL)
// is a typed record of its own, joined to M1 (the part's _id, message, conversation and timestamp are M1's), and the host
// gives it the relation superseded-version; the mapping states no deletion.
func TestGoogleMessagesSupersededPartImageJoins(t *testing.T) {
	res := typedRun(t, androidgooglemessages.Parser{}, fixtureFiles(t, "gm-deleted"))
	n := 0
	for _, i := range recoveredParts(res)[1] {
		r, f := res.recs[i], res.facts[i]
		if r.Body != "Hello from Ann (old)" {
			continue
		}
		n++
		if j, why := partJoinOf(r); j != "joined" || why != "" || rawOf(r)["joined_message_rowid"] != int64(1) {
			t.Errorf("%s: part_join %q reason %q joined_message_rowid %v, want joined to 1", f.Locator, j, why, rawOf(r)["joined_message_rowid"])
		}
		if f.Relation != "superseded-version" {
			t.Errorf("%s: relation %q, want superseded-version", f.Locator, f.Relation)
		}
		if str(r, "direction") != "in" || !slices.Equal(rolesOf(r), []string{"from 5550000002"}) {
			t.Errorf("%s: direction %q participants %v, want M1's in and from 5550000002", f.Locator, str(r, "direction"), rolesOf(r))
		}
		if _, del := r.Payload["deleted"]; r.Deleted || del || r.Recovery != "" {
			t.Errorf("%s: Deleted %v payload.deleted %v Recovery %q: the mapping states no deletion", f.Locator, r.Deleted, del, r.Recovery)
		}
	}
	if n == 0 {
		t.Fatal("gm-deleted: no typed record of the older image of part 1")
	}
}

// jsonText is v as JSON (a nil slice and an empty one differ: null and []).
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unencodable: " + err.Error()
	}
	return string(b)
}

// faultParser is the parser whose mapping context fails as told: get[table] is every Get of that table's error; matchErr
// is every Match's error; twiceMaster answers the sqlite_master lookup of that name with its rows twice.
type faultParser struct {
	androidgooglemessages.Parser
	get         map[string]error
	matchErr    error
	twiceMaster string
}

func (p faultParser) Mappings() []parse.TableMapping {
	ms := p.Parser.Mappings()
	for i := range ms {
		inner := ms[i].Map
		ms[i].Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			return inner(faultCtx{MapContext: m, p: p}, row)
		}
	}
	return ms
}

type faultCtx struct {
	parse.MapContext
	p faultParser
}

func (c faultCtx) Get(table string, rowid int64) (parse.Row, bool, error) {
	if err := c.p.get[table]; err != nil {
		return nil, false, err
	}
	return c.MapContext.Get(table, rowid)
}

func (c faultCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	if c.p.matchErr != nil {
		return nil, c.p.matchErr
	}
	rows, err := c.MapContext.Match(table, col, key)
	if err == nil && c.p.twiceMaster != "" && strings.EqualFold(table, "sqlite_master") && key.S == c.p.twiceMaster {
		rows = append(rows, rows...)
	}
	return rows, err
}
