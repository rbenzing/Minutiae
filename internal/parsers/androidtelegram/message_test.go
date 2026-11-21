package androidtelegram_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
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
	if len(res.Records) != len(exp.Records) || res.Notes["rows.messages_v2"] != "26" || len(res.Records) != 26 {
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

// topFields are TL_message's top-level fields that carry bytes, in wire order, with their flags bit (-1: ungated),
// from constructors.tsv (TLRPC.java:60187@A-HEAD): what lies after a stop is decided from them.
func topFields() []struct {
	name string
	bit  int
} {
	return []struct {
		name string
		bit  int
	}{
		{"flags", -1},
		{"flags2", -1},
		{"id", -1},
		{"from_id", 8},
		{"peer_id", -1},
		{"fwd_from", 2},
		{"via_bot_id", 11},
		{"reply_to", 3},
		{"date", -1},
		{"message", -1},
		{"media", 9},
		{"entities", 7},
		{"views", 10},
		{"edit_date", 15},
		{"grouped_id", 17},
		{"ttl_period", 25},
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
	}
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
			if (i > at || stop == head) && slices.Contains(mapped, f.name) && (f.bit < 0 || !flagsRead || flags>>f.bit&1 == 1) {
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
	if n != 10 {
		t.Errorf("%d truncated rows, want 10 (H2: nine cuts and the string past the end)", n)
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
// dialogs: two records, equal guid, their own thread ids and locators, never merged.
func TestTelegramSameMidInTwoDialogsIsNotMerged(t *testing.T) {
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
	want := "sqlite:table=messages_v2: telegram-blob-undecoded: messages_v2.data: 16 records (partial:2,unknown-constructor:1,truncated:10,malformed:2,cap:1)"
	if len(got) != 1 || got[0] != want {
		t.Errorf("warnings %q, want one %q", got, want)
	}
	notes := map[string]string{
		"blob_states":       "messages_v2.data=decoded:9,partial:2,unknown-constructor:1,truncated:10,malformed:2,cap:1",
		"constructors_seen": "0x7600b9d3:20,0x555555f9:2,0x0badf00d:1,0x22eb6aba:1",
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
	if got := v181.Notes["blob_states"]; got != "messages_v2.data=decoded:49,partial:7,unknown-constructor:1,null:1;messages_v2.replydata=decoded:1;users.data=decoded:1,decoded-prefix:8,unknown-constructor:1" {
		t.Errorf("telegram-v181: blob_states %q (the oracle: %q)", got, exp.Notes["blob_states"])
	}
}
