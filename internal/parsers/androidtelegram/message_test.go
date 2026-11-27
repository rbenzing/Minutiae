package androidtelegram_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// hostileStopRows are the telegram-hostile rows whose blob stops (H1, the cut rows, bad bool, bad vector, vector count,
// string past the end); the rows that decode (prefix byte 255, non-zero padding) and the cell rows are not among them.
func hostileStopRows() []int64 {
	return []int64{2001, 2002, 2003, 2004, 2005, 2006, 2007, 2008, 2009, 2010, 2011, 2012, 2013, 2014}
}

// Call 4, F7 (spec 8.4): a blob that stops still yields its record from the plain columns, with its stated state:
// kind unknown when the stop is at or before media, and the media rule when it lies after media (the rows stopped in
// entities: media gated off, so text; Task 8 review I1), a Body exactly when the stop lies after message (the same
// rows), and guid, thread.id, Time and direction from their readable cells. Over all telegram-hostile
// rows there is one record per row the engine counts.
func TestTelegramUndecodableBlobStillYieldsRecord(t *testing.T) {
	res := parseFixture(t, "telegram-hostile")
	exp := parsertest.LoadFixture(t, "testdata", "telegram-hostile").Expect
	// 26 rows of plan 4I4, then plan 4I4b's 68 cuts of R76 (H2401-H2468), 51 of R60 (H2601-H2651) and H2801-H2814
	if len(res.Records) != len(exp.Records) || res.Notes["rows.messages_v2"] != "159" || len(res.Records) != 159 {
		t.Errorf("%d records, the engine counts %s rows (oracle %d)", len(res.Records), res.Notes["rows.messages_v2"], len(exp.Records))
	}
	rows := byRow(res.Records)
	bodyRows := []int64{2010, 2012, 2013} // stopped in entities, after message and media
	for _, id := range hostileStopRows() {
		key := "messages_v2/" + itoa(id)
		r, ok := rows[key]
		if !ok {
			t.Errorf("%s: no record", key)
			continue
		}
		p, raw := r.Payload, rawOf(r)
		if st, _ := raw["data_state"].(string); st == "" || st == "decoded" {
			t.Errorf("%s: data_state %v", key, raw["data_state"])
		}
		wantKind := "unknown"
		if slices.Contains(bodyRows, id) {
			wantKind = "text" // stopped in entities, after media (gated off by the flags read)
		}
		if p["kind"] != wantKind {
			t.Errorf("%s: kind %v, want %s", key, p["kind"], wantKind)
		}
		if want := slices.Contains(bodyRows, id); (r.Body != "") != want {
			t.Errorf("%s: Body %q, want present = %v", key, r.Body, want)
		}
		th, _ := p["thread"].(map[string]any)
		if p["guid"] != itoa(id) || th["id"] != "1000002" || r.Time == nil || p["direction"] != "in" {
			t.Errorf("%s: guid %v, thread %v, time %v, direction %v: the plain cells are readable", key, p["guid"], th, r.Time, p["direction"])
		}
	}
}

// topFields are TL_message's top-level fields that carry bytes, in wire order, with their flags word (2: flags2) and
// bit (-1: ungated), from constructors.tsv (TLRPC.java:60187@A-HEAD): what lies after a stop is decided from them. Plan
// 4I4b Task 4: every field of the row, the tail included (the H2601 cuts stop in it).
func topFields() []struct {
	name string
	word int
	bit  int
} {
	return []struct {
		name string
		word int
		bit  int
	}{
		{"flags", 1, -1},
		{"flags2", 1, -1},
		{"id", 1, -1},
		{"from_id", 1, 8},
		{"from_boosts_applied", 1, 29},
		{"from_rank", 2, 12},
		{"peer_id", 1, -1},
		{"saved_peer_id", 1, 28},
		{"fwd_from", 1, 2},
		{"via_bot_id", 1, 11},
		{"via_business_bot_id", 2, 0},
		{"guestchat_via_from", 2, 19},
		{"reply_to", 1, 3},
		{"date", 1, -1},
		{"message", 1, -1},
		{"media", 1, 9},
		{"reply_markup", 1, 6},
		{"entities", 1, 7},
		{"views", 1, 10},
		{"forwards", 1, 10},
		{"replies", 1, 23},
		{"edit_date", 1, 15},
		{"post_author", 1, 16},
		{"grouped_id", 1, 17},
		{"reactions", 1, 20},
		{"restriction_reason", 1, 22},
		{"ttl_period", 1, 25},
		{"quick_reply_shortcut_id", 1, 30},
		{"effect", 2, 2},
		{"factcheck", 2, 3},
		{"report_delivery_until_date", 2, 5},
		{"paid_message_stars", 2, 6},
		{"suggested_post", 2, 7},
		{"schedule_repeat_period", 2, 10},
		{"summary_from_language", 2, 11},
		{"rich_message", 2, 13},
	}
}

// Spec 8.4: after a stop no field at or past it is filled. For every truncated telegram-hostile row the raw keys and
// payload fields derived from a field at or after data_stop_path are absent, and data_undecided lists exactly the
// mapped fields after it that are present by their flags (or whose flags word was not read), and the stopped field
// itself when the stop is its own read (Task 8 review M2: 2009 and 2014 list message).
func TestTelegramNoFieldAfterStop(t *testing.T) {
	derived := map[string][]string{ // field -> the raw keys it gives
		"flags": {"blob_flags"}, "flags2": {"blob_flags2"}, "id": {"id", "id_conflict"}, "via_bot_id": {"via_bot_id"},
		"reply_to": {"reply_to_msg_id"}, "date": {"date", "date_conflict"}, "views": {"views"},
		"edit_date": {"edit_date", "edit_date_unit"}, "grouped_id": {"grouped_id"}, "ttl_period": {"ttl_period"},
		"forwards": {"blob_forwards", "forwards_conflict"}, "post_author": {"post_author"}, "replies": {"replies", "replies_state"},
		"reactions":          {"reactions", "reactions_results", "reactions_recent", "reactions_top_reactors", "reactions_state"},
		"restriction_reason": {"restriction_reason", "restriction_reason_total"}, "quick_reply_shortcut_id": {"quick_reply_shortcut_id"},
		"effect": {"effect"}, "factcheck": {"factcheck", "factcheck_state"}, "report_delivery_until_date": {"report_delivery_until_date"},
		"paid_message_stars": {"paid_message_stars"}, "suggested_post": {"suggested_post", "suggested_post_state"},
		"schedule_repeat_period": {"schedule_repeat_period"}, "summary_from_language": {"summary_from_language"}, "entities": {"entities", "entities_total"},
	}
	noFieldAfterStopTail4I4b(t)
	mapped := androidtelegram.MappedFields()
	res := parseFixture(t, "telegram-hostile")
	n := 0
	for _, r := range res.Records {
		raw := rawOf(r)
		if raw["data_state"] != "truncated" {
			continue
		}
		n++
		key := locatorRow(r.Locator)
		stop, _ := raw["data_stop_path"].(string)
		head, _, _ := strings.Cut(strings.SplitN(stop, "[", 2)[0], ".")
		flags, flagsRead := raw["blob_flags"].(int64)
		flags2, flags2Read := raw["blob_flags2"].(int64)
		at := -1
		var want []string
		for i, f := range topFields() {
			if f.name == head {
				at = i
			}
			if at < 0 {
				continue
			}
			for _, k := range derived[f.name] {
				if _, has := raw[k]; has {
					t.Errorf("%s: raw.%s is filled from %s, at or after the stop %q", key, k, f.name, stop)
				}
			}
			if f.name == "message" && (r.Body != "" || r.Payload["text_source"] != nil) {
				t.Errorf("%s: a Body from message, at or after the stop %q", key, stop)
			}
			if f.name == "media" && r.Payload["kind"] == "media" {
				t.Errorf("%s: kind media, at or after the stop %q", key, stop)
			}
			// the stopped field itself is undecided when the stop is its own read (path == head; review M2)
			word, read := flags, flagsRead
			if f.word == 2 {
				word, read = flags2, flags2Read
			}
			if (i > at || stop == head) && slices.Contains(mapped, f.name) && (f.bit < 0 || !read || word>>f.bit&1 == 1) {
				want = append(want, f.name)
			}
		}
		if head != "" && at < 0 {
			t.Errorf("%s: stop path %q names no top-level field", key, stop)
		}
		got, _ := raw["data_undecided"].(string)
		if got != strings.Join(want, ",") {
			t.Errorf("%s: data_undecided %q, want %q", key, got, strings.Join(want, ","))
		}
	}
	if n != 129 {
		t.Errorf("%d truncated rows, want 129 (H2: nine cuts and the string past the end; plan 4I4b: 68 deep cuts of R76, 51 of R60)", n)
	}
	// review M3: a head spread over several fields (TL_messageForwarded_old's fwd_from.from_id, fwd_from.date) ends
	// with its last field, and the stop's own head is never listed as after it
	fwdOld := append(append(le32(0x05f46804), le32(5)...), le32(1000002)...) // id, fwd_from.from_id; cut in fwd_from.date
	raw, _ := androidtelegram.DecodeBlob(fwdOld, androidtelegram.TypeMessage, parse.NewBudget(1<<20).View())
	if raw["data_stop_path"] != "fwd_from.date" || raw["data_undecided"] != "from_id,peer_id,out,date,message,media" {
		t.Errorf("TL_messageForwarded_old cut in fwd_from.date: stop %v, data_undecided %v", raw["data_stop_path"], raw["data_undecided"])
	}
}

// Spec 8.5 (writer's call): the out column decides direction; a decoded blob that disagrees makes it unknown with
// raw.direction_conflict. R34 (column 0, blob out) conflicts; R1 and R2 agree; a row whose column says out and whose
// blob says from_scheduled without out is out (a scheduled message the app stored as outgoing), no conflict.
func TestTelegramDirectionConflictIsStated(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	for key, want := range map[string]string{"messages_v2/34": "unknown", "messages_v2/1": "in", "messages_v2/2": "out"} {
		r := rows[key]
		conflict := rawOf(r)["direction_conflict"] == true
		if r.Payload["direction"] != want || conflict != (want == "unknown") {
			t.Errorf("%s: direction %v, direction_conflict %v", key, r.Payload["direction"], rawOf(r)["direction_conflict"])
		}
	}
	sched := tlMessage{flags: bitFromScheduled, id: 7, peer: 1000002, date: baseDate, text: "scheduled"}
	noOut := tlMessage{id: 8, peer: 1000002, date: baseDate, text: "plain"}
	db := goBuiltMessages(181, nil, []msgCells{
		{mid: 7, uid: 1000002, out: int64(1), read: int64(0), data: sched.bytes()},
		{mid: 8, uid: 1000002, out: int64(1), read: int64(0), data: noOut.bytes()},
	})
	got := byRow(parseAt(t, remoteApp, dbOnly(db)).Records)
	if r := got["messages_v2/1"]; r.Payload["direction"] != "out" || rawOf(r)["direction_conflict"] != nil || rawOf(r)["from_scheduled"] != true {
		t.Errorf("from_scheduled without out: direction %v, raw %v", r.Payload["direction"], rawOf(r))
	}
	if r := got["messages_v2/2"]; r.Payload["direction"] != "unknown" || rawOf(r)["direction_conflict"] != true {
		t.Errorf("column out with neither out nor from_scheduled: direction %v, raw %v", r.Payload["direction"], rawOf(r))
	}
}

// Spec 8.5 (MO:138-141, MO:10196-10205): status from send_state by direction, read from read_state bit0 on incoming
// rows only. R3 sending, R36 failed, R37 editing, R38 media-read-only (bit0 clear), R39 out of range, R40 an
// incoming row in a send state; Go-built incoming rows with read_state 0..3 read false, true, false, true.
func TestTelegramStatusAndReadFollowTheColumns(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	cases := []struct {
		key, status, label string
		read               any
		readState          any
	}{
		{"messages_v2/3", "queued", "sending", nil, nil},
		{"messages_v2/36", "failed", "", nil, nil},
		{"messages_v2/37", "unknown", "editing", nil, nil},
		{"messages_v2/38", "received", "", false, nil},
		{"messages_v2/39", "received", "", nil, "out-of-range"},
		{"messages_v2/40", "unknown", "", true, nil},
		{"messages_v2/1", "received", "", true, nil},
		{"messages_v2/2", "sent", "", nil, nil},
	}
	for _, c := range cases {
		r := rows[c.key]
		raw := rawOf(r)
		label, _ := raw["label_send_state"].(string)
		if r.Payload["status"] != c.status || label != c.label || r.Payload["read"] != c.read || raw["read_state_state"] != c.readState {
			t.Errorf("%s: status %v, label %q, read %v, read_state_state %v; want %s, %q, %v, %v", c.key, r.Payload["status"],
				label, r.Payload["read"], raw["read_state_state"], c.status, c.label, c.read, c.readState)
		}
	}
	var cells []msgCells
	for rs := range int64(4) {
		m := tlMessage{id: int32(rs + 1), from: 1000002, peer: 1000001, date: baseDate, text: "read"}
		cells = append(cells, msgCells{mid: rs + 1, uid: 1000002, read: rs, data: m.bytes()})
	}
	got := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, cells))).Records
	want := []any{false, true, false, true}
	for i, r := range got {
		if r.Payload["read"] != want[i] {
			t.Errorf("read_state %d: read %v, want %v", i, r.Payload["read"], want[i])
		}
	}
	if len(got) != 4 {
		t.Errorf("%d records, want 4", len(got))
	}
}

// Spec 8.3 (4I R1, F5): both message tables holding rows are both read in full and labelled, nothing merged: R1 and
// R2's scenario in each generation gives four records, two per source table, one counted warning with both row
// counts, and a complete run.
func TestTelegramBothGenerationsEmitBothLabelled(t *testing.T) {
	res := parseFixture(t, "telegram-both")
	if len(res.Violations) > 0 || res.Refused > 0 {
		t.Errorf("violations %v, refused %d", res.Violations, res.Refused)
	}
	count := map[string]int{}
	for _, r := range res.Records {
		if r.Type != "message" {
			t.Errorf("%s: type %s", r.Locator, r.Type)
		}
		s, _ := rawOf(r)["source_table"].(string)
		count[s]++
	}
	if len(res.Records) != 4 || count["messages"] != 2 || count["messages_v2"] != 2 {
		t.Errorf("%d records, by source table %v", len(res.Records), count)
	}
	n := 0
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "telegram-both-generations") {
			n++
			if w.Locator != "sqlite:schema" || w.Reason != "telegram-both-generations: messages=2 messages_v2=2" {
				t.Errorf("warning %+v", w)
			}
		}
	}
	if n != 1 || res.Notes["generation"] != "both" {
		t.Errorf("%d both-generations warnings, note generation %q", n, res.Notes["generation"])
	}
}

// Review Focus 1: messages_v2 is keyed (mid, uid), so a mid repeats across dialogs. R1 and R44 share mid 101 in two
// dialogs: two records, equal guid, their own thread ids and locators, never merged. The recovered half (plan 4I4
// Task 14, ruling T2): a recovered image of mid 101 mapped against telegram-v181's live view is present by its own
// (mid, uid) only: with uid 1000003 by R44, with uid 1000002 by R1, and with a uid no live row of mid 101 has
// (1000099) absent, although mid 101 is live twice.
func TestTelegramSameMidInTwoDialogsIsNotMerged(t *testing.T) {
	t.Run("recovered", func(t *testing.T) {
		hd := parsertest.NewHarness(t)
		rr, ok := parsertest.FirstClaimedRow(t, hd, androidtelegram.Parser{}, deletedBundle(t, hd), tableV2,
			func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 1 })
		if !ok {
			t.Fatal("no recovered image of R1 in telegram-deleted")
		}
		h := parsertest.NewHarness(t)
		v181 := parsertest.LoadFixture(t, "testdata", "telegram-v181").Bundle(h, remoteApp)
		uidAt := recoveredColumn(t, "uid")
		for _, c := range []struct {
			uid  int64
			want string
		}{{1000003, "present"}, {1000002, "present"}, {1000099, "absent"}} {
			rr.Values[uidAt].Int = c.uid
			got := parsertest.MapRecoveredRow(t, h, androidtelegram.Parser{}, v181, rr)
			if got.Err != nil || len(got.Records) != 1 {
				t.Fatalf("uid %d: err %v, unmapped %q, %d records", c.uid, got.Err, got.Unmapped, len(got.Records))
			}
			raw := rawOf(got.Records[0])
			if raw["mid"] != int64(101) || raw["uid"] != c.uid || raw["live_key_state"] != c.want {
				t.Errorf("recovered (mid %v, uid %v): live_key_state %v, want %q", raw["mid"], raw["uid"], raw["live_key_state"], c.want)
			}
		}
	})
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r1, r44 := rows["messages_v2/1"], rows["messages_v2/44"]
	th := func(r records.Record) any { m, _ := r.Payload["thread"].(map[string]any); return m["id"] }
	if r1.Payload["guid"] != "101" || r44.Payload["guid"] != "101" {
		t.Errorf("guids %v, %v, want 101 twice", r1.Payload["guid"], r44.Payload["guid"])
	}
	if th(r1) != "1000002" || th(r44) != "1000003" {
		t.Errorf("thread ids %v, %v", th(r1), th(r44))
	}
	// R44 lies in a WAL frame: its locator names the frame after the rowid
	if r1.Locator != "sqlite:table=messages_v2;rowid=1" || !strings.HasPrefix(r44.Locator, "sqlite:table=messages_v2;rowid=44;") {
		t.Errorf("locators %q, %q", r1.Locator, r44.Locator)
	}
}

// Review Focus 4, 4I4-P7, F17: one store at the account-0 and the account-1 path in two jobs states each job's account
// in its note and in every record's raw.account; the records are otherwise the same (equal locators), never merged.
// An account<x> whose tail is not digits states "undecided"; away from the app path raw.account is absent (review M4).
func TestTelegramTwoAccountsAreKeptApart(t *testing.T) {
	files := fixtureFiles(t, "telegram-v159")
	jobs := []struct {
		remote, note, account string // account "": raw.account absent (placeOther, 4I4-P7)
	}{
		{remoteApp, "0", "0"},
		{remoteAccount1, "1", "1"},
		{"/data/data/org.telegram.messenger/files/accountX/cache4.db", "undecided", "undecided"}, // review M4
		{remoteOther, "unknown", ""},
	}
	var first []string
	for i, j := range jobs {
		res := parseAt(t, j.remote, files)
		if res.Err != nil {
			t.Fatalf("%s: Parse err %v", j.remote, res.Err)
		}
		if res.Notes["account"] != j.note {
			t.Errorf("%s: note account %q, want %q", j.remote, res.Notes["account"], j.note)
		}
		var msgs []records.Record
		for _, r := range res.Records {
			if r.Type != "message" {
				continue
			}
			got, has := rawOf(r)["account"]
			if (j.account == "" && has) || (j.account != "" && got != j.account) {
				t.Errorf("%s %s: raw.account %v (present %v), want %q", j.remote, r.Locator, got, has, j.account)
			}
			delete(rawOf(r), "account")
			r.ArtifactID, r.SourcePath = "", ""
			msgs = append(msgs, r)
		}
		c := parsertest.CanonicalRecords(msgs)
		if i == 0 {
			first = c
			if len(c) == 0 {
				t.Fatal("no message record")
			}
			continue
		}
		if !slices.Equal(c, first) {
			t.Errorf("%s: the records differ beyond raw.account, artifact_id and source_path: %v vs %v", j.remote, c, first)
		}
	}
}

// B9, 4I4-P9: the undecoded blobs of a column are one counted warning after the run, with the per-state counts,
// never one per row; the notes blob_states and constructors_seen state the same counts (telegram-hostile as its
// oracle; telegram-v181 as its oracle without users.data of Task 12).
func TestTelegramBlobUndecodedIsOneCountedWarningPerColumn(t *testing.T) {
	res := parseFixture(t, "telegram-hostile")
	var got []string
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "telegram-blob-undecoded") {
			got = append(got, w.Locator+": "+w.Reason)
		}
	}
	// plan 4I4b Task 4: the 119 cut rows (truncated), H2802 (cap) and H2806, H2807 (partial) join the 4I4 rows
	want := "sqlite:table=messages_v2: telegram-blob-undecoded: messages_v2.data: 138 records (partial:4,unknown-constructor:1,truncated:129,malformed:2,cap:2)"
	if len(got) != 1 || got[0] != want {
		t.Errorf("warnings %q, want one %q", got, want)
	}
	notes := map[string]string{
		"blob_states":       "messages_v2.data=decoded:20,partial:4,unknown-constructor:1,truncated:129,malformed:2,cap:2",
		"constructors_seen": "0x7600b9d3:151,0x555555f9:2,0x0badf00d:1,0x22eb6aba:1",
	}
	for k, v := range notes {
		if res.Notes[k] != v {
			t.Errorf("telegram-hostile: note %s = %q, want %q", k, res.Notes[k], v)
		}
	}
	v181 := parseFixture(t, "telegram-v181")
	exp := parsertest.LoadFixture(t, "testdata", "telegram-v181").Expect
	if v181.Notes["constructors_seen"] != exp.Notes["constructors_seen"] {
		t.Errorf("telegram-v181: constructors_seen %q, the oracle %q", v181.Notes["constructors_seen"], exp.Notes["constructors_seen"])
	}
	// plan 4I4b: R29's Dice is full and well-formed, so it decodes (49 decoded and 7 partial before); R60-R80 add 20
	// decoded rows and R79 (partial). replies_data is a counted column too (P6, D13): R63 and R64 decode, R55 stops at an unknown constructor.
	if got := v181.Notes["blob_states"]; got != "messages_v2.data=decoded:70,partial:7,unknown-constructor:1,null:1;messages_v2.replydata=decoded:1;messages_v2.replies_data=decoded:2,unknown-constructor:1;users.data=decoded:1,decoded-prefix:8,unknown-constructor:1" ||
		got != exp.Notes["blob_states"] {
		t.Errorf("telegram-v181: blob_states %q (the oracle: %q)", got, exp.Notes["blob_states"])
	}
	// P6: replies_data is counted like data: R55 stops at an unknown constructor, one counted warning for the column
	var rd []string
	for _, w := range v181.Warnings {
		if strings.Contains(w.Reason, "telegram-blob-undecoded: messages_v2.replies_data") {
			rd = append(rd, w.Reason)
		}
	}
	if len(rd) != 1 || rd[0] != "telegram-blob-undecoded: messages_v2.replies_data: 1 records (unknown-constructor:1)" {
		t.Errorf("telegram-v181: replies_data warnings %q, want one counting the unknown constructor", rd)
	}
}

// idReplyKeyboardHide is TL_replyKeyboardHide (ReplyMarkup), an opaque row in this plan (Stage B reads reply markup).
const idReplyKeyboardHide = 0xa03e5b85

// Plan 4I4b design D9, rulings R5, B3 (Review Focus 4): edit_date is Unix seconds at all four pins (facts.tsv
// unit.edit_date), so a read valid edit_date is Times.edited beside the raw value and its unit; 0 is a sentinel and
// -100 invalid, stated in raw.edit_date_status with no time; edit_hide does not void it; a stop after edit_date keeps
// it (the field was read), and a stop before it gives no time and lists edit_date in data_undecided.
func TestTelegramEditTimeFollowsTheUnitFact(t *testing.T) {
	const ann, self = 1000002, 1000001
	edited := time.Date(2023, 11, 14, 22, 23, 20, 0, time.UTC) // baseDate + 600
	msg := func(id int32, edit int32, set bool, flags uint32, markup, tail []byte) msgCells {
		return msgCells{mid: int64(id), uid: ann, data: tlMessage{
			flags: flags, id: id, from: ann, peer: self, date: baseDate, editDate: edit, editSet: set, text: "x",
			markup: markup, tail: tail,
		}.bytes()}
	}
	old := append(append(append(le32(idReactionCountOld), le32(0)...), tlStr("👍")...), le32(2)...)
	rows := []msgCells{
		msg(1, baseDate+600, false, 0, nil, nil),
		msg(2, 0, true, 0, nil, nil),
		msg(3, -100, false, 0, nil, nil),
		msg(4, baseDate+600, false, bitEditHide, nil, nil),
		msg(5, baseDate+600, false, bitReactions, nil, reactionsWith(old)),
		msg(6, baseDate+600, false, 0, append(le32(idReplyKeyboardHide), le32(0)...), nil),
	}
	got := byRow(parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, rows))).Records)
	rec := func(n int) (records.Record, map[string]any) {
		r := got["messages_v2/"+itoa(int64(n))]
		return r, rawOf(r)
	}
	hasEdited := func(r records.Record) bool {
		return len(r.Times) == 1 && r.Times[0].Kind == "edited" && r.Times[0].Time.T.Equal(edited) &&
			r.Times[0].Time.Basis == records.BasisUTC
	}

	r, raw := rec(1)
	if !hasEdited(r) || raw["edit_date"] != int64(baseDate+600) || raw["edit_date_unit"] != "seconds" || raw["edit_date_status"] != nil {
		t.Errorf("edit D+600: times %v, raw edit_date %v unit %v status %v", r.Times, raw["edit_date"], raw["edit_date_unit"], raw["edit_date_status"])
	}
	for n, st := range map[int]string{2: "sentinel", 3: "invalid"} {
		r, raw := rec(n)
		if len(r.Times) != 0 || raw["edit_date_status"] != st || raw["edit_date"] == nil {
			t.Errorf("row %d: times %v, edit_date %v status %v, want no time and %s", n, r.Times, raw["edit_date"], raw["edit_date_status"], st)
		}
	}
	if r, raw := rec(4); !hasEdited(r) || raw["edit_hide"] != true {
		t.Errorf("edit_hide: times %v, raw.edit_hide %v", r.Times, raw["edit_hide"])
	}
	if r, raw := rec(5); !hasEdited(r) || raw["data_stop_path"] != "reactions.results[0]" {
		t.Errorf("stop after edit_date: times %v, stop %v", r.Times, raw["data_stop_path"])
	}
	r, raw = rec(6)
	undecided, _ := raw["data_undecided"].(string)
	if len(r.Times) != 0 || raw["edit_date"] != nil || raw["data_stop_path"] != "reply_markup" ||
		!slices.Contains(strings.Split(undecided, ","), "edit_date") {
		t.Errorf("stop before edit_date: times %v, edit_date %v, stop %v, data_undecided %q", r.Times, raw["edit_date"], raw["data_stop_path"], undecided)
	}
}
