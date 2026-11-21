package androidgooglemessages_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// Spec 5.4: an uncited user version is stated as a note and one counted warning, and is read by the AOSP core signature
// (the outcome stays complete); a cited one has no such warning.
func TestGoogleMessagesUncitedUserVersionIsStated(t *testing.T) {
	res := parseAt(t, remoteApp, fixtureFiles(t, "gm-proprietary-shaped"))
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	if res.Notes["user_version"] != "9000" || res.Notes["user_version_cited"] != "no" {
		t.Errorf("notes user_version %q, user_version_cited %q", res.Notes["user_version"], res.Notes["user_version_cited"])
	}
	want := parsertest.Warning{Locator: "sqlite:schema", Reason: "googlemessages-schema-uncited: user_version 9000 is not a cited AOSP version; read by the AOSP core signature"}
	if len(res.Warnings) != 1 || res.Warnings[0] != want {
		t.Errorf("warnings %+v, want exactly %+v", res.Warnings, want)
	}
	res = parseAt(t, remoteApp, fixtureFiles(t, "gm-aosp-v1"))
	if res.Err != nil || res.Notes["user_version"] != "1" || res.Notes["user_version_cited"] != "1" {
		t.Errorf("gm-aosp-v1: err %v, notes %q %q", res.Err, res.Notes["user_version"], res.Notes["user_version_cited"])
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "schema-uncited") {
			t.Errorf("gm-aosp-v1: warning %+v", w)
		}
	}
}

// Spec 5.4: the notes state the store before any mapping, all within the host's MaxNotes.
func TestAndroidGoogleMessagesNotesStateTheStore(t *testing.T) {
	v2 := parsertest.LoadFixture(t, "testdata", "gm-aosp-v2").Expect.Notes // the counts the generator read back from the engine
	cases := []struct {
		variant, remote string
		want            map[string]string
	}{
		{"gm-aosp-v2", remoteApp, map[string]string{
			"user_version": "2", "user_version_cited": "2", "journal_mode": "rollback", "app": "google_messages", "database_user": "0",
			"rows.messages": "36", "rows.participants": "7", "rows.conversations": "5",
			"rows.parts": v2["rows.parts"], "rows.conversation_participants": v2["rows.conversation_participants"],
		}},
		{"gm-aosp-v2-wal", remoteApp, map[string]string{"journal_mode": "wal"}},
		{"gm-aosp-v2", remoteAOSP, map[string]string{"app": "aosp_messaging", "database_user": "0"}},
		{"gm-aosp-v2", remoteOther, map[string]string{"app": "unknown", "database_user": "undecided"}},
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
		if len(res.Notes) > 64 || len(res.Notes) >= parse.DefaultLimits().MaxNotes {
			t.Errorf("%s: %d notes: at the cap a note may have been dropped", c.variant, len(res.Notes))
		}
	}
}

// recorder is a parse.Emitter that keeps the notes and counts the Note calls per key.
type recorder struct {
	notes map[string]string
	calls map[string]int
}

func newRecorder() *recorder { return &recorder{notes: map[string]string{}, calls: map[string]int{}} }

func (r *recorder) Emit(context.Context, records.Record) error { return nil }
func (r *recorder) Warn(context.Context, string, string) error { return nil }
func (r *recorder) Progress(int64, int64)                      {}
func (r *recorder) Note(key, value string) {
	r.notes[key] = value
	r.calls[key]++
}

// The DDL of a v2 store lacking exactly messages.mms_subject and parts.width: every other column of the mapping is there.
const (
	ddlMessagesNoSubject = `CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id INT, sender_id INT, self_id INT, sent_timestamp INT, received_timestamp INT, message_protocol INT, message_status INT, seen INT, read INT, sms_message_uri TEXT, sms_priority INT, sms_message_size INT, mms_transaction_id TEXT, mms_content_location TEXT, mms_expiry INT, raw_status INT, retry_start_timestamp INT)`
	ddlPartsNoWidth      = `CREATE TABLE parts (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_id INT, text TEXT, uri TEXT, content_type TEXT, height INT, timestamp INT, conversation_id INT)`
	ddlParticipantsFull  = `CREATE TABLE participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, sub_id INT, sim_slot_id INT, normalized_destination TEXT, send_destination TEXT, display_destination TEXT, full_name TEXT, contact_id INT, blocked INT, subscription_name TEXT)`
	ddlConvsFull         = `CREATE TABLE conversations (_id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, sms_thread_id INT, archive_status INT, participant_count INT, sort_timestamp INT, last_read_timestamp INT, IS_ENTERPRISE INT)`
)

// B4 (spec 5.4, R15): a column the mapping reads and the signature does not require, missing from the store, is stated
// once per job as columns_missing.<table>; a store that has every column states nothing.
func TestGoogleMessagesMissingOptionalColumnIsNotedOnce(t *testing.T) {
	run := func(files map[string][]byte) *recorder {
		t.Helper()
		h := parsertest.NewHarness(t)
		p := androidgooglemessages.Parser{}
		rec := newRecorder()
		if err := p.Parse(t.Context(), inputAt(t, h, p, remoteApp, files, false), rec); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return rec
	}
	rec := run(dbOnly(goBuilt(2, table{"messages", ddlMessagesNoSubject}, table{"parts", ddlPartsNoWidth}, table{"participants", ddlParticipantsFull},
		table{"conversations", ddlConvsFull}, table{"conversation_participants", ddlMembers})))
	for k, want := range map[string]string{"columns_missing.messages": "mms_subject", "columns_missing.parts": "width"} {
		if rec.notes[k] != want || rec.calls[k] != 1 {
			t.Errorf("note %s = %q (%d calls), want %q once", k, rec.notes[k], rec.calls[k], want)
		}
	}
	for _, tbl := range []string{"participants", "conversations", "conversation_participants"} {
		if _, has := rec.calls["columns_missing."+tbl]; has {
			t.Errorf("note columns_missing.%s = %q for a store that has every column", tbl, rec.notes["columns_missing."+tbl])
		}
	}
	v2 := run(fixtureFiles(t, "gm-aosp-v2"))
	for k, v := range v2.notes {
		if strings.HasPrefix(k, "columns_missing.") {
			t.Errorf("gm-aosp-v2: note %s = %q", k, v)
		}
	}
	v1 := run(fixtureFiles(t, "gm-aosp-v1"))
	if v1.notes["columns_missing.conversations"] != "IS_ENTERPRISE" || v1.calls["columns_missing.conversations"] != 1 {
		t.Errorf("gm-aosp-v1: note columns_missing.conversations = %q (%d calls)", v1.notes["columns_missing.conversations"], v1.calls["columns_missing.conversations"])
	}
	for _, tbl := range []string{"messages", "parts", "participants", "conversation_participants"} {
		if _, has := v1.calls["columns_missing."+tbl]; has {
			t.Errorf("gm-aosp-v1: note columns_missing.%s", tbl)
		}
	}
}

// Review Focus 1: two Android users' copies of the same database are two jobs, each stating its own user.
func TestGoogleMessagesTwoUsersAreKeptApart(t *testing.T) {
	files := fixtureFiles(t, "gm-aosp-v2")
	for remote, user := range map[string]string{remoteApp: "0", remoteUser10: "10"} {
		res := parseAt(t, remote, files)
		if res.Err != nil || res.Notes["database_user"] != user {
			t.Errorf("%s: err %v, note database_user = %q, want %s", remote, res.Err, res.Notes["database_user"], user)
		}
	}
}
