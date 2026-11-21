package androidgooglemessages_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 10, 4I5 Task 4: every variant with records parses to the records, warnings and notes of its literal oracle, with no
// contract violation, as far as the tasks built so far go (pendingOracle states what is not built yet). No variant is
// dropped.
func TestAndroidGoogleMessagesMatchesOracle(t *testing.T) {
	ran := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused (no signature): the probe tests state those
		}
		ran++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			p := androidgooglemessages.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect), "messages", "parts")
		})
	}
	if ran != 8 {
		t.Errorf("%d variants with records, want 8", ran)
	}
}

// ownedRaw are the raw keys the messages mapping of Task 4 states (spec 6.8): the codes, the times, the read state, the
// stored scalars, the telephony link and the place. Every other raw key is Task 5's (participants, thread, direction
// conflict, sender, self, members, conversation) or Task 6's (parts, body).
var ownedRaw = map[string]bool{
	"app": true, "app_basis": true, "message_protocol": true, "message_protocol_state": true, "label_message_protocol": true,
	"message_status": true, "message_status_state": true, "label_message_status": true, "read": true, "seen": true,
	"sms_priority": true, "sms_message_size": true, "raw_status": true, "retry_start_timestamp": true, "retry_start_timestamp_unit": true,
	"mms_expiry": true, "mms_expiry_status": true, "mms_transaction_id": true, "mms_content_location": true,
	"received_timestamp": true, "received_timestamp_status": true, "sent_timestamp": true, "sent_timestamp_status": true, "time_basis": true,
	"sms_message_uri": true, "sms_message_uri_state": true, "sms_message_uri_len": true, "sms_message_uri_sha256": true,
	"sms_link": true, "sms_link_id_reuse": true, "sms_link_state": true, "unknown_columns": true, "text_flags": true, "row_flags": true,
}

// ownedUnknown are the unknown_columns entries and ownedText the text_flags fields of the same cells.
var (
	ownedUnknown = map[string]bool{
		"message_protocol": true, "message_status": true, "read": true, "seen": true, "sms_priority": true, "sms_message_size": true,
		"raw_status": true, "retry_start_timestamp": true, "mms_expiry": true, "mms_transaction_id": true, "mms_content_location": true,
		"received_timestamp": true, "sent_timestamp": true, "sms_message_uri": true,
	}
	ownedText = map[string]bool{"mms_transaction_id": true, "mms_content_location": true, "sms_message_uri": true}
)

// pendingOracle is the oracle of a variant as far as the tasks built so far go: a copy of exp without what Tasks 5 and 6
// add (Task 5 deletes its entries, Task 6 the rest and the function). It drops every parts-table record (and the
// notes and warnings of that table and of the Task 5 warnings), deletes the payload and raw keys of Task 5 (participants,
// thread title, group and members, sender, self, member and conversation keys, direction_conflict, sender_is_self,
// subscription; direction is rewritten to the status-only value and a conflicted incoming row regains its received time)
// and of Task 6 (Body, text_source, kind, attachments, subject, the body keys), and sets every Summary to nil. It never
// drops a variant.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	out := exp
	out.Records = nil
	for _, r := range exp.Records {
		if r.Table == "parts" {
			continue
		}
		r.Payload = copyMap(r.Payload)
		r.Times = copyStrings(r.Times)
		r.Summary, r.Body = nil, nil
		raw, _ := r.Payload["raw"].(map[string]any)
		if dc, ok := raw["direction_conflict"].(map[string]any); ok {
			dir, _ := dc["status_direction"].(string)
			r.Payload["direction"] = dir
			if dir == "in" && raw["time_basis"] == "received" && r.Time != nil {
				r.Times["received"] = *r.Time // the conflict made the direction unknown only in Task 5's reading
			}
		}
		for _, k := range []string{"subscription", "subject", "text_source", "attachments", "body_flags", "body_truncated", "body_total_bytes", "body_raw_b64", "body_raw_sha256", "body_raw_len"} {
			delete(r.Payload, k)
		}
		r.Payload["kind"] = "unknown"
		r.Payload["participants"] = []any{}
		r.Payload["participants_unknown"] = true
		if th, ok := r.Payload["thread"].(map[string]any); ok {
			r.Payload["thread"] = map[string]any{"id": th["id"]}
		}
		for k := range raw {
			if !ownedRaw[k] {
				delete(raw, k)
			}
		}
		if unk, ok := raw["unknown_columns"].(map[string]any); ok {
			for k := range unk {
				if !ownedUnknown[k] {
					delete(unk, k)
				}
			}
			if len(unk) == 0 {
				delete(raw, "unknown_columns")
			}
		}
		if tf, ok := raw["text_flags"].([]any); ok {
			var keep []any
			for _, f := range tf {
				if s, _ := f.(string); ownedText[s] {
					keep = append(keep, f)
				}
			}
			if len(keep) == 0 {
				delete(raw, "text_flags")
			} else {
				raw["text_flags"] = keep
			}
		}
		out.Records = append(out.Records, r)
	}
	out.Notes = map[string]string{}
	for k, v := range exp.Notes {
		if k != "schema.parts" && k != "unmapped_columns.parts" {
			out.Notes[k] = v
		}
	}
	out.Warnings = nil
	for _, w := range exp.Warnings {
		if strings.Contains(w.Contains, "googlemessages-direction-conflict") || strings.Contains(w.Contains, "googlemessages-members-cut") {
			continue
		}
		out.Warnings = append(out.Warnings, w)
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = copyAny(v)
	}
	return out
}

func copyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return copyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyAny(e)
		}
		return out
	}
	return v
}

func copyStrings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
