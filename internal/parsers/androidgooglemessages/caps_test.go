package androidgooglemessages_test

import (
	"fmt"
	"strings"
	"testing"
)

// Spec 9, 6.4, 6.5, 8.2: every cap is a counted, stated cut, never a silent one (H7, H8, H10, H11, H12).
func TestGoogleMessagesCapsAreCountedNeverSilent(t *testing.T) {
	res, recs := fixtureRecords(t, "gm-hostile")

	// H7: 1,001 text parts, 1,000 kept in part id order
	h7 := recs[107]
	var want []string
	for i := 0; i < 1000; i++ {
		want = append(want, fmt.Sprintf("p%04d", i))
	}
	if rawOf(h7)["parts"] != int64(1000) || rawOf(h7)["parts_total"] != int64(1001) || rawOf(h7)["parts_truncated"] != true || h7.Body != strings.Join(want, "\n") {
		t.Errorf("H7: parts %v total %v truncated %v body %d bytes", rawOf(h7)["parts"], rawOf(h7)["parts_total"], rawOf(h7)["parts_truncated"], len(h7.Body))
	}

	// H8: one 4 MiB text part, the Body cut at 256 KiB with the total
	h8 := recs[108]
	if len(h8.Body) != 256<<10 || h8.Payload["body_truncated"] != true || fmt.Sprint(h8.Payload["body_total_bytes"]) != "4194304" {
		t.Errorf("H8: body %d bytes, truncated %v, total %v", len(h8.Body), h8.Payload["body_truncated"], h8.Payload["body_total_bytes"])
	}

	// H10: a 10 KiB content_type and uri, both cut at 4 KiB and named in text_flags
	h10 := recs[110]
	atts, _ := h10.Payload["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("H10: %d attachments", len(atts))
	}
	a, _ := atts[0].(map[string]any)
	if mime, _ := a["mime"].(string); len(mime) != 4<<10 {
		t.Errorf("H10: mime %d bytes, want 4096", len(mime))
	}
	if ref, _ := a["source_ref"].(string); len(ref) != 4<<10 {
		t.Errorf("H10: source_ref %d bytes, want 4096", len(ref))
	}
	if fmt.Sprint(rawOf(h10)["text_flags"]) != "[attachments.mime attachments.source_ref]" {
		t.Errorf("H10: text_flags %v", rawOf(h10)["text_flags"])
	}

	// H11: 1,001 members, 1,000 kept, members_total and the warning say so
	for _, id := range []int64{111, 112} {
		r := recs[id]
		if rawOf(r)["members_total"] != int64(1001) || rawOf(r)["members_cut"] != true || len(threadMembers(r)) != 1000 {
			t.Errorf("H11 message %d: members_total %v cut %v kept %d", id, rawOf(r)["members_total"], rawOf(r)["members_cut"], len(threadMembers(r)))
		}
	}
	if w := warningsWith(res, "googlemessages-members-cut: conversation 6: 1001 members, 1000 kept"); len(w) != 1 {
		t.Errorf("H11 warnings %q", w)
	}

	// H12: a 2 KiB destination is never cut, never an address
	h12 := recs[113]
	if rawOf(h12)["from_address_state"] != "too-long" || rawOf(h12)["from_address_len"] != int64(2048) || len(rolesOf(h12)) != 0 {
		t.Errorf("H12: raw %v parties %v", rawOf(h12), rolesOf(h12))
	}
}
