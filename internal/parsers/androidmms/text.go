package androidmms

import (
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const (
	maxBody = 256 << 10 // bytes of body text kept; the rest is flagged (D9)
	maxList = 1000      // participants, attachments, thread members, recipient ids per record
	maxAddr = 1024      // the message contract's limit for a participant address
)

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// setBody puts a cleaned body into the typed message (spec C4, C6).
func setBody(msg *message.Message, c common.Cleaned) {
	msg.BodyFlags = c.Flags
	if c.Truncated {
		t, n := true, int64(c.TotalBytes)
		msg.BodyTruncated, msg.BodyTotalBytes = &t, &n
	}
	msg.BodyRawB64, msg.BodyRawSHA256 = c.RawB64, c.RawSHA256
	if c.RawB64 != "" || c.RawSHA256 != "" {
		n := int64(c.RawLen)
		msg.BodyRawLen = &n
	}
}

// participant builds a participant from stored address bytes. ok is false for NULL,
// empty and over-long addresses (raw says "too-long": an address is never cut, a cut
// number is another number). Invalid UTF-8 is flagged and the original kept (C4).
func participant(b []byte, k rowrun.CellKind, role string, raw map[string]any, prefix string) (message.Participant, bool) {
	if k != rowrun.CellValue || len(b) == 0 {
		return message.Participant{}, false
	}
	cl := common.CleanText(b, maxBody)
	if len(cl.Text) > maxAddr {
		raw[prefix+"_state"] = "too-long"
		return message.Participant{}, false
	}
	if len(cl.Flags) > 0 {
		raw[prefix+"_flags"] = anyStrings(cl.Flags)
		if cl.RawB64 != "" {
			raw[prefix+"_raw_b64"] = cl.RawB64
		}
	}
	return message.Participant{Role: role, Address: cl.Text, Kind: common.AddressKind(cl.Text)}, true
}

// markText lists field in raw.text_flags (sorted): the text of that field was altered
// (invalid UTF-8 or NUL became U+FFFD, or it was cut). The original bytes stay in the
// artifact; nothing is altered without a flag (spec 7.6).
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// cleanField is common.CleanText for a free-text field that has no flag slot of its own
// in the typed payload; an altered text is listed in raw.text_flags.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxBody)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}
