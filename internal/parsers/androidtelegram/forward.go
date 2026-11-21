package androidtelegram

import (
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// stateConflict states two identities read where the record holds one (a forward header with two inline origins);
// stateContradictory an inline id whose class the pinned source leaves unresolved (T5, 4I4-P11).
const (
	stateConflict      = "conflict"
	stateContradictory = "contradictory"
)

// fwdRaw states the forward header of a message blob as decoded in raw.fwd (spec 8.5 raw list): from (an address of
// 4I4-P11), from_name and post_author (cleaned at 1 KiB, flagged in raw.text_flags), date, channel_post,
// saved_from_peer (an address) and saved_from_msg_id. It reads the header object (fwd_from) and the inline fields of
// an old layer's message (fwd_from.from_id, fwd_from.date of TL_messageForwarded_old). An old header that read two
// inline origins (TL_messageFwdHeader_layer68..118: the user at bit 0, the channel at bit 1, both named from_id by the
// table) states both in from_ids with from_state "conflict" and no from: neither is chosen (T5). An inline origin
// of a class the table leaves unresolved gives no address: from_id (the id as stored) and from_class_state
// "contradictory" (4I4-P11; the facts generator holds no such field today, telegram_tl_facts.py contradictory_ids).
func fwdRaw(v view, raw map[string]any) {
	if !v.Has("fwd_from") && !v.Has("fwd_from.from_id") && !v.Has("fwd_from.date") {
		return
	}
	fwd := map[string]any{}
	from, unresolved := addressesAt(v, "fwd_from.from_id")
	switch {
	case len(from)+len(unresolved) > 1:
		from = append(from, unresolved...)
		fwd["from_ids"], fwd["from_state"] = strings.Join(from, ","), stateConflict
	case len(from) == 1:
		fwd["from"] = from[0]
	case len(unresolved) == 1:
		fwd["from_id"], fwd["from_class_state"] = unresolved[0], stateContradictory
	}
	for _, k := range []string{"date", "channel_post", "saved_from_msg_id"} {
		if x, ok := v.Int("fwd_from." + k); ok {
			fwd[k] = x
		}
	}
	for _, k := range []string{"from_name", "post_author"} {
		if b, ok := v.Str("fwd_from." + k); ok {
			cleanInto(fwd, raw, k, "fwd."+k, b, maxName)
		}
	}
	if a, _ := addressesAt(v, "fwd_from.saved_from_peer"); len(a) == 1 {
		fwd["saved_from_peer"] = a[0]
	}
	raw["fwd"] = fwd
}

// addressesAt are the participant addresses read at path (4I4-P11, T4): a Peer object by the field its class reads
// (user_id, chat_id, channel_id), an inline id (id32@ or id64@) by the class the table pins for it (T5), the id as
// stored (a 32-bit id sign-extended as the app reads it). A path read more than once (an old header's two from_id
// fields) gives each, in wire order; an inline id of no address class is unresolved (its id as stored, decimal).
func addressesAt(v view, path string) (out, unresolved []string) {
	for i := range v.r.Fields {
		n := &v.r.Fields[i]
		if n.Path != path {
			continue
		}
		switch n.Kind {
		case tl.KObject:
			for _, k := range addressClasses() {
				if id, ok := v.Int(path + "." + string(k) + "_id"); ok {
					out = append(out, string(k)+":"+strconv.FormatInt(id, 10))
					break
				}
			}
		case tl.KID32, tl.KID64:
			switch dialogClass(n.Class) {
			case classUser, classChat, classChannel:
				out = append(out, n.Class+":"+strconv.FormatInt(n.Int, 10))
			default:
				unresolved = append(unresolved, strconv.FormatInt(n.Int, 10))
			}
		}
	}
	return out, unresolved
}
