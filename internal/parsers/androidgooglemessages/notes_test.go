package androidgooglemessages_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
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
	// Task 4: the store without mms_subject has no subject and no unknown_columns entry for the column on any record
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 2})
	msgs := b.CreateTable("messages", ddlMessagesNoSubject)
	msgs.Insert(1, nil, int64(1), int64(2), int64(1), tT, tT+1000, int64(0), int64(100), int64(1), int64(1), nil, int64(0), int64(0), nil, nil, int64(0), int64(0), int64(0))
	b.CreateTable("parts", ddlPartsNoWidth).Insert(7, nil, int64(1), nil, "content://mms/part/1", "image/png", int64(480), tT, int64(1))
	b.CreateTable("participants", ddlParticipantsFull)
	b.CreateTable("conversations", ddlConvsFull)
	b.CreateTable("conversation_participants", ddlMembers)
	res := parseAt(t, remoteApp, dbOnly(b.Bytes()))
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("a store without mms_subject: err %v, %d records, want 1", res.Err, len(res.Records))
	}
	for _, r := range res.Records {
		if _, has := r.Payload["subject"]; has {
			t.Errorf("%s: subject %v", r.Locator, r.Payload["subject"])
		}
		for k := range unknownOf(r) {
			if strings.Contains(k, "mms_subject") {
				t.Errorf("%s: unknown_columns names %s", r.Locator, k)
			}
		}
		// Task 6: the store without parts.width states no width of the attachment part and no unknown entry for the column
		ap, _ := rawOf(r)["attachment_parts"].([]any)
		if len(ap) != 1 || !reflect.DeepEqual(ap[0], map[string]any{"part_id": int64(7), "height": int64(480)}) {
			t.Errorf("%s: attachment_parts %v, want part 7 with a height and no width", r.Locator, ap)
		}
		for k := range unknownOf(r) {
			if strings.Contains(k, "width") {
				t.Errorf("%s: unknown_columns names %s", r.Locator, k)
			}
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
	// Task 4: the canonical records of the two jobs differ only in artifact_id and source_path
	canon := func(remote string) []string {
		res := parseAt(t, remote, files)
		var out []string
		for _, line := range parsertest.CanonicalRecords(res.Records) {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatal(err)
			}
			delete(m, "artifact_id")
			delete(m, "source_path")
			b, _ := json.Marshal(m)
			out = append(out, string(b))
		}
		return out
	}
	a, b := canon(remoteApp), canon(remoteUser10)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%d and %d records", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("record %d differs beyond artifact_id and source_path:\n%s\n%s", i, a[i], b[i])
		}
	}
	if ra, rb := parseAt(t, remoteApp, files), parseAt(t, remoteUser10, files); ra.Records[0].SourcePath == rb.Records[0].SourcePath {
		t.Errorf("both users' records have the source path %s", ra.Records[0].SourcePath)
	}
}
