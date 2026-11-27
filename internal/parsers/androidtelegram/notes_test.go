package androidtelegram_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// Spec 8.10, 4I4-P7 and the plan's "Notes and warnings": the notes state the store before any mapping (the live view's
// user version, the account and Android user from the path, the generation of the message tables holding rows, the
// census row counts and the tables never read), all within the host's MaxNotes.
func TestAndroidTelegramNotesStateTheStore(t *testing.T) {
	cases := []struct {
		variant, remote string
		want            map[string]string
	}{
		{"telegram-v181", remoteApp, map[string]string{
			"database_version": "181", "account": "0", "database_user": "0", "generation": "messages_v2",
			// the census counts the live view: R1-R59 less R52, whose WAL frame is uncommitted. Ruling F1 gave 55 on
			// R1-R56; R57-R59 (F33, F22) add three; plan 4I4b's R60-R80 add 21.
			"rows.messages_v2": "95",
			"rows.users":       "10", "rows.enc_chats": "2", "rows.chats": "5", "rows.contacts": "3", "rows.users_data": "1",
			"rows.dialogs": "0", "rows.media_v4": "0", "rows.messages_topics": "0", "rows.scheduled_messages_v2": "0",
			"rows.user_contacts_v7": "0",
			"unmapped_tables":       "dialogs,media_v4,messages_holes,messages_seq,messages_topics,randoms_v2,scheduled_messages_v2,user_contacts_v7,user_phones_v7",
		}},
		{"telegram-both", remoteApp, map[string]string{"generation": "both", "rows.messages": "2", "rows.messages_v2": "2"}},
		{"telegram-v64", remoteApp, map[string]string{
			"database_version": "64", "generation": "messages", "rows.messages": "8", "rows.media_v2": "0", // P1-P6, plan 4I4b P7, P8
			"unmapped_tables": "dialogs,media_v2,messages_holes,messages_seq,user_contacts_v7,user_phones_v7",
		}},
		{"telegram-v181", remoteAccount1, map[string]string{"account": "1", "database_user": "0"}},
		{"telegram-v181", "/data/user/10/org.telegram.messenger/files/account007/cache4.db", map[string]string{"account": "007", "database_user": "10"}},
		{"telegram-v181", remoteOther, map[string]string{"account": "unknown", "database_user": "unknown", "database_version": "181"}},
	}
	for _, c := range cases {
		res := parseAt(t, c.remote, fixtureFiles(t, c.variant))
		if res.Err != nil {
			t.Errorf("%s at %s: Parse err %v", c.variant, c.remote, res.Err)
			continue
		}
		for k, v := range c.want {
			if got := res.Notes[k]; got != v {
				t.Errorf("%s at %s: note %s = %q, want %q", c.variant, c.remote, k, got, v)
			}
		}
		if _, has := res.Notes["not_applicable"]; has {
			t.Errorf("%s at %s: note not_applicable %q", c.variant, c.remote, res.Notes["not_applicable"])
		}
		if limit := parse.DefaultLimits().MaxNotes; len(res.Notes) >= limit {
			t.Errorf("%s: %d notes: at the cap MaxNotes %d a note may have been dropped", c.variant, len(res.Notes), limit)
		}
	}
	// the generation counts rows, not tables: telegram-v181 has no messages table, telegram-v64 no messages_v2
	res := parseAt(t, remoteApp, fixtureFiles(t, "telegram-v181"))
	for _, k := range []string{"rows.messages", "rows.media_v2", "rows.media_v3"} {
		if _, has := res.Notes[k]; has {
			t.Errorf("telegram-v181: note %s stated for a table the store lacks", k)
		}
	}
}
