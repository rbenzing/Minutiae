package androidtelegram

import (
	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// maxReplyText is the cap of the reply copy's text (spec 8.5, 12).
const maxReplyText = 4 << 10

// replyCopy states the replydata blob of a message row (spec 8.5): the app's copy of the replied-to message, never a
// record, never Body, never target_guid (the 4I quote rule). It is decoded as a Message with the blob keys of spec
// 8.4 under replydata_ (without _undecided: its fields are not the record's). A copy whose top constructor is a Message
// one is raw.reply_copy {copy: true, constructor, id, date, text (cleaned at 4 KiB, flagged as reply_copy.text in
// raw.text_flags)}, each field as read; any other (a StoryItem, an unknown ID) is raw.reply_copy_state, the blob state.
// The decode charges bud and is freed before replyCopy returns (F8).
func replyCopy(b []byte, raw map[string]any, tab tl.Table, bud tl.Budget) {
	v, state := decodeBlob(raw, colReplyData, b, typeMessage, tab, bud)
	defer func() {
		if bud != nil {
			bud.Free(v.r.Charged())
		}
	}()
	if _, ok := topOf(v, tab, typeMessage, state); !ok {
		raw["reply_copy_state"] = state
		return
	}
	cp := map[string]any{"copy": true, "constructor": hexID(v.r.Top)}
	for _, k := range []string{"id", "date"} {
		if x, ok := v.Int(k); ok {
			cp[k] = x
		}
	}
	if text, ok := v.Str("message"); ok {
		cleanInto(cp, raw, "text", "reply_copy.text", text, maxReplyText)
	}
	raw["reply_copy"] = cp
}
