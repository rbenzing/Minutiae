package androidgooglemessages

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// code is one cited AOSP code: its label (labelOf of the pinned constant), its contract value (the channel of a protocol,
// "<direction>/<status>" of a status) and its source ("<file>:<lines>@M": the Messaging commit of the pin M).
type code struct{ Label, Contract, Source string }

// The code tables are the cited AOSP sets (spec 6.1 and 6.2, MessageData.java at M) and nothing else: a value outside
// them is unknown with its raw value, never interpreted. TestGoogleMessagesCodeTableMatchesFacts holds them equal to
// tools/fixtures/schemas/google-messages/facts.tsv.
var protocolCodes = map[int64]code{
	-1: {"unknown", "unknown", "MessageData.java:114@M"},
	0:  {"sms", "sms", "MessageData.java:115@M"},
	1:  {"mms", "mms", "MessageData.java:116@M"},
	2:  {"mms_push_notification", "mms", "MessageData.java:117@M"},
}

var statusCodes = map[int64]code{
	0:   {"unknown", "unknown/unknown", "MessageData.java:120@M"},
	1:   {"outgoing_complete", "out/sent", "MessageData.java:123@M"},
	2:   {"outgoing_delivered", "out/delivered", "MessageData.java:124@M"},
	3:   {"outgoing_draft", "out/draft", "MessageData.java:126@M"},
	4:   {"outgoing_yet_to_send", "out/queued", "MessageData.java:127@M"},
	5:   {"outgoing_sending", "out/queued", "MessageData.java:128@M"},
	6:   {"outgoing_resending", "out/queued", "MessageData.java:129@M"},
	7:   {"outgoing_awaiting_retry", "out/queued", "MessageData.java:130@M"},
	8:   {"outgoing_failed", "out/failed", "MessageData.java:131@M"},
	9:   {"outgoing_failed_emergency_number", "out/failed", "MessageData.java:132@M"},
	100: {"incoming_complete", "in/received", "MessageData.java:135@M"},
	101: {"incoming_yet_to_manual_download", "in/unknown", "MessageData.java:136@M"},
	102: {"incoming_retrying_manual_download", "in/unknown", "MessageData.java:137@M"},
	103: {"incoming_manual_downloading", "in/unknown", "MessageData.java:138@M"},
	104: {"incoming_retrying_auto_download", "in/unknown", "MessageData.java:139@M"},
	105: {"incoming_auto_downloading", "in/unknown", "MessageData.java:140@M"},
	106: {"incoming_download_failed", "in/unknown", "MessageData.java:141@M"},
	107: {"incoming_expired_or_not_available", "in/unknown", "MessageData.java:142@M"},
}

// labelOf is the label of a pinned MessageData constant (4I5-P4): the name lowercased with the prefix PROTOCOL_ or
// BUGLE_STATUS_ removed, so the label follows the pin.
func labelOf(constant string) string {
	for _, p := range []string{"PROTOCOL_", "BUGLE_STATUS_"} {
		if s, ok := strings.CutPrefix(constant, p); ok {
			return strings.ToLower(s)
		}
	}
	return strings.ToLower(constant)
}

// codeCell reads an integer code column for a message record (spec 7 R1 to R3): the value as stored in raw[key], the NULL
// as raw[key+"_state"] = "null", an unreadable, NaN or wrongly typed cell in unknown (never read as NULL or as a value).
// It returns the value and whether it is a decided integer.
func codeCell(row parse.Row, col int, key string, raw, unknown map[string]any) (int64, bool) {
	v, k := intCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		raw[key] = v
		return v, true
	case rowrun.CellNull:
		raw[key+"_state"] = "null"
	default:
		rowrun.Flag(unknown, row, key, col, k)
	}
	return 0, false
}

// channelOf is the channel of a message (spec 6.1, C1): the cited transport of message_protocol (0 sms, 1 and 2 mms), else
// the free token unknown (C1a). The raw protocol is as stored; a cited code keeps its label.
func channelOf(row parse.Row, col int, raw, unknown map[string]any) string {
	v, ok := codeCell(row, col, "message_protocol", raw, unknown)
	if !ok {
		return channelUnknown
	}
	c, cited := protocolCodes[v]
	if !cited {
		return channelUnknown
	}
	raw["label_message_protocol"] = c.Label
	return c.Contract
}

// channelUnknown is the message channel token of a transport that is not decided (C1a): no recordtypes constant.
const channelUnknown = "unknown"

// statusOf is the direction and status of a message from message_status (spec 6.2, B7): only the cited set decides; any
// other value, NULL or an unreadable cell is unknown/unknown, never inferred from the band or the sender. The raw status
// is as stored; a cited code keeps its label.
func statusOf(row parse.Row, col int, raw, unknown map[string]any) (direction, status string) {
	v, ok := codeCell(row, col, "message_status", raw, unknown)
	if !ok {
		return "unknown", "unknown"
	}
	c, cited := statusCodes[v]
	if !cited {
		return "unknown", "unknown"
	}
	raw["label_message_status"] = c.Label
	direction, status, _ = strings.Cut(c.Contract, "/")
	return direction, status
}
