package ioswhatsapp

// enumEntry is one code the parser interprets: the column it comes from, the stored value, the label and the sources
// that agree about that column (4I2-R8). Sources is a comma list of the research source IDs, or ref:<run id>. A code is
// mapped only when two independent sources agree about the SAME column and the value is in no research conflict row;
// TestIOSWhatsAppChatsEnumTableCitesTwoSources enforces exactly this bar over the table. The table is restated from
// androidwhatsapp/enums.go: parsers import no other parser package (docs/architecture.md).
type enumEntry struct {
	Column  string
	Value   int64
	Label   string
	Sources string
}

// The columns of the table, named as the stored column.
const (
	colMessageType    = "ZMESSAGETYPE"
	colGroupEventType = "ZGROUPEVENTTYPE"
	colMessageStatus  = "ZMESSAGESTATUS"
	colIsFromMe       = "ZISFROMME"
	colSessionType    = "ZSESSIONTYPE"
)

// The codes the mapping's own rules name (each is in enumTable, see the tests).
const (
	typeText        = 0
	typeContactCard = 4
	typeLocation    = 5
	typeGroupEvent  = 6
	typeLink        = 7
	typeRevoked     = 14

	sessionGroup  = 1
	sessionStatus = 3
)

// Labels of the message types that drive the kind: text gives kind text, system (group_event, system_notice) gives
// kind system, revoked the revoke rule (kind unknown), every other label gives kind media.
const (
	labelText        = "text"
	labelGroupEvent  = "group_event"
	labelSysNotice   = "system_notice"
	labelRevoked     = "revoked"
	labelDelivered   = "delivered"
	labelReadByRecpt = "read_by_recipient"
)

// enumTable is read-only. A value that fewer than two sources state, or that sits in a research conflict row, has no
// entry on purpose (ZMESSAGETYPE 9, 12, 13, 38, 39, 40; ZGROUPEVENTTYPE 1, 9, 12; ZMESSAGESTATUS 0 to 5; ZISFROMME 2):
// its stored value stays in raw, its label is absent and its kind or direction is unknown (4I2-R8).
var enumTable = []enumEntry{
	{colMessageType, 0, "text", "S12,S16,S17,S18,S20"},
	{colMessageType, 1, "image", "S12,S16,S17,S18,S20,S22"},
	{colMessageType, 2, "video", "S12,S16,S17,S18,S22"},
	{colMessageType, 3, "audio", "S12,S16,S17,S18,S22"},
	{colMessageType, 4, "contact_card", "S12,S16,S18,S22"},
	{colMessageType, 5, "location", "S12,S16,S18,S22,S7"},
	{colMessageType, 6, "group_event", "S1,S12,S16,S17,S18"},
	{colMessageType, 7, "link", "S12,S16,S18,S22"},
	{colMessageType, 8, "document", "S12,S16,S18,S22"},
	{colMessageType, 10, "system_notice", "S16,S18,S20"},
	{colMessageType, 11, "gif", "S16,S18,S22"},
	{colMessageType, 14, "revoked", "S1,S16,S18,S21"},
	{colMessageType, 15, "sticker", "S1,S16,S18,S22"},
	{colGroupEventType, 2, "participant_added", "S16,S17,S18"},
	{colGroupEventType, 3, "participant_left", "S16,S17,S18"},
	{colGroupEventType, 4, "group_icon_changed", "S17,S18"},
	{colMessageStatus, 6, "delivered", "S18,S20,S24"},
	{colMessageStatus, 8, "read_by_recipient", "S18,S20,S24"},
	{colIsFromMe, 0, "in", "S1,S7,S10,S12,S19"},
	{colIsFromMe, 1, "out", "S1,S7,S10,S12,S19"},
	{colSessionType, 0, "private", "S12,S16"},
	{colSessionType, 1, "group", "S12,S16"},
	{colSessionType, 2, "broadcast", "S12,S16"},
	{colSessionType, 3, "status", "S12,S16"},
}

// lookupEnum returns the label of a stored value of a column, and false for a code the research does not support.
func lookupEnum(column string, v int64) (string, bool) {
	for _, e := range enumTable {
		if e.Column == column && e.Value == v {
			return e.Label, true
		}
	}
	return "", false
}
