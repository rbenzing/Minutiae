package androidwhatsapp_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The hostile rows of spec 10 that earlier tasks do not cover. Each test builds its database here, runs the parser through
// the harness and requires: no panic, no invented value, and a stated warning or raw marker. A bound is a typed error or a
// counted warning, never a silent cut.

func hostileRun(t *testing.T, db, wal []byte, budget int64) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	b := parsertest.Fixture{Name: "hostile", Files: files}.Bundle(h, remoteLive)
	in := b.Input(h, p, false)
	if budget > 0 {
		in.Budget = parse.NewBudget(budget).View()
	}
	return h.RunLenient(p, in)
}

// A database cut inside the message table: the pages that are gone are stated by the decoder, the table is stated as
// unreadable in a note and its row count is not stated, and nothing is guessed about the rows that were lost. Every cut
// length holds the contract: only typed errors, no refused record, and no record the database did not hold.
func TestWhatsAppTruncatedDatabaseStatesUnreadable(t *testing.T) {
	const rows = 600
	full, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		for i := int64(1); i <= rows; i++ {
			msgRow(tb, i, 1, 0, nil, 0, goodTS, fmt.Sprintf("m%d %s", i, strings.Repeat("x", 60)))
		}
	})
	const ps = 4096
	pages := len(full) / ps
	var unreadable int
	for cut := pages - 1; cut >= pages-6 && cut >= 3; cut-- {
		res := hostileRun(t, slices.Clone(full[:cut*ps]), nil, 0)
		if res.Err != nil && !parsertest.HostileOK(res.Err) && !errors.Is(res.Err, androidwhatsapp.ErrPurgeJobsIncomplete) &&
			!errors.Is(res.Err, androidwhatsapp.ErrNeitherTable) && !errors.Is(res.Err, androidwhatsapp.ErrSchemaUndecidable) {
			t.Fatalf("cut at page %d: untyped error %v", cut, res.Err)
		}
		if res.Refused != 0 || len(res.Violations) != 0 {
			t.Fatalf("cut at page %d: refused %d, violations %v", cut, res.Refused, res.Violations)
		}
		for _, r := range res.Records {
			var id int
			if _, err := fmt.Sscanf(r.Body, "m%d ", &id); err != nil || id < 1 || id > rows {
				t.Fatalf("cut at page %d: the record %q was not in the database", cut, r.Body)
			}
		}
		if res.Notes["message"] != "unreadable" {
			continue
		}
		unreadable++
		if v, has := res.Notes["rows.message"]; has {
			t.Errorf("cut at page %d: message is unreadable and rows.message %q is stated", cut, v)
		}
		if len(res.Warnings) == 0 {
			t.Errorf("cut at page %d: the table is unreadable and no warning says why", cut)
		}
	}
	if unreadable == 0 {
		t.Errorf("no cut gave message=unreadable: the check proves nothing")
	}
}

// A hot journal is rolled back: the records reflect the rolled-back state and a journal-hot warning says so.
func TestWhatsAppHotJournalIsRolledBack(t *testing.T) {
	const ps = 512
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := tables{}
	for _, name := range baseOrder {
		if baseDDL[name] != "" {
			tb[name] = b.CreateTable(name, baseDDL[name])
		}
	}
	jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
	tb["chat"].Insert(1, nil, int64(1), nil)
	msgRow(tb, 1, 1, 0, nil, 0, goodTS, "original text of the message")
	snap := b.Snapshot()
	tb["message"].Update(1, nil, int64(1), int64(0), "KB", nil, int64(0), goodTS, nil, int64(0), "interrupted rewrite")
	db := b.Bytes()
	j := b.NewJournal(512, 0x5eed0003, snap.Pages())
	j.Record(tb["message"].Root(), snap.Page(tb["message"].Root()))
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	bun := parsertest.Fixture{Name: "hot", Files: map[string][]byte{"db": db, "journal": j.Bytes()}}.Bundle(h, remoteLive)
	res := h.RunLenient(p, bun.Input(h, p, false))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	var bodies []string
	for _, r := range res.Records {
		bodies = append(bodies, r.Body)
	}
	if !slices.Equal(bodies, []string{"original text of the message"}) {
		t.Errorf("bodies %q, want only the rolled-back state", bodies)
	}
	var hot bool
	for _, w := range res.Warnings {
		hot = hot || strings.Contains(w.Reason, "journal-hot")
	}
	if !hot {
		t.Errorf("no journal-hot warning: %+v", res.Warnings)
	}
}

// Every foreign key of spec 10 that points at a missing row is stated, never read as an absence or invented.
func TestWhatsAppDanglingKeysAreStated(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		jidRow(tb, 2, "100000000000001", "lid", "100000000000001@lid")
		tb["chat"].Insert(1, nil, int64(1), nil)
		tb["chat"].Insert(2, nil, int64(99), nil) // a chat whose jid row is missing
		tb["chat"].Insert(3, nil, int64(2), nil)  // a LID chat
		tb["jid_map"].Insert(2, nil, int64(99))   // its target is missing
		msgRow(tb, 1, 50, 0, nil, 0, goodTS, "no chat")
		msgRow(tb, 2, 2, 0, nil, 0, goodTS, "chat without jid")
		msgRow(tb, 3, 3, 0, nil, 0, goodTS, "lid chat, target missing")
		msgRow(tb, 4, 1, 0, int64(77), 0, goodTS, "sender missing")
		msgRow(tb, 5, 1, 0, nil, 10, goodTS, "call notice")
		tb["message_media"].Insert(5, nil, "/WhatsApp/Media/x.jpg", int64(1), "image/jpeg", "x")         // a media row of a text message
		tb["message_add_on"].Insert(1, nil, int64(1), int64(0), "A1", nil, int64(99), goodTS, int64(56)) // the parent is missing
		tb["message_add_on_reaction"].Insert(1, nil, "x", goodTS)
		tb["call_log"].Insert(1, nil, int64(88), int64(0), "C1", goodTS, int64(0), int64(30), int64(5), int64(77)) // jid and group missing
		tb["deleted_chat_job"].Insert(1, nil, int64(66), int64(5), nil, nil, nil)                                  // names no chat
		tb["deleted_chat_job"].Insert(2, nil, int64(1), int64(0), nil, nil, nil)                                   // a bound that covers nothing
	})
	res := hostileRun(t, db, nil, 0)
	recs := byRowid(t, res)
	raw := func(k string) map[string]any { return rawMap(t, recs[k]) }
	for k, want := range map[string]string{"message/1": "no-chat-row", "message/2": "no-jid-row"} {
		if raw(k)["chat_state"] != want {
			t.Errorf("%s: chat_state %v, want %s", k, raw(k)["chat_state"], want)
		}
	}
	if got := raw("message/3")["lid_unresolved"]; !slices.Equal(anyStrings(got), []string{"100000000000001@lid"}) {
		t.Errorf("message/3: lid_unresolved %v, want the LID (its jid_map target is missing)", got)
	}
	if raw("message/4")["sender_state"] != "no-jid-row" {
		t.Errorf("message/4: sender_state %v", raw("message/4")["sender_state"])
	}
	if raw("message_add_on/1")["parent_state"] != "no-message-row" {
		t.Errorf("reaction: parent_state %v", raw("message_add_on/1")["parent_state"])
	}
	if c := raw("call_log/1"); c["address_state"] != "no-jid-row" || c["group_jid_state"] != "no-jid-row" {
		t.Errorf("call: address_state %v, group_jid_state %v, want no-jid-row for both", c["address_state"], c["group_jid_state"])
	}
	if res.Notes["deleted_chat_jobs_dangling"] != "1" {
		t.Errorf("deleted_chat_jobs_dangling %q, want 1 (job 1 names chat 66)", res.Notes["deleted_chat_jobs_dangling"])
	}
	for k, r := range recs {
		if r.Deleted {
			t.Errorf("%s is deleted: a job that names no chat must never be applied to another", k)
		}
	}
}

func anyStrings(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// JIDs with no @, many @, a NUL, a device suffix or a megabyte of digits appear as stored, and no participant is invented
// from a string that is not an address.
func TestWhatsAppHostileJIDs(t *testing.T) {
	jids := []string{
		"no-at-sign", "a@b@s.whatsapp.net", "1555\x00@s.whatsapp.net", strings.Repeat("1", 1<<20) + "@s.whatsapp.net",
		"15550000001:7@s.whatsapp.net", "@s.whatsapp.net", "15550000001@", "1555\x00@g.us", "15\xff55@s.whatsapp.net",
	}
	db, _ := buildDB(nil, func(tb tables) {
		for i, j := range jids {
			id := int64(i + 1)
			jidRow(tb, id, "u", "s.whatsapp.net", j)
			tb["chat"].Insert(id, nil, id, nil)
			msgRow(tb, id, id, 0, nil, 0, goodTS, "hostile")
		}
	})
	res := hostileRun(t, db, nil, 0)
	recs := byRowid(t, res)
	if res.Refused != 0 || len(res.Violations) != 0 {
		t.Fatalf("refused %d, violations %v", res.Refused, res.Violations)
	}
	for i, j := range jids {
		k := fmt.Sprintf("message/%d", i+1)
		r, ok := recs[k]
		if !ok {
			t.Errorf("%s: no record for the hostile JID %.20q", k, j)
			continue
		}
		raw := rawMap(t, r)
		th := threadOf(r)
		parts := partsOf(r)
		switch {
		case len(j) > 1024:
			if raw["chat_state"] != "too-long" || th["id"] != nil || raw["chat_jid"] != nil {
				t.Errorf("%s: a 1 MiB JID: chat_state %v thread %v: never cut and never stored half", k, raw["chat_state"], th)
			}
		case strings.ContainsRune(j, 0) || !utf8.ValidString(j):
			// a JID that cleaning changed is not an address (fix 5, M3): unknown, the cleaned text raw with its text_flags,
			// never a thread id, a group or a participant
			if raw["chat_state"] != "altered" || th["id"] != nil || th["group"] != nil || len(parts) != 0 || raw["chat_jid"] != cleanedForRaw(j) {
				t.Errorf("%s: chat_state %v thread %v participants %v chat_jid %v, want altered, no thread id, group or participant, the cleaned text raw",
					k, raw["chat_state"], th, parts, raw["chat_jid"])
			}
			if fl, _ := raw["text_flags"].([]any); len(fl) == 0 {
				t.Errorf("%s: no text_flags for a cleaned JID", k)
			}
			continue
		default:
			if th["id"] != cleanedForRaw(j) {
				t.Errorf("%s: thread id %v, want the stored %q", k, th["id"], j)
			}
		}
		// the counterparty of an incoming 1:1 message is the chat address, as stored; a string that does not parse is
		// still the address the database holds (identifiers as stored), and nothing else is invented
		for _, p := range parts {
			addr, _ := p.(map[string]any)["address"].(string)
			if addr != cleanedForRaw(j) {
				t.Errorf("%s: participant %q, want only the stored address", k, addr)
			}
		}
	}
}

// cleanedForRaw is the stored text as the record shows it: a NUL becomes U+FFFD.
func cleanedForRaw(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "�"), "�")
}

// Negative, zero, -1, MaxInt64, MinInt64 and seconds in a millisecond column give a status, never a time.
func TestWhatsAppAbsurdTimestamps(t *testing.T) {
	cases := map[int64]string{
		-1: "sentinel", 0: "sentinel", math.MaxInt64: "sentinel", math.MinInt64: "sentinel", 5: "invalid", 1600000: "invalid", -5000: "invalid",
		99_999_999_999: "invalid", 4102444800000: "invalid",
	}
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		id := int64(0)
		for ts := range cases {
			id++
			msgRow(tb, id, 1, 0, nil, 0, ts, "t")
		}
		id++
		msgRow(tb, id, 1, 0, nil, 0, goodTS, "good")
	})
	recs := byRowid(t, hostileRun(t, db, nil, 0))
	got := map[int64]string{}
	n := 0
	for k, r := range recs {
		raw := rawMap(t, r)
		if r.Body == "good" {
			if r.Time == nil || raw["timestamp_status"] != nil {
				t.Errorf("%s: the good row has time %v status %v", k, r.Time, raw["timestamp_status"])
			}
			continue
		}
		n++
		if r.Time != nil {
			t.Errorf("%s: a time %v from the absurd timestamp %v", k, r.Time, raw["timestamp"])
		}
		ts, _ := raw["timestamp"].(int64)
		st, _ := raw["timestamp_status"].(string)
		got[ts] = st
	}
	if n != len(cases) {
		t.Fatalf("%d absurd rows, want %d", n, len(cases))
	}
	for ts, want := range cases {
		if got[ts] != want {
			t.Errorf("timestamp %d: status %q, want %q", ts, got[ts], want)
		}
	}
}

// A 4 MiB text is cut at 256 KiB and flagged with its full length, and a megabyte vCard is cut and flagged: the parse stays
// inside a MemBudget of 128 MiB.
func TestWhatsAppHugeValuesAreCapped(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 0, goodTS, big)
		msgRow(tb, 2, 1, 0, nil, 4, goodTS, "card")
		tb["message_vcard"].Insert(1, nil, int64(2), "BEGIN:VCARD\r\nNOTE:"+strings.Repeat("v", 1<<20)+"\r\nEND:VCARD\r\n")
	})
	res := hostileRun(t, db, nil, 128<<20)
	if res.Err != nil {
		t.Fatalf("Parse at MemBudget 128 MiB: %v", res.Err)
	}
	recs := byRowid(t, res)
	r1 := recs["message/1"]
	if r1.Payload["body_truncated"] != true || r1.Payload["body_total_bytes"] != int64(4<<20) || len(r1.Body) > 256<<10 || len(r1.Body) < 200<<10 {
		t.Errorf("message 1: body %d bytes, truncated %v total %v, want the 4 MiB text cut at 256 KiB with its total", len(r1.Body), r1.Payload["body_truncated"], r1.Payload["body_total_bytes"])
	}
	// the 1 MiB vCard is the body of the contact card, cut at 256 KiB and flagged
	if r2 := recs["message/2"]; len(r2.Body) == 0 || len(r2.Body) > 256<<10 || r2.Payload["body_truncated"] != true {
		t.Errorf("message 2: body %d bytes, truncated %v, want the 1 MiB vCard cut at 256 KiB and flagged", len(r2.Body), r2.Payload["body_truncated"])
	}
}

// A 64 MiB blob in a column the parser does not read: the decoder charges the cell twice to the budget (the chain it reads and
// the value it holds), so MemBudget 128 MiB ends in the typed ErrBudget with no record invented, and 256 MiB parses the store with
// the blob never reaching a record. The plan's bound (the parse fits 128 MiB) is NOT met: it is a limitation of the decoder
// (`sqlitedb`), recorded in docs/limitations/parsers.md item 15, and this test pins the bound that holds.
func TestWhatsAppHugeBlobCellIsChargedTwiceToTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 64 MiB blob (about 300 MB of memory): skipped under -short")
	}
	db, _ := buildDB(map[string]string{
		"message": "create table message(_id integer primary key, chat_row_id integer, from_me integer, key_id text, " +
			"sender_jid_row_id integer, status integer, timestamp integer, received_timestamp integer, message_type integer, text_data text, thumb_image blob)",
	}, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		tb["message"].Insert(1, nil, int64(1), int64(0), "K1", nil, int64(0), goodTS, int64(0), int64(0), "blob row", bytes.Repeat([]byte{0xAB}, 64<<20))
		tb["message"].Insert(2, nil, int64(1), int64(0), "K2", nil, int64(0), goodTS, int64(0), int64(0), "plain row", nil)
	})
	tight := hostileRun(t, db, nil, 128<<20)
	if !errors.Is(tight.Err, parse.ErrBudget) || len(tight.Records) != 0 {
		t.Errorf("MemBudget 128 MiB: err %v, %d records, want the typed ErrBudget and no record", tight.Err, len(tight.Records))
	}
	roomy := hostileRun(t, db, nil, 256<<20)
	if roomy.Err != nil || len(roomy.Records) != 2 {
		t.Fatalf("MemBudget 256 MiB: err %v, %d records, want 2", roomy.Err, len(roomy.Records))
	}
	for _, r := range roomy.Records {
		for k, v := range rawMap(t, r) {
			if s, ok := v.(string); ok && len(s) > 1<<20 {
				t.Errorf("%s: raw.%s holds %d bytes", r.Locator, k, len(s))
			}
		}
		if len(r.Body) > 64 {
			t.Errorf("%s: a body of %d bytes: the blob reached a record", r.Locator, len(r.Body))
		}
	}
}

// 1,000,000 message_vcard rows on one message: at most 1000 are listed with their total and a truncation flag, or the join
// is stated undecided; the parse stays inside its budget either way.
func TestWhatsAppMillionVCardRowsAreCapped(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a million-row table (about 1 GB of memory): skipped under -short")
	}
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 4, goodTS, "card")
		const card = "BEGIN:VCARD\r\nEND:VCARD\r\n"
		serials, body := []uint64{0, 1, uint64(13 + 2*len(card))}, append([]byte{1}, card...) // (NULL, 1, card)
		for i := int64(1); i <= 1_000_000; i++ {
			tb["message_vcard"].InsertRaw(i, serials, body) // Insert is quadratic
		}
	})
	res := hostileRun(t, db, nil, 0) // the default MemBudget (1 GiB): the join index of a million rows alone is charged about 260 MiB
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	r := byRowid(t, res)["message/1"]
	raw := rawMap(t, r)
	if raw["vcards_total"] != int64(1_000_000) || raw["vcards_truncated"] != true {
		t.Errorf("vcards_total %v truncated %v, want 1000000 and true", raw["vcards_total"], raw["vcards_truncated"])
	}
	if n := strings.Count(r.Body, "BEGIN:VCARD"); n != 1000 {
		t.Errorf("the body holds %d cards, want the cap of 1000", n)
	}
	// the join index of a million rows is charged to the budget: a tight budget is the typed ErrBudget, not an allocation in
	// proportion to the rows and not a silent cut
	if tight := hostileRun(t, db, nil, 128<<20); !errors.Is(tight.Err, parse.ErrBudget) {
		t.Errorf("MemBudget 128 MiB: err %v, want the typed ErrBudget", tight.Err)
	}
}

// 4I-R4 (f), spec 10: 200,000 deleted_chat_job rows on one chat, at the production cap of 100,000, give one counted warning,
// ErrPurgeJobsIncomplete, every record kept and no ErrBudget.
func TestWhatsAppDeletedChatJobFloodIsIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 200,000 job rows (about 400 MB of memory): skipped under -short")
	}
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		for i := int64(1); i <= 3; i++ {
			msgRow(tb, i, 1, 0, nil, 0, goodTS, fmt.Sprintf("m%d", i))
		}
		serials, body := []uint64{0, 1, 8, 0, 0, 0}, []byte{1} // (NULL, chat 1, bound 0, NULL, NULL, NULL): a bound that covers nothing
		for i := int64(1); i <= 200_000; i++ {
			tb["deleted_chat_job"].InsertRaw(i, serials, body) // Insert is quadratic
		}
	})
	res := hostileRun(t, db, nil, 256<<20)
	if !errors.Is(res.Err, androidwhatsapp.ErrPurgeJobsIncomplete) || errors.Is(res.Err, parse.ErrBudget) {
		t.Fatalf("Parse = %v, want ErrPurgeJobsIncomplete and no ErrBudget at 256 MiB", res.Err)
	}
	if ws := warningsContaining(res, "whatsapp-deleted-chat-job-incomplete: 200000 rows, 100000 read"); len(ws) != 1 {
		t.Errorf("warnings %v, want one whatsapp-deleted-chat-job-incomplete: 200000 rows, 100000 read", res.Warnings)
	}
	recs := byRowid(t, parsertest.Result{Records: res.Records})
	if len(recs) != 3 {
		t.Fatalf("%d records, want 3", len(recs))
	}
	for k, r := range recs {
		raw := rawMap(t, r)
		if !r.Deleted && raw["pending_purge"] != "undecided" {
			t.Errorf("%s: live with no verdict (raw %v): a row no read job clears is undecided", k, raw)
		}
	}
}

// A message_type, action_type or call_result that no two sources support is unknown plus raw, never a label.
func TestWhatsAppUnknownFutureCodes(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		msgRow(tb, 1, 1, 0, nil, 9999, goodTS, "future type")
		msgRow(tb, 2, 1, 0, nil, 7, goodTS, "system")
		tb["call_log"].Insert(1, nil, int64(1), int64(0), "C1", goodTS, int64(0), int64(30), int64(4242), int64(0))
	})
	recs := byRowid(t, hostileRun(t, db, nil, 0))
	m1 := recs["message/1"]
	raw1 := rawMap(t, m1)
	if m1.Payload["kind"] != "unknown" || raw1["message_type"] != int64(9999) || raw1["label_message_type"] != nil {
		t.Errorf("message 1: kind %v raw %v, want unknown with message_type 9999 and no label", m1.Payload["kind"], raw1)
	}
	c := recs["call_log/1"]
	craw := rawMap(t, c)
	if c.Payload["outcome"] != "unknown" || craw["call_result"] != int64(4242) || craw["label_call_result"] != nil {
		t.Errorf("call 1: outcome %v raw %v, want unknown with call_result 4242 and no label", c.Payload["outcome"], craw)
	}
}

// A path that holds .., *, a backslash, a NUL or leaves the media roots makes no lookup and gives the source reference only.
func TestWhatsAppHostileAttachmentPaths(t *testing.T) {
	paths := []string{
		"/WhatsApp/Media/../../../data/secret.db", "/WhatsApp/Media/*", "/WhatsApp/Media/a\\b.jpg", "/WhatsApp/Media/a\x00b.jpg",
		"/etc/passwd", "relative/path.jpg", "", "//WhatsApp//Media//x.jpg",
	}
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		for i, p := range paths {
			id := int64(i + 1)
			msgRow(tb, id, 1, 0, nil, 1, goodTS, "pic")
			tb["message_media"].Insert(id, nil, p, int64(10), "image/jpeg", "x.jpg")
		}
	})
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	// the case holds files a lookup could find; none may be linked
	bun := parsertest.Fixture{Name: "paths", Files: map[string][]byte{"db": db}}.Bundle(h, remoteLive)
	res := h.RunLenient(p, bun.Input(h, p, false))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	recs := byRowid(t, res)
	for i, path := range paths {
		r := recs[fmt.Sprintf("message/%d", i+1)]
		atts, _ := r.Payload["attachments"].([]any)
		for _, a := range atts {
			am, _ := a.(map[string]any)
			if am["artifact_id"] != nil {
				t.Errorf("path %q: linked to artifact %v", path, am["artifact_id"])
			}
		}
	}
}

// Review Focus 3: the detector's notes (generation, rows.*, app, database_user, jid_map) are written before any mapping, so
// they survive the host's cap on notes, where the first write of a key wins and the later notes of the mapped tables and of
// the joins are the ones dropped. The cap is the harness's real 64, and then lowered to 6 so that it bites on this fixture.
func TestWhatsAppDetectorNotesSurviveTheNoteCap(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "android-modern-2026")
	run := func(limit int) parsertest.Result {
		h := parsertest.NewHarness(t)
		p := androidwhatsapp.Parser{}
		in := fx.Bundle(h, remoteFor("android-modern-2026")).Input(h, p, false)
		in.Limits.MaxNotes = limit
		return h.RunLenient(p, in)
	}
	detector := map[string]string{"generation": "modern", "app": "whatsapp", "database_user": "10"}
	full := run(parse.DefaultLimits().MaxNotes)
	if parse.DefaultLimits().MaxNotes != 64 || full.Err != nil || len(full.Notes) > 64 {
		t.Fatalf("limit %d, err %v, %d notes", parse.DefaultLimits().MaxNotes, full.Err, len(full.Notes))
	}
	if full.Notes["rows.message"] == "" {
		t.Errorf("rows.message is missing: %v", full.Notes)
	}
	for k, want := range detector {
		if full.Notes[k] != want {
			t.Errorf("cap 64: note %s = %q, want %q", k, full.Notes[k], want)
		}
	}
	capped := run(6)
	if capped.Err != nil || len(capped.Notes) != 6 {
		t.Fatalf("cap 6: err %v, %d notes (the cap must bite: the run writes %d)", capped.Err, len(capped.Notes), len(full.Notes))
	}
	for k, want := range detector {
		if capped.Notes[k] != want {
			t.Errorf("cap 6: note %s = %q, want %q (written before the mapping, so kept)", k, capped.Notes[k], want)
		}
	}
	if capped.Notes["rows.message"] == "" {
		t.Errorf("cap 6: rows.message was dropped: %v", capped.Notes)
	}
}
