package ioswhatsapp_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The in-test databases of the state vocabulary (4I2-R7) and of the other unit tests of this package: built with the test
// builder, so every state is pinned on a row the test itself wrote, never on the parser's own output. The DDL is the
// subset of columns the mapping reads, in the Core Data form of tools/fixtures/schemas/whatsapp-ios/chatstorage.sql.

type col []string // a table's declared columns, Z_PK first

var chatDDL = map[string]struct {
	sql  string
	cols col
}{
	"ZWAMESSAGE": {
		"CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGETYPE INTEGER, ZMESSAGEDATE TIMESTAMP, ZGROUPEVENTTYPE INTEGER, ZMESSAGESTATUS INTEGER, ZGROUPMEMBER INTEGER, ZMEDIAITEM INTEGER, ZMESSAGEINFO INTEGER, ZFROMJID VARCHAR, ZTOJID VARCHAR, ZPUSHNAME VARCHAR, ZSTANZAID VARCHAR, ZTEXT VARCHAR)",
		col{"Z_PK", "Z_ENT", "ZCHATSESSION", "ZISFROMME", "ZMESSAGETYPE", "ZMESSAGEDATE", "ZGROUPEVENTTYPE", "ZMESSAGESTATUS", "ZGROUPMEMBER", "ZMEDIAITEM", "ZMESSAGEINFO", "ZFROMJID", "ZTOJID", "ZPUSHNAME", "ZSTANZAID", "ZTEXT"},
	},
	"ZWACHATSESSION": {
		"CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZSESSIONTYPE INTEGER, ZREMOVED INTEGER, ZHIDDEN INTEGER, ZFLAGS INTEGER, ZCONTACTJID VARCHAR, ZPARTNERNAME VARCHAR)",
		col{"Z_PK", "ZSESSIONTYPE", "ZREMOVED", "ZHIDDEN", "ZFLAGS", "ZCONTACTJID", "ZPARTNERNAME"},
	},
	"ZWAGROUPMEMBER": {
		"CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID VARCHAR)",
		col{"Z_PK", "ZCHATSESSION", "ZMEMBERJID"},
	},
	"ZWAMEDIAITEM": {
		"CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZFILESIZE INTEGER, ZMEDIAURLDATE TIMESTAMP, ZLATITUDE FLOAT, ZLONGITUDE FLOAT, ZMEDIALOCALPATH VARCHAR, ZTITLE VARCHAR, ZVCARDNAME VARCHAR, ZVCARDSTRING VARCHAR, ZMEDIAKEY BLOB, ZMETADATA BLOB)",
		col{"Z_PK", "ZMESSAGE", "ZFILESIZE", "ZMEDIAURLDATE", "ZLATITUDE", "ZLONGITUDE", "ZMEDIALOCALPATH", "ZTITLE", "ZVCARDNAME", "ZVCARDSTRING", "ZMEDIAKEY", "ZMETADATA"},
	},
	"ZWAMESSAGEINFO": {
		"CREATE TABLE ZWAMESSAGEINFO (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZRECEIPTINFO BLOB)",
		col{"Z_PK", "ZMESSAGE", "ZRECEIPTINFO"},
	},
}

var chatOrder = []string{"ZWAMESSAGE", "ZWACHATSESSION", "ZWAGROUPMEMBER", "ZWAMEDIAITEM", "ZWAMESSAGEINFO"}

// chatTable puts rows by column name.
type chatTable struct {
	t    *sqlitetest.Table
	cols col
}

func (c chatTable) put(pk int64, kv map[string]any) {
	vals := make([]any, len(c.cols))
	for i, n := range c.cols {
		if n != "Z_PK" {
			vals[i] = kv[n]
		}
	}
	c.t.Insert(pk, vals...)
}

type chatTables map[string]chatTable

// buildChat builds a ChatStorage-like store from chatDDL. over replaces a table's DDL (an empty string drops the table);
// the Core Data model names WAMessage (9) and WAChatSession (4). fill adds the rows.
func buildChat(over map[string]string, fill func(tb chatTables)) (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := chatTables{}
	for _, name := range chatOrder {
		d := chatDDL[name]
		sql := d.sql
		if o, ok := over[name]; ok {
			sql = o
		}
		if sql != "" {
			tb[name] = chatTable{t: b.CreateTable(name, sql), cols: d.cols}
		}
	}
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	z.Insert(4, nil, "WAChatSession", int64(0), int64(0))
	z.Insert(9, nil, "WAMessage", int64(0), int64(0))
	fill(tb)
	return b.Bytes(), b.NewWAL(false, 1, 2, 0).Bytes()
}

const (
	p1       = "15550000001@s.whatsapp.net"
	p2       = "15550000002@s.whatsapp.net"
	p4       = "15550000004@s.whatsapp.net"
	grp      = "120363000000000001@g.us"
	goodDate = int64(600000001)
)

// runChat parses a Go-built store at the consumer backup domain (with a -wal when wal is not nil) and returns the records
// by Z_PK.
func runChat(t *testing.T, db, wal []byte, allowed ...string) (map[int64]records.Record, parsertest.Result) {
	t.Helper()
	files := map[string][]byte{"db": db}
	if wal != nil {
		files["wal"] = wal
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), allowed...)
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		out[rowidOf(r.Locator)] = r
	}
	return out, res
}

func rowidOf(loc string) int64 {
	_, rest, _ := strings.Cut(loc, ";rowid=")
	num, _, _ := strings.Cut(rest, ";")
	n, _ := strconv.ParseInt(num, 10, 64)
	return n
}

func rawOf(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

func payloadMap(r records.Record, key string) map[string]any {
	m, _ := r.Payload[key].(map[string]any)
	return m
}

// fromOf is the address of the participant with the role, "" when there is none.
func fromOf(r records.Record, role string) string {
	ps, _ := r.Payload["participants"].([]any)
	for _, p := range ps {
		m, _ := p.(map[string]any)
		if m["role"] == role {
			s, _ := m["address"].(string)
			return s
		}
	}
	return ""
}

func unknownParticipants(r records.Record) bool {
	v, _ := r.Payload["participants_unknown"].(bool)
	return v
}

// msgRow is a ZWAMESSAGE row: an incoming text message of chat session 1 unless kv says otherwise.
func msgRow(kv map[string]any) map[string]any {
	r := map[string]any{"Z_ENT": int64(9), "ZCHATSESSION": int64(1), "ZISFROMME": int64(0), "ZMESSAGETYPE": int64(0), "ZMESSAGEDATE": goodDate, "ZTEXT": "t"}
	for k, v := range kv {
		r[k] = v
	}
	return r
}

// 4I2-R7: every raw state key and value of the chats parser, pinned on a row of a database written here with hard-coded
// expectations. The vocabulary is the coordinator's; this test is its oracle.
func TestIOSWhatsAppChatsStateVocabularyIsPinned(t *testing.T) {
	long := strings.Repeat("x", 1100) + "@s.whatsapp.net"
	cases := map[string]int64{} // the Z_PK of the row built for each case
	db, _ := buildChat(nil, func(tb chatTables) {
		s := tb["ZWACHATSESSION"]
		s.put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		s.put(2, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp, "ZPARTNERNAME": "G"})
		s.put(3, map[string]any{"ZSESSIONTYPE": int64(3), "ZCONTACTJID": "status@broadcast"})
		s.put(4, map[string]any{"ZSESSIONTYPE": int64(0)})                                                  // JID NULL
		s.put(5, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": ""})                               // JID empty
		s.put(6, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "1555\x00@s.whatsapp.net"})        // altered by cleaning
		s.put(7, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": long})                             // over the contract's limit
		s.put(8, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": int64(7)})                         // not text
		s.put(9, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "100000000000001@lid"})            // a lid
		s.put(10, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "120363111111111111@newsletter"}) // a newsletter
		s.put(11, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "x@weird.example"})               // another server
		s.put(12, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": "nonsense"})                      // no server
		s.put(13, map[string]any{"ZSESSIONTYPE": int64(2), "ZCONTACTJID": "1234@broadcast"})

		g := tb["ZWAGROUPMEMBER"]
		g.put(1, map[string]any{"ZMEMBERJID": p2})
		g.put(2, map[string]any{})
		g.put(3, map[string]any{"ZMEMBERJID": ""})
		g.put(4, map[string]any{"ZMEMBERJID": "1555\x00@s.whatsapp.net"})
		g.put(5, map[string]any{"ZMEMBERJID": long})
		g.put(6, map[string]any{"ZMEMBERJID": grp})
		g.put(7, map[string]any{"ZMEMBERJID": "1234@broadcast"})
		g.put(8, map[string]any{"ZMEMBERJID": "status@broadcast"})
		g.put(9, map[string]any{"ZMEMBERJID": int64(5)})

		m := tb["ZWAMESSAGE"]
		pk := int64(0)
		add := func(kv map[string]any) int64 {
			pk++
			m.put(pk, msgRow(kv))
			return pk
		}
		cases["chat absent NULL"] = add(map[string]any{"ZCHATSESSION": nil})
		cases["chat absent 0"] = add(map[string]any{"ZCHATSESSION": int64(0)})
		cases["chat no row"] = add(map[string]any{"ZCHATSESSION": int64(77)})
		cases["chat no jid NULL"] = add(map[string]any{"ZCHATSESSION": int64(4)})
		cases["chat no jid empty"] = add(map[string]any{"ZCHATSESSION": int64(5)})
		cases["chat altered"] = add(map[string]any{"ZCHATSESSION": int64(6)})
		cases["chat too long"] = add(map[string]any{"ZCHATSESSION": int64(7)})
		cases["chat undecided"] = add(map[string]any{"ZCHATSESSION": int64(8)})
		cases["class user"] = add(map[string]any{"ZCHATSESSION": int64(1)})
		cases["class group"] = add(map[string]any{"ZCHATSESSION": int64(2), "ZGROUPMEMBER": int64(1)})
		cases["class status"] = add(map[string]any{"ZCHATSESSION": int64(3)})
		cases["class lid"] = add(map[string]any{"ZCHATSESSION": int64(9)})
		cases["class newsletter"] = add(map[string]any{"ZCHATSESSION": int64(10)})
		cases["class other"] = add(map[string]any{"ZCHATSESSION": int64(11)})
		cases["class unparseable"] = add(map[string]any{"ZCHATSESSION": int64(12)})
		cases["class broadcast"] = add(map[string]any{"ZCHATSESSION": int64(13)})
		for i, want := range []string{"absent", "no-ZWAGROUPMEMBER-row", "no-member-jid", "no-member-jid", "altered", "too-long", "not-a-person", "not-a-person", "not-a-person", "undecided"} {
			key := "sender " + want + " " + strconv.Itoa(i)
			switch i {
			case 0:
				cases[key] = add(map[string]any{"ZCHATSESSION": int64(2)})
			case 1:
				cases[key] = add(map[string]any{"ZCHATSESSION": int64(2), "ZGROUPMEMBER": int64(99)})
			default:
				cases[key] = add(map[string]any{"ZCHATSESSION": int64(2), "ZGROUPMEMBER": int64(i)}) // members 2..9
			}
		}
		cases["conflict in"] = add(map[string]any{"ZFROMJID": p2})
		cases["conflict out"] = add(map[string]any{"ZISFROMME": int64(1), "ZTOJID": p2})
		cases["no conflict"] = add(map[string]any{"ZFROMJID": p1})
		cases["receipt no row"] = add(map[string]any{"ZMESSAGEINFO": int64(99)})
		cases["body from title"] = add(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil})
		cases["body from vcard"] = add(map[string]any{"ZMESSAGETYPE": int64(4), "ZTEXT": nil})
		cases["two media rows"] = add(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil})
		paths := map[string]any{ // the stored ZMEDIALOCALPATH of each media_state case (a lone NUL: cleaning changes it)
			"path NULL": nil, "path empty": "", "path BLOB": []byte{1, 2}, "path INTEGER": int64(7), "path NUL": "\x00", "path refused": "../x", "path star": "a*b", "path found-nothing": "Media/none.jpg",
		}
		for name := range paths {
			cases[name] = add(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil})
		}
		cases["no item"] = add(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil})
		mi := tb["ZWAMEDIAITEM"]
		mi.put(1, map[string]any{"ZMESSAGE": cases["body from title"], "ZTITLE": "pic"})
		mi.put(2, map[string]any{"ZMESSAGE": cases["body from vcard"], "ZVCARDSTRING": "BEGIN:VCARD"})
		mi.put(3, map[string]any{"ZMESSAGE": cases["two media rows"]})
		mi.put(4, map[string]any{"ZMESSAGE": cases["two media rows"]})
		n := int64(4)
		for name, p := range paths {
			n++
			mi.put(n, map[string]any{"ZMESSAGE": cases[name], "ZMEDIALOCALPATH": p, "ZVCARDSTRING": "image/jpeg"})
		}
	})
	recs, _ := runChat(t, db, nil, "whatsapp-ios-join-undecided")
	type want struct {
		key, val string
	}
	check := func(name string, wants ...want) {
		t.Helper()
		r, ok := recs[cases[name]]
		if !ok {
			t.Fatalf("%s: no record", name)
		}
		raw := rawOf(r)
		for _, w := range wants {
			if got, _ := raw[w.key].(string); got != w.val {
				t.Errorf("%s: raw.%s = %v, want %q (raw %v)", name, w.key, raw[w.key], w.val, raw)
			}
		}
	}
	// chat_state
	check("chat absent NULL", want{"chat_state", "absent"})
	check("chat absent 0", want{"chat_state", "absent"})
	check("chat no row", want{"chat_state", "no-ZWACHATSESSION-row"})
	check("chat no jid NULL", want{"chat_state", "no-contact-jid"})
	check("chat no jid empty", want{"chat_state", "no-contact-jid"})
	check("chat altered", want{"chat_state", "altered"})
	check("chat too long", want{"chat_state", "too-long"})
	check("chat undecided", want{"chat_state", "undecided"})
	// chat_class
	for name, class := range map[string]string{
		"class user": "user", "class group": "group", "class status": "status", "class lid": "lid",
		"class newsletter": "newsletter", "class other": "other", "class unparseable": "unparseable", "class broadcast": "broadcast",
	} {
		check(name, want{"chat_class", class})
	}
	// sender_state, in the order of the cases
	for i, w := range []string{"absent", "no-ZWAGROUPMEMBER-row", "no-member-jid", "no-member-jid", "altered", "too-long", "not-a-person", "not-a-person", "not-a-person", "undecided"} {
		check("sender "+w+" "+strconv.Itoa(i), want{"sender_state", w})
	}
	// who an incoming 1:1 message is from (amendment 4I2-A1): a person's class is the chat JID; a list, another server and a
	// JID that does not parse name no author
	for name, from := range map[string]string{
		"class user": p1, "class lid": "100000000000001@lid", "class newsletter": "120363111111111111@newsletter",
		"class broadcast": "", "class other": "", "class unparseable": "",
	} {
		r := recs[cases[name]]
		if fromOf(r, "from") != from || unknownParticipants(r) != (from == "") {
			t.Errorf("%s: from %q participants_unknown %v, want from %q", name, fromOf(r, "from"), unknownParticipants(r), from)
		}
	}
	// address_conflict
	if got := rawOf(recs[cases["conflict in"]])["address_conflict"]; !reflect.DeepEqual(got, []any{"ZFROMJID"}) {
		t.Errorf("conflict in: %v", got)
	}
	if got := rawOf(recs[cases["conflict out"]])["address_conflict"]; !reflect.DeepEqual(got, []any{"ZTOJID"}) {
		t.Errorf("conflict out: %v", got)
	}
	if _, has := rawOf(recs[cases["no conflict"]])["address_conflict"]; has {
		t.Error("an agreeing ZFROMJID states a conflict")
	}
	// media_state and its counter (4I2-R6, PF15): NULL or empty text is no path; a cell that is no text is unreadable, a path
	// cleaning changed or holding * cannot be expressed (neither is unresolved); a refused path is unresolved
	counted := func(name, state, counter string) {
		t.Helper()
		check(name, want{"media_state", state})
		if rawOf(recs[cases[name]])[counter] != int64(1) || len(attachmentsOf(recs[cases[name]])) != 1 {
			t.Errorf("%s: %v, want %s 1 and one attachment", name, rawOf(recs[cases[name]]), counter)
		}
	}
	counted("path NULL", "no-local-path", "attachments_unresolved")
	counted("path empty", "no-local-path", "attachments_unresolved")
	counted("path BLOB", "local-path-unreadable", "attachments_lookup_undecided")
	counted("path INTEGER", "local-path-unreadable", "attachments_lookup_undecided")
	counted("path NUL", "lookup-path-not-expressible", "attachments_lookup_undecided")
	counted("path star", "lookup-path-not-expressible", "attachments_lookup_undecided")
	counted("path refused", "lookup-path-refused", "attachments_unresolved")
	if r := recs[cases["path found-nothing"]]; rawOf(r)["media_state"] != nil || rawOf(r)["attachments_unresolved"] != int64(1) {
		t.Errorf("a path with no file: %v", rawOf(r))
	}
	check("no item", want{"media_state", "no-ZWAMEDIAITEM-row"})
	// receipt_info_state, body_column, join_undecided
	check("receipt no row", want{"receipt_info_state", "no-ZWAMESSAGEINFO-row"})
	check("body from title", want{"body_column", "ZTITLE"})
	check("body from vcard", want{"body_column", "ZVCARDSTRING"})
	if got := rawOf(recs[cases["two media rows"]])["join_undecided"]; !reflect.DeepEqual(got, []any{"ZWAMEDIAITEM"}) {
		t.Errorf("two media rows name one message: join_undecided = %v", got)
	}
}

// A table the guard cannot trust under a -wal (the main file proves nothing, 4I review F1) makes every join through it
// undecided: raw.join_undecided, sender_state undecided, the note with the reason and one schema warning (4I2-R7, P7).
func TestIOSWhatsAppChatsStateVocabularyOfAGuardedTable(t *testing.T) {
	db, wal := buildChat(map[string]string{
		"ZWAGROUPMEMBER": "CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER, ZCHATSESSION INTEGER, ZMEMBERJID VARCHAR)", // no rowid alias
	}, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZCHATSESSION": int64(1), "ZGROUPMEMBER": int64(1)}))
		tb["ZWAGROUPMEMBER"].t.Insert(1, int64(1), int64(1), p2)
	})
	recs, res := runChat(t, db, wal, "wal-frames-not-applied", "whatsapp-ios-join-undecided")
	raw := rawOf(recs[1])
	if !reflect.DeepEqual(raw["join_undecided"], []any{"ZWAGROUPMEMBER"}) || raw["sender_state"] != "undecided" || !unknownParticipants(recs[1]) {
		t.Errorf("guarded member table: raw %v participants_unknown %v", raw, unknownParticipants(recs[1]))
	}
	if got := res.Notes["join_undecided.ZWAGROUPMEMBER"]; got != "column Z_PK is not the rowid alias" {
		t.Errorf("note join_undecided.ZWAGROUPMEMBER = %q", got)
	}
	var schema, counted int
	for _, w := range res.Warnings {
		switch {
		case w.Locator == "sqlite:schema" && w.Reason == "whatsapp-ios-join-undecided: table ZWAGROUPMEMBER: column Z_PK is not the rowid alias; every join through it is undecided":
			schema++
		case w.Locator == "sqlite:table=ZWAGROUPMEMBER" && w.Reason == "whatsapp-ios-join-undecided: ZWAGROUPMEMBER: 1 records":
			counted++
		}
	}
	if schema != 1 || counted != 1 {
		t.Errorf("schema warnings %d, counted warnings %d, want 1 and 1: %v", schema, counted, res.Warnings)
	}
}
