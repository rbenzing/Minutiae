package ioswacontacts_test

import (
	"strings"
	"testing"
)

// Final review M3: a row with no name or number whose JID is undecided (altered by cleaning, over the contract's limit, or a
// cell that is not text) is warned "JID undecided", never "JID only" (which says the row holds a decided user JID); a row with
// a decided user JID stays "JID only".
func TestIOSWhatsAppContactsNamelessRowWithAnUndecidedJIDSaysSo(t *testing.T) {
	const undecided = "ZWAADDRESSBOOKCONTACT row has no name or number (JID undecided)"
	const only = "ZWAADDRESSBOOKCONTACT row has no name or number (JID only)"
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZWHATSAPPID": "1\x00@s.whatsapp.net"}))
		put(2, contactRow(map[string]any{"ZWHATSAPPID": strings.Repeat("7", 2000) + "@s.whatsapp.net"}))
		put(3, contactRow(map[string]any{"ZWHATSAPPID": int64(3)}))
		put(4, contactRow(map[string]any{"ZWHATSAPPID": []byte("15550000004@s.whatsapp.net")}))
		put(5, contactRow(map[string]any{"ZWHATSAPPID": "15550000005@s.whatsapp.net"}))
	})
	recs, res := runContacts(t, db, nil, "ZWAADDRESSBOOKCONTACT row ")
	for id, want := range map[int64]string{1: undecided, 2: undecided, 3: undecided, 4: undecided, 5: only} {
		if got, n := warningFor(res, id); got != want || n != 1 {
			t.Errorf("row %d: %d warnings, %q, want exactly one %q", id, n, got, want)
		}
		if _, has := recs[id]; has {
			t.Errorf("row %d is a record", id)
		}
	}
}
