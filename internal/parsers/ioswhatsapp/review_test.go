package ioswhatsapp_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// A JID over the contract's limit, or cut by cleaning, is too-long and keeps no text anywhere: not in raw, and so not on
// every message of the chat (review I4); the generator's rows 52 and 53 pin the same for a 1 MiB JID with a NUL.
func TestIOSWhatsAppChatsOverLongJIDIsTooLongNotAltered(t *testing.T) {
	huge := "1555\x00" + strings.Repeat("7", 300<<10) + "@s.whatsapp.net" // over 256 KiB: cleaning cuts it
	long := "1\x00" + strings.Repeat("7", 2000) + "@s.whatsapp.net"       // over 1024 and altered by a NUL
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": huge})
		tb["ZWACHATSESSION"].put(2, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWACHATSESSION"].put(3, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": long})
		tb["ZWAGROUPMEMBER"].put(1, map[string]any{"ZMEMBERJID": huge})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZCHATSESSION": int64(1)}))
		tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZCHATSESSION": int64(2), "ZGROUPMEMBER": int64(1)}))
		tb["ZWAMESSAGE"].put(3, msgRow(map[string]any{"ZCHATSESSION": int64(3)}))
	})
	got, _ := runChat(t, db, nil)
	for n, key := range map[int64]string{1: "chat_state", 2: "sender_state", 3: "chat_state"} {
		raw := rawOf(got[n])
		for _, k := range []string{"chat_jid", "member_jid_text", "text_flags"} {
			if raw[k] != nil && (n != 2 || k != "chat_jid") { // message 2's chat is the readable group
				t.Errorf("message %d: raw.%s = %.40v: the text of an over-long JID is kept", n, k, raw[k])
			}
		}
		if raw[key] != "too-long" || !unknownParticipants(got[n]) {
			t.Errorf("message %d: %s = %v, want too-long (raw %.200v)", n, key, raw[key], raw)
		}
	}
}

// Review I5: a ZMEDIAITEM that is neither NULL nor an INTEGER is unknown, not unset: the join is undecided (never "no media
// row", never the one row that names the message, whatever the other direction says).
func TestIOSWhatsAppChatsUnreadableMediaKeyIsUndecided(t *testing.T) {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		img := func(key any) map[string]any {
			return msgRow(map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil, "ZMEDIAITEM": key})
		}
		tb["ZWAMESSAGE"].put(1, img("x"))          // TEXT, and an item names the message
		tb["ZWAMESSAGE"].put(2, img([]byte{1, 2})) // BLOB, and no item names it
		tb["ZWAMESSAGE"].put(3, img(int64(3)))     // control: both directions agree
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": int64(1), "ZTITLE": "t1"})
		tb["ZWAMEDIAITEM"].put(3, map[string]any{"ZMESSAGE": int64(3), "ZTITLE": "t3"})
	})
	got, _ := runChat(t, db, nil, "whatsapp-ios-join-undecided")
	for _, n := range []int64{1, 2} {
		raw := rawOf(got[n])
		uc, _ := raw["unknown_columns"].(map[string]any)
		if !reflect.DeepEqual(raw["join_undecided"], []any{"ZWAMEDIAITEM"}) || got[n].Body != "" || raw["ZTITLE"] != nil || uc["ZWAMESSAGE.ZMEDIAITEM"] != "type-mismatch" {
			t.Errorf("message %d: %v", n, raw)
		}
	}
	if got[3].Body != "t3" || rawOf(got[3])["join_undecided"] != nil {
		t.Errorf("control: %v", rawOf(got[3]))
	}
}

// Review M1: a REAL NaN in a date column survives the reader, the rowrun context and the WAL overlay as a NULL with its
// serial type, so the mapping states it invalid instead of reading it as absent.
func TestIOSWhatsAppChatsNaNDateIsInvalidAlsoThroughTheWAL(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := chatTables{}
	for _, name := range chatOrder {
		tb[name] = chatTable{t: b.CreateTable(name, chatDDL[name].sql), cols: chatDDL[name].cols}
	}
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	z.Insert(4, nil, "WAChatSession", int64(0), int64(0))
	z.Insert(9, nil, "WAMessage", int64(0), int64(0))
	tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
	tb["ZWAMESSAGE"].put(1, msgRow(nil))
	db := b.Bytes()
	tb["ZWAMESSAGE"].put(2, msgRow(map[string]any{"ZMESSAGEDATE": math.NaN()})) // the WAL's version of the table page
	w := b.NewWAL(false, 1, 2, 0)
	w.Frame(2, b.PageBytes(2), b.Snapshot().Pages())
	got, _ := runChat(t, db, w.Bytes(), "wal-mode-mismatch") // the builder's header is not in WAL mode: the reader applies the WAL all the same
	if _, ok := got[2]; !ok || got[2].Time != nil || rawOf(got[2])["ZMESSAGEDATE_status"] != "invalid" || got[1].Time == nil {
		t.Errorf("a NaN date in a WAL frame: %v", got)
	}
}
