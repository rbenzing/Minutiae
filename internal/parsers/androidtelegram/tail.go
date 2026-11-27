package androidtelegram

import (
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The caps of the tail (plan 4I4b "Raw shapes"): a list is cut at its cap and states its _total and _truncated, a text
// is cut and flagged, an identifier is never cut.
const (
	maxReactions    = 64
	maxTopReactors  = 16
	maxRepliers     = 16
	maxRestrictions = 16
	maxEntities     = 64
)

// colRepliesData is the replies_data column.
const colRepliesData = "replies_data"

// tailRaw states the tail of the message blob in raw (plan 4I4b "Raw shapes", Tail; spec 8.3): never a record field and
// never a participant. A scalar is stated when it was read; an object or list only when read whole (P3), else
// <key>_state "partial" when the stop lies at or inside it (nothing when the decode never reached it: data_undecided
// names it). The blob's forwards stays beside the forwards column (B10); the times of no pinned unit are raw with
// _unit "unestablished" (P8). It returns the blob's full decoded replier list (nil when replies was not read whole), which
// readRepliesData compares beside the capped shape (ruling F1).
func tailRaw(v view, raw map[string]any) []string {
	if x, ok := v.Int("forwards"); ok {
		raw["blob_forwards"] = x
		if col, ok := raw["forwards"].(int64); ok && col != x {
			raw["forwards_conflict"] = true
		}
	}
	if b, ok := v.Str("post_author"); ok {
		cleanInto(raw, raw, "post_author", "post_author", b, maxName)
	}
	for _, k := range []string{"quick_reply_shortcut_id", "effect", "paid_message_stars"} {
		if x, ok := v.Int(k); ok {
			raw[k] = x
		}
	}
	for _, k := range []string{"report_delivery_until_date", "schedule_repeat_period"} {
		if x, ok := v.Int(k); ok {
			raw[k], raw[k+"_unit"] = x, unitUnestablished
		}
	}
	if b, ok := v.Str("summary_from_language"); ok {
		identInto(raw, raw, "summary_from_language", "summary_from_language", b, maxTiny)
	}
	var repliers []string
	switch {
	case v.Whole("replies"):
		if m, all, ok := repliesOf(v, "replies."); ok {
			raw["replies"], repliers = m, all
		}
	case v.inside("replies"):
		raw["replies_state"] = statePartial
	}
	reactionsRaw(v, raw)
	putList(v, raw, "restriction_reason", "restriction_reason", maxRestrictions, func(p string) (any, bool) {
		x := map[string]any{}
		for _, k := range []string{"platform", "reason", "text"} {
			if b, ok := v.Str(p + "." + k); ok {
				cleanInto(x, raw, k, "restriction_reason."+k, b, maxName)
			}
		}
		return x, len(x) > 0
	})
	switch {
	case v.Whole("factcheck"):
		raw["factcheck"] = factcheckOf(v, raw)
	case v.inside("factcheck"):
		raw["factcheck_state"] = statePartial
	}
	switch {
	case v.Whole("suggested_post"):
		raw["suggested_post"] = suggestedPostOf(v)
	case v.inside("suggested_post"):
		raw["suggested_post_state"] = statePartial
	}
	putList(v, raw, "entities", "entities", maxEntities, func(p string) (any, bool) { return entityOf(v, raw, p), true })
	return repliers
}

// entityLabel is the type label of a MessageEntity class: classLabel after TL_messageEntity, and for
// TL_inputMessageEntity... "input_" plus its label with that prefix (input_mention_name).
func entityLabel(class string) string {
	if strings.HasPrefix(class, "TL_inputMessageEntity") {
		return "input_" + classLabel(class, "TL_inputMessageEntity")
	}
	return classLabel(class, "TL_messageEntity")
}

// entityOf is one scalar-only entry of raw.entities: its type label, offset, length, url (4 KiB, cleaned), the user of
// a mention (an inline id, "user:<id>"; an InputUser object read in its place is no id: user_class, its class label,
// ruling F2), document_id and language (64 B).
func entityOf(v view, raw map[string]any, p string) map[string]any {
	x := map[string]any{"type": entityLabel(v.Class(p))}
	for _, k := range []string{"offset", "length", "document_id"} {
		if n, ok := v.Int(p + "." + k); ok {
			x[k] = n
		}
	}
	if b, ok := v.Str(p + ".url"); ok {
		cleanInto(x, raw, "url", "entities.url", b, maxShort)
	}
	if b, ok := v.Str(p + ".language"); ok {
		cleanInto(x, raw, "language", "entities.language", b, maxTiny)
	}
	if n, ok := v.Named(p + ".user_id"); ok {
		switch n.Kind {
		case tl.KInt32, tl.KInt64:
			x["user"] = "user:" + strconv.FormatInt(n.Int, 10)
		case tl.KObject:
			// ruling F2: an InputUser object read in place of an id (inputUserSelf, inputUserEmpty) is stated by its
			// class label, never as a user
			class := v.Class(p + ".user_id")
			if label := classLabel(class, "TL_inputUser"); label != "" {
				class = label
			}
			x["user_class"] = class
		}
	}
	return x
}

// factcheckOf is raw.factcheck of a fact check read whole: need_check, country (64 B), text (4 KiB), the count of the
// text's entities and the hash.
func factcheckOf(v view, raw map[string]any) map[string]any {
	const p = "factcheck."
	o := map[string]any{}
	if v.True(p + "need_check") {
		o["need_check"] = true
	}
	if b, ok := v.Str(p + "country"); ok {
		cleanInto(o, raw, "country", "factcheck.country", b, maxTiny)
	}
	if b, ok := v.Str(p + "text.text"); ok {
		cleanInto(o, raw, "text", "factcheck.text", b, maxShort)
	}
	if n, ok := v.Len(p + "text.entities"); ok {
		o["entities_total"] = int64(n)
	}
	if x, ok := v.Int(p + "hash"); ok {
		o["hash"] = x
	}
	return o
}

// suggestedPostOf is raw.suggested_post of a suggested post read whole: accepted, rejected, the price (stars or ton, by
// its class) and the schedule date (unit unestablished, P8).
func suggestedPostOf(v view) map[string]any {
	const p = "suggested_post."
	o := map[string]any{}
	for _, f := range []string{"accepted", "rejected"} {
		if v.True(p + f) {
			o[f] = true
		}
	}
	if class := v.Class(p + "price"); class != "" {
		price := map[string]any{"kind": "unknown"}
		switch class {
		case "TL_starsAmount":
			price["kind"] = "stars"
		case "TL_starsTonAmount":
			price["kind"] = "ton"
		}
		for _, k := range []string{"amount", "nanos"} {
			if x, ok := v.Int(p + "price." + k); ok {
				price[k] = x
			}
		}
		o["price"] = price
	}
	if x, ok := v.Int(p + "schedule_date"); ok {
		o["schedule_date"], o["schedule_date_unit"] = x, unitUnestablished
	}
	return o
}

// reactionOf is a reaction read at path (plan 4I4b "Raw shapes"): emoji:<emoticon> (a boxed TL_reactionEmoji, or the
// inline reaction.emoticon of the old results, cleaned at 64 B), custom:<document_id>, paid or empty (the class label
// after TL_reaction); false when nothing was read.
func reactionOf(v view, path string, raw map[string]any) (string, bool) {
	emoji := func(p string) (string, bool) {
		b, ok := v.Str(p)
		if !ok {
			return "", false
		}
		return "emoji:" + cleaned(raw, "reactions.emoticon", b, maxTiny), true
	}
	class := v.Class(path)
	if class == "" {
		return emoji(path + ".emoticon")
	}
	switch label := classLabel(class, "TL_reaction"); label {
	case "emoji":
		return emoji(path + ".emoticon") // an emoticon not read is no reaction, never "emoji:"
	case "custom_emoji":
		if x, ok := v.Int(path + ".document_id"); ok {
			return "custom:" + strconv.FormatInt(x, 10), true
		}
		return "", false
	default:
		return label, label != ""
	}
}

// peerOf is the participant-shaped address of the peer read at path, as text for raw only: a reactor or replier is
// never a participant (spec 8.3).
func peerOf(v view, path string) (string, bool) {
	a, _, ok := peerAddress(v, path)
	return a, ok
}

// reactionsRaw states raw.reactions {flags, <list>_total, <list>_truncated} and the three lists at the top of raw
// (B1: reactions_results, reactions_recent, reactions_top_reactors) when the reactions were read whole; a stop at or
// inside them is raw.reactions_state "partial" and nothing else (P3).
func reactionsRaw(v view, raw map[string]any) {
	switch {
	case v.inside("reactions"):
		raw["reactions_state"] = statePartial
		return
	case !v.Whole("reactions"):
		return
	}
	const p = "reactions"
	o := map[string]any{}
	for _, f := range []string{"min", "can_see_list", "reactions_as_tags"} {
		if v.True(p + "." + f) {
			o[f] = true
		}
	}
	list := func(sub, key string, limit int, entry func(e string) map[string]any) {
		path := p + "." + sub
		n, ok := v.Len(path)
		if !ok {
			return
		}
		var out []any
		for i := range min(n, limit) {
			if x := entry(elem(path, i)); len(x) > 0 {
				out = append(out, x)
			}
		}
		if len(out) > 0 {
			raw["reactions_"+key] = out
		}
		o[key+"_total"] = int64(n)
		if n > limit {
			o[key+"_truncated"] = true
		}
	}
	list("results", "results", maxReactions, func(e string) map[string]any {
		x := map[string]any{}
		if r, ok := reactionOf(v, e+".reaction", raw); ok {
			x["reaction"] = r
		}
		for _, k := range []string{"count", "chosen_order"} {
			if n, ok := v.Int(e + "." + k); ok {
				x[k] = n
			}
		}
		return x
	})
	list("recent_reactions", "recent", maxReactions, func(e string) map[string]any {
		x := map[string]any{}
		if a, ok := peerOf(v, e+".peer_id"); ok {
			x["peer"] = a
		}
		if r, ok := reactionOf(v, e+".reaction", raw); ok {
			x["reaction"] = r
		}
		for _, f := range []string{"big", "unread"} {
			if v.True(e + "." + f) {
				x[f] = true
			}
		}
		if n, ok := v.Int(e + ".date"); ok { // P4: no unit is invented
			x["date"], x["date_unit"] = n, unitUnestablished
		}
		return x
	})
	list("top_reactors", "top_reactors", maxTopReactors, func(e string) map[string]any {
		x := map[string]any{}
		if a, ok := peerOf(v, e+".peer_id"); ok {
			x["peer"] = a
		}
		if n, ok := v.Int(e + ".count"); ok {
			x["count"] = n
		}
		for _, f := range []string{"top", "my", "anonymous"} {
			if v.True(e + "." + f) {
				x[f] = true
			}
		}
		return x
	})
	raw["reactions"] = o
}

// repliesOf is the replies object (plan 4I4b "Raw shapes") of a MessageReplies read under prefix: "replies." in the
// blob, "" for the replies_data cell (whose top object it is). It is stated only when the object was read whole: false
// otherwise (P3). Its repliers are addresses in raw, never participants. all is every decoded replier (bounded by the
// decoder's vector bound, never by maxRepliers), for the conflict comparison only and never emitted (ruling F1): an
// entry that is no address is "?" and its class, so its position still counts.
func repliesOf(v view, prefix string) (r map[string]any, all []string, ok bool) {
	if prefix == "" && v.r.Stop != nil || prefix != "" && !v.Whole(strings.TrimSuffix(prefix, ".")) {
		return nil, nil, false
	}
	r = map[string]any{}
	for _, k := range []string{"replies", "replies_pts", "max_id", "read_max_id"} {
		if x, ok := v.Int(prefix + k); ok {
			r[k] = x
		}
	}
	if v.True(prefix + "comments") {
		r["comments"] = true
	}
	if x, ok := v.Int(prefix + "channel_id"); ok {
		r["channel"] = "channel:" + strconv.FormatInt(x, 10)
	}
	rp := prefix + "recent_repliers"
	if n, ok := v.Len(rp); ok {
		var addrs []any
		all = []string{} // grown as read: never sized from the count
		for i := range n {
			a, ok := peerOf(v, elem(rp, i))
			if !ok {
				all = append(all, "?"+v.Class(elem(rp, i)))
				continue
			}
			all = append(all, a)
			if i < maxRepliers {
				addrs = append(addrs, a)
			}
		}
		if len(addrs) > 0 {
			r["recent_repliers"] = addrs
		}
		r["recent_repliers_total"] = int64(n)
		if n > maxRepliers {
			r["recent_repliers_truncated"] = true
		}
	}
	return r, all, true
}

// readRepliesData states the replies_data cell (design D13, rulings B8, P6): the serialized message.replies, decoded as
// a MessageReplies beside the blob with the blob keys (replies_data_state, _len, _sha256, _constructor, _flags,
// _stop_*; no _undecided) and nothing merged: raw.replies_data is its replies shape when it decoded whole, and
// raw.replies_conflict is true when the blob's own replies was read whole too and the two decoded values differ (B8):
// the capped shapes, and the full replier lists beside them (ruling F1: lists equal in their first 16 entries may differ
// after them).
// A cell of another class or an unreadable one is listed in unknown_columns, a NULL states nothing. What the decode
// charged is freed when the row returns (F8).
func (r *msgRow) readRepliesData(row parse.Row, c rowrun.Cols, tab tl.Table, bud tl.Budget) {
	col := c.At(colRepliesData)
	b, k := blobCell(row, col)
	switch k {
	case rowrun.CellValue:
		v, state := decodeBlob(r.raw, colRepliesData, b, typeReplies, tab, bud)
		defer func() {
			if bud != nil {
				bud.Free(v.r.Charged())
			}
		}()
		if _, ok := topOf(v, tab, typeReplies, state); !ok || state != stateDecoded {
			return
		}
		m, all, ok := repliesOf(v, "")
		if !ok {
			return
		}
		r.raw[colRepliesData] = m
		if blob, ok := r.raw["replies"].(map[string]any); ok && (!sameReplies(blob, m) || !slices.Equal(r.repliers, all)) {
			r.raw["replies_conflict"] = true
		}
	case rowrun.CellOther:
		r.unknown[r.unknownName(colRepliesData)] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[r.unknownName(colRepliesData)] = rowrun.StateOf(row, col).String()
	}
}

// sameReplies says two replies shapes hold the same decoded values (B8): the same keys with equal integers, strings,
// booleans and address lists (repliesOf's only value kinds).
func sameReplies(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok {
			return false
		}
		la, isList := x.([]any)
		if !isList {
			if !sameValue(x, y) {
				return false
			}
			continue
		}
		lb, ok := y.([]any)
		if !ok || len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !sameValue(la[i], lb[i]) {
				return false
			}
		}
	}
	return true
}

// sameValue says two replies values are equal, by an explicit kind: int64, string and bool (repliesOf's only scalar
// kinds). A value of any other kind, or of two kinds, is not the same.
func sameValue(x, y any) bool {
	switch x := x.(type) {
	case int64:
		y, ok := y.(int64)
		return ok && x == y
	case string:
		y, ok := y.(string)
		return ok && x == y
	case bool:
		y, ok := y.(bool)
		return ok && x == y
	}
	return false
}
