package androidgooglemessages

import (
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The telephony authorities a sms_message_uri names (spec 6.8, F10 Telephony.java:395 and 2790) and what the pinned
// sources say of the reuse of their row ids: sms._id has no AUTOINCREMENT (T10 MmsSmsDatabaseHelper.java:995-996), so an id
// can be reused after a delete; pdu._id has (T10 :728-729).
const (
	authoritySMS  = "sms"
	authorityMMS  = "mms"
	maxLinkID     = 19 // the digits of the largest int64
	reusePossible = "possible"
	reuseNone     = "none"
)

// smsLink states the telephony link of a message (spec 6.8, C2, R10): the stored sms_message_uri as cleaned text, and,
// when it is exactly content://sms/<digits> or content://mms/<digits> (1 to 19 ASCII digits whose value fits int64,
// leading zeros allowed), the reference {authority, id} with whether the row id can be reused. Anything else is
// sms_link_state "unparsed". NULL states nothing; a BLOB or another class is listed in unknown. The link is a stated
// reference: nothing is looked up, merged or used as a key.
func smsLink(raw, unknown map[string]any, row parse.Row, col int) {
	b, k := textCellOf(row, col)
	rowrun.Flag(unknown, row, "sms_message_uri", col, k)
	if k != rowrun.CellValue {
		return
	}
	text, ok := ident(raw, "sms_message_uri", b, maxIdent)
	if !ok {
		return
	}
	raw["sms_message_uri"] = text
	for _, a := range []struct{ authority, reuse string }{{authoritySMS, reusePossible}, {authorityMMS, reuseNone}} {
		tail, found := strings.CutPrefix(text, "content://"+a.authority+"/")
		if !found {
			continue
		}
		if id, ok := linkID(tail); ok {
			raw["sms_link"] = map[string]any{"authority": a.authority, "id": id}
			raw["sms_link_id_reuse"] = a.reuse
			return
		}
	}
	raw["sms_link_state"] = "unparsed"
}

// linkID is the row id of the tail of a telephony URI: 1 to 19 ASCII digits whose value fits int64.
func linkID(tail string) (int64, bool) {
	if !isDigits(tail, maxLinkID) {
		return 0, false
	}
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
