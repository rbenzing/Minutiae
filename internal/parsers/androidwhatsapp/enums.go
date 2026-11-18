package androidwhatsapp

// enumEntry is one code the parser interprets: the column it comes from, the stored value, the label and the sources
// that agree about that column (4I-R7, R4). Sources is a comma list of the research source IDs, or ref:<run id>. A code
// is mapped only when two independent sources agree about the SAME column and the value is in no research conflict
// row; TestWhatsAppEnumTablesCiteTwoSources enforces exactly this bar over the table.
type enumEntry struct {
	Column  string
	Value   int64
	Label   string
	Sources string
}

// The columns of the table.
const (
	colMessageType = "message_type"
	colMediaWAType = "media_wa_type"
	colFromMe      = "from_me"
	colKeyFromMe   = "key_from_me"
	colActionType  = "action_type"
)

// The codes the parser's own rules name (each is in enumTable or in a research conflict row, see the tests).
const (
	typeText           = 0
	typeContactCard    = 4
	typeCallNotice     = 10 // a call notice row: unmapped (R4), its call_log row is raw
	statusLegacySystem = 6  // legacy status 6: with type 0 a system row (spec 7.1); status itself is never interpreted (R4)
)

// Where the body text came from (message.TextSource*), and the label of a contact card.
const (
	textSourceText   = "text"
	textSourcePart   = "part"
	labelContactCard = "contact_card"
)

// Labels of the message types that drive the kind (4I-R7): text gives kind text, system gives kind system, revoked
// the revoke rule (kind unknown), every other label of a message type gives kind media.
const (
	labelText    = "text"
	labelSystem  = "system"
	labelRevoked = "revoked"
)

// enumTable is read-only. The delivery status columns (message.status, messages.status) have no entry on purpose: every
// value of them is in a research conflict row or single-source (R4), so the stored value stays in raw and the status is
// unknown.
var enumTable = []enumEntry{
	{colMessageType, 0, "text", "S8,S6,S3,S11"},
	{colMessageType, 1, "image", "S8,S6,S3,S11"},
	{colMessageType, 2, "audio", "S8,S6,S3,S11"},
	{colMessageType, 3, "video", "S8,S6,S3,S11"},
	{colMessageType, 4, "contact_card", "S6,S3,S11"},
	{colMessageType, 5, "location", "S8,S6,S3,S11"},
	{colMessageType, 7, "system", "S8,S6,S3,S11"},
	{colMessageType, 9, "document", "S8,S6,S3,S11"},
	{colMessageType, 13, "gif", "S6,S3"},
	{colMessageType, 15, "revoked", "S1,S6,S3,S11,S5"},
	{colMessageType, 16, "live_location", "S8,S6,S3,S11"},
	{colMessageType, 20, "sticker", "S1,S6,S3"},
	{colMessageType, 24, "group_invite", "S6,S3"},
	{colMessageType, 64, "revoked", "S5,S6"},
	{colActionType, 18, "security_code_changed", "S1,S6"},
	{colMediaWAType, 0, "text", "S9,S10"},
	{colMediaWAType, 1, "image", "S9,S10"},
	{colMediaWAType, 2, "audio", "S9,S10"},
	{colMediaWAType, 3, "video", "S9,S10"},
	{colMediaWAType, 4, "contact_card", "S9,S10"},
	{colMediaWAType, 5, "location", "S9,S10"},
	{colFromMe, 0, "in", "S8,S1,S11"},
	{colFromMe, 1, "out", "S8,S1,S11"},
	{colKeyFromMe, 0, "in", "S9,S10,S1"},
	{colKeyFromMe, 1, "out", "S9,S10,S1"},
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
