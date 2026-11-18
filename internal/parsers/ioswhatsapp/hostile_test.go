package ioswhatsapp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The hostile rows of spec 10 that earlier tasks do not cover. Each test builds its database here, runs the parser through the
// harness and requires: no panic, no invented value, and a stated warning or raw marker. A bound is a typed error or a counted
// warning, never a silent cut.

// chatBuilder is buildChat with the builder and the page size handed back, for a test that needs a snapshot or a journal.
func chatBuilder(pageSize int, fill func(tb chatTables)) (*sqlitetest.Builder, chatTables) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: pageSize})
	tb := chatTables{}
	for _, name := range chatOrder {
		d := chatDDL[name]
		tb[name] = chatTable{t: b.CreateTable(name, d.sql), cols: d.cols}
	}
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	z.Insert(4, nil, "WAChatSession", int64(0), int64(0))
	z.Insert(9, nil, "WAMessage", int64(0), int64(0))
	fill(tb)
	return b, tb
}

// hostileRun parses db (with wal and journal when not nil) at the consumer backup domain with the given MemBudget (0: the
// default). It does not fail on an error: the tests judge it.
func hostileRun(t *testing.T, db, wal, journal []byte, budget int64) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	if journal != nil {
		files["journal"] = journal
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "hostile", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	in := b.Input(h, p, false)
	if budget > 0 {
		in.Budget = parse.NewBudget(budget).View()
	}
	return h.RunLenient(p, in)
}

// typedOutcome fails the test on an error that is not one of the parser's or the decoder's typed answers to a hostile file.
func typedOutcome(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil || parsertest.HostileOK(err) {
		return
	}
	for _, typed := range []error{ioswhatsapp.ErrUnsupportedModel, ioswhatsapp.ErrNoWhatsAppTables} {
		if errors.Is(err, typed) {
			return
		}
	}
	t.Fatalf("%s: untyped error %v", label, err)
}

// A database cut inside the message table: the pages that are gone are stated by the decoder as warnings, and nothing is
// guessed about the rows that were lost. Every cut length holds the contract (typed errors only, no refused record, no record
// the database did not hold), and a cut that loses rows is never an empty complete result: an error or a warning says so.
func TestIOSWhatsAppChatsTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 600
	full, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		for i := int64(1); i <= rows; i++ {
			tb["ZWAMESSAGE"].put(i, msgRow(map[string]any{"ZTEXT": fmt.Sprintf("m%d %s", i, strings.Repeat("x", 60))}))
		}
	})
	const ps = 4096
	pages := len(full) / ps
	var stated int
	for cut := pages - 1; cut >= pages-8 && cut >= 3; cut-- {
		res := hostileRun(t, slices.Clone(full[:cut*ps]), nil, nil, 0)
		typedOutcome(t, fmt.Sprintf("cut at page %d", cut), res.Err)
		if res.Refused != 0 || len(res.Violations) != 0 {
			t.Fatalf("cut at page %d: refused %d, violations %v", cut, res.Refused, res.Violations)
		}
		for _, r := range res.Records {
			var id int
			if _, err := fmt.Sscanf(r.Body, "m%d ", &id); err != nil || id < 1 || id > rows {
				t.Fatalf("cut at page %d: the record %q was not in the database", cut, r.Body)
			}
		}
		if len(res.Records) == rows {
			continue
		}
		if res.Err == nil && len(res.Warnings) == 0 {
			t.Errorf("cut at page %d: %d of %d rows, no error and no warning: an empty or short complete result", cut, len(res.Records), rows)
			continue
		}
		stated++
	}
	if stated == 0 {
		t.Error("no cut lost a row: the check proves nothing")
	}
}

// A hot journal is rolled back: the record reflects the rolled-back state and a journal-hot warning says so.
func TestIOSWhatsAppChatsHotJournalIsRolledBack(t *testing.T) {
	const ps = 512
	b, tb := chatBuilder(ps, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZTEXT": "original text of the message"}))
	})
	snap := b.Snapshot()
	tb["ZWAMESSAGE"].update(1, msgRow(map[string]any{"ZTEXT": "interrupted rewrite"}))
	root := tb["ZWAMESSAGE"].t.Root()
	db := b.Bytes()
	j := b.NewJournal(ps, 0x5eed0003, snap.Pages())
	j.Record(root, snap.Page(root))
	res := hostileRun(t, db, nil, j.Bytes(), 0)
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

// Plan 4I2 Task 6: row 45 of ios-chatstorage lies only in a committed WAL frame and row 46 only in an uncommitted one. The record
// of row 45 points into the WAL artifact (its artifact id, path and range), and row 46 is not a record at all.
func TestIOSWhatsAppChatsWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-chatstorage"), "ios-chatstorage")
	wal, ok := b.ByRole["wal"]
	if !ok || wal.ID == "" || wal.ID == b.Primary.ID || wal.Source.RemotePath == b.Primary.Source.RemotePath {
		t.Fatalf("the bundle has no distinct WAL artifact: %+v", wal)
	}
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied", "whatsapp-ios-join-undecided")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	byRow := map[int64]records.Record{}
	for _, r := range res.Records {
		byRow[rowidOf(r.Locator)] = r
	}
	r45, has45 := byRow[45]
	if !has45 {
		t.Fatal("row 45 (committed WAL frame) is no record")
	}
	if r45.ArtifactID != wal.ID || r45.SourcePath != wal.Source.RemotePath || r45.Range == nil || r45.Range.Offset+r45.Range.Length > wal.Size {
		t.Errorf("row 45: artifact %s path %s range %+v, want the WAL artifact %s at %s", r45.ArtifactID, r45.SourcePath, r45.Range, wal.ID, wal.Source.RemotePath)
	}
	if _, has := byRow[46]; has {
		t.Error("row 46 lies only in an uncommitted frame and is a record")
	}
}

// Every foreign key of spec 10 that points at a missing row is stated, never read as an absence or invented: ZCHATSESSION,
// ZGROUPMEMBER, ZMEDIAITEM and ZMESSAGEINFO each name a row that does not exist.
func TestIOSWhatsAppChatsDanglingKeysAreStated(t *testing.T) {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZCHATSESSION": int64(999)}))
		tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZCHATSESSION": int64(1), "ZGROUPMEMBER": int64(999)}))
		tb["ZWAMESSAGE"].put(3, msgRow(map[string]any{"ZCHATSESSION": int64(1), "ZMESSAGETYPE": int64(1), "ZTEXT": nil, "ZMEDIAITEM": int64(999)}))
		tb["ZWAMESSAGE"].put(4, msgRow(map[string]any{"ZCHATSESSION": int64(1), "ZMESSAGEINFO": int64(999)}))
	})
	recs, _ := runChat(t, db, nil, "whatsapp-ios-join-undecided")
	for _, c := range []struct {
		row       int64
		key, want string
	}{
		{1, "chat_state", "no-ZWACHATSESSION-row"},
		{2, "sender_state", "no-ZWAGROUPMEMBER-row"},
		{4, "receipt_info_state", "no-ZWAMESSAGEINFO-row"},
	} {
		r, ok := recs[c.row]
		if !ok || rawOf(r)[c.key] != c.want {
			t.Errorf("message %d: raw.%s = %v, want %q (raw %v)", c.row, c.key, rawOf(r)[c.key], c.want, rawOf(r))
		}
	}
	// the dangling media key: the converse direction names no row and the forward key names none, so the media row is stated
	// absent (never undecided, never an attachment out of nothing)
	r3 := recs[3]
	if rawOf(r3)["media_state"] != "no-ZWAMEDIAITEM-row" || rawOf(r3)["join_undecided"] != nil || len(attachmentsOf(r3)) != 0 {
		t.Errorf("message 3: media_state %v join_undecided %v attachments %v, want no-ZWAMEDIAITEM-row, none, none", rawOf(r3)["media_state"], rawOf(r3)["join_undecided"], attachmentsOf(r3))
	}
}

// ZCONTACTJID values that are no address: no @, many @, a NUL, a megabyte, a device suffix. The stored text is kept as raw (cleaned,
// never cut, never repaired), and no participant is invented from a JID that is not an address; the megabyte one is stated
// too-long and its text is not copied.
func TestIOSWhatsAppChatsHostileJIDs(t *testing.T) {
	huge := strings.Repeat("9", 1<<20) + "@s.whatsapp.net"
	jids := []struct {
		name, jid string
		state     string // raw.chat_state, "" when none
		class     string // raw.chat_class, "" when none
		from      bool   // a from participant may exist
	}{
		{"no at", "15550000001", "", "unparseable", false},
		{"many at", "1@2@3@s.whatsapp.net", "", "", true}, // the shared parser splits at the last @; the address is still the stored text
		{"NUL", "1555\x00@s.whatsapp.net", "altered", "", false},
		{"1 MiB", huge, "too-long", "", false},
		{"device suffix", "15550000001:12@s.whatsapp.net", "", "", true},
	}
	db, _ := buildChat(nil, func(tb chatTables) {
		for i, c := range jids {
			s := int64(i + 1)
			tb["ZWACHATSESSION"].put(s, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": c.jid})
			tb["ZWAMESSAGE"].put(s, msgRow(map[string]any{"ZCHATSESSION": s}))
		}
	})
	recs, res := runChat(t, db, nil)
	if len(res.Records) != len(jids) {
		t.Fatalf("%d records for %d messages", len(res.Records), len(jids))
	}
	for i, c := range jids {
		r := recs[int64(i+1)]
		raw := rawOf(r)
		if c.state != "" && raw["chat_state"] != c.state {
			t.Errorf("%s: chat_state %v, want %q", c.name, raw["chat_state"], c.state)
		}
		if c.class != "" && raw["chat_class"] != c.class {
			t.Errorf("%s: chat_class %v, want %q", c.name, raw["chat_class"], c.class)
		}
		from := fromOf(r, "from")
		if !c.from && (from != "" || !unknownParticipants(r)) {
			t.Errorf("%s: a participant %q was invented from a JID that is no address (participants_unknown %v)", c.name, from, unknownParticipants(r))
		}
		if from != "" && from != c.jid {
			t.Errorf("%s: from %q is not the stored JID %q: the address is as stored or none", c.name, from, c.jid)
		}
		if strings.Contains(fmt.Sprint(raw), strings.Repeat("9", 4096)) {
			t.Errorf("%s: the megabyte JID's text was copied into raw", c.name)
		}
	}
	if raw := rawOf(recs[2]); raw["chat_jid"] != nil && raw["chat_jid"] != "1@2@3@s.whatsapp.net" {
		t.Errorf("many at: raw.chat_jid %v changed the stored text", raw["chat_jid"])
	}
	// CleanText turns the NUL into U+FFFD: the one cleaned form, stated with its flag
	if raw := rawOf(recs[3]); raw["chat_jid"] != "1555�@s.whatsapp.net" || raw["chat_state"] != "altered" {
		t.Errorf("NUL: raw.chat_jid %q chat_state %v, want the cleaned 1555\ufffd@s.whatsapp.net and altered", raw["chat_jid"], raw["chat_state"])
	}
	// a device suffix is a user address as stored: class user, the author is the stored text, no state of its own
	if raw := rawOf(recs[5]); raw["chat_class"] != "user" || raw["chat_state"] != nil || raw["chat_jid"] != "15550000001:12@s.whatsapp.net" {
		t.Errorf("device suffix: chat_class %v chat_state %v chat_jid %v", raw["chat_class"], raw["chat_state"], raw["chat_jid"])
	}
}

// A 4 MiB text is cut at 256 KiB and flagged with its full length; a 64 MiB ZMETADATA is only a length and a SHA-256 and the parse
// stays inside a MemBudget of 256 MiB (no ErrBudget); a megabyte vCard is the body of its contact card, cut and flagged.
func TestIOSWhatsAppChatsHugeValuesAreCapped(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	meta := make([]byte, 64<<20)
	for i := range meta {
		meta[i] = byte(i * 31)
	}
	sum := sha256.Sum256(meta)
	blob := make([]byte, 3<<20) // a BLOB in a column that raw states as stored: only its length may reach raw
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZTEXT": big}))
		tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil, "ZMEDIAITEM": int64(1)}))
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": int64(2), "ZMETADATA": meta})
		tb["ZWAMESSAGE"].put(3, msgRow(map[string]any{"ZMESSAGETYPE": int64(4), "ZTEXT": nil, "ZMEDIAITEM": int64(2)}))
		tb["ZWAMESSAGE"].put(4, msgRow(map[string]any{"ZMESSAGETYPE": blob}))
		tb["ZWAMEDIAITEM"].put(2, map[string]any{"ZMESSAGE": int64(3), "ZVCARDSTRING": "BEGIN:VCARD\r\nNOTE:" + strings.Repeat("v", 1<<20) + "\r\nEND:VCARD\r\n"})
	})
	res := hostileRun(t, db, nil, nil, 256<<20)
	if res.Err != nil {
		t.Fatalf("Parse at MemBudget 256 MiB: %v", res.Err)
	}
	recs := map[int64]records.Record{}
	for _, r := range res.Records {
		recs[rowidOf(r.Locator)] = r
	}
	if len(recs) != 4 {
		t.Fatalf("%d records, want 4", len(recs))
	}
	r1 := recs[1]
	if r1.Payload["body_truncated"] != true || r1.Payload["body_total_bytes"] != int64(4<<20) || len(r1.Body) > 256<<10 || len(r1.Body) < 200<<10 {
		t.Errorf("message 1: body %d bytes, truncated %v total %v, want the 4 MiB text cut at 256 KiB with its total", len(r1.Body), r1.Payload["body_truncated"], r1.Payload["body_total_bytes"])
	}
	md, _ := rawOf(recs[2])["ZMETADATA"].(map[string]any)
	if md["bytes"] != int64(64<<20) || md["sha256"] != hex.EncodeToString(sum[:]) || len(md) != 2 {
		t.Errorf("message 2: raw.ZMETADATA = %v, want {bytes 67108864, sha256 %x} and nothing else", md, sum)
	}
	if got := rawOf(recs[4])["ZMESSAGETYPE"]; !reflect.DeepEqual(got, map[string]any{"blob_len": int64(3 << 20)}) {
		t.Errorf("message 4: raw.ZMESSAGETYPE = %.80v, want {blob_len 3145728} and no base64", got)
	}
	if r3 := recs[3]; len(r3.Body) == 0 || len(r3.Body) > 256<<10 || r3.Payload["body_truncated"] != true {
		t.Errorf("message 3: body %d bytes, truncated %v, want the 1 MiB vCard cut at 256 KiB and flagged", len(r3.Body), r3.Payload["body_truncated"])
	}
}

// A message type or group event type this version has never seen is unknown with its value raw: never a label, never a kind.
func TestIOSWhatsAppChatsUnknownFutureTypes(t *testing.T) {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZMESSAGETYPE": int64(4242)}))
		tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZMESSAGETYPE": int64(6), "ZGROUPEVENTTYPE": int64(77)}))
	})
	recs, _ := runChat(t, db, nil)
	r1, r2 := rawOf(recs[1]), rawOf(recs[2])
	if recs[1].Payload["kind"] != "unknown" || r1["ZMESSAGETYPE"] != int64(4242) || r1["label_ZMESSAGETYPE"] != nil {
		t.Errorf("type 4242: kind %v raw %v", recs[1].Payload["kind"], r1)
	}
	if r2["ZGROUPEVENTTYPE"] != int64(77) || r2["label_ZGROUPEVENTTYPE"] != nil {
		t.Errorf("group event 77: raw %v", r2)
	}
	if k := recs[2].Payload["kind"]; k == "text" || k == "image" {
		t.Errorf("group event 77: kind %v", k)
	}
}

// The notes of the identity (container, app, entity numbers) are written before any mapping, so they survive the host's cap on
// notes, where the first write of a key wins and the later notes are the ones dropped. The cap is the real 64 and then lowered
// so that it bites on this fixture.
func TestIOSWhatsAppChatsNotesSurviveTheNoteCap(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-chatstorage")
	run := func(limit int) parsertest.Result {
		h := parsertest.NewHarness(t)
		p := ioswhatsapp.Parser{}
		in := bundleOf(h, fx, "ios-chatstorage").Input(h, p, false)
		in.Limits.MaxNotes = limit
		return h.RunLenient(p, in)
	}
	want := map[string]string{"container": "backup_domain", "app": "whatsapp", "entity.WAMessage": "9"}
	full := run(parse.DefaultLimits().MaxNotes)
	if parse.DefaultLimits().MaxNotes != 64 || full.Err != nil || len(full.Notes) > 64 || len(full.Notes) < 7 {
		t.Fatalf("limit %d, err %v, %d notes", parse.DefaultLimits().MaxNotes, full.Err, len(full.Notes))
	}
	for k, v := range want {
		if full.Notes[k] != v {
			t.Errorf("cap 64: note %s = %q, want %q", k, full.Notes[k], v)
		}
	}
	capped := run(4)
	if capped.Err != nil || len(capped.Notes) != 4 {
		t.Fatalf("cap 4: err %v, %d notes (the cap must bite: the run writes %d)", capped.Err, len(capped.Notes), len(full.Notes))
	}
	for k, v := range want {
		if capped.Notes[k] != v {
			t.Errorf("cap 4: note %s = %q, want %q (written before the mapping, so kept)", k, capped.Notes[k], v)
		}
	}
}
