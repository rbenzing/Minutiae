package message_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// v1Fixture is a stored v1 payload as the writer canonicalizes it, written out
// by hand so it does not move when the builder changes. It carries a field a
// future version might add (future_field) and one inside a participant.
const v1Fixture = `{
  "attachments": [{"artifact_id": "art-9", "artifact_sha256": "` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `", "mime": "image/jpeg", "name": "p.jpg", "size": 4096}],
  "body_flags": ["invalid_utf8"],
  "body_raw_b64": "aGkA/w==",
  "channel": "sms",
  "deleted": {"source": "wal"},
  "direction": "in",
  "future_field": {"anything": [1, 2, 3]},
  "guid": "ABC-1",
  "kind": "text",
  "participants": [{"address": "+15551234567", "future_in_participant": 1, "kind": "phone", "name": "Alex", "role": "from"}, {"address": "+15557654321", "role": "to"}],
  "raw": {"date": 1700000000123, "ratio": 0.5, "tags": ["a", "b"], "nested": {"ok": true}},
  "read": true,
  "recovery": {"relation": "uncommitted", "via": "wal", "wal": {"frame": 3, "committed": false}},
  "snapshot": {"name": "snap-1", "xid": 12},
  "status": "received",
  "subscription": 1,
  "thread": {"group": false, "id": 77, "members": ["+15551234567"], "title": "Alex"}
}`

func TestRecordTypesDecodeOlderVersions(t *testing.T) {
	t.Run("the version constant is the registered payload version", func(t *testing.T) {
		ty, ok := records.LookupType(message.Type)
		if !ok {
			t.Fatal("message is not a registered type")
		}
		if ty.PayloadVersion != message.PayloadVersion {
			t.Errorf("registered payload version %d, package constant %d", ty.PayloadVersion, message.PayloadVersion)
		}
	})

	t.Run("round trip equals the input", func(t *testing.T) {
		for name, m := range map[string]message.Message{"minimal": minimal(), "full": full(), "group MMS": groupMMS()} {
			b, err := json.Marshal(m.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := message.Decode(message.PayloadVersion, b)
			if err != nil {
				t.Fatalf("%s: Decode = %v", name, err)
			}
			if !reflect.DeepEqual(got, m) {
				t.Errorf("%s: round trip\n got  %+v\n want %+v", name, got, m)
			}
			if !reflect.DeepEqual(got.Payload(), m.Payload()) {
				t.Errorf("%s: the decoded message builds another payload", name)
			}
		}
	})

	t.Run("v1 fixture bytes decode", func(t *testing.T) {
		got, err := message.Decode(1, []byte(v1Fixture))
		if err != nil {
			t.Fatalf("Decode(v1 fixture) = %v", err)
		}
		want := message.Message{
			Channel: "sms", Direction: "in", Kind: "text",
			Participants: []message.Participant{
				{Role: "from", Address: "+15551234567", Name: "Alex", Kind: "phone"},
				{Role: "to", Address: "+15557654321"},
			},
			Thread:    &message.Thread{ID: "77", Title: "Alex", Group: ptr(false), Members: []string{"+15551234567"}},
			Status:    "received",
			Read:      ptr(true),
			GUID:      "ABC-1",
			BodyFlags: []string{"invalid_utf8"}, BodyRawB64: "aGkA/w==",
			Subscription: ptr(int64(1)),
			Attachments: []message.Attachment{{
				Name: "p.jpg", Mime: "image/jpeg", Size: ptr(int64(4096)), ArtifactID: "art-9",
				ArtifactSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			}},
			Deleted:  map[string]any{"source": "wal"},
			Recovery: map[string]any{"relation": "uncommitted", "via": "wal", "wal": map[string]any{"frame": int64(3), "committed": false}},
			Snapshot: map[string]any{"name": "snap-1", "xid": int64(12)},
			Raw:      map[string]any{"date": int64(1700000000123), "ratio": 0.5, "tags": []any{"a", "b"}, "nested": map[string]any{"ok": true}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Decode(v1 fixture)\n got  %+v\n want %+v", got, want)
		}
		// the fixture is itself a valid payload (a future field does not make it invalid)
		var p map[string]any
		dec := json.NewDecoder(strings.NewReader(v1Fixture))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if err := message.Validate(p); err != nil {
			t.Errorf("Validate(v1 fixture) = %v", err)
		}
	})

	t.Run("a future-added unknown field is ignored", func(t *testing.T) {
		withFuture := `{"channel":"sms","direction":"out","kind":"text","participants":[],"participants_unknown":true,"new_in_v1_1":{"deep":[1,{"x":null}]},"another":"x"}`
		got, err := message.Decode(1, []byte(withFuture))
		if err != nil {
			t.Fatalf("Decode = %v", err)
		}
		if got.Channel != "sms" || got.Direction != "out" || !got.ParticipantsUnknown || got.Participants != nil {
			t.Errorf("Decode = %+v", got)
		}
	})

	t.Run("other payload versions are unsupported", func(t *testing.T) {
		for _, v := range []int{0, 2, 3, -1, 1 << 30} {
			_, err := message.Decode(v, []byte(v1Fixture))
			if !errors.Is(err, message.ErrUnsupportedPayloadVersion) {
				t.Errorf("Decode(%d) = %v, want ErrUnsupportedPayloadVersion", v, err)
			}
		}
		if _, err := message.Decode(1, []byte(v1Fixture)); errors.Is(err, message.ErrUnsupportedPayloadVersion) {
			t.Error("v1 reported as unsupported")
		}
	})

	t.Run("damaged payloads are errors, never a panic and never an unsupported version", func(t *testing.T) {
		const marker = "SECRET-VALUE-9931"
		for name, b := range map[string]string{
			"empty":              ``,
			"not json":           `{"channel": `,
			"null":               `null`,
			"array":              `[]`,
			"string":             `"x"`,
			"trailing value":     `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true} {}`,
			"trailing garbage":   `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true} x`,
			"missing channel":    `{"direction":"in","kind":"text","participants":[],"participants_unknown":true}`,
			"wrong type":         `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":"` + marker + `"}`,
			"enum":               `{"channel":"sms","direction":"` + marker + `","kind":"text","participants":[],"participants_unknown":true}`,
			"big exponent":       `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"subscription":1e999}`,
			"fractional integer": `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"subscription":1.5}`,
			"exponent integer":   `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"subscription":1e3}`,
			"decimal integer":    `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"subscription":2.0}`,
			"exponent total":     `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"body_truncated":true,"body_total_bytes":1e3}`,
			"exponent thread id": `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"thread":{"id":1e3}}`,
		} {
			_, err := message.Decode(1, []byte(b))
			if err == nil {
				t.Errorf("%s: no error", name)
				continue
			}
			if errors.Is(err, message.ErrUnsupportedPayloadVersion) {
				t.Errorf("%s: reported as an unsupported version", name)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s: error repeats a payload value: %v", name, err)
			}
		}
	})

	t.Run("deeply nested input does not panic", func(t *testing.T) {
		deep := strings.Repeat(`{"a":`, 2000) + `1` + strings.Repeat(`}`, 2000)
		if _, err := message.Decode(1, []byte(`{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"raw":`+deep+`}`)); err == nil {
			t.Error("a payload nested 2000 deep was accepted")
		}
	})
}

// TestDecodeKeepsRecoveryAndSnapshot: a typed reader can never present a recovered
// or snapshot-derived message as a live one, so Decode carries both objects and
// Payload writes them back.
func TestDecodeKeepsRecoveryAndSnapshot(t *testing.T) {
	const in = `{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,` +
		`"recovery":{"relation":"uncommitted","via":"wal","wal":{"frame":3,"salt1":7,"committed":false},"notes":["a","b"]},` +
		`"snapshot":{"name":"snap-1","xid":12}}`
	got, err := message.Decode(1, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	wantRec := map[string]any{
		"relation": "uncommitted", "via": "wal", "notes": []any{"a", "b"},
		"wal": map[string]any{"frame": int64(3), "salt1": int64(7), "committed": false},
	}
	if !reflect.DeepEqual(got.Recovery, wantRec) {
		t.Errorf("Recovery = %#v, want %#v", got.Recovery, wantRec)
	}
	if want := map[string]any{"name": "snap-1", "xid": int64(12)}; !reflect.DeepEqual(got.Snapshot, want) {
		t.Errorf("Snapshot = %#v, want %#v", got.Snapshot, want)
	}
	p := got.Payload()
	if !reflect.DeepEqual(p["recovery"], wantRec) || !reflect.DeepEqual(p["snapshot"], map[string]any{"name": "snap-1", "xid": int64(12)}) {
		t.Errorf("Payload dropped the provenance: recovery %#v snapshot %#v", p["recovery"], p["snapshot"])
	}
	if err := message.Validate(p); err != nil {
		t.Errorf("the re-built payload does not validate: %v", err)
	}
	// a live message has neither
	live, err := message.Decode(1, []byte(`{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true}`))
	if err != nil || live.Recovery != nil || live.Snapshot != nil {
		t.Errorf("a live message decoded with provenance: %+v, %v", live, err)
	}
	if _, has := live.Payload()["recovery"]; has {
		t.Error("a live message built a recovery key")
	}
	// the deleted marker and recovery are independent
	both, err := message.Decode(1, []byte(`{"channel":"sms","direction":"in","kind":"text","participants":[],"participants_unknown":true,"deleted":{"source":"x"},"recovery":{}}`))
	if err != nil || both.Deleted == nil || both.Recovery == nil || len(both.Recovery) != 0 {
		t.Errorf("deleted+empty recovery: %+v, %v", both, err)
	}
	// the Message does not alias the payload it builds
	rec, snap := objOf(t, p, "recovery"), objOf(t, p, "snapshot")
	rec["via"] = "changed"
	if wal, ok := rec["wal"].(map[string]any); ok {
		wal["frame"] = "changed"
	}
	snap["name"] = "changed"
	if got.Recovery["via"] != "wal" || !reflect.DeepEqual(got.Recovery["wal"], map[string]any{"frame": int64(3), "salt1": int64(7), "committed": false}) || got.Snapshot["name"] != "snap-1" {
		t.Error("the payload aliases the message's recovery or snapshot")
	}
}
