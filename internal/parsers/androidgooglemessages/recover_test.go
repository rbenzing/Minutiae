package androidgooglemessages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
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

// joinStore is a Go-built bugle_db holding the live message M2 of gm-deleted (outgoing, conversation 1, received at
// T+2000, the self P1 and the member Ann P2) and sqlite_sequence. auto false gives messages._id INTEGER PRIMARY KEY without
// AUTOINCREMENT; seqRow false deletes the messages row of sqlite_sequence after inserting it.
func joinStore(t *testing.T, auto, seqRow bool) map[string][]byte {
	t.Helper()
	s := store{
		participants:  []mrow{selfRow(1, 1), person(2, "5550000002")},
		conversations: []mrow{{1, map[string]any{}}},
		members:       memberRows(1, 2),
		messages:      []mrow{{2, map[string]any{"sender_id": int64(1), "message_status": int64(1), "conversation_id": int64(1), "received_timestamp": tT + 2000}}},
	}
	if !auto {
		s.ddlOf = func(table, ddl string) string {
			if table != "messages" {
				return ddl
			}
			out := strings.Replace(ddl, "_id INTEGER PRIMARY KEY AUTOINCREMENT", "_id INTEGER PRIMARY KEY", 1)
			if out == ddl {
				t.Fatalf("the messages DDL has no AUTOINCREMENT _id: %s", ddl)
			}
			return out
		}
	}
	b := s.builder(t)
	seq := b.CreateTable("sqlite_sequence", "CREATE TABLE sqlite_sequence(name,seq)")
	seq.Insert(1, "conversations", int64(1))
	seq.Insert(2, "messages", int64(2))
	seq.Insert(3, "participants", int64(2))
	if !seqRow {
		seq.Delete(2)
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

	// Go-built stores holding M2 as gm-deleted does: only the condition named fails
	p52 := recoveredRow(t, "gm-deleted", "parts", 52)
	for _, c := range []struct {
		name         string
		auto, seqRow bool
		join, reason string
	}{
		{"AUTOINCREMENT with its sqlite_sequence row", true, true, "joined", ""},
		{"INTEGER PRIMARY KEY without AUTOINCREMENT", false, true, "not-joined", "no-autoincrement"},
		{"AUTOINCREMENT with the sqlite_sequence row deleted", true, false, "not-joined", "no-sqlite-sequence"},
	} {
		r := mapOn(t, androidgooglemessages.Parser{}, joinStore(t, c.auto, c.seqRow), p52)
		if j, why := partJoinOf(r); j != c.join || why != c.reason {
			t.Errorf("%s: part_join %q reason %q, want %q %q", c.name, j, why, c.join, c.reason)
		}
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
	} {
		if got := androidgooglemessages.AutoincrementID(c.sql); got != c.want {
			t.Errorf("autoincrementID(%q) = %v, want %v", c.sql, got, c.want)
		}
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
