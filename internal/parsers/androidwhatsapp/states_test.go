package androidwhatsapp_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The in-test databases of the state vocabulary (spec 4I-R22) and of the review fixes: built with the test builder, so
// every state is pinned on a row the test itself wrote, never on the parser's own output.

var baseDDL = map[string]string{
	"message": "create table message(_id integer primary key, chat_row_id integer, from_me integer, key_id text, " +
		"sender_jid_row_id integer, status integer, timestamp integer, received_timestamp integer, message_type integer, text_data text)",
	"chat":            "create table chat(_id integer primary key, jid_row_id integer, subject text)",
	"jid":             "create table jid(_id integer primary key, user text, server text, agent integer, device integer, raw_string text)",
	"jid_map":         "create table jid_map(lid_row_id integer primary key, jid_row_id integer)",
	"message_vcard":   "create table message_vcard(_id integer primary key, message_row_id integer, vcard text)",
	"message_revoked": "create table message_revoked(message_row_id integer primary key, revoked_key_id text, admin_jid_row_id integer, revoke_timestamp integer)",
	"message_add_on": "create table message_add_on(_id integer primary key, chat_row_id integer, from_me integer, key_id text, " +
		"sender_jid_row_id integer, parent_message_row_id integer, timestamp integer, message_add_on_type integer)",
	"message_add_on_reaction": "create table message_add_on_reaction(message_add_on_row_id integer primary key, reaction text, sender_timestamp integer)",
	"call_log": "create table call_log(_id integer primary key, jid_row_id integer, from_me integer, call_id text, timestamp integer, " +
		"video_call integer, duration integer, call_result integer, group_jid_row_id integer)",
	"call_log_participant_v2": "create table call_log_participant_v2(_id integer primary key, call_log_row_id integer, jid_row_id integer, call_result integer)",
	"deleted_chat_job": "create table deleted_chat_job(_id integer primary key, chat_row_id, deleted_message_row_id integer, " +
		"deleted_starred_message_row_id integer, deleted_message_categories text, singular_message_delete_rows_id text)",
}

var baseOrder = []string{"message", "chat", "jid", "jid_map", "message_vcard", "message_revoked", "message_add_on", "message_add_on_reaction", "call_log", "call_log_participant_v2", "deleted_chat_job"}

type tables map[string]*sqlitetest.Table

// buildDB builds a modern store from baseDDL with the given overrides (an empty override drops the table).
func buildDB(over map[string]string, fill func(tb tables)) (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := tables{}
	for _, name := range baseOrder {
		ddl := baseDDL[name]
		if o, ok := over[name]; ok {
			ddl = o
		}
		if ddl != "" {
			tb[name] = b.CreateTable(name, ddl)
		}
	}
	fill(tb)
	return b.Bytes(), b.NewWAL(false, 1, 2, 0).Bytes()
}

func runBytes(t *testing.T, db, wal []byte, remote string) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	return parseFixture(t, parsertest.Fixture{Name: "inline", Files: files}, remote)
}

func byRowid(t *testing.T, res parsertest.Result) map[string]records.Record {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	out := map[string]records.Record{}
	for _, r := range res.Records {
		out[keyOf(r)] = r
	}
	return out
}

func msgRow(tb tables, id, chat, fromMe int64, sender any, typ int64, ts int64, text any) {
	tb["message"].Insert(id, nil, chat, fromMe, "K"+string(rune('A'+id)), sender, int64(0), ts, nil, typ, text)
}

const goodTS = int64(1700000000000)

func jidRow(tb tables, id int64, user, server, raw any) {
	tb["jid"].Insert(id, nil, user, server, nil, nil, raw)
}

// 4I-R22: every raw state key and value the parser emits, pinned on a row of a database written here with hard-coded
// expectations. The vocabulary is the coordinator's; this test is its oracle.
func TestWhatsAppStateVocabularyIsPinned(t *testing.T) {
	long := strings.Repeat("x", 1100) + "@s.whatsapp.net"
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		jidRow(tb, 2, "120363000000000001", "g.us", "120363000000000001@g.us")
		jidRow(tb, 3, "15550000009", "s.whatsapp.net", nil) // no raw_string
		jidRow(tb, 4, "100000000000001", "lid", "100000000000001@lid")
		jidRow(tb, 5, "15550000005", "s.whatsapp.net", "15550000005@s.whatsapp.net")
		jidRow(tb, 6, "x", "s.whatsapp.net", long)
		jidRow(tb, 7, "100000000000002", "lid", "100000000000002@lid")
		jidRow(tb, 8, "100000000000003", "lid", "100000000000003@lid")
		jidRow(tb, 9, "100000000000004", "lid", "100000000000004@lid")
		for i, j := range []int64{1, 2, 3, 4, 99, 6, 7, 8, 9} {
			tb["chat"].Insert(int64(i+1), nil, j, nil)
		}
		tb["jid_map"].Insert(7, nil, int64(99)) // the target jid row does not exist
		tb["jid_map"].Insert(8, nil, int64(2))  // the target is a group, not a phone
		tb["jid_map"].Insert(9, nil, int64(5))  // a phone
		msgRow(tb, 1, 50, 0, nil, 0, goodTS, "no chat row")
		msgRow(tb, 2, 5, 0, nil, 0, goodTS, "chat without jid row")
		msgRow(tb, 3, 6, 0, nil, 0, goodTS, "too long")
		msgRow(tb, 4, 2, 0, int64(99), 0, goodTS, "sender has no jid row")
		msgRow(tb, 5, 2, 0, int64(3), 0, goodTS, "sender has no raw string")
		msgRow(tb, 6, 2, 0, int64(0), 0, goodTS, "sender absent")
		msgRow(tb, 7, 7, 0, nil, 0, goodTS, "lid, target missing")
		msgRow(tb, 8, 8, 0, nil, 0, goodTS, "lid, target a group")
		msgRow(tb, 9, 9, 0, nil, 0, goodTS, "lid, phone")
		msgRow(tb, 10, 1, 0, nil, 4, goodTS, "vcard without a row")
		msgRow(tb, 11, 1, 0, nil, 4, goodTS, "vcard unreadable")
		tb["message_vcard"].Insert(1, nil, int64(11), int64(7)) // an INTEGER in the text column
		msgRow(tb, 12, 1, 0, nil, 0, int64(0), "sentinel time")
		msgRow(tb, 13, 1, 0, nil, 0, int64(5), "invalid time")
		msgRow(tb, 14, 3, 0, nil, 0, goodTS, "chat jid has no raw string")
		msgRow(tb, 15, 1, 0, nil, 4, goodTS, "vcard stored NULL")
		tb["message_vcard"].Insert(2, nil, int64(15), nil)
	})
	res := runBytes(t, db, nil, remoteLive)
	recs := byRowid(t, res)
	raw := func(id string) map[string]any { return rawMap(t, recs["message/"+id]) }

	for id, want := range map[string]string{"1": "no-chat-row", "2": "no-jid-row", "3": "too-long"} {
		if got := raw(id)["chat_state"]; got != want {
			t.Errorf("message %s: chat_state %v, want %s", id, got, want)
		}
	}
	for id, want := range map[string]string{"4": "no-jid-row", "5": "no-raw-string", "6": "absent"} {
		if got := raw(id)["sender_state"]; got != want {
			t.Errorf("message %s: sender_state %v, want %s", id, got, want)
		}
	}
	if got := raw("7")["lid_unresolved"]; !reflect.DeepEqual(got, []any{"100000000000002@lid"}) {
		t.Errorf("message 7: lid_unresolved %v (target jid row missing)", got)
	}
	if got := raw("9")["lid_map"]; !reflect.DeepEqual(got, []any{map[string]any{"lid": "100000000000004@lid", "pn": "15550000005@s.whatsapp.net", "source": "jid_map"}}) {
		t.Errorf("message 9: lid_map %v", got)
	}
	if got := raw("10")["vcard_state"]; got != "no-message_vcard-row" {
		t.Errorf("message 10: vcard_state %v", got)
	}
	if got := raw("15")["vcard_state"]; got != "null-vcard" || raw("15")["unknown_columns"] != nil {
		t.Errorf("message 15: raw %v, want vcard_state null-vcard and no unknown_columns", raw("15"))
	}
	r11 := raw("11")
	if r11["vcard_state"] != "unreadable" || !reflect.DeepEqual(r11["unknown_columns"], map[string]any{"message_vcard.vcard": "type-mismatch"}) {
		t.Errorf("message 11: raw %v, want vcard_state unreadable and the column in unknown_columns", r11)
	}
	if raw("12")["timestamp_status"] != "sentinel" || raw("13")["timestamp_status"] != "invalid" {
		t.Errorf("timestamp_status %v and %v, want sentinel and invalid", raw("12")["timestamp_status"], raw("13")["timestamp_status"])
	}
	// a chat whose jid.raw_string is NULL: a thread with group set and no id, the parts in raw
	r14 := recs["message/14"]
	th := threadOf(r14)
	if g, has := th["group"]; !has || g != false {
		t.Errorf("message 14: thread %v, want group false", th)
	}
	if _, has := th["id"]; has {
		t.Errorf("message 14: thread %v has an id", th)
	}
	if got := raw("14")["chat_jid_parts"]; !reflect.DeepEqual(got, map[string]any{"user": "15550000009", "server": "s.whatsapp.net"}) {
		t.Errorf("message 14: chat_jid_parts %v", got)
	}
	if _, has := raw("14")["chat_jid"]; has {
		t.Errorf("message 14: chat_jid composed from the parts")
	}
	for k, r := range recs {
		if _, has := rawMap(t, r)["join_undecided"]; has {
			t.Errorf("%s: join_undecided %v on a clean database", k, rawMap(t, r)["join_undecided"])
		}
	}
}

// 4I-R22 on the fixtures: the unit state of a time and the LID statements.
func TestWhatsAppStateVocabularyOnFixtureRows(t *testing.T) {
	modern, _ := recordsOf(t, "android-modern-2026")
	rv := sub(t, modern["message/30"], "revoke")
	if rv["revoke_timestamp_unit"] != "unestablished" {
		t.Errorf("revoke unit %v", rv["revoke_timestamp_unit"])
	}
	if got := rawMap(t, modern["message/43"])["lid_unmapped"]; !reflect.DeepEqual(got, []any{"100000000000002@lid"}) {
		t.Errorf("message 43: lid_unmapped %v", got)
	}
	absent := 0
	for _, r := range modern {
		if rawMap(t, r)["sender_state"] == "absent" {
			absent++
		}
	}
	if absent != 5 {
		t.Errorf("%d fixture rows state sender_state absent, want 5", absent)
	}
}

// 4I-R22 legacy: chat_list_state, quoted_state and the empty chat.
func TestWhatsAppLegacyStatesArePinned(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	m := b.CreateTable("messages", "create table messages(_id integer primary key, key_remote_jid text, key_from_me integer, key_id text, status integer, "+
		"timestamp integer, media_wa_type text, data text, remote_resource text, quoted_row_id integer)")
	b.CreateTable("chat_list", "create table chat_list(_id integer primary key, key_remote_jid text, subject text)").Insert(1, nil, "other@s.whatsapp.net", "T")
	b.CreateTable("messages_quotes", "create table messages_quotes(_id integer primary key, key_id text, data text)")
	m.Insert(1, nil, "15550000001@s.whatsapp.net", int64(0), "L1", int64(0), goodTS, "0", "no chat_list row", nil, nil)
	m.Insert(2, nil, "other@s.whatsapp.net", int64(0), "L2", int64(0), goodTS, "0", "quote dangles", nil, int64(77))
	m.Insert(3, nil, "", int64(0), "L3", int64(0), goodTS, "0", "empty chat", nil, nil)
	res := runBytes(t, b.Bytes(), nil, remoteLive)
	recs := byRowid(t, res)
	if got := rawMap(t, recs["messages/1"])["chat_list_state"]; got != "no-chat_list-row" {
		t.Errorf("messages 1: chat_list_state %v", got)
	}
	if got := rawMap(t, recs["messages/2"])["quoted_state"]; got != "no-messages_quotes-row" {
		t.Errorf("messages 2: quoted_state %v", got)
	}
	if got := rawMap(t, recs["messages/3"])["chat_state"]; got != "empty" {
		t.Errorf("messages 3: chat_state %v", got)
	}
}

func joinUndecided(t *testing.T, r records.Record) []any {
	t.Helper()
	j, _ := rawMap(t, r)["join_undecided"].([]any)
	return j
}

func schemaWarnings(res parsertest.Result, table string) []parsertest.Warning {
	var out []parsertest.Warning
	for _, w := range res.Warnings {
		if w.Locator == "sqlite:schema" && strings.Contains(w.Reason, "table "+table) && strings.Contains(w.Reason, "whatsapp-join-undecided") {
			out = append(out, w)
		}
	}
	return out
}

// F1: with a -wal companion Probe defers its alias checks, so Parse must check them: a jid whose _id is a plain column
// is joined by rowid only by a lie. Every join through it is undecided, never a wrong row and never "absent".
func TestWhatsAppRowidAliasRecheckedUnderCompanion(t *testing.T) {
	db, wal := buildDB(map[string]string{
		"jid": "create table jid(_id integer, user text, server text, agent integer, device integer, raw_string text)",
	}, func(tb tables) {
		// the named key _id and the rowid disagree: the row with rowid 1 has _id 2, and the other way round
		tb["jid"].Insert(1, int64(2), "a", "s.whatsapp.net", nil, nil, "WRONG1@s.whatsapp.net")
		tb["jid"].Insert(2, int64(1), "b", "s.whatsapp.net", nil, nil, "WRONG2@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, int64(2), 0, goodTS, "one")
		msgRow(tb, 2, 1, 0, int64(2), 0, goodTS, "two")
	})
	res := runBytes(t, db, wal, remoteLive)
	recs := byRowid(t, res)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2 (the job stays complete)", len(recs))
	}
	for k, r := range recs {
		raw := rawMap(t, r)
		if !reflect.DeepEqual(joinUndecided(t, r), []any{"jid"}) {
			t.Errorf("%s: join_undecided %v, want [jid]", k, joinUndecided(t, r))
		}
		if raw["chat_jid"] != nil || raw["sender_jid"] != nil || strings.Contains(r.Summary, "WRONG") || len(partsOf(r)) != 0 {
			t.Errorf("%s: a wrong jid row was attached: raw %v participants %v summary %q", k, raw, partsOf(r), r.Summary)
		}
		if th := threadOf(r); th != nil && th["id"] != nil {
			t.Errorf("%s: thread %v", k, th)
		}
		if raw["sender_state"] != nil && raw["sender_state"] != "undecided" {
			t.Errorf("%s: sender_state %v", k, raw["sender_state"])
		}
	}
	if n := len(schemaWarnings(res, "jid")); n != 1 {
		t.Errorf("%d warnings naming table jid, want exactly 1: %v", n, res.Warnings)
	}
	if got := res.Notes["join_undecided.jid"]; got == "" {
		t.Errorf("notes %v lack join_undecided.jid", res.Notes)
	}
}

// F1: the primary table's key that is not the rowid alias is the typed unsupported-schema error.
func TestWhatsAppPrimaryTableWithoutRowidAliasIsUnsupportedSchema(t *testing.T) {
	db, wal := buildDB(map[string]string{
		"message": "create table message(_id integer, chat_row_id integer, from_me integer, key_id text, " +
			"sender_jid_row_id integer, status integer, timestamp integer, received_timestamp integer, message_type integer, text_data text)",
	}, func(tb tables) {
		tb["message"].Insert(1, int64(7), int64(1), int64(0), "K", nil, int64(0), goodTS, nil, int64(0), "x")
	})
	res := runBytes(t, db, wal, remoteLive)
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(res.Err, &us) || !errors.Is(res.Err, sqlitedb.ErrUnsupportedSchema) {
		t.Fatalf("Parse err = %v, want the typed unsupported-schema error", res.Err)
	}
	if len(res.Records) != 0 {
		t.Errorf("%d records from a table joined by a key that is not the rowid", len(res.Records))
	}
}

// F1 part 4: a join table that lacks the column the join needs is undecided with one warning, never a silent return.
func TestWhatsAppJoinTableLackingItsColumnIsUndecided(t *testing.T) {
	db, wal := buildDB(map[string]string{
		"jid_map":         "create table jid_map(lid_row_id integer primary key, other integer)",
		"message_revoked": "create table message_revoked(_id integer primary key, revoked_key_id text)",
	}, func(tb tables) {
		jidRow(tb, 1, "100000000000001", "lid", "100000000000001@lid")
		tb["chat"].Insert(1, nil, int64(1), nil)
		tb["jid_map"].Insert(1, nil, int64(1))
		tb["message_revoked"].Insert(1, nil, "K1")
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "one")
		msgRow(tb, 2, 1, 0, nil, 0, goodTS, "two")
	})
	res := runBytes(t, db, wal, remoteLive)
	recs := byRowid(t, res)
	for k, r := range recs {
		got := joinUndecided(t, r)
		if !reflect.DeepEqual(got, []any{"message_revoked", "jid_map"}) && !reflect.DeepEqual(got, []any{"jid_map", "message_revoked"}) {
			t.Errorf("%s: join_undecided %v, want jid_map and message_revoked", k, got)
		}
		raw := rawMap(t, r)
		if raw["lid_map"] != nil || raw["lid_unmapped"] != nil || raw["lid_unresolved"] != nil {
			t.Errorf("%s: a LID statement from an unusable jid_map: %v", k, raw)
		}
		if r.Deleted || raw["revoke"] != nil {
			t.Errorf("%s: a revoke from an unusable message_revoked", k)
		}
	}
	for _, tbl := range []string{"jid_map", "message_revoked"} {
		if n := len(schemaWarnings(res, tbl)); n != 1 {
			t.Errorf("%d warnings naming table %s, want 1: %v", n, tbl, res.Warnings)
		}
		if res.Notes["join_undecided."+tbl] == "" {
			t.Errorf("no note join_undecided.%s: %v", tbl, res.Notes)
		}
	}
}

// F3: a vCard cell that is not readable text is counted and stated; with some read the body is those; with none the
// state is unreadable, never "no rows" when rows exist.
func TestWhatsAppUnreadableVCardCellsAreCounted(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 4, goodTS, "fallback")
		msgRow(tb, 2, 1, 0, nil, 4, goodTS, "fallback")
		tb["message_vcard"].Insert(1, nil, int64(1), int64(5))
		tb["message_vcard"].Insert(2, nil, int64(2), "BEGIN:VCARD")
		tb["message_vcard"].Insert(3, nil, int64(2), int64(6))
	})
	recs := byRowid(t, runBytes(t, db, nil, remoteLive))
	r1, r2 := rawMap(t, recs["message/1"]), rawMap(t, recs["message/2"])
	if r1["vcard_state"] != "unreadable" || !reflect.DeepEqual(r1["unknown_columns"], map[string]any{"message_vcard.vcard": "type-mismatch"}) {
		t.Errorf("message 1: raw %v", r1)
	}
	if r2["vcard_state"] != nil || recs["message/2"].Body != "BEGIN:VCARD" || !reflect.DeepEqual(r2["unknown_columns"], map[string]any{"message_vcard.vcard": "type-mismatch"}) {
		t.Errorf("message 2: body %q raw %v, want the read card and the column in unknown_columns", recs["message/2"].Body, r2)
	}
}

// F4: jid_map's target gives a phone only when it classifies as a phone (user) address.
func TestWhatsAppLIDTargetMustBeAPhone(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "100000000000001", "lid", "100000000000001@lid")
		jidRow(tb, 2, "100000000000002", "lid", "100000000000002@lid")
		jidRow(tb, 3, "120363000000000001", "g.us", "120363000000000001@g.us")
		jidRow(tb, 4, "15550000005", "s.whatsapp.net", "15550000005@s.whatsapp.net")
		jidRow(tb, 5, "100000000000009", "lid", "100000000000009@lid")
		for i := int64(1); i <= 4; i++ {
			tb["chat"].Insert(i, nil, i, nil)
		}
		tb["jid_map"].Insert(1, nil, int64(3)) // a group
		tb["jid_map"].Insert(2, nil, int64(4)) // a phone
		tb["chat"].Insert(5, nil, int64(5), nil)
		tb["jid_map"].Insert(5, nil, int64(1)) // another LID
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, "to a group")
		msgRow(tb, 2, 2, 0, nil, 0, goodTS, "to a phone")
		msgRow(tb, 3, 5, 0, nil, 0, goodTS, "to a lid")
	})
	recs := byRowid(t, runBytes(t, db, nil, remoteLive))
	for _, id := range []string{"1", "3"} {
		raw := rawMap(t, recs["message/"+id])
		if raw["lid_map"] != nil || len(raw["lid_unresolved"].([]any)) != 1 {
			t.Errorf("message %s: raw %v, want lid_unresolved and no lid_map", id, raw)
		}
	}
	if got := rawMap(t, recs["message/2"])["lid_map"]; got == nil {
		t.Errorf("message 2: no lid_map for a phone target")
	}
}

// F5: a nil input, or one with no primary reader, is a typed error before anything is read.
func TestWhatsAppParseGuardsNilInput(t *testing.T) {
	p := androidwhatsapp.Parser{}
	if err := p.Parse(t.Context(), nil, nil); !errors.Is(err, rowrun.ErrNilInput) {
		t.Errorf("Parse(nil) = %v, want rowrun.ErrNilInput", err)
	}
	var in parse.Input
	if err := p.Parse(t.Context(), &in, nil); !errors.Is(err, rowrun.ErrNilInput) {
		t.Errorf("Parse(no primary) = %v, want rowrun.ErrNilInput", err)
	}
}

// 4I-R22 (review): a stored NULL vCard cell is a known value, counted apart from damage. All NULL gives null-vcard and
// nothing in unknown_columns; NULL beside a readable card emits the card; a missing column or a damaged cell is
// unreadable; the kinds of unknown_columns are joined, never overwritten.
func TestWhatsAppNullVCardIsAKnownState(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		for i := int64(1); i <= 4; i++ {
			msgRow(tb, i, 1, 0, nil, 4, goodTS, "fallback")
		}
		tb["message_vcard"].Insert(1, nil, int64(1), nil)
		tb["message_vcard"].Insert(2, nil, int64(1), nil)
		tb["message_vcard"].Insert(3, nil, int64(2), nil)
		tb["message_vcard"].Insert(4, nil, int64(2), "BEGIN:VCARD")
		tb["message_vcard"].Insert(5, nil, int64(3), int64(5))
		tb["message_vcard"].Insert(6, nil, int64(3), nil)
		tb["message_vcard"].Insert(7, nil, int64(4), int64(5))
		tb["message_vcard"].Insert(8, nil, int64(4), 2.5)
	})
	recs := byRowid(t, runBytes(t, db, nil, remoteLive))
	r1, r2, r3, r4 := rawMap(t, recs["message/1"]), rawMap(t, recs["message/2"]), rawMap(t, recs["message/3"]), rawMap(t, recs["message/4"])
	if r1["vcard_state"] != "null-vcard" || r1["unknown_columns"] != nil {
		t.Errorf("all-NULL cards: raw %v, want vcard_state null-vcard and no unknown_columns", r1)
	}
	if r2["vcard_state"] != nil || recs["message/2"].Body != "BEGIN:VCARD" || r2["unknown_columns"] != nil {
		t.Errorf("NULL beside a card: body %q raw %v", recs["message/2"].Body, r2)
	}
	if r3["vcard_state"] != "unreadable" || !reflect.DeepEqual(r3["unknown_columns"], map[string]any{"message_vcard.vcard": "type-mismatch"}) {
		t.Errorf("damaged beside NULL: raw %v, want unreadable and type-mismatch only", r3)
	}
	if r4["vcard_state"] != "unreadable" || !reflect.DeepEqual(r4["unknown_columns"], map[string]any{"message_vcard.vcard": "type-mismatch"}) {
		t.Errorf("two damaged cells: raw %v", r4)
	}
}

// A message_vcard table without its vcard column: the cards are unreadable, stated, never "no rows".
func TestWhatsAppMissingVCardColumnIsUnreadable(t *testing.T) {
	db, _ := buildDB(map[string]string{"message_vcard": "create table message_vcard(_id integer primary key, message_row_id integer)"}, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 4, goodTS, "fallback")
		tb["message_vcard"].Insert(1, nil, int64(1))
	})
	r := rawMap(t, byRowid(t, runBytes(t, db, nil, remoteLive))["message/1"])
	if r["vcard_state"] != "unreadable" || !reflect.DeepEqual(r["unknown_columns"], map[string]any{"message_vcard.vcard": "missing"}) {
		t.Errorf("raw %v, want unreadable and missing", r)
	}
}
