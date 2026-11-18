package androidwhatsapp_test

import (
	"fmt"
	"testing"
)

// The review of plan 4I tasks 8 to 11 (fix 4): a cleared-chat key never defaults to live, and a media path that a glob
// could misread is never looked up.

// I1: a chat_row_id that is neither NULL nor an INTEGER is an undecided chat, never "no chat": a row that a job could
// clear is pending_purge undecided, not live.
const sortedMessage = "create table message(_id integer primary key, chat_row_id integer, from_me integer, key_id text, " +
	"sender_jid_row_id integer, status integer, timestamp integer, received_timestamp integer, message_type integer, text_data text, " +
	"sort_id integer, starred integer)"

func TestWhatsAppDamagedChatRowIdIsUndecidedNotLive(t *testing.T) {
	db, _ := buildDB(map[string]string{"message": sortedMessage}, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["chat"].Insert(1, nil, int64(1), nil)
		tb["message"].Insert(1, nil, int64(1), int64(0), "K1", nil, int64(0), goodTS, nil, int64(0), "ok", int64(10), int64(0))
		// the same chat as 1, written as TEXT: the app's own query would not find it
		tb["message"].Insert(2, nil, "1", int64(0), "K2", nil, int64(0), goodTS, nil, int64(0), "damaged", int64(11), int64(0))
		tb["deleted_chat_job"].Insert(1, nil, int64(1), int64(100), nil, nil, nil)
	})
	recs := byRowid(t, runBytes(t, db, nil, remoteLive))
	if r := recs["message/1"]; !r.Deleted {
		t.Errorf("message 1 is in the cleared chat and below the bound, want Deleted")
	}
	r := recs["message/2"]
	raw := rawMap(t, r)
	if r.Deleted || raw["pending_purge"] != "undecided" || raw["pending_purge_reason"] != "row-chat-undecided" {
		t.Errorf("message 2: deleted %v raw %v, want undecided (row-chat-undecided)", r.Deleted, raw)
	}
}

// I2: when the join guard marks chat (its _id is not the rowid alias) the existence of a job's chat is unproven: no job is
// Dangling, none clears a row, and every row is pending_purge undecided with a counted warning.
func TestWhatsAppJobsOnGuardedChatTableAreUndecided(t *testing.T) {
	db, wal := buildDB(map[string]string{
		"message": sortedMessage,
		"chat":    "create table chat(_id integer, jid_row_id integer, subject text)",
	}, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		// chat 1 is stored at rowid 5: a join by rowid would call it absent
		tb["chat"].Insert(5, int64(1), int64(1), nil)
		tb["message"].Insert(1, nil, int64(1), int64(0), "K1", nil, int64(0), goodTS, nil, int64(0), "one", int64(10), int64(0))
		tb["deleted_chat_job"].Insert(1, nil, int64(1), int64(100), nil, nil, nil)
	})
	res := runBytes(t, db, wal, remoteLive)
	r := byRowid(t, res)["message/1"]
	raw := rawMap(t, r)
	if r.Deleted || raw["pending_purge"] != "undecided" {
		t.Errorf("message 1: deleted %v raw %v, want undecided and not cleared", r.Deleted, raw)
	}
	if res.Notes["deleted_chat_jobs_dangling"] != "" {
		t.Errorf("a job was called dangling on an unproven rowid join: %v", res.Notes)
	}
	if ws := warningsContaining(res, "whatsapp-pending-purge-undecided: deleted_chat_job 1: chat-join-undecided"); len(ws) != 1 {
		t.Errorf("warnings %v, want one chat-join-undecided for job 1", res.Warnings)
	}
}

// M1: a media path with a glob metacharacter is never looked up: raw.media_state and a counted undecided lookup, never
// "unresolved".
func TestWhatsAppMediaPathWithGlobMetacharacterIsUndecided(t *testing.T) {
	for _, name := range []string{"IMG[1].jpg", "IMG?.jpg", "IMG{a,b}.jpg", "IMG*.jpg"} {
		db, _ := buildDB(nil, func(tb tables) {
			jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
			tb["chat"].Insert(1, nil, int64(1), nil)
			tb["message"].Insert(1, nil, int64(1), int64(0), "K1", nil, int64(0), goodTS, nil, int64(1), nil)
			tb["message_media"].Insert(1, nil, "Media/WhatsApp Images/"+name, int64(10), "image/jpeg", name)
		})
		r := byRowid(t, runBytes(t, db, nil, remoteLive))["message/1"]
		raw := rawMap(t, r)
		if raw["media_state"] != "lookup-path-not-expressible" || fmt.Sprint(raw["attachments_lookup_undecided"]) != "1" ||
			fmt.Sprint(raw["attachments_unresolved"]) != "0" || fmt.Sprint(raw["attachments_resolved"]) != "0" {
			t.Errorf("%q: raw %v", name, raw)
		}
	}
}

// M2 (spec 6.6, 4I-R7): an outgoing call with call_result 2 is neither answered nor missed (two sources agree only for an
// incoming one): its outcome is unknown, the research label and the stored value stay raw, and "failed" is never invented.
func TestWhatsAppOutgoingCallResultTwoIsNeverInvented(t *testing.T) {
	db, _ := buildDB(nil, func(tb tables) {
		jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
		tb["call_log"].Insert(1, nil, int64(1), int64(1), "C1", goodTS, int64(0), int64(0), int64(2), int64(0))
	})
	r := callRecord(t, byRowid(t, runBytes(t, db, nil, remoteLive)), 1)
	raw := rawMap(t, r)
	if r.Payload["direction"] != "out" || r.Payload["outcome"] != "unknown" || raw["label_call_result"] != "not_answered" || fmt.Sprint(raw["call_result"]) != "2" {
		t.Errorf("payload %v raw %v, want out, unknown, label not_answered, raw call_result 2", r.Payload, raw)
	}
	if fmt.Sprint(r.Payload) == "" || r.Payload["outcome"] == "failed" || raw["label_call_result"] == "failed" {
		t.Errorf("failed was invented: %v", r.Payload)
	}
}
