package androidtelegram_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8), F4, plan 4I4b Task 4: every variant with records parses to exactly the message, call and contact records, warnings
// and notes of its literal oracle, with no contract violation (the users table included, Task 12). The phone-call
// rows compare as call records (Task 11). No variant is dropped.
func TestAndroidTelegramMatchesOracle(t *testing.T) {
	ran := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused (version band, no message table): the probe and parse tests state those
		}
		ran++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			p := androidtelegram.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect))
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}

// mediaKeys4I4 are the raw.media keys the 4I4 mapping states for each media type (spec 8.5 of 4I4): pendingOracle keeps
// these, type, constructor and state, and nothing else of raw.media.
func mediaKeys4I4(typ string) []string {
	photo := []string{"id", "date", "access_hash_present", "file_reference_present"}
	geo := []string{"lat", "long", "lat_state", "lat_bits", "long_state", "long_bits"}
	switch typ {
	case "photo":
		return photo
	case "document":
		return append(photo, "size", "dc_id", "mime_type", "key_present", "iv_present")
	case "geo", "geo_live":
		return geo
	case "venue":
		return append(geo, "title", "address")
	case "contact":
		return []string{"phone_number", "phone_number_state", "phone_number_len", "phone_number_sha256", "first_name", "last_name", "vcard", "user_id"}
	case "web_page":
		return []string{"url", "display_url"}
	}
	return nil
}

// tailKeys4I4b are the raw keys of the plan's "Raw shapes" Tail that Task 6 maps; each also drops its _state, _len,
// _sha256, _unit, _total and _truncated siblings.
func tailKeys4I4b() []string {
	return []string{
		"blob_forwards", "forwards_conflict", "post_author", "quick_reply_shortcut_id", "effect", "paid_message_stars",
		"report_delivery_until_date", "schedule_repeat_period", "summary_from_language", "replies", "reactions",
		"reactions_results", "reactions_recent", "reactions_top_reactors", "restriction_reason", "factcheck",
		"suggested_post", "entities",
	}
}

// mappedFields4I4b are the fourteen names plan 4I4b adds to mappedFields() after ttl_period (Task 6).
func mappedFields4I4b() []string {
	return []string{
		"forwards", "post_author", "replies", "reactions", "restriction_reason", "quick_reply_shortcut_id", "effect",
		"factcheck", "report_delivery_until_date", "paid_message_stars", "suggested_post", "schedule_repeat_period",
		"summary_from_language", "entities",
	}
}

// pendingOracle is a copy of exp without what plan 4I4b Tasks 5 and 6 map (plan 4I4b Task 4, as 4I4's own bridge): the
// oracle restates the whole 4I4b mapping from its own decode before the Go mapping exists. It never drops a variant or
// a record. Media part (Task 5 deletes it): every attachment's name; the attachments of a video or audio record; in
// raw.media every key outside type, constructor, state and the 4I4 keys of its type (mediaKeys4I4); every raw key
// starting media_; the raw.text_flags names media.<k> whose k is not kept and media_<list>.<k>. Tail part (Task 6
// deletes it and the function): the Tail keys (tailKeys4I4b) with their siblings; replies_data (the decoded map) with
// its _state, _flags and _stop_* keys and replies_conflict; the text-flag names post_author, summary_from_language,
// restriction_reason.*, factcheck.*, entities.* and reactions.emoticon; the fourteen new names in data_undecided (the
// key removed when empty); the <table>.replies_data segments of the note blob_states and the telegram-blob-undecoded
// warnings of a replies_data column.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	out := exp
	out.Notes = maps.Clone(exp.Notes)
	if s, ok := out.Notes["blob_states"]; ok {
		var kept []string
		for seg := range strings.SplitSeq(s, ";") {
			if !strings.Contains(seg, ".replies_data=") {
				kept = append(kept, seg)
			}
		}
		out.Notes["blob_states"] = strings.Join(kept, ";")
	}
	out.Warnings = nil
	for _, w := range exp.Warnings {
		if strings.HasPrefix(w.Contains, "telegram-blob-undecoded: ") && strings.Contains(w.Contains, ".replies_data: ") {
			continue
		}
		out.Warnings = append(out.Warnings, w)
	}
	out.Records = make([]parsertest.ExpRecord, len(exp.Records))
	for i, r := range exp.Records {
		r.Payload = pendingPayload(r.Payload)
		out.Records[i] = r
	}
	return out
}

// pendingPayload is one record payload of pendingOracle (a deep copy; the oracle's own maps are never changed).
func pendingPayload(p map[string]any) map[string]any {
	p, _ = cloneJSON(p).(map[string]any)
	raw, _ := p["raw"].(map[string]any)
	if raw == nil {
		return p
	}
	media, _ := raw["media"].(map[string]any)
	typ, _ := media["type"].(string)
	keep := append([]string{"type", "constructor", "state"}, mediaKeys4I4(typ)...)
	for k := range media {
		if !slices.Contains(keep, k) {
			delete(media, k)
		}
	}
	if l, ok := p["attachments"].([]any); ok {
		var kept []any
		for _, a := range l {
			m, _ := a.(map[string]any)
			delete(m, "name")
			if len(m) > 0 && typ != "video" && typ != "audio" {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(p, "attachments")
		} else {
			p["attachments"] = kept
		}
	}
	tail := tailKeys4I4b()
	for k := range raw {
		base := k
		for _, suf := range []string{"_state", "_len", "_sha256", "_unit", "_total", "_truncated"} {
			if b, ok := strings.CutSuffix(k, suf); ok && slices.Contains(tail, b) {
				base = b
			}
		}
		switch {
		case strings.HasPrefix(k, "media_"), slices.Contains(tail, base), k == "replies_conflict",
			k == "replies_data", k == "replies_data_state", k == "replies_data_flags", strings.HasPrefix(k, "replies_data_stop_"):
			delete(raw, k)
		}
	}
	if s, ok := raw["text_flags"].(string); ok {
		var kept []string
		for name := range strings.SplitSeq(s, ",") {
			k, isMedia := strings.CutPrefix(name, "media.")
			switch {
			case isMedia && !slices.Contains(keep, k), strings.HasPrefix(name, "media_"), name == "post_author",
				name == "summary_from_language", name == "reactions.emoticon", strings.HasPrefix(name, "restriction_reason."),
				strings.HasPrefix(name, "factcheck."), strings.HasPrefix(name, "entities."):
				continue
			}
			kept = append(kept, name)
		}
		if len(kept) == 0 {
			delete(raw, "text_flags")
		} else {
			raw["text_flags"] = strings.Join(kept, ",")
		}
	}
	if s, ok := raw["data_undecided"].(string); ok {
		var kept []string
		for name := range strings.SplitSeq(s, ",") {
			if !slices.Contains(mappedFields4I4b(), name) {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			delete(raw, "data_undecided")
		} else {
			raw["data_undecided"] = strings.Join(kept, ",")
		}
	}
	return p
}

// cloneJSON is a deep copy of a decoded JSON value.
func cloneJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = cloneJSON(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = cloneJSON(e)
		}
		return l
	}
	return v
}

// pendingRecover is a copy of the typed recovery oracle want with every record's payload through pendingPayload and
// its digest recomputed from the result as the generator computes it (telephony_common.record_digest: the canonical
// JSON of type, time, time_end, times, body and payload, sorted keys, no spaces, no HTML escaping). Every digest of the
// telegram-deleted oracle has its record in full, so no digest is left unchecked. Task 6 deletes it with pendingOracle.
func pendingRecover(t *testing.T, want parsertest.RecoverExpect) parsertest.RecoverExpect {
	t.Helper()
	out := want
	out.Records = make([]parsertest.RecoverExpectRecord, len(want.Records))
	digests := map[string]string{}
	key := func(table string, rowid int64, file string, page uint32, off int64) string {
		return table + "/" + itoa(rowid) + "/" + file + "/" + itoa(int64(page)) + "/" + itoa(off)
	}
	digest := func(r parsertest.ExpRecord) string {
		times := r.Times
		if times == nil {
			times = map[string]string{}
		}
		body := ""
		if r.Body != nil {
			body = *r.Body
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]any{
			"type": r.Type, "time": r.Time, "time_end": r.TimeEnd, "times": times, "body": body, "payload": r.Payload,
		}); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte{10}))
		return hex.EncodeToString(sum[:])
	}
	stored := map[string]string{}
	for _, d := range want.Digests {
		stored[key(d.Table, d.Rowid, d.File, d.Page, d.Offset)] = d.Digest
	}
	for i, r := range want.Records {
		var rowid int64
		if r.Rowid != nil {
			rowid = *r.Rowid
		}
		k := key(r.Table, rowid, r.File, r.Page, r.Offset)
		if got := digest(r.Record); got != stored[k] { // the canonical form is the generator's: checked on every record
			t.Fatalf("recover oracle %s: the recomputed digest %s is not the generator's %s", k, got, stored[k])
		}
		r.Record.Payload = pendingPayload(r.Record.Payload)
		out.Records[i] = r
		digests[k] = digest(r.Record)
	}
	out.Digests = make([]parsertest.RecoverDigest, len(want.Digests))
	for i, d := range want.Digests {
		got, ok := digests[key(d.Table, d.Rowid, d.File, d.Page, d.Offset)]
		if !ok {
			t.Fatalf("recover oracle digest %s/%d at %s page %d offset %d has no record in full: pendingRecover cannot filter it", d.Table, d.Rowid, d.File, d.Page, d.Offset)
		}
		d.Digest = got
		out.Digests[i] = d
	}
	return out
}
