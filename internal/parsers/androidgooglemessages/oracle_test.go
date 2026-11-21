package androidgooglemessages_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 10, 4I5 Tasks 4 and 5: every variant with records parses to the records, warnings and notes of its literal oracle,
// with no contract violation, as far as the tasks built so far go (pendingOracle states what is not built yet). No variant
// is dropped.
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

// task6Raw are the raw keys of the body and the parts (Task 6): the oracle's record states them, the parser does not yet.
// Every other raw key is Task 4's or Task 5's and is compared.
var task6Raw = map[string]bool{
	"parts": true, "parts_total": true, "parts_truncated": true, "parts_text_and_uri": true, "parts_empty": true, "body_state": true,
	"attachment_parts": true,
}

// task6Unknown and task6Text are the unknown_columns entries and the text_flags fields of the same cells.
var (
	task6Unknown = map[string]bool{"mms_subject": true}
	task6Text    = map[string]bool{"subject": true, "attachments.mime": true, "attachments.source_ref": true}
)

// pendingOracle is the oracle of a variant as far as the tasks built so far go: a copy of exp without what Task 6 adds
// (Task 6 deletes the rest and the function). It drops every parts-table record (and the notes of that table), deletes the
// payload and raw keys of Task 6 (Body, text_source, kind, attachments, subject, the body and parts keys), and sets every
// Summary to nil (it states the body). It never drops a variant.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	out := exp
	out.Records = nil
	for _, r := range exp.Records {
		if r.Table == "parts" {
			continue
		}
		r.Payload = copyMap(r.Payload)
		r.Summary, r.Body = nil, nil
		raw, _ := r.Payload["raw"].(map[string]any)
		for _, k := range []string{"subject", "text_source", "attachments", "body_flags", "body_truncated", "body_total_bytes", "body_raw_b64", "body_raw_sha256", "body_raw_len"} {
			delete(r.Payload, k)
		}
		r.Payload["kind"] = "unknown"
		for k := range raw {
			if task6Raw[k] {
				delete(raw, k)
			}
		}
		if unk, ok := raw["unknown_columns"].(map[string]any); ok {
			for k := range unk {
				if task6Unknown[k] || strings.HasPrefix(k, "parts.") {
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
				if s, _ := f.(string); !task6Text[s] {
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
