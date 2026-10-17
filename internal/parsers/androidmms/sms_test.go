package androidmms_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

func baselineRun(t *testing.T) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

func byLocator(t *testing.T, res parsertest.Result, loc string) records.Record {
	t.Helper()
	for _, r := range res.Records {
		if r.Locator == loc {
			return r
		}
	}
	t.Fatalf("no record %s", loc)
	return records.Record{}
}

// at digs into a payload: maps by key, lists by decimal index.
func at(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, k := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[k]
		case []any:
			i := int(k[0] - '0')
			if i >= len(x) {
				t.Fatalf("path %v: index %s out of range", path, k)
			}
			v = x[i]
		default:
			t.Fatalf("path %v: %T has no %s", path, v, k)
		}
	}
	return v
}

func TestSMSSpotChecksFromTheSpec(t *testing.T) {
	res := baselineRun(t)
	one := byLocator(t, res, "sqlite:table=sms;rowid=1")
	if !one.Time.T.Equal(time.Date(2023, 5, 1, 10, 0, 0, 0, time.UTC)) || one.Body != "hello" || one.Deleted || one.Recovery != "" {
		t.Errorf("row 1: %+v", one)
	}
	if one.SourcePath != remotePath {
		t.Errorf("row 1: SourcePath %q, want %q", one.SourcePath, remotePath)
	}
	p := one.Payload
	if at(t, p, "direction") != "in" || at(t, p, "status") != "received" || at(t, p, "participants", "0", "role") != "from" ||
		at(t, p, "participants", "0", "address") != "+15551234567" || at(t, p, "participants", "0", "kind") != "phone" ||
		at(t, p, "thread", "id") != "1" || at(t, p, "thread", "group") != false || at(t, p, "thread", "members", "0") != "+15551234567" {
		t.Errorf("row 1 payload: %v", p)
	}
	if one.Summary != "sms in +15551234567: hello" {
		t.Errorf("summary %q", one.Summary)
	}
	// 0 is the "no time" sentinel: no 1970, the raw value stays
	five := byLocator(t, res, "sqlite:table=sms;rowid=5")
	if five.Time != nil || at(t, five.Payload, "raw", "date") != int64(0) || at(t, five.Payload, "raw", "date_status") != "sentinel" {
		t.Errorf("row 5: time %v raw %v", five.Time, five.Payload["raw"])
	}
	// epoch SECONDS in a milliseconds column: invalid, not guessed (spec 9.1)
	six := byLocator(t, res, "sqlite:table=sms;rowid=6")
	if six.Time != nil || at(t, six.Payload, "raw", "date") != int64(1700000000) || at(t, six.Payload, "raw", "date_status") != "invalid" {
		t.Errorf("row 6: time %v raw %v", six.Time, six.Payload["raw"])
	}
	seven := byLocator(t, res, "sqlite:table=sms;rowid=7")
	if seven.Body != "nul�and bad �� bytes" || at(t, seven.Payload, "body_flags", "0") != "invalid_utf8" || at(t, seven.Payload, "body_flags", "1") != "nul" ||
		at(t, seven.Payload, "body_raw_b64") == nil {
		t.Errorf("row 7: body %q payload %v", seven.Body, seven.Payload)
	}
	fifteen := byLocator(t, res, "sqlite:table=sms;rowid=15")
	if at(t, fifteen.Payload, "participants_unknown") != true || len(fifteen.Payload["participants"].([]any)) != 0 {
		t.Errorf("row 15 (NULL address): %v", fifteen.Payload)
	}
	big := byLocator(t, res, "sqlite:table=sms;rowid=16")
	if len(big.Body) != 256<<10 || at(t, big.Payload, "body_truncated") != true || at(t, big.Payload, "body_total_bytes") != int64(300*1024) {
		t.Errorf("row 16: body %d bytes, payload %v", len(big.Body), big.Payload["body_truncated"])
	}
	dangling := byLocator(t, res, "sqlite:table=sms;rowid=17")
	if at(t, dangling.Payload, "thread", "id") != "5" || at(t, dangling.Payload, "raw", "thread_recipients_missing", "0") != int64(7) {
		t.Errorf("row 17: %v", dangling.Payload)
	}
	if _, has := dangling.Payload["thread"].(map[string]any)["members"]; has {
		t.Error("row 17 lists members it could not resolve")
	}
	flagged := byLocator(t, res, "sqlite:table=sms;rowid=22")
	if at(t, flagged.Payload, "subject") != "bad � subject" || at(t, flagged.Payload, "raw", "text_flags", "0") != "subject" {
		t.Errorf("row 22: an altered subject must be listed in raw.text_flags: %v", flagged.Payload)
	}
	orphan := byLocator(t, res, "sqlite:table=sms;rowid=14")
	if at(t, orphan.Payload, "raw", "thread_state") != "no-threads-row" || at(t, orphan.Payload, "raw", "thread_id") != int64(99) {
		t.Errorf("row 14: %v", orphan.Payload)
	}
	long := byLocator(t, res, "sqlite:table=sms;rowid=18")
	if at(t, long.Payload, "raw", "address_state") != "too-long" || at(t, long.Payload, "participants_unknown") != true {
		t.Errorf("row 18: %v", long.Payload)
	}
	twenty := byLocator(t, res, "sqlite:table=sms;rowid=20")
	if twenty.Time != nil || at(t, twenty.Payload, "raw", "date_status") != "invalid" {
		t.Errorf("row 20 (year 2100): time %v", twenty.Time)
	}
	zero := byLocator(t, res, "sqlite:table=sms;rowid=12")
	if at(t, zero.Payload, "direction") != "unknown" || at(t, zero.Payload, "status") != "unknown" || at(t, zero.Payload, "participants", "0", "role") != "member" {
		t.Errorf("row 12 (type 0): %v", zero.Payload)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w.Locator, "table=sms") {
			t.Errorf("unexpected warning for an sms row: %+v", w)
		}
	}
}

func TestSMSNotesNameTheVariantAndTheColumnsNoOneReads(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "oem-extended")
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if got := res.Notes["unmapped_columns.sms"]; got != "person,reply_path_present,sim_slot,is_favorite,msg_hash" {
		t.Errorf("unmapped_columns.sms = %q", got)
	}
}

func TestSMSShortRecordsTakeTheColumnDefaultAndASubscriptionOfMinusOneIsNoSubscription(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-subid-alter")
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	old := byLocator(t, res, "sqlite:table=sms;rowid=1") // written before ALTER TABLE ADD COLUMN sub_id DEFAULT -1
	if _, has := old.Payload["subscription"]; has || at(t, old.Payload, "raw", "sub_id") != int64(-1) {
		t.Errorf("short record: subscription %v raw %v", old.Payload["subscription"], old.Payload["raw"])
	}
	nw := byLocator(t, res, "sqlite:table=sms;rowid=30")
	if at(t, nw.Payload, "subscription") != int64(2) || at(t, nw.Payload, "raw", "creator") != "com.example.sender" {
		t.Errorf("new record: %v", nw.Payload)
	}
}

// Ruling T48: a record's SourcePath is the remote path of the file that holds its ArtifactID's bytes:
// the database for a database cell, the -wal file for a WAL cell, the -journal file for a journal cell.
// The locator names the frame or record.
func TestSourcePathFollowsTheFileThatHoldsTheCell(t *testing.T) {
	suffix := map[string]string{"db": "", "wal": "-wal", "journal": "-journal"}
	sawWAL := false
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remotePath)
		p := androidmms.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		for _, r := range res.Records {
			role := "db"
			switch {
			case strings.Contains(r.Locator, ";wal_frame="):
				role, sawWAL = "wal", true
			case strings.Contains(r.Locator, ";journal_rec="):
				role = "journal"
			}
			if r.ArtifactID != b.ByRole[role].ID || r.SourcePath != remotePath+suffix[role] {
				t.Errorf("%s %s: artifact %q SourcePath %q, want the %s file %q (%q)", v, r.Locator, r.ArtifactID, r.SourcePath, role, b.ByRole[role].ID, remotePath+suffix[role])
			}
		}
	}
	if !sawWAL {
		t.Error("no record lies in a WAL: the -wal rule is not exercised")
	}
}
