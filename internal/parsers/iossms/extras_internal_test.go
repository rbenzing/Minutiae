package iossms

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

const (
	xmlHead = `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0">`
	xmlTail = `</plist>`
)

func goodSummary() []byte {
	return []byte(xmlHead + `<dict><key>ec</key><dict><key>0</key><array><dict/></array></dict></dict>` + xmlTail)
}

func goodArchive() []byte {
	return []byte(xmlHead + `<dict><key>$archiver</key><string>NSKeyedArchiver</string><key>$version</key><integer>100000</integer>` +
		`<key>$top</key><dict><key>root</key><dict><key>CF$UID</key><integer>1</integer></dict></dict>` +
		`<key>$objects</key><array><string>$null</string><dict><key>$class</key><dict><key>CF$UID</key><integer>2</integer></dict></dict>` +
		`<dict><key>$classname</key><string>LPLinkMetadata</string><key>$classes</key><array><string>LPLinkMetadata</string></array></dict></array></dict>` + xmlTail)
}

// The budget refusing the decode is "budget", a fact about the run: with a nil view and with a view too small.
func TestPlistBudgetRefusalIsStatusBudget(t *testing.T) {
	for name, view := range map[string]*parse.BudgetView{"nil view": nil, "tiny budget": parse.NewBudget(8).View()} {
		raw := map[string]any{}
		summaryInfo(goodSummary(), view, raw)
		if raw["message_summary_info_status"] != "budget" || raw["summary_info"] != nil {
			t.Errorf("%s: summary raw %v", name, raw)
		}
		raw = map[string]any{}
		payloadData(goodArchive(), view, raw)
		if raw["payload_data_status"] != "budget" || raw["payload_data"] != nil {
			t.Errorf("%s: payload raw %v", name, raw)
		}
	}
}

// A decode, successful or failed, leaves nothing charged: many messages cannot drain the budget.
func TestPlistDecodeReleasesItsCharge(t *testing.T) {
	b := parse.NewBudget(1 << 24)
	v := b.View()
	for i := 0; i < 500; i++ {
		raw := map[string]any{}
		summaryInfo(goodSummary(), v, raw)
		payloadData(goodArchive(), v, raw)
		if raw["summary_info"] == nil || raw["payload_data"] == nil {
			t.Fatalf("round %d: good documents not read: %v", i, raw)
		}
	}
	if u := b.Used(); u != 0 {
		t.Errorf("%d bytes still charged after successful decodes", u)
	}
	for _, bad := range [][]byte{[]byte("not a plist"), []byte("bplist00\xd1\x01\x02\xff"), []byte(xmlHead + `<dict><key>a</key>`)} {
		raw := map[string]any{}
		summaryInfo(bad, v, raw)
		payloadData(bad, v, raw)
		if raw["message_summary_info_status"] != "undecodable" || raw["payload_data_status"] != "undecodable" {
			t.Errorf("bad document: raw %v", raw)
		}
	}
	if u := b.Used(); u != 0 {
		t.Errorf("%d bytes still charged after failed decodes", u)
	}
	// a budget that holds one decode but not many: only the returned charge lets the loop go on
	var limit int64
	for limit = 1 << 10; ; limit *= 2 {
		raw := map[string]any{}
		summaryInfo(goodSummary(), parse.NewBudget(limit).View(), raw)
		if raw["summary_info"] != nil {
			break
		}
		if limit > 1<<26 {
			t.Fatal("no budget decodes the summary")
		}
	}
	sv := parse.NewBudget(limit).View()
	for i := 0; i < 50; i++ {
		raw := map[string]any{}
		summaryInfo(goodSummary(), sv, raw)
		if raw["summary_info"] == nil {
			t.Fatalf("round %d: the budget was drained: %v", i, raw)
		}
	}
}

func TestSummaryInfoKeyCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(xmlHead + "<dict>")
	for i := 0; i < 70; i++ {
		sb.WriteString("<key>k" + string(rune('A'+i/26)) + string(rune('a'+i%26)) + "</key><integer>1</integer>")
	}
	sb.WriteString("</dict>" + xmlTail)
	raw := map[string]any{}
	summaryInfo([]byte(sb.String()), parse.NewBudget(1<<24).View(), raw)
	si, _ := raw["summary_info"].(map[string]any)
	keys, _ := si["keys"].([]any)
	if len(keys) != 64 || si["keys_total"] != int64(70) || si["keys_truncated"] != true {
		t.Errorf("%d keys, summary_info %v", len(keys), si["keys_total"])
	}
}
