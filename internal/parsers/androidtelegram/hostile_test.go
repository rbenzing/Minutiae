package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The hostile rows of spec 12 (plan 4I4 Task 13). Each test builds its input in Go (or reads a committed fixture),
// runs the parser through the harness and requires: no panic, no invented value, and a stated warning or raw marker.
// A bound is a typed error, a stated state or a counted warning, never a silent cut.

// hostileOK says err is a documented answer to hostile input: a typed decoder error or one of the parser's own.
func hostileOK(err error) bool {
	if err == nil || parsertest.HostileOK(err) {
		return true
	}
	for _, typed := range []error{androidtelegram.ErrNoMessageTable, androidtelegram.ErrSchemaIncomplete, androidtelegram.ErrUnsupportedVersion} {
		if errors.Is(err, typed) {
			return true
		}
	}
	return false
}

func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// padTo is b followed by zero bytes up to n bytes.
func padTo(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

// Spec 8.4, 12, call 4, ruling T7: every telegram-hostile row is a record whose data cell is stated through the whole
// parse: a blob by its state (the vocabulary of spec 8.4), length and SHA-256 of the stored bytes, and after a stop
// its kind; a data cell that is not a blob (H text in a blob column) by unknown_columns with no data_state. A blob
// above tl.MaxBlob (17 MiB, Go-built) is cap before any read, with its length and SHA-256, and still a record from its
// plain columns; a blob of exactly tl.MaxBlob is read by its content (here a whole message, then an unparsed
// trailer), never cap.
func TestTelegramHostileBlobsAreStated(t *testing.T) {
	res := parseFixture(t, "telegram-hostile")
	if len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("violations %v, refused %d", res.Violations, res.Refused)
	}
	blobs := fixtureBlobs(t, "telegram-hostile", "messages_v2", "data")
	vocab := androidtelegram.BlobStates()
	recs := byRow(res.Records)
	for id := int64(2001); id <= 2026; id++ {
		key := "messages_v2/" + itoa(id)
		r, ok := recs[key]
		if !ok {
			t.Errorf("%s: no record", key)
			continue
		}
		raw := rawOf(r)
		unk, _ := raw["unknown_columns"].(map[string]any)
		b, isBlob := blobs[id]
		if !isBlob {
			if unk["messages_v2.data"] == nil || raw["data_state"] != nil || raw["data_len"] != nil {
				t.Errorf("%s: a data cell that is not a blob: unknown_columns %v, data_state %v", key, unk, raw["data_state"])
			}
			continue
		}
		st, _ := raw["data_state"].(string)
		if !slices.Contains(vocab, st) {
			t.Errorf("%s: data_state %q is not in the vocabulary %v", key, st, vocab)
		}
		if jsonText(raw["data_len"]) != itoa(int64(len(b))) || raw["data_sha256"] != sha256Bytes(b) {
			t.Errorf("%s: data_len %v, data_sha256 %v; want %d and the stored bytes' digest", key, raw["data_len"], raw["data_sha256"], len(b))
		}
		if !strings.HasPrefix(st, "decoded") && raw["data_stop_kind"] == nil {
			t.Errorf("%s: state %q without data_stop_kind", key, st)
		}
	}

	hostileCutsAndCaps(t, recs, blobs)
	blobKeysCovered(t)

	// a blob above MaxBlob and one of exactly MaxBlob, in a Go-built database
	msg := func(id int32, text string) []byte {
		return tlMessage{id: id, from: idAnn, peer: idSelf, date: baseDate, text: text}.bytes()
	}
	over := padTo(msg(1, "over the cap"), tl.MaxBlob+1<<20)
	exact := padTo(msg(2, "exactly the cap"), tl.MaxBlob)
	db := goBuiltMessages(181, nil, []msgCells{{mid: 1, uid: idAnn, data: over}, {mid: 2, uid: idAnn, data: exact}})
	big := parseAt(t, remoteApp, dbOnly(db))
	if big.Err != nil || len(big.Violations) != 0 || len(big.Records) != 2 {
		t.Fatalf("Parse: err %v, violations %v, %d records", big.Err, big.Violations, len(big.Records))
	}
	got := byRow(big.Records)
	r1, r2 := got["messages_v2/1"], got["messages_v2/2"]
	raw := rawOf(r1)
	if raw["data_state"] != "cap" || raw["data_stop_kind"] != "cap" || jsonText(raw["data_len"]) != strconv.Itoa(len(over)) ||
		raw["data_sha256"] != sha256Bytes(over) || raw["data_constructor"] != "0x7600b9d3" {
		t.Errorf("17 MiB blob: raw %v; want cap with its length, SHA-256 and constructor", dataKeys(raw))
	}
	if r1.Body != "" || r1.Payload["kind"] != "unknown" || r1.Payload["guid"] != "1" || r1.Time == nil {
		t.Errorf("17 MiB blob: Body %q, kind %v, guid %v, time %v; want the plain columns only", r1.Body, r1.Payload["kind"], r1.Payload["guid"], r1.Time)
	}
	raw = rawOf(r2)
	if raw["data_state"] != "decoded" || raw["data_stop_kind"] != nil || jsonText(raw["data_len"]) != strconv.Itoa(tl.MaxBlob) {
		t.Errorf("a blob of exactly MaxBlob: raw %v; want decoded by its content", dataKeys(raw))
	}
	if r2.Body != "exactly the cap" || raw["trailer_state"] != "unparsed" {
		t.Errorf("a blob of exactly MaxBlob: Body %q, trailer_state %v; want the text and the zero tail unparsed", r2.Body, raw["trailer_state"])
	}
	if !hasWarning(big, "telegram-blob-undecoded: messages_v2.data: 1 records (cap:1)") {
		t.Errorf("no counted warning for the capped blob: %v", big.Warnings)
	}
}

// hasWarning says the run raised a warning with exactly this reason.
func hasWarning(res parsertest.Result, reason string) bool {
	for _, w := range res.Warnings {
		if w.Reason == reason {
			return true
		}
	}
	return false
}

// Spec 12, 4I2-A6: the telegram-hostile H cell rows. A NULL, REAL, NaN or TEXT uid, a NULL mid, a REAL date and a
// TEXT data cell are each listed in unknown_columns ("null" for a NULL key column, else "type-mismatch") and the
// field they give is absent with its state (thread_state or guid_state undecided, no record time, no data_state),
// never a stored NULL in raw.<col> and never a default; out 7 is kept as stored and gives direction unknown. Every
// other field of those rows is read (the blob decodes).
func TestTelegramHostileCellsAreUndecided(t *testing.T) {
	res := parseFixture(t, "telegram-hostile")
	recs := byRow(res.Records)
	type want struct {
		col, kind string
	}
	cells := map[int64]want{
		2017: {"uid", "null"}, 2018: {"uid", "type-mismatch"}, 2019: {"uid", "type-mismatch"}, 2020: {"uid", "type-mismatch"},
		2021: {"mid", "null"}, 2022: {"date", "type-mismatch"}, 2023: {"data", "type-mismatch"},
	}
	for id, w := range cells {
		key := "messages_v2/" + itoa(id)
		r, ok := recs[key]
		if !ok {
			t.Errorf("%s: no record", key)
			continue
		}
		raw := rawOf(r)
		unk, _ := raw["unknown_columns"].(map[string]any)
		if len(unk) != 1 || unk["messages_v2."+w.col] != w.kind {
			t.Errorf("%s: unknown_columns %v, want messages_v2.%s = %s only", key, unk, w.col, w.kind)
		}
		switch w.col {
		case "uid":
			if _, has := raw["uid"]; has || r.Payload["thread"] != nil || raw["thread_state"] != "undecided" || raw["dialog_class"] != "unknown" {
				t.Errorf("%s: raw.uid %v, thread %v, thread_state %v, dialog_class %v; want no uid, no thread, undecided", key, raw["uid"], r.Payload["thread"], raw["thread_state"], raw["dialog_class"])
			}
			if pu, _ := r.Payload["participants_unknown"].(bool); !pu && party(r, "from") == "" {
				t.Errorf("%s: no participant and none stated unknown", key)
			}
		case "mid":
			if _, has := raw["mid"]; has || r.Payload["guid"] != nil || raw["guid_state"] != "undecided" {
				t.Errorf("%s: raw.mid %v, guid %v, guid_state %v; want no mid, no guid, undecided", key, raw["mid"], r.Payload["guid"], raw["guid_state"])
			}
		case "date":
			if r.Time != nil || raw["date_status"] != nil || raw["date_conflict"] != nil {
				t.Errorf("%s: time %v, date_status %v, date_conflict %v; want no time and no conflict against an unread column", key, r.Time, raw["date_status"], raw["date_conflict"])
			}
		case "data":
			if raw["data_state"] != nil || r.Payload["kind"] != "unknown" || r.Body != "" {
				t.Errorf("%s: data_state %v, kind %v, Body %q; want nothing from an unread blob", key, raw["data_state"], r.Payload["kind"], r.Body)
			}
		}
		if w.col != "data" && (raw["data_state"] != "decoded" || r.Body != "Hello Ann here") {
			t.Errorf("%s: data_state %v, Body %q; the blob of the row is readable", key, raw["data_state"], r.Body)
		}
		for k, v := range raw {
			if v == nil && slices.Contains([]string{"mid", "uid", "date", "out", "data"}, k) {
				t.Errorf("%s: raw.%s is a stored NULL", key, k)
			}
		}
	}
	r := recs["messages_v2/2024"]
	if raw := rawOf(r); jsonText(raw["out"]) != "7" || r.Payload["direction"] != "unknown" || raw["unknown_columns"] != nil {
		t.Errorf("out 7: raw.out %v, direction %v, unknown_columns %v; want 7 as stored and direction unknown", raw["out"], r.Payload["direction"], raw["unknown_columns"])
	}
	if !strings.HasPrefix(res.Notes["secret_chats"], ">=") {
		t.Errorf("note secret_chats %q: rows with an unreadable uid make it a stated lower bound", res.Notes["secret_chats"])
	}
}

// Spec 12 (damage): a database cut inside messages_v2 states the pages it lost; a cut never gives an empty result that
// claims to be whole, and no record names a row the database did not hold (each record's guid and Body are those of
// one written row).
func TestTelegramTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 300
	text := func(i int64) string { return fmt.Sprintf("M%d %s", i, strings.Repeat("t", 200)) }
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	b.CreateTable("users", ddlUsers)
	tb := b.CreateTable("messages_v2", ddlV2Full)
	for i := int64(1); i <= rows; i++ {
		m := tlMessage{id: int32(i), from: idAnn, peer: idSelf, date: baseDate, text: text(i)}
		tb.Insert(i, msgCells{mid: i, uid: idAnn, data: m.bytes()}.v2Values()...)
	}
	full := b.Bytes()
	const ps = 4096
	pages, root := len(full)/ps, int(tb.Root())
	if pages-root < 5 {
		t.Fatalf("messages_v2 spans %d pages after its root: the scenario needs leaves to cut", pages-root)
	}
	stated := 0
	for cut := pages - 1; cut > root; cut-- {
		res := parseAt(t, remoteApp, dbOnly(slices.Clone(full[:cut*ps])))
		if !hostileOK(res.Err) {
			t.Fatalf("cut at page %d: untyped error %v", cut, res.Err)
		}
		if res.Refused != 0 || len(res.Violations) != 0 {
			t.Fatalf("cut at page %d: refused %d, violations %v", cut, res.Refused, res.Violations)
		}
		for _, r := range res.Records {
			id, err := strconv.ParseInt(fmt.Sprint(r.Payload["guid"]), 10, 64)
			if err != nil || id < 1 || id > rows || r.Body != text(id) {
				t.Fatalf("cut at page %d: record %s (guid %v) was not in the database", cut, r.Locator, r.Payload["guid"])
			}
		}
		if res.Err == nil && len(res.Records) < rows {
			if len(res.Warnings) == 0 {
				t.Errorf("cut at page %d: %d of %d records and no warning: a cut result is stated, never complete", cut, len(res.Records), rows)
			}
			stated++
		}
	}
	if stated == 0 {
		t.Error("no cut lost messages_v2 rows and parsed: the check proves nothing")
	}
}

// telegram-v181 (spec 13): R51, committed only in the WAL, is a record whose place names the WAL artifact; R52, in a
// frame the writer never committed, is no live record.
func TestTelegramWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "telegram-v181").Bundle(h, remoteApp)
	p := androidtelegram.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	recs := byRow(res.Records)
	r, ok := recs["messages_v2/51"]
	if !ok {
		t.Fatal("no record for R51")
	}
	if wal, ok := b.ByRole["wal"]; !ok || r.ArtifactID != wal.ID || !strings.Contains(r.Locator, "wal_frame=") {
		t.Errorf("R51: artifact %q, locator %q; want the WAL artifact", r.ArtifactID, r.Locator)
	}
	if r.Body != "in the WAL, committed" {
		t.Errorf("R51: Body %q", r.Body)
	}
	if _, live := recs["messages_v2/52"]; live {
		t.Error("R52 (an uncommitted WAL frame) is a live record")
	}
	for _, rec := range res.Records {
		if strings.Contains(rec.Body, "uncommitted R52") {
			t.Errorf("%s: the uncommitted R52 text is a live record", rec.Locator)
		}
	}
}

// Spec 12 (damage), 4I addendum 6.2: a chats table whose pages are damaged is the note chats = "unreadable", and every
// group message's chats join is undecided (raw.join_undecided chats, thread_title_state undecided), never
// "no-chats-row" and never a title.
func TestTelegramUnreadableJoinTableIsNotEmpty(t *testing.T) {
	var chats []builtRow
	for i := int64(1); i <= 200; i++ {
		chats = append(chats, builtRow{2000000 + i, map[string]any{"name": strings.Repeat("c", 100), "data": tlChat(2000000+i, "Group "+itoa(i))}})
	}
	out := tlMessage{flags: bitOut, id: 1, peerObj: peerChat(2000150), date: baseDate, text: "to the group"}
	v2 := []msgCells{{mid: 1, uid: -2000150, out: int64(1), read: int64(0), data: out.bytes()}, {mid: 2, uid: -2000001, out: int64(1), read: int64(0), data: out.bytes()}}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	b.CreateTable("users", ddlUsers)
	mt := b.CreateTable("messages_v2", ddlV2Full)
	for i, c := range v2 {
		mt.Insert(int64(i+1), c.v2Values()...)
	}
	ct := b.CreateTable("chats", ddlChats)
	for _, r := range chats {
		ct.Insert(r.rowid, r.values([]string{"uid", "name", "data"}, "uid")...)
	}
	db := b.Bytes()
	const ps = 4096
	root := int(ct.Root())
	if db[(root-1)*ps] != 0x05 {
		t.Fatalf("the chats root (page %d) is not an interior page (%#x): the scenario needs leaves to damage", root, db[(root-1)*ps])
	}
	db[(root-1)*ps] = 0x00 // not a b-tree page: the table cannot be read
	res := parseAt(t, remoteApp, dbOnly(db))
	if res.Err != nil || len(res.Violations) != 0 {
		t.Fatalf("Parse: err %v, violations %v", res.Err, res.Violations)
	}
	if res.Notes["chats"] != "unreadable" {
		t.Errorf("note chats = %q, want unreadable (notes %v)", res.Notes["chats"], res.Notes)
	}
	if _, has := res.Notes["rows.chats"]; has {
		t.Errorf("note rows.chats = %q: an unreadable table has no row count", res.Notes["rows.chats"])
	}
	recs := byRow(res.Records)
	for _, key := range []string{"messages_v2/1", "messages_v2/2"} {
		r, ok := recs[key]
		if !ok {
			t.Errorf("%s: no record", key)
			continue
		}
		raw := rawOf(r)
		if l, _ := raw["join_undecided"].([]any); !slices.Contains(l, any("chats")) {
			t.Errorf("%s: raw.join_undecided %v, want chats", key, raw["join_undecided"])
		}
		if raw["thread_title_state"] != "undecided" || threadOf(r)["title"] != nil {
			t.Errorf("%s: thread_title_state %v, title %v: an unreadable table is never empty", key, raw["thread_title_state"], threadOf(r)["title"])
		}
	}
	if !hasWarning(res, "telegram-join-undecided: chats: 2 records") {
		t.Errorf("no counted warning names the undecided chats join: %v", res.Warnings)
	}
}

// Spec 12 (text), C4: NUL and invalid UTF-8 in a message text, a user's names and a chat title are cleaned (U+FFFD)
// and flagged, never dropped: the Body with body_flags and the original bytes, a name part and a title named in
// raw.text_flags; RTL text and a bidi override are kept as stored.
func TestTelegramHostileTexts(t *testing.T) {
	const rtl = "\xe2\x80\xaeRTL \xd7\xa9\xd7\x9c\xd7\x95\xd7\x9d" // U+202E (right-to-left override), then Hebrew
	dirty := "a\x00b\xffc"
	in := tlMessage{id: 1, from: idAnn, peer: idSelf, date: baseDate, text: dirty}
	right := tlMessage{id: 2, from: idAnn, peer: idSelf, date: baseDate, text: rtl}
	group := tlMessage{flags: bitOut, id: 3, peerObj: peerChat(2000001), date: baseDate, text: "x"}
	v2 := []msgCells{
		{mid: 1, uid: idAnn, data: in.bytes()},
		{mid: 2, uid: idAnn, data: right.bytes()},
		{mid: 3, uid: -2000001, out: int64(1), read: int64(0), data: group.bytes()},
	}
	given, family := "G\x00n", "F\xfe"+rtl
	users := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid", rows: []builtRow{
		{idAnn, map[string]any{"name": "x", "status": int64(0), "data": tlUser(idAnn, 0, false, &given, &family, nil, nil)}},
	}}
	res := parseAt(t, remoteApp, dbOnly(goBuiltWith(181, v2, users, chatsTable(builtRow{2000001, map[string]any{"data": tlChat(2000001, "T\x00"+rtl)}}))))
	if res.Err != nil || len(res.Violations) != 0 || res.Refused != 0 {
		t.Fatalf("Parse: err %v, violations %v, refused %d", res.Err, res.Violations, res.Refused)
	}
	recs := byRow(res.Records)
	r1 := recs["messages_v2/1"]
	if r1.Body != "a�b�c" || !sameFlags(r1.Payload["body_flags"], "invalid_utf8", "nul") || r1.Payload["body_raw_b64"] == nil {
		t.Errorf("dirty text: Body %q, body_flags %v, body_raw_b64 %v", r1.Body, r1.Payload["body_flags"], r1.Payload["body_raw_b64"])
	}
	if r2 := recs["messages_v2/2"]; r2.Body != rtl || r2.Payload["body_flags"] != nil {
		t.Errorf("RTL text: Body %q, body_flags %v; want it as stored, unflagged", r2.Body, r2.Payload["body_flags"])
	}
	r3 := recs["messages_v2/3"]
	if title, _ := threadOf(r3)["title"].(string); title != "T�"+rtl || !strings.Contains(fmt.Sprint(rawOf(r3)["text_flags"]), "thread.title") {
		t.Errorf("dirty title: %q, text_flags %v", title, rawOf(r3)["text_flags"])
	}
	var c records.Record
	for _, r := range res.Records {
		if r.Type == "contact" {
			c = r
		}
	}
	names, _ := c.Payload["names"].(map[string]any)
	if names["given"] != "G�n" || names["family"] != "F�"+rtl {
		t.Errorf("dirty names: %v", names)
	}
	if flags := fmt.Sprint(rawOf(c)["text_flags"]); !strings.Contains(flags, "names.given") || !strings.Contains(flags, "names.family") {
		t.Errorf("dirty names: text_flags %q, want names.given and names.family", flags)
	}
	if !utf8Valid(c.Payload) || !utf8Valid(r1.Payload) || !utf8Valid(r3.Payload) {
		t.Error("a payload string is not valid UTF-8")
	}
}

// sameFlags says v is a list holding exactly want, in order.
func sameFlags(v any, want ...string) bool {
	var got []string
	switch l := v.(type) {
	case []string:
		got = l
	case []any:
		for _, x := range l {
			s, _ := x.(string)
			got = append(got, s)
		}
	}
	return slices.Equal(got, want)
}

// utf8Valid says every string in v (walked through maps and lists) is valid UTF-8 without NUL.
func utf8Valid(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ToValidUTF8(x, "") == x && !strings.ContainsRune(x, 0)
	case map[string]any:
		for _, e := range x {
			if !utf8Valid(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !utf8Valid(e) {
				return false
			}
		}
	}
	return true
}

// blobKey says the raw key states a value read from the blob's media or tail (not a plain column, nor the data_* and
// trailer statements about the cell itself).
func blobKey(k string) bool {
	for _, p := range []string{
		"media", "live_photo", "blob_forwards", "post_author", "quick_reply", "effect", "report_delivery", "paid_message",
		"schedule_repeat", "summary_from", "replies", "reactions", "restriction_reason", "factcheck", "suggested_post", "entities", "edit_date", "views",
	} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// notBlobKeys are the top-level raw keys of a message record that blobKey leaves out of the cut comparison, each on
// purpose (final-review item 2): the plain and side columns and their states, the data_*, trailer_* and other cells'
// statements about the cell itself, the join and record-level states, the 4I4 blob header keys outside the 4I4b media
// and tail (readBlobRaw, fwdRaw, the call and label mapping) and forwards_conflict, a relation of the blob to the
// forwards column (the cut rows carry another column value than R60), not a value read.
func notBlobKeys() []string {
	return []string{
		"account", "address_state", "attach_params", "attach_params_total", "attach_path", "attach_path_state", "blob_flags",
		"blob_flags2", "call_id", "chat_data_constructor", "chat_data_len", "chat_data_sha256", "chat_data_state",
		"counterparty_state", "custom_params_len", "custom_params_sha256", "data_constructor", "data_flags", "data_len",
		"data_sha256", "data_state", "data_stop_id", "data_stop_kind", "data_stop_offset", "data_stop_path", "data_undecided",
		"date", "date_conflict", "date_status", "dialog_basis", "dialog_class", "dialog_class_state", "direction_conflict",
		"duration", "enc_chat_id", "flags", "forwards", "forwards_conflict", "fwd", "fwd_msg_id", "group_id", "grouped_id",
		"guid_state", "id", "id_conflict", "imp", "is_channel", "label_constructor", "label_reason", "label_send_state",
		"live_key_state", "mention", "message_id", "message_id_basis", "mid", "mid_state", "out", "peer_id", "post",
		"read_state", "read_state_state", "reason", "reason_state", "reply_copy", "reply_to_message_id", "reply_to_msg_id",
		"reply_to_story_id", "replydata_constructor", "replydata_len", "replydata_sha256", "replydata_state", "secret_chat",
		"send_state", "sender_basis", "sender_state", "slug", "source_table", "thread_reply_id", "thread_state",
		"thread_title_state", "thread_user_state", "time_end_basis", "trailer_len", "trailer_sha256", "trailer_state", "ttl",
		"ttl_state", "uid", "unknown_columns", "video",
	}
}

// blobKeysCovered is final-review item 2's part of TestTelegramHostileBlobsAreStated: every top-level raw key a message
// record of any committed variant emits is either compared at the cut (blobKey) or listed in notBlobKeys, so a new
// media or tail key outside blobKey's prefixes fails here instead of going silently unchecked by hostileCutsAndCaps.
func blobKeysCovered(t *testing.T) {
	t.Helper()
	exempt := notBlobKeys()
	for _, v := range variants {
		for _, r := range parseAt(t, remoteApp, fixtureFiles(t, v)).Records {
			raw := rawOf(r)
			if st := raw["source_table"]; st != "messages" && st != "messages_v2" {
				continue
			}
			for k := range raw {
				if !blobKey(k) && !slices.Contains(exempt, k) {
					t.Errorf("%s %s: raw.%s is neither compared at the cut (blobKey) nor exempt (notBlobKeys)", v, r.Locator, k)
				}
			}
		}
	}
}

// hostileCutsAndCaps is plan 4I4b Task 7's part of TestTelegramHostileBlobsAreStated (spec 8.4, rulings P3, B1): every
// H2401/H2601 row is R76's or R60's blob cut at a boundary, so it is a record that is truncated or partial, whose stop
// lies within the cut, and that states no raw value the whole blob does not state identically (a field at or after the
// cut is never stated: a list the cut reaches is its own _state partial and none of its entries, never a shorter
// list); H2802 is cap, and H2803 and H2804 carry unknown-flag-bits.
func hostileCutsAndCaps(t *testing.T, recs map[string]records.Record, blobs map[int64][]byte) {
	t.Helper()
	full := byRow(parseFixture(t, "telegram-v181").Records)
	var diff func(path string, cut, whole map[string]any)
	diff = func(path string, cut, whole map[string]any) {
		for k, v := range cut {
			if path == "" && !blobKey(k) {
				continue // a plain column, or the cut's own statement about the cell
			}
			if path == "media." && k == "state" {
				continue // the stop's own statement about the media (mediaStop), not a value read
			}
			w, ok := whole[k]
			if cm, isMap := v.(map[string]any); isMap {
				wm, _ := w.(map[string]any)
				diff(path+k+".", cm, wm)
				continue
			}
			switch {
			case strings.HasSuffix(k, "_state") && v == "partial":
				if _, listed := cut[strings.TrimSuffix(k, "_state")]; listed {
					t.Errorf("%s%s partial but %s stated: a cut list shows no entry", path, k, strings.TrimSuffix(k, "_state"))
				}
			case strings.HasSuffix(k, "_state") && !ok:
				// a state the whole blob does not need (an unread name, an unreached list): it states the cut, not a value
			case !ok || jsonText(v) != jsonText(w):
				t.Errorf("%s%s = %v at the cut, %v whole: a value at or after the cut was stated", path, k, v, w)
			}
		}
	}
	cuts := 0
	for id := int64(2401); id < 2801; id++ {
		r, ok := recs["messages_v2/"+itoa(id)]
		if !ok {
			continue
		}
		cuts++
		whole := full["messages_v2/76"]
		if id >= 2601 {
			whole = full["messages_v2/60"]
		}
		raw := rawOf(r)
		if st := raw["data_state"]; st != "truncated" && st != "partial" {
			t.Errorf("H%d: data_state %v, want truncated or partial", id, st)
		}
		if off, err := strconv.Atoi(jsonText(raw["data_stop_offset"])); err != nil || off > len(blobs[id]) {
			t.Errorf("H%d: data_stop_offset %v beyond the cut at %d", id, raw["data_stop_offset"], len(blobs[id]))
		}
		diff("", raw, rawOf(whole))
	}
	if cuts != 68+51 {
		t.Errorf("%d cut rows, want %d", cuts, 68+51)
	}
	if r := rawOf(recs["messages_v2/2802"]); r["data_state"] != "cap" {
		t.Errorf("H2802: data_state %v, want cap", r["data_state"])
	}
	for _, id := range []string{"2803", "2804"} {
		if f, _ := rawOf(recs["messages_v2/"+id])["data_flags"].(string); !strings.Contains(f, "unknown-flag-bits") {
			t.Errorf("H%s: data_flags %q, want unknown-flag-bits", id, f)
		}
	}
}
