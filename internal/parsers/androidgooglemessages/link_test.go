package androidgooglemessages_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Spec 6.8, R10 (C2): sms_message_uri is parsed only in the exact forms content://sms/<digits> and content://mms/<digits>
// (1 to 19 ASCII digits that fit int64, leading zeros allowed); anything else is unparsed with the text as stored; an
// identifier over 1 KiB is never cut; NULL states nothing; a BLOB is listed.
func TestGoogleMessagesSmsLinkParsedStrictly(t *testing.T) {
	type link struct {
		authority string
		id        int64
		reuse     string
	}
	good := map[string]link{
		"content://sms/41":                  {"sms", 41, "possible"},
		"content://mms/7":                   {"mms", 7, "none"},
		"content://sms/007":                 {"sms", 7, "possible"},
		"content://mms/9223372036854775807": {"mms", 9223372036854775807, "none"},
	}
	bad := []string{
		"content://sms/-1", "content://sms/+1", "content://sms/ 1", "content://SMS/1", "content://sms/1/", "content://sms/1?x",
		"content://sms/", "content://mms-sms/1", "content://mms/9223372036854775808", "content://sms/12a",
	}
	var rows []mrow
	var uris []string
	for u := range good {
		uris = append(uris, u)
	}
	uris = append(uris, bad...)
	for i, u := range uris {
		rows = append(rows, mrow{int64(i + 1), map[string]any{"sms_message_uri": u}})
	}
	long := "content://sms/" + strings.Repeat("9", 1100)
	n := int64(len(uris))
	rows = append(rows,
		mrow{n + 1, map[string]any{"sms_message_uri": long}},
		mrow{n + 2, map[string]any{"sms_message_uri": nil}},
		mrow{n + 3, map[string]any{"sms_message_uri": []byte{1, 2, 3}}})
	_, got := parsedRows(t, rows)
	for i, u := range uris {
		r := got[int64(i+1)]
		raw := rawOf(r)
		if want, ok := good[u]; ok {
			sl, _ := raw["sms_link"].(map[string]any)
			if raw["sms_message_uri"] != u || !reflect.DeepEqual(sl, map[string]any{"authority": want.authority, "id": want.id}) ||
				raw["sms_link_id_reuse"] != want.reuse || has(r, "sms_link_state") {
				t.Errorf("%q: raw %v", u, raw)
			}
			continue
		}
		if raw["sms_link_state"] != "unparsed" || raw["sms_message_uri"] != u || has(r, "sms_link") || has(r, "sms_link_id_reuse") {
			t.Errorf("%q: raw %v", u, raw)
		}
	}
	r := got[n+1]
	raw := rawOf(r)
	if raw["sms_message_uri_state"] != "too-long" || raw["sms_message_uri_len"] != int64(len(long)) || raw["sms_message_uri_sha256"] != sha([]byte(long)) ||
		has(r, "sms_message_uri") || has(r, "sms_link") || has(r, "sms_link_state") {
		t.Errorf("1,114 bytes: raw %v", raw)
	}
	if len(long) != 1114 {
		t.Fatalf("the long uri has %d bytes", len(long))
	}
	r = got[n+2]
	for _, k := range []string{"sms_message_uri", "sms_message_uri_state", "sms_message_uri_len", "sms_message_uri_sha256", "sms_link", "sms_link_id_reuse", "sms_link_state"} {
		if has(r, k) {
			t.Errorf("NULL: raw key %s = %v", k, rawOf(r)[k])
		}
	}
	if unknownOf(r) != nil {
		t.Errorf("NULL: unknown_columns %v", unknownOf(r))
	}
	r = got[n+3]
	if unknownOf(r)["sms_message_uri"] != "type-mismatch" || has(r, "sms_message_uri") || has(r, "sms_link_state") {
		t.Errorf("BLOB: raw %v", rawOf(r))
	}
}

// Review Focus 3: the sms _id can be reused, so two messages naming the same row are two records, each stating the link
// and neither a guid.
func TestGoogleMessagesReusedSmsIdIsTwoRecords(t *testing.T) {
	res, got := parsedRows(t, []mrow{
		{1, map[string]any{"sms_message_uri": "content://sms/41"}},
		{2, map[string]any{"sms_message_uri": "content://sms/41"}},
	})
	if len(res.Records) != 2 || len(got) != 2 {
		t.Fatalf("%d records", len(res.Records))
	}
	for id, r := range got {
		sl, _ := rawOf(r)["sms_link"].(map[string]any)
		if sl["authority"] != "sms" || sl["id"] != int64(41) || rawOf(r)["sms_link_id_reuse"] != "possible" {
			t.Errorf("row %d: raw %v", id, rawOf(r))
		}
		if _, ok := r.Payload["guid"]; ok {
			t.Errorf("row %d has a guid", id)
		}
	}
}
