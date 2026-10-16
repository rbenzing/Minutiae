package androidmms_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator is the scenario oracle; these literal facts are written from the spec (9.1, 9.2)
// and the public PduHeaders source (ruling T42), not read back from the generator, so a generator
// that drifted from the rules fails here before any parser exists.
func TestBaselineOracleStatesTheSpecRules(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	byKey := map[string]parsertest.ExpRecord{}
	counts := map[string]int{}
	for _, r := range fx.Expect.Records {
		counts[r.Table]++
		byKey[r.Table+"/"+strconv.FormatInt(r.Rowid, 10)] = r
		if r.Type != "message" {
			t.Errorf("%s %d: type %q, want message", r.Table, r.Rowid, r.Type)
		}
		if r.SourcePath != remoteDB {
			t.Errorf("%s %d: source_path %q, want %q", r.Table, r.Rowid, r.SourcePath, remoteDB)
		}
		if r.Deleted {
			t.Errorf("%s %d: a live oracle row is marked deleted", r.Table, r.Rowid)
		}
	}
	if counts["sms"] != 22 || counts["pdu"] != 6 || len(fx.Expect.Records) != 28 {
		t.Fatalf("record counts %v, want 22 sms and 6 pdu", counts)
	}
	raw := func(k string) map[string]any {
		r, ok := byKey[k]
		if !ok {
			t.Fatalf("no oracle record %s", k)
		}
		m, _ := r.Payload["raw"].(map[string]any)
		return m
	}
	// 6: a seconds value in a milliseconds column is invalid, not guessed (D6).
	r6 := byKey["sms/6"]
	if r6.Time != nil || len(r6.Times) != 0 || raw("sms/6")["date_status"] != "invalid" {
		t.Errorf("sms 6: time %v times %v date_status %v", r6.Time, r6.Times, raw("sms/6")["date_status"])
	}
	// 5: date 0 is a sentinel; 20: the year 2100 is out of range.
	if raw("sms/5")["date_status"] != "sentinel" || byKey["sms/5"].Time != nil {
		t.Errorf("sms 5: %v", raw("sms/5"))
	}
	if raw("sms/20")["date_status"] != "invalid" || byKey["sms/20"].Time != nil {
		t.Errorf("sms 20: %v", raw("sms/20"))
	}
	// 7: NUL and invalid UTF-8 become U+FFFD, are flagged, and the raw bytes are kept.
	p7 := byKey["sms/7"].Payload
	if !reflect.DeepEqual(p7["body_flags"], []any{"invalid_utf8", "nul"}) || p7["body_raw_b64"] == nil {
		t.Errorf("sms 7 flags %v raw %v", p7["body_flags"], p7["body_raw_b64"])
	}
	// 16: a 300 KiB body is cut at 256 KiB and says so.
	p16 := byKey["sms/16"].Payload
	if p16["body_truncated"] != true || p16["body_total_bytes"] == nil || len(*byKey["sms/16"].Body) != 256<<10 {
		t.Errorf("sms 16 truncation: %v", p16["body_truncated"])
	}
	// 1: an incoming message has received and sent times; 2: an outgoing one only sent.
	if len(byKey["sms/1"].Times) != 2 || len(byKey["sms/2"].Times) != 1 {
		t.Errorf("times: sms 1 %v, sms 2 %v", byKey["sms/1"].Times, byKey["sms/2"].Times)
	}
	// pdu 2: addr types 137, 151, 151, 130, 129, 99 are from, to, to, cc, bcc, member.
	var roles []string
	parts, _ := byKey["pdu/2"].Payload["participants"].([]any)
	for _, x := range parts {
		roles = append(roles, x.(map[string]any)["role"].(string))
	}
	if want := []string{"from", "to", "to", "cc", "bcc", "member"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("pdu 2 roles %v, want %v", roles, want)
	}
	if got := fx.Expect.Notes["unmapped_columns.sms"]; got != "person,reply_path_present" {
		t.Errorf("unmapped_columns.sms = %q", got)
	}
	if len(fx.Expect.ExtraFiles) != 1 {
		t.Errorf("%d extra files, want the one present attachment", len(fx.Expect.ExtraFiles))
	}
}

const remoteDB = "/data/user_de/0/com.android.providers.telephony/databases/mmssms.db"
