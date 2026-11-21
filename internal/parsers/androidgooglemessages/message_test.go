package androidgooglemessages_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const tT = int64(1700000000000) // 2023-11-14T22:13:20Z

// v2DDL is the CREATE TABLE statement of table in aosp-v2.sql (the cited snapshot the Go-built databases use).
func v2DDL(t testing.TB, table string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "google-messages", "aosp-v2.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "CREATE TABLE "+table+" (") {
			return strings.TrimSuffix(strings.TrimSpace(l), ";")
		}
	}
	t.Fatalf("no CREATE TABLE %s in aosp-v2.sql", table)
	return ""
}

// columnsOf is the column names of a CREATE TABLE statement in order (a comma inside parentheses does not split; table
// constraints and the FOREIGN KEY clauses of the AOSP DDL are skipped).
func columnsOf(ddl string) []string {
	body := ddl[strings.Index(ddl, "(")+1 : strings.LastIndex(ddl, ")")]
	var parts []string
	depth, start := 0, 0
	for i, c := range body {
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, body[start:i])
			start = i + 1
		}
	}
	parts = append(parts, body[start:])
	var cols []string
	for _, p := range parts {
		f := strings.Fields(p)
		if len(f) == 0 || f[0] == "UNIQUE" || f[0] == "PRIMARY" || f[0] == "FOREIGN" || f[0] == "CHECK" {
			continue
		}
		cols = append(cols, f[0])
	}
	return cols
}

// mrow is one Go-built row: its rowid and the stored values by column (a column not named stores the default of
// messageDefaults, an explicit nil stores NULL).
type mrow struct {
	id int64
	kw map[string]any
}

func messageDefaults() map[string]any {
	return map[string]any{
		"conversation_id": int64(1), "sender_id": int64(2), "self_id": int64(1), "sent_timestamp": tT, "received_timestamp": tT + 1000,
		"message_protocol": int64(0), "message_status": int64(100), "seen": int64(1), "read": int64(1), "sms_priority": int64(0),
		"sms_message_size": int64(0), "mms_expiry": int64(0), "raw_status": int64(0), "retry_start_timestamp": int64(0),
	}
}

// builtDB is a Go-built bugle_db (user version 2) with the five tables of aosp-v2.sql and the given messages rows.
func builtDB(t testing.TB, rows []mrow, more ...func(*sqlitetest.Table)) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 2})
	for _, name := range []string{"conversations", "messages", "parts", "participants", "conversation_participants"} {
		ddl := v2DDL(t, name)
		tb := b.CreateTable(name, ddl)
		if name != "messages" {
			continue
		}
		cols := columnsOf(ddl)
		def := messageDefaults()
		for _, r := range rows {
			vals := make([]any, len(cols))
			for i, c := range cols {
				if c == "_id" {
					continue // the rowid alias stores NULL
				}
				v, ok := r.kw[c]
				if !ok {
					v = def[c]
				}
				vals[i] = v
			}
			tb.Insert(r.id, vals...)
		}
		for _, f := range more {
			f(tb)
		}
	}
	return b.Bytes()
}

// parsedRows parses a Go-built database at the Google Messages app path and indexes the messages records by rowid.
func parsedRows(t *testing.T, rows []mrow, more ...func(*sqlitetest.Table)) (parsertest.Result, map[int64]records.Record) {
	t.Helper()
	res := parseAt(t, remoteApp, dbOnly(builtDB(t, rows, more...)))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	return res, byRowid(t, res)
}

// byRowid indexes the messages records of a run by the rowid of their locator.
func byRowid(t testing.TB, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		rest, ok := strings.CutPrefix(r.Locator, "sqlite:table=messages;rowid=")
		if !ok {
			continue
		}
		num, _, _ := strings.Cut(rest, ";")
		id, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("locator %q", r.Locator)
		}
		out[id] = r
	}
	return out
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func unknownOf(r records.Record) map[string]any {
	m, _ := rawOf(r)["unknown_columns"].(map[string]any)
	return m
}

// has reports whether the raw key is present.
func has(r records.Record, key string) bool { _, ok := rawOf(r)[key]; return ok }

// fixtureRecords parses a committed variant at the app path and indexes its messages records by rowid.
func fixtureRecords(t *testing.T, variant string) (parsertest.Result, map[int64]records.Record) {
	t.Helper()
	res := parseAt(t, remoteApp, fixtureFiles(t, variant))
	if res.Err != nil {
		t.Fatalf("%s: Parse: %v", variant, res.Err)
	}
	return res, byRowid(t, res)
}

func str(r records.Record, key string) string { s, _ := r.Payload[key].(string); return s }

// Spec 6.1, C1: the channel is the cited transport (0 sms, 1 and 2 mms) or the free token unknown; the raw protocol is as
// stored, a NULL is stated as such and a wrongly typed cell is listed, never read as a value.
func TestGoogleMessagesChannelFromCitedProtocolOnly(t *testing.T) {
	proto := []any{int64(0), int64(1), int64(2), int64(-1), int64(3), int64(7), int64(99), nil, "0", 1.0}
	want := []string{"sms", "mms", "mms", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown", "unknown"}
	var rows []mrow
	for i, p := range proto {
		rows = append(rows, mrow{int64(i + 1), map[string]any{"message_protocol": p}})
	}
	_, recs := parsedRows(t, rows)
	for i, p := range proto {
		r, ok := recs[int64(i+1)]
		if !ok {
			t.Fatalf("no record for protocol %v", p)
		}
		if got := str(r, "channel"); got != want[i] {
			t.Errorf("protocol %#v: channel %q, want %q", p, got, want[i])
		}
		switch v := p.(type) {
		case int64:
			if rawOf(r)["message_protocol"] != v || has(r, "message_protocol_state") || unknownOf(r) != nil {
				t.Errorf("protocol %d: raw %v", v, rawOf(r))
			}
		case nil:
			if rawOf(r)["message_protocol_state"] != "null" || has(r, "message_protocol") || unknownOf(r) != nil {
				t.Errorf("protocol NULL: raw %v", rawOf(r))
			}
		default:
			if unknownOf(r)["message_protocol"] != "type-mismatch" || has(r, "message_protocol") || has(r, "message_protocol_state") {
				t.Errorf("protocol %#v: raw %v", p, rawOf(r))
			}
		}
	}
	if rawOf(recs[1])["label_message_protocol"] != "sms" || rawOf(recs[3])["label_message_protocol"] != "mms_push_notification" ||
		rawOf(recs[4])["label_message_protocol"] != "unknown" || has(recs[5], "label_message_protocol") {
		t.Errorf("labels: %v %v %v %v", rawOf(recs[1]), rawOf(recs[3]), rawOf(recs[4]), rawOf(recs[5]))
	}
	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		for _, r := range res.Records {
			if c := str(r, "channel"); c != "sms" && c != "mms" && c != "unknown" {
				t.Errorf("%s %s: channel %q", v, r.Locator, c)
			}
		}
	}
}

// C1a: the channel token unknown is a valid message payload and the Summary names it.
func TestMessageChannelUnknownIsAValidToken(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	r, ok := recs[12]
	if !ok {
		t.Fatal("no record for M12")
	}
	if str(r, "channel") != "unknown" {
		t.Fatalf("M12 channel %q", str(r, "channel"))
	}
	if err := message.Validate(r.Payload); err != nil {
		t.Errorf("message.Validate: %v", err)
	}
	if !strings.HasPrefix(r.Summary, "unknown in ") {
		t.Errorf("Summary %q does not start with %q", r.Summary, "unknown in ")
	}
}

// Spec 6.2, R1 to R3, B7: a status outside the cited set (or NULL, or not an integer) is unknown with the raw value as
// stored and no label; direction comes from the cited sets only.
func TestGoogleMessagesUnknownCodesStayUnknownWithRaw(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	m := func(n int64) records.Record { return recs[n] }
	if r := m(5); str(r, "direction") != "in" || str(r, "status") != "unknown" || rawOf(r)["label_message_protocol"] != "mms_push_notification" {
		t.Errorf("M5: %v", r.Payload)
	}
	if r := m(9); str(r, "direction") != "in" || str(r, "status") != "unknown" || rawOf(r)["label_message_status"] != "incoming_expired_or_not_available" {
		t.Errorf("M9: %v", r.Payload)
	}
	if r := m(10); str(r, "direction") != "unknown" || str(r, "status") != "unknown" || rawOf(r)["label_message_status"] != "unknown" {
		t.Errorf("M10: %v", r.Payload)
	}
	if r := m(11); str(r, "direction") != "unknown" || str(r, "status") != "unknown" || has(r, "label_message_status") || rawOf(r)["message_status"] != int64(150) {
		t.Errorf("M11: %v", r.Payload)
	}
	if r := m(12); str(r, "channel") != "unknown" || rawOf(r)["label_message_protocol"] != "unknown" || rawOf(r)["message_protocol"] != int64(-1) {
		t.Errorf("M12: %v", r.Payload)
	}
	if r := m(13); str(r, "channel") != "unknown" || has(r, "label_message_protocol") || rawOf(r)["message_protocol"] != int64(7) {
		t.Errorf("M13: %v", r.Payload)
	}
	if r := m(14); str(r, "channel") != "unknown" || rawOf(r)["message_protocol_state"] != "null" || has(r, "message_protocol") {
		t.Errorf("M14: %v", r.Payload)
	}
	rows := []mrow{
		{1, map[string]any{"message_status": int64(150)}},
		{2, map[string]any{"message_status": int64(99)}},
		{3, map[string]any{"message_status": int64(-3)}},
		{4, map[string]any{"message_status": nil}},
		{5, map[string]any{"message_status": math.NaN()}},
	}
	_, got := parsedRows(t, rows)
	for id, v := range map[int64]int64{1: 150, 2: 99, 3: -3} {
		r := got[id]
		if str(r, "direction") != "unknown" || str(r, "status") != "unknown" || has(r, "label_message_status") || rawOf(r)["message_status"] != v || unknownOf(r) != nil {
			t.Errorf("status %d: payload %v", v, r.Payload)
		}
	}
	if r := got[4]; rawOf(r)["message_status_state"] != "null" || has(r, "message_status") || unknownOf(r) != nil || str(r, "direction") != "unknown" {
		t.Errorf("status NULL: raw %v", rawOf(r))
	}
	if r := got[5]; unknownOf(r)["message_status"] == nil || has(r, "message_status_state") || has(r, "message_status") || str(r, "direction") != "unknown" {
		t.Errorf("status NaN: raw %v (a NaN is undecided, never NULL)", rawOf(r))
	}
}

// engineCount is the number of rows the engine reads from messages over the live view of the files.
func engineCount(t *testing.T, files map[string][]byte) int {
	t.Helper()
	f := sqlitedb.Files{DB: bytes.NewReader(files["db"]), DBSize: int64(len(files["db"]))}
	if w, ok := files["wal"]; ok {
		f.WAL, f.WALSize = bytes.NewReader(w), int64(len(w))
	}
	if j, ok := files["journal"]; ok {
		f.Journal, f.JournalSize = bytes.NewReader(j), int64(len(j))
	}
	db, err := sqlitedb.Open(t.Context(), f, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), "messages", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// C2: every messages row of the live view is a record of its own, whatever its link, status or sender (gm-hostile
// included); a row with no sms_message_uri (M6, a draft) is a record.
func TestGoogleMessagesEveryRowIsARecord(t *testing.T) {
	ran := 0
	for _, v := range variants {
		files := fixtureFiles(t, v)
		if len(parsertest.LoadFixture(t, "testdata", v).Expect.Records) == 0 {
			continue
		}
		ran++
		res := parseAt(t, remoteApp, files)
		if res.Err != nil {
			t.Fatalf("%s: %v", v, res.Err)
		}
		if got, want := len(byRowid(t, res)), engineCount(t, files); got != want {
			t.Errorf("%s: %d messages records, the engine counts %d rows", v, got, want)
		}
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	if r, ok := recs[6]; !ok || has(r, "sms_message_uri") || has(r, "sms_link") || has(r, "sms_link_state") {
		t.Errorf("M6 (a draft without a link): present %v, raw %v", ok, rawOf(r))
	}
}

// C2: a bugle_db message is never merged with a telephony one: no record carries a guid or target_guid, the link is
// stated only in raw, and a linked message has the same payload keys outside raw as an unlinked one.
func TestGoogleMessagesAndTelephonyCopiesAreNeverMerged(t *testing.T) {
	for _, v := range variants {
		res := parseAt(t, remoteApp, fixtureFiles(t, v))
		for _, r := range res.Records {
			for _, k := range []string{"guid", "target_guid", "sms_link", "sms_message_uri"} {
				if _, ok := r.Payload[k]; ok {
					t.Errorf("%s %s: payload key %s outside raw", v, r.Locator, k)
				}
			}
		}
	}
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	keys := func(r records.Record) map[string]bool {
		out := map[string]bool{}
		for k := range r.Payload {
			if k != "raw" {
				out[k] = true
			}
		}
		return out
	}
	linked, unlinked := recs[1], recs[10]
	if !has(linked, "sms_link") || has(unlinked, "sms_link") {
		t.Fatalf("M1 link %v, M10 link %v", has(linked, "sms_link"), has(unlinked, "sms_link"))
	}
	a, b := keys(linked), keys(unlinked)
	for k := range a {
		if !b[k] {
			t.Errorf("M1 has the key %s, M10 does not", k)
		}
	}
	for k := range b {
		if !a[k] {
			t.Errorf("M10 has the key %s, M1 does not", k)
		}
	}
}

// Spec 6.7: read is 0 or 1 only; any other value is absent with its raw value; an unreadable cell is listed.
func TestGoogleMessagesReadState(t *testing.T) {
	_, recs := fixtureRecords(t, "gm-aosp-v2")
	if recs[1].Payload["read"] != true || recs[4].Payload["read"] != false {
		t.Errorf("M1 read %v, M4 read %v", recs[1].Payload["read"], recs[4].Payload["read"])
	}
	if _, ok := recs[31].Payload["read"]; ok || rawOf(recs[31])["read"] != int64(2) {
		t.Errorf("M31: read %v, raw %v", recs[31].Payload["read"], rawOf(recs[31]))
	}
	_, hostile := fixtureRecords(t, "gm-hostile")
	h14 := hostile[115] // H14: a wrongly typed read cell
	if _, ok := h14.Payload["read"]; ok || unknownOf(h14)["read"] != "type-mismatch" || has(h14, "read") {
		t.Errorf("H14: read %v, raw %v", h14.Payload["read"], rawOf(h14))
	}
}
