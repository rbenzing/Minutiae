package androidtelegram_test

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The fixtures' ids (tools/fixtures/telegram_android.py): the account owner, Ann, the helper bot.
const (
	idSelf = 1000001
	idAnn  = 1000002
	idBot  = 1000003
	// minInt64 is the uid no dialog has: never negated (4I4-P11).
	minInt64 = -1 << 63
)

// The constructor IDs of the Go-built peers and chats (constructors.tsv, TLRPC.java@A-HEAD).
const (
	idPeerChat         = 0x36c6019a // TL_peerChat
	idPeerUserLayer131 = 0x9db1bc6d // TL_peerUser_layer131: user_id int32
	idTLChat           = 0x41cbf256 // TL_chat: flags, id, title (a prefix body)
	idTLChannel        = 0x7ad15240 // TL_channel: flags, flags2, id, [access_hash], title (a prefix body)
)

func peerChat(id int64) []byte { return append(le32(idPeerChat), le64(id)...) }

func peerUser131(id uint32) []byte { return append(le32(idPeerUserLayer131), le32(id)...) }

// tlChat is a TL_chat blob with no flag set; tlChannel a TL_channel blob with neither flags word set.
func tlChat(id int64, title string) []byte {
	b := append(le32(idTLChat), le32(0)...)
	return append(append(b, le64(id)...), tlStr(title)...)
}

func tlChannel(id int64, title string) []byte {
	b := append(append(le32(idTLChannel), le32(0)...), le32(0)...)
	return append(append(b, le64(id)...), tlStr(title)...)
}

// The DDL of the Go-built join tables (spec 6.3): chats and enc_chats keyed by their uid rowid alias.
const (
	ddlChats    = `CREATE TABLE chats (uid INTEGER PRIMARY KEY, name TEXT, data BLOB)`
	ddlEncChats = `CREATE TABLE enc_chats (uid INTEGER PRIMARY KEY, user INTEGER, name TEXT, data BLOB)`
)

// builtRow is one row of a Go-built or copied table: its rowid and its cells by column name (a column not named is
// NULL; the rowid alias is always stored NULL).
type builtRow struct {
	rowid int64
	cells map[string]any
}

// builtTable is a table of a Go-built database beside the message table: its name, DDL, column order, rowid alias
// column ("" for none) and rows.
type builtTable struct {
	name, ddl string
	cols      []string
	alias     string
	rows      []builtRow
}

func chatsTable(rows ...builtRow) builtTable {
	return builtTable{"chats", ddlChats, []string{"uid", "name", "data"}, "uid", rows}
}

func encChatsTable(rows ...builtRow) builtTable {
	return builtTable{"enc_chats", ddlEncChats, []string{"uid", "user", "name", "data"}, "uid", rows}
}

func usersTable(uids ...int64) builtTable {
	t := builtTable{name: "users", ddl: ddlUsers, cols: []string{"uid", "name", "status", "data"}, alias: "uid"}
	for _, u := range uids {
		t.rows = append(t.rows, builtRow{u, map[string]any{"name": "x", "status": int64(0)}})
	}
	return t
}

// values are a row's cells in column order, the rowid alias column NULL.
func (r builtRow) values(cols []string, alias string) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		if c != alias {
			out[i] = r.cells[c]
		}
	}
	return out
}

// goBuiltWith is a Go-built database at userVersion with messages_v2 holding v2 (rowids from 1) and the tables given
// (an empty users table when none is given).
func goBuiltWith(userVersion int32, v2 []msgCells, tables ...builtTable) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: userVersion})
	hasUsers := false
	for _, t := range tables {
		hasUsers = hasUsers || t.name == "users"
	}
	if !hasUsers {
		b.CreateTable("users", ddlUsers)
	}
	t := b.CreateTable("messages_v2", ddlV2Full)
	for i, c := range v2 {
		t.Insert(int64(i+1), c.v2Values()...)
	}
	for _, bt := range tables {
		tb := b.CreateTable(bt.name, bt.ddl)
		for _, r := range bt.rows {
			tb.Insert(r.rowid, r.values(bt.cols, bt.alias)...)
		}
	}
	return b.Bytes()
}

// fixtureCopy is a Go-built copy of a committed variant's live view (main file and WAL): every table of the
// variant's DDL file (tools/fixtures/schemas/telegram-android/<ddl>), each row through edit (nil keeps it as read),
// then the extra rows; the tables named in drop are left out. Indexes are not copied (no mapping reads them).
func fixtureCopy(t *testing.T, variant, ddl string, edit func(table string, cols []string, vals []any), extra map[string][]builtRow, drop ...string) []byte {
	t.Helper()
	files := fixtureFiles(t, variant)
	f := sqlitedb.Files{DB: bytes.NewReader(files["db"]), DBSize: int64(len(files["db"]))}
	if w, ok := files["wal"]; ok {
		f.WAL, f.WALSize = bytes.NewReader(w), int64(len(w))
	}
	db, err := sqlitedb.Open(t.Context(), f, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	sql, err := os.ReadFile("../../../tools/fixtures/schemas/telegram-android/" + ddl)
	if err != nil {
		t.Fatal(err)
	}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: db.Info().Header.UserVersion})
	for _, line := range strings.Split(string(sql), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		rest, ok := strings.CutPrefix(line, "CREATE TABLE ")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		if contains(drop, name) {
			continue
		}
		out := b.CreateTable(name, line)
		tb, err := db.Table(t.Context(), name, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var cols []string
		alias := ""
		for _, c := range tb.Cols() {
			cols = append(cols, c.Name)
			if c.RowidAlias {
				alias = c.Name
			}
		}
		err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
			id, _ := row.Rowid()
			vals := make([]any, len(cols))
			for i, c := range tb.Cols() {
				if !c.RowidAlias {
					vals[i] = goValue(row.Value(i))
				}
			}
			if edit != nil {
				edit(name, cols, vals)
			}
			out.Insert(id, vals...)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range extra[name] {
			out.Insert(r.rowid, r.values(cols, alias)...)
		}
	}
	return b.Bytes()
}

// goValue is a stored value as sqlitetest writes it.
func goValue(v sqlitefile.Value) any {
	switch v.Kind {
	case sqlitefile.KindInt:
		return v.Int
	case sqlitefile.KindFloat:
		return v.Float
	case sqlitefile.KindText:
		return string(v.Bytes)
	case sqlitefile.KindBlob:
		return bytes.Clone(v.Bytes)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// party is the address of a record's first participant of role ("" when none).
func party(r records.Record, role string) string {
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

// threadOf is a record's thread object.
func threadOf(r records.Record) map[string]any {
	th, _ := r.Payload["thread"].(map[string]any)
	return th
}

// Spec 8.6, T4, 4I4-P11: a participant address names the TL class of the peer with its id as stored. The CORE rows
// (R1 user, R4 a user in a basic group, R5 a channel post); outgoing Go-built rows in the dialogs of R4-R7 (a basic
// group, a broadcast channel, a megagroup, a community: the dialog's own address, the community's from its chats
// row); the six Peer constructors of telegram-layers; a _layer131 peer of id 0xfffffffe is user:-2 (sign-extended as
// stored, never re-signed); no participant address of any variant is a bare number (C5).
func TestTelegramAddressesNameTheirClass(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	for key, want := range map[string]string{"messages_v2/1": "user:1000002", "messages_v2/4": "user:1000003", "messages_v2/5": "channel:3000001"} {
		if got := party(rows[key], "from"); got != want {
			t.Errorf("%s: from %q, want %q", key, got, want)
		}
	}
	r7 := fixtureBlobs(t, "telegram-v181", "messages_v2", "data")[7]
	out := func(id int32, peer []byte) []byte {
		return tlMessage{flags: bitOut, id: id, peerObj: peer, date: baseDate, text: "to the dialog"}.bytes()
	}
	row := func(rid int64, uid int64, data []byte) builtRow {
		return builtRow{rid, map[string]any{
			"mid": rid, "uid": uid, "read_state": int64(0), "send_state": int64(0), "date": int64(baseDate), "data": data,
			"out": int64(1), "ttl": int64(0), "media": int64(-1), "is_channel": int64(0),
		}}
	}
	db := fixtureCopy(t, "telegram-v181", "v181.sql", nil, map[string][]builtRow{"messages_v2": {
		row(901, -2000001, out(901, peerChat(2000001))),
		row(902, -3000001, out(902, peerChannel(3000001))),
		row(903, -3000002, out(903, peerChannel(3000002))),
		row(904, -4000001, r7), // a blob the table cannot read: the community comes from its chats row
	}})
	got := byRow(parseAt(t, remoteApp, dbOnly(db)).Records)
	for key, want := range map[string]string{
		"messages_v2/901": "chat:2000001", "messages_v2/902": "channel:3000001", "messages_v2/903": "channel:3000002",
		"messages_v2/904": "community:4000001",
	} {
		if got := party(got[key], "to"); got != want {
			t.Errorf("%s: to %q, want %q", key, got, want)
		}
	}

	layers := byRow(parseFixture(t, "telegram-layers").Records)
	classes := map[string]int{}
	for rid := int64(1201); rid <= 1206; rid++ {
		from := party(layers["messages_v2/"+itoa(rid)], "from")
		switch from {
		case "user:1000002", "chat:2000001", "channel:3000001":
			cls, _, _ := strings.Cut(from, ":")
			classes[cls]++
		default:
			t.Errorf("telegram-layers row %d: from %q, want the peer's class and stored id", rid, from)
		}
	}
	if classes["user"] != 2 || classes["chat"] != 2 || classes["channel"] != 2 {
		t.Errorf("telegram-layers peers by class %v, want two of each (the current and the _layer131 form)", classes)
	}

	m := tlMessage{id: 5, fromPeer: peerUser131(0xfffffffe), peerObj: peerChat(2000001), date: baseDate, text: "old peer"}
	one := parseAt(t, remoteApp, dbOnly(goBuiltMessages(181, nil, []msgCells{{mid: 5, uid: -2000001, data: m.bytes()}}))).Records
	if len(one) != 1 || party(one[0], "from") != "user:-2" {
		t.Errorf("a _layer131 peer of id 0xfffffffe: %v, want from user:-2", one)
	}

	bare := regexp.MustCompile(`^[0-9+]`)
	for _, v := range variants {
		for _, r := range parseAt(t, remoteApp, fixtureFiles(t, v)).Records {
			ps, _ := r.Payload["participants"].([]any)
			for _, p := range ps {
				a, _ := p.(map[string]any)["address"].(string)
				if bare.MatchString(a) || !strings.Contains(a, ":") {
					t.Errorf("%s %s: participant address %q names no class", v, r.Locator, a)
				}
			}
		}
	}
}

// Spec 6.4, 8.6 (DO:51,57): a channel's dialog id is -channel_id, no -100 offset. A Bot-API-shaped uid
// -1001234567890 keeps thread id "-1001234567890" and the dialog address channel:1001234567890 (the class from the
// peer and the chats row), and the chats row is joined by Get(chats, 1001234567890): its title is the thread's, never
// that of a decoy row at 1234567890.
func TestTelegramChannelIdHasNoOffset(t *testing.T) {
	const uid = -1001234567890
	m := tlMessage{flags: bitOut, id: 1, peerObj: peerChannel(-uid), date: baseDate, text: "to a big channel"}
	db := goBuiltWith(181, []msgCells{{mid: 1, uid: uid, out: int64(1), read: int64(0), data: m.bytes()}},
		chatsTable(
			builtRow{-uid, map[string]any{"name": "big", "data": tlChannel(-uid, "Big")}},
			builtRow{1234567890, map[string]any{"name": "decoy", "data": tlChannel(1234567890, "Decoy")}},
		))
	recs := parseAt(t, remoteApp, dbOnly(db)).Records
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	th := threadOf(r)
	if th["id"] != "-1001234567890" || th["title"] != "Big" || th["group"] != true {
		t.Errorf("thread %v, want id -1001234567890, title Big, group", th)
	}
	if party(r, "to") != "channel:1001234567890" || rawOf(r)["dialog_class"] != "channel" {
		t.Errorf("to %q, dialog_class %v", party(r, "to"), rawOf(r)["dialog_class"])
	}
	if s := jsonText(r.Payload); strings.Contains(s, ":1234567890\"") || strings.Contains(s, "Decoy") {
		t.Errorf("an offset id or the decoy row reached the record: %s", s)
	}
}

// Spec 8.6 (TLRPC:59550-59559, F16): a sender is never synthesized. R5 (a channel post without from_id) is from its
// channel with raw.sender_basis "channel-post"; a Go-built incoming group row with neither from_id nor post has
// unknown participants and sender_state "absent"; R7 (a blob the table cannot read) "blob-undecided". In
// telegram-v181 no record is from its own peer_id unless the blob holds that from_id (R46: from_id is the owner) or
// the record is a channel post.
func TestTelegramSenderIsNeverSynthesized(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r5 := rows["messages_v2/5"]
	if party(r5, "from") != "channel:3000001" || rawOf(r5)["sender_basis"] != "channel-post" {
		t.Errorf("R5: from %q, sender_basis %v", party(r5, "from"), rawOf(r5)["sender_basis"])
	}
	r7 := rows["messages_v2/7"]
	if r7.Payload["participants_unknown"] != true || rawOf(r7)["sender_state"] != "blob-undecided" {
		t.Errorf("R7: participants_unknown %v, sender_state %v", r7.Payload["participants_unknown"], rawOf(r7)["sender_state"])
	}
	m := tlMessage{id: 3, peerObj: peerChat(2000001), date: baseDate, text: "who wrote this"}
	db := goBuiltWith(181, []msgCells{{mid: 3, uid: -2000001, data: m.bytes()}},
		chatsTable(builtRow{2000001, map[string]any{"name": "family", "data": tlChat(2000001, "Family")}}))
	recs := parseAt(t, remoteApp, dbOnly(db)).Records
	if len(recs) != 1 || recs[0].Payload["participants_unknown"] != true || rawOf(recs[0])["sender_state"] != "absent" ||
		len(recs[0].Payload["participants"].([]any)) != 0 {
		t.Errorf("a group row without from_id or post: %v", recs)
	}
	holdsFromID := map[string]bool{"messages_v2/46": true} // tlmsg(..., frm=pu(SELF)) in a dialog with the owner
	for key, r := range rows {
		from, peer := party(r, "from"), rawOf(r)["peer_id"]
		if from == "" || from != peer || holdsFromID[key] || rawOf(r)["sender_basis"] == "channel-post" {
			continue
		}
		t.Errorf("%s: from %q equals its own peer_id without a from_id or a channel post", key, from)
	}
}

// Spec 8.6 (MS:10877-10896, MS:11047): users.name and chats.name are lowercase search strings, never a name or a
// title. In a copy of telegram-v181 whose users.name and chats.name hold distinctive strings, no payload, raw value,
// Body, Summary, note or warning holds them.
func TestTelegramSearchStringIsNeverAName(t *testing.T) {
	db := fixtureCopy(t, "telegram-v181", "v181.sql", func(table string, cols []string, vals []any) {
		for i, c := range cols {
			switch {
			case table == "users" && c == "name":
				vals[i] = "zz-search-user"
			case table == "chats" && c == "name":
				vals[i] = "zz-search-chat"
			}
		}
	}, nil)
	if !bytes.Contains(db, []byte("zz-search-user")) || !bytes.Contains(db, []byte("zz-search-chat")) {
		t.Fatal("the copy does not hold the search strings")
	}
	res := parseAt(t, remoteApp, dbOnly(db))
	if res.Err != nil || len(res.Records) == 0 {
		t.Fatalf("Parse: %v, %d records", res.Err, len(res.Records))
	}
	titled := 0
	for _, r := range res.Records {
		if threadOf(r)["title"] != nil {
			titled++
		}
	}
	if titled == 0 {
		t.Error("no thread title at all: the test proves nothing")
	}
	all, err := json.Marshal(struct {
		R []records.Record
		N map[string]string
		W any
	}{res.Records, res.Notes, res.Warnings})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"zz-search-user", "zz-search-chat"} {
		if bytes.Contains(all, []byte(s)) {
			t.Errorf("the output holds the search string %q", s)
		}
	}
}

// Spec 8.6, 4I4-P11, B13: a dialog id the uid cannot decide is unknown, never negated and never joined: uid 0 and
// MinInt64 in messages_v2 have class unknown, no address and unknown participants. R47 (peer a basic group, chats row
// a channel) is a conflict with no title; R48 a folder with unknown participants; R49 a user dialog without its users
// row.
func TestTelegramDialogIdEdgeValuesAreUndecided(t *testing.T) {
	in := func(id int32) []byte {
		return tlMessage{id: id, from: idAnn, peer: idSelf, date: baseDate, text: "edge"}.bytes()
	}
	outRow := func(id int32) []byte {
		return tlMessage{flags: bitOut, id: id, peer: idAnn, date: baseDate, text: "edge"}.bytes()
	}
	db := goBuiltMessages(181, nil, []msgCells{
		{mid: 1, uid: 0, data: in(1)},
		{mid: 2, uid: minInt64, data: in(2)},
		{mid: 3, uid: 0, out: int64(1), read: int64(0), data: outRow(3)},
		{mid: 4, uid: minInt64, out: int64(1), read: int64(0), data: outRow(4)},
	})
	for _, r := range parseAt(t, remoteApp, dbOnly(db)).Records {
		raw := rawOf(r)
		ps, _ := r.Payload["participants"].([]any)
		th := threadOf(r)
		if raw["dialog_class"] != "unknown" || r.Payload["participants_unknown"] != true || len(ps) != 0 || th["group"] != nil ||
			raw["thread_title_state"] != nil || raw["thread_user_state"] != nil || raw["join_undecided"] != nil {
			t.Errorf("%s (uid %v): class %v, participants %v (unknown %v), thread %v, raw %v", r.Locator, raw["uid"],
				raw["dialog_class"], ps, r.Payload["participants_unknown"], th, raw)
		}
		if id := th["id"]; id != "0" && id != "-9223372036854775808" {
			t.Errorf("%s: thread id %v, want the uid as stored", r.Locator, id)
		}
	}
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r47, r48, r49 := rows["messages_v2/47"], rows["messages_v2/48"], rows["messages_v2/49"]
	if rawOf(r47)["dialog_class_state"] != "conflict" || rawOf(r47)["dialog_class"] != "negative" ||
		rawOf(r47)["thread_title_state"] != "class-mismatch" || threadOf(r47)["title"] != nil {
		t.Errorf("R47: raw %v, thread %v", rawOf(r47), threadOf(r47))
	}
	if rawOf(r48)["dialog_class"] != "folder" || r48.Payload["participants_unknown"] != true || threadOf(r48)["group"] != nil {
		t.Errorf("R48: raw %v, payload %v", rawOf(r48), r48.Payload)
	}
	if rawOf(r49)["thread_user_state"] != "no-users-row" || party(r49, "from") != "user:1000099" {
		t.Errorf("R49: thread_user_state %v, from %q", rawOf(r49)["thread_user_state"], party(r49, "from"))
	}
}

// Review Focus 2: the Saved Messages dialog (uid = the owner's own id) is a user dialog. R45 (outgoing) is to
// user:1000001, R46 (incoming, a forward from Ann) from user:1000001 with the origin in raw.fwd only; neither has a
// thread title, both are group false.
func TestTelegramSavedMessagesDialogIsAUserDialog(t *testing.T) {
	rows := byRow(parseFixture(t, "telegram-v181").Records)
	r45, r46 := rows["messages_v2/45"], rows["messages_v2/46"]
	if party(r45, "to") != "user:1000001" || party(r46, "from") != "user:1000001" {
		t.Errorf("R45 to %q, R46 from %q", party(r45, "to"), party(r46, "from"))
	}
	fwd, _ := rawOf(r46)["fwd"].(map[string]any)
	if fwd["from"] != "user:1000002" {
		t.Errorf("R46: raw.fwd %v", fwd)
	}
	for key, r := range map[string]records.Record{"R45": r45, "R46": r46} {
		th := threadOf(r)
		if rawOf(r)["dialog_class"] != "user" || th["title"] != nil || th["group"] != false || th["id"] != "1000001" {
			t.Errorf("%s: dialog_class %v, thread %v", key, rawOf(r)["dialog_class"], th)
		}
		ps, _ := r.Payload["participants"].([]any)
		if len(ps) != 1 {
			t.Errorf("%s: participants %v, want one", key, ps)
		}
	}
}
