package iossms_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const ddlDir = "../../../tools/fixtures/schemas/ios/"

// ddlTable is one CREATE TABLE statement of a DDL snapshot with its stored (non-generated) columns in order.
type ddlTable struct {
	sql  string
	cols []string
}

// tablesOf reads the CREATE TABLE lines of a snapshot (edit may rewrite a statement first).
func tablesOf(t *testing.T, file string, edit func(string) string) map[string]ddlTable {
	t.Helper()
	data, err := os.ReadFile(ddlDir + file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ddlTable{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CREATE TABLE ") {
			continue
		}
		name := strings.Fields(line)[2]
		if edit != nil {
			line = edit(line)
		}
		body := line[strings.Index(line, "(")+1 : strings.LastIndex(line, ")")]
		var cols []string
		depth, start := 0, 0
		parts := []string{}
		for i, c := range body {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					parts = append(parts, body[start:i])
					start = i + 1
				}
			}
		}
		parts = append(parts, body[start:])
		for _, p := range parts {
			f := strings.Fields(p)
			switch strings.ToUpper(f[0]) {
			case "UNIQUE", "PRIMARY", "FOREIGN", "CHECK", "CONSTRAINT":
				continue
			}
			if strings.Contains(strings.ToUpper(p), "GENERATED ALWAYS") {
				continue
			}
			cols = append(cols, f[0])
		}
		out[name] = ddlTable{sql: line, cols: cols}
	}
	return out
}

// memDB is an in-memory sms.db built from a DDL snapshot.
type memDB struct {
	b    *sqlitetest.Builder
	tbl  map[string]*sqlitetest.Table
	cols map[string][]string
	next map[string]int64
}

func newMem(t *testing.T, file string, edit func(string) string, only ...string) *memDB {
	t.Helper()
	d := &memDB{b: sqlitetest.New(sqlitetest.Options{PageSize: 2048}), tbl: map[string]*sqlitetest.Table{}, cols: map[string][]string{}, next: map[string]int64{}}
	all := tablesOf(t, file, edit)
	for _, n := range []string{"message", "handle", "chat", "chat_message_join", "chat_handle_join", "attachment", "message_attachment_join", "chat_recoverable_message_join"} {
		if len(only) > 0 && !contains(only, n) {
			continue
		}
		tb, ok := all[n]
		if !ok {
			continue
		}
		d.tbl[n] = d.b.CreateTable(n, tb.sql)
		d.cols[n] = tb.cols
	}
	return d
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func newSMS(t *testing.T, only ...string) *memDB { return newMem(t, "sms-ios17.sql", nil, only...) }

// add inserts a row; id is the ROWID (for a table without one, the next sequence number); columns not in over are NULL.
func (d *memDB) add(table string, id int64, over map[string]any) {
	cols := d.cols[table]
	vals := make([]any, len(cols))
	for i, c := range cols {
		if c == "ROWID" {
			vals[i] = id
		} else {
			vals[i] = over[c]
		}
	}
	d.tbl[table].Insert(id, vals...)
}

// join adds a row to a table without a ROWID alias.
func (d *memDB) join(table string, over map[string]any) {
	d.next[table]++
	d.add(table, d.next[table], over)
}

// msg is the default message row: an incoming iMessage from handle 1, read, at 2023 plus id seconds.
func (d *memDB) msg(id int64, over map[string]any) {
	row := map[string]any{
		"guid": fmt.Sprintf("G%d", id), "text": fmt.Sprintf("m%d", id), "handle_id": int64(1), "service": "iMessage",
		"date": int64(704628000) + id, "is_from_me": int64(0), "is_read": int64(1), "item_type": int64(0),
		"associated_message_type": int64(0), "error": int64(0), "is_delivered": int64(0), "is_sent": int64(0),
	}
	for k, v := range over {
		row[k] = v
	}
	d.add("message", id, row)
}

// out is msg for a sent message.
func (d *memDB) out(id int64, over map[string]any) {
	row := map[string]any{"is_from_me": int64(1), "is_sent": int64(1)}
	for k, v := range over {
		row[k] = v
	}
	d.msg(id, row)
}

// dims adds three handles and two chats: chat 1 (1:1, handle 1) and chat 2 (group "Group", members 3, 1, 2).
func (d *memDB) dims() {
	d.add("handle", 1, map[string]any{"id": "+15551230001", "service": "iMessage"})
	d.add("handle", 2, map[string]any{"id": "alice@example.test", "service": "iMessage"})
	d.add("handle", 3, map[string]any{"id": "+15551230003", "service": "SMS"})
	d.add("chat", 1, map[string]any{"guid": "c1", "style": int64(45)})
	d.add("chat", 2, map[string]any{"guid": "c2", "style": int64(43), "display_name": "Group"})
	d.join("chat_handle_join", map[string]any{"chat_id": int64(1), "handle_id": int64(1)})
	for _, h := range []int64{3, 1, 2} {
		d.join("chat_handle_join", map[string]any{"chat_id": int64(2), "handle_id": h})
	}
}

func (d *memDB) inChat(msg, chat int64) {
	d.join("chat_message_join", map[string]any{"chat_id": chat, "message_id": msg, "message_date": int64(1)})
}

const (
	dbRemote  = "/private/var/mobile/Library/SMS/sms.db"
	dbLogical = "ios:HomeDomain/Library/SMS/sms.db"
)

func runBytes(t *testing.T, data []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "mem", Files: map[string][]byte{"db": data}}.BundleIOS(h, dbRemote, dbLogical)
	p := iossms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	for _, v := range res.Violations {
		t.Errorf("contract violation: %v", v)
	}
	return res
}

func (d *memDB) run(t *testing.T) parsertest.Result { t.Helper(); return runBytes(t, d.b.Bytes()) }

// rec returns the record of message row id.
func rec(t *testing.T, res parsertest.Result, id int64) records.Record {
	t.Helper()
	loc := fmt.Sprintf("sqlite:table=message;rowid=%d", id)
	for _, r := range res.Records {
		if r.Locator == loc {
			return r
		}
	}
	t.Fatalf("no record %s among %d", loc, len(res.Records))
	return records.Record{}
}

func rawOfRec(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func partsOf(r records.Record) []map[string]any {
	l, _ := r.Payload["participants"].([]any)
	var out []map[string]any
	for _, e := range l {
		out = append(out, e.(map[string]any))
	}
	return out
}

func addrsOf(r records.Record) []string {
	var out []string
	for _, p := range partsOf(r) {
		out = append(out, p["address"].(string))
	}
	return out
}

func timeOf(r records.Record, kind string) *records.Time {
	for _, nt := range r.Times {
		if nt.Kind == kind {
			tm := nt.Time
			return &tm
		}
	}
	return nil
}

// is reports whether got prints as want (an int64, a json number and a string of digits all compare by their text).
func is(got any, want any) bool { return got != nil && fmt.Sprint(got) == fmt.Sprint(want) }

// typedstream builds the smallest stream the decoder reads: the header, the class name NSString, the "+"
// encoding and the length-prefixed text.
func typedstream(text []byte) []byte {
	b := append([]byte{0x04, 0x0B}, "streamtyped"...)
	b = append(b, 0x81, 0xE8, 0x03, 0x84, 0x01, '@', 0x84, 0x84, 0x84, 8)
	b = append(b, "NSString"...)
	b = append(b, 0x01, 0x84, 0x01, '+')
	switch n := len(text); {
	case n < 0x80:
		b = append(b, byte(n))
	case n < 1<<16:
		b = append(b, 0x81, byte(n), byte(n>>8))
	default:
		b = append(b, 0x82, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	}
	return append(b, text...)
}

func sprint(v any) string { return fmt.Sprint(v) }
