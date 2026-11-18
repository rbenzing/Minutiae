package androidwhatsapp_test

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator is the scenario oracle; these literal facts are written from the spec (6.4, 7.1, 7.3, 7.6 and
// the rulings R1, R3, R4), not read back from the generator, so a generator that drifted from the rules fails
// here before any parser exists.

func factsOf(t *testing.T, variant string) (byRow map[string]parsertest.ExpRecord, fx parsertest.Fixture) {
	t.Helper()
	fx = parsertest.LoadFixture(t, "testdata", variant)
	byRow = map[string]parsertest.ExpRecord{}
	for _, r := range fx.Expect.Records {
		byRow[r.Table+"/"+strconv.FormatInt(r.Rowid, 10)] = r
		// only a revoke tombstone is deleted, and it names the app's own structure that says so (spec 6.5, plan 4I task 7)
		del, _ := r.Payload["deleted"].(map[string]any)
		if r.Type == "call" {
			if r.Deleted || del != nil {
				t.Errorf("%s %s/%d: a deleted call", variant, r.Table, r.Rowid)
			}
			continue
		}
		if r.Type != "message" || r.Deleted != (del != nil) || (r.Deleted && del["source"] != "message_revoked" && del["source"] != "message_type" && del["source"] != "deleted_chat_job") {
			t.Errorf("%s %s/%d: type %q deleted %v %v", variant, r.Table, r.Rowid, r.Type, r.Deleted, del)
		}
		if r.Payload["status"] != "unknown" { // R4: no delivery status is mapped
			t.Errorf("%s %s/%d: status %v, want unknown", variant, r.Table, r.Rowid, r.Payload["status"])
		}
	}
	return byRow, fx
}

func rawOf(t *testing.T, r parsertest.ExpRecord) map[string]any {
	t.Helper()
	m, ok := r.Payload["raw"].(map[string]any)
	if !ok {
		t.Fatalf("%s/%d has no raw", r.Table, r.Rowid)
	}
	return m
}

func participants(r parsertest.ExpRecord) []any {
	p, _ := r.Payload["participants"].([]any)
	return p
}

func TestCoreOracleStatesTheSpecRules(t *testing.T) {
	modern, _ := factsOf(t, "android-modern-2026")
	if len(modern) != 59 { // the 18 rows of the core, rows 30 to 44 of plan 4I task 7 the five reactions of task 8, and rows 60 to 71 the reaction 9 of task 9 and the eight calls of task 10
		t.Fatalf("android-modern-2026 has %d records, want 59", len(modern))
	}
	get := func(set map[string]parsertest.ExpRecord, key string) parsertest.ExpRecord {
		r, ok := set[key]
		if !ok {
			t.Fatalf("no oracle record %s", key)
		}
		return r
	}
	// 4: an image: kind media, the label of the type, the caption as Body.
	m4 := get(modern, "message/4")
	if m4.Payload["kind"] != "media" || rawOf(t, m4)["label_message_type"] != "image" || m4.Body == nil || *m4.Body != "pic" {
		t.Errorf("message 4: %v body %v", m4.Payload, m4.Body)
	}
	// 12: an incoming status update has no attributed author (R3).
	m12 := get(modern, "message/12")
	if m12.Payload["participants_unknown"] != true || len(participants(m12)) != 0 || rawOf(t, m12)["sender_jid_row_id"] == nil {
		t.Errorf("message 12: %v", m12.Payload)
	}
	// 13: an outgoing status update keeps where it was posted.
	m13 := get(modern, "message/13")
	if !reflect.DeepEqual(participants(m13), []any{map[string]any{"role": "to", "address": "status@broadcast"}}) {
		t.Errorf("message 13 participants %v", participants(m13))
	}
	// 10: a group message with no sender has no participant.
	if m10 := get(modern, "message/10"); m10.Payload["participants_unknown"] != true || len(participants(m10)) != 0 {
		t.Errorf("message 10: %v", m10.Payload)
	}
	// 9: the sender, never the group.
	m9 := get(modern, "message/9")
	if !reflect.DeepEqual(participants(m9), []any{map[string]any{"role": "from", "address": "15550000002@s.whatsapp.net"}}) {
		t.Errorf("message 9 participants %v", participants(m9))
	}
	if th, _ := m9.Payload["thread"].(map[string]any); th["id"] != "120363000000000001@g.us" || th["group"] != true || th["title"] != "Group A" {
		t.Errorf("message 9 thread %v", m9.Payload["thread"])
	}
	// 14: an unmapped type: kind unknown, the stored value in raw, no label (R4).
	m14 := get(modern, "message/14")
	if m14.Payload["kind"] != "unknown" || fmt.Sprint(rawOf(t, m14)["message_type"]) != "999" { // numbers decode as json.Number
		t.Errorf("message 14: %v", m14.Payload)
	}
	if _, has := rawOf(t, m14)["label_message_type"]; has {
		t.Errorf("message 14 has a label for an unmapped type")
	}
	// 16: a seconds value in a millisecond column is invalid, not rescaled.
	m16 := get(modern, "message/16")
	if m16.Time != nil || rawOf(t, m16)["timestamp_status"] != "invalid" {
		t.Errorf("message 16: time %v raw %v", m16.Time, rawOf(t, m16))
	}
	// 17: zero is a sentinel.
	if m17 := get(modern, "message/17"); m17.Time != nil || rawOf(t, m17)["timestamp_status"] != "sentinel" {
		t.Errorf("message 17: time %v raw %v", m17.Time, rawOf(t, m17))
	}
	// 18: a channel is its own author of record.
	m18 := get(modern, "message/18")
	if !reflect.DeepEqual(participants(m18), []any{map[string]any{"role": "from", "address": "120363000000000099@newsletter"}}) {
		t.Errorf("message 18 participants %v", participants(m18))
	}
	// 19: a chat whose jid row has no raw_string: no address, the parts in raw, the parser never composes one.
	m19 := get(modern, "message/19")
	if len(participants(m19)) != 0 || m19.Payload["participants_unknown"] != true ||
		!reflect.DeepEqual(rawOf(t, m19)["from_jid"], map[string]any{"user": "15550000009", "server": "s.whatsapp.net"}) {
		t.Errorf("message 19: %v", m19.Payload)
	}
	// every record names the app by the path (D9).
	for k, r := range modern {
		if raw := rawOf(t, r); raw["app"] != "whatsapp" || raw["app_basis"] != "path" || (raw["source_table"] != "message" && r.Payload["kind"] != "reaction" && r.Type != "call") {
			t.Errorf("%s: app %v basis %v table %v", k, raw["app"], raw["app_basis"], raw["source_table"])
		}
	}
	if modern["message/2"].Time == nil || *modern["message/2"].Time != "2020-09-13T12:26:40Z" {
		t.Errorf("message 2 time %v", modern["message/2"].Time)
	}

	business, _ := factsOf(t, "android-modern-business")
	if raw := rawOf(t, business["message/2"]); raw["app"] != "whatsapp_business" {
		t.Errorf("a w4b path reads app %v", raw["app"])
	}

	legacy, lfx := factsOf(t, "android-legacy-2.11")
	// 15: legacy type 0 with status 6 is not text (spec 7.1).
	if l15 := get(legacy, "messages/15"); l15.Payload["kind"] != "unknown" {
		t.Errorf("legacy message 15: kind %v", l15.Payload["kind"])
	}
	// 4: the legacy caption is raw only.
	l4 := get(legacy, "messages/4")
	if l4.Body != nil || rawOf(t, l4)["media_caption"] != "pic" || l4.Payload["kind"] != "media" {
		t.Errorf("legacy message 4: body %v raw %v", l4.Body, rawOf(t, l4))
	}
	// 21 '1' is an image; 22 ' 1', 23 '1.0', 24 'x' and 25 a BLOB are unknown with the stored value.
	if l21 := get(legacy, "messages/21"); l21.Payload["kind"] != "media" || rawOf(t, l21)["label_media_wa_type"] != "image" {
		t.Errorf("legacy message 21: %v", l21.Payload)
	}
	for i, want := range map[int]any{22: " 1", 23: "1.0", 24: "x", 25: map[string]any{"blob_b64": "AQ=="}} {
		r := get(legacy, "messages/"+strconv.Itoa(i))
		raw := rawOf(t, r)
		if r.Payload["kind"] != "unknown" || !reflect.DeepEqual(raw["media_wa_type"], want) {
			t.Errorf("legacy message %d: kind %v media_wa_type %v, want unknown and %v", i, r.Payload["kind"], raw["media_wa_type"], want)
		}
	}
	// the sentinel is a warning, never a record.
	if _, has := legacy["messages/1"]; has {
		t.Error("the sentinel row has a record")
	}
	found := false
	for _, w := range lfx.Expect.Warnings {
		found = found || (w.Table == "messages" && w.Rowid == 1 && w.Contains == "sentinel")
	}
	if !found {
		t.Errorf("no sentinel warning: %v", lfx.Expect.Warnings)
	}
	// the incoming 1:1 message reads from the chat; the delivered time is the legacy receipt_device_timestamp.
	if l3 := get(legacy, "messages/3"); l3.Times["delivered"] == "" {
		t.Errorf("legacy message 3 has no delivered time: %v", l3.Times)
	}
}

func TestTransitionBothOracleHasBothTables(t *testing.T) {
	recs, fx := factsOf(t, "android-transition-both")
	for id := 2; id <= 17; id++ {
		a, aok := recs["messages/"+strconv.Itoa(id)]
		b, bok := recs["message/"+strconv.Itoa(id)]
		if !aok || !bok {
			t.Errorf("id %d: messages %v, message %v (each table must have a record)", id, aok, bok)
			continue
		}
		if rawOf(t, a)["source_table"] != "messages" || rawOf(t, b)["source_table"] != "message" {
			t.Errorf("id %d: source_table %v and %v", id, rawOf(t, a)["source_table"], rawOf(t, b)["source_table"])
		}
		if a.Payload["guid"] == nil || a.Payload["guid"] != b.Payload["guid"] {
			t.Errorf("id %d: guid %v and %v (a dual-written row shares its key id)", id, a.Payload["guid"], b.Payload["guid"])
		}
	}
	if len(recs) != 32 {
		t.Errorf("%d records, want 32 (16 per table)", len(recs))
	}
	found := false
	for _, w := range fx.Expect.Warnings {
		found = found || w.Contains == "whatsapp-both-generations"
	}
	if !found {
		t.Error("no whatsapp-both-generations warning (R1)")
	}
	if fx.Expect.Notes["rows.messages"] != "16" || fx.Expect.Notes["rows.message"] != "16" || fx.Expect.Notes["generation"] != "transition" {
		t.Errorf("notes %v", fx.Expect.Notes)
	}
}

func TestStructuralVariantsHaveTheirShape(t *testing.T) {
	for variant, want := range map[string]struct {
		records int
		gen     string
	}{
		"android-transition-legacydata": {25, "transition"}, "android-transition-moderndata": {16, "transition"},
		"android-transition-walonly": {17, "transition"}, "android-transition-empty": {0, "transition"},
		"android-modern-2021": {18, "modern"}, "android-neither": {0, ""}, "android-crypt14": {0, ""},
		"android-crypt15": {0, ""}, "android-backup-notsqlite": {0, ""}, "android-backup-plain": {0, ""},
		"android-other-location": {0, ""},
	} {
		_, fx := factsOf(t, variant)
		if len(fx.Expect.Records) != want.records || fx.Expect.Notes["generation"] != want.gen {
			t.Errorf("%s: %d records, generation %q; want %d and %q", variant, len(fx.Expect.Records), fx.Expect.Notes["generation"], want.records, want.gen)
		}
	}
	// the WAL variants carry a WAL companion; the walonly one's extra row exists only in a WAL frame.
	for _, v := range []string{"android-transition-walonly", "android-modern-2026"} {
		if fx := parsertest.LoadFixture(t, "testdata", v); len(fx.Files["wal"]) == 0 {
			t.Errorf("%s has no WAL", v)
		}
	}
}

// The oracle states the path the database is bundled at: a record of a variant without a WAL names it, and a
// record of a variant with a WAL states none (the harness derives it from the role of the cell).
func TestOracleSourcePathsFollowTheBundlePath(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		_, hasWAL := fx.Files["wal"]
		for _, r := range fx.Expect.Records {
			want := remoteFor(v)
			if hasWAL {
				want = ""
			}
			if r.SourcePath != want {
				t.Fatalf("%s %s/%d: source_path %q, want %q", v, r.Table, r.Rowid, r.SourcePath, want)
			}
		}
	}
}
