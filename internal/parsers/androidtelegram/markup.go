package androidtelegram

import (
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// The caps of the reply markup (plan 4I4c "Raw shapes"): the button list is cut at its cap and states its total and
// that it was cut, the peer types of a switch-inline button are cut at theirs and state their total.
const (
	maxButtons   = 64
	maxPeerTypes = 8
)

const (
	keyMarkup        = "reply_markup"
	keyMarkupButtons = "reply_markup_buttons"

	// classInlineRow228 is the A-HEAD row class that holds inline buttons only: a plain keyboard button in it is what
	// the A-HEAD app drops (ruling C3).
	classInlineRow228 = "TL_keyboardInlineButtonRow_layer228"
	typeKeyboardBtn   = "TL_keyboard.KeyboardButton"
)

// markupKind is the kind of a ReplyMarkup class (design 9.1): keyboard, inline, force_reply or hide; "" for a class
// that is none of them.
func markupKind(class string) string {
	switch {
	case strings.HasPrefix(class, "TL_replyKeyboardMarkup"):
		return "keyboard"
	case strings.HasPrefix(class, "TL_replyInlineMarkup"):
		return "inline"
	case strings.HasPrefix(class, "TL_replyKeyboardForceReply"):
		return "force_reply"
	case strings.HasPrefix(class, "TL_replyKeyboardHide"):
		return "hide"
	}
	return ""
}

// markupRaw states the reply markup of a message blob in raw (design 9.1, 9.2; plan 4I4c "Raw shapes"): raw.reply_markup
// and, at the top of raw (B1), raw.reply_markup_buttons, never a record field and never a participant. The markup is
// stated only when read whole (P3); a stop inside it is raw.reply_markup_state "partial" and no button; a stop at the
// markup itself is markupStop.
func markupRaw(v view, raw map[string]any) {
	if s := v.stopAt(keyMarkup); s != nil {
		raw[keyMarkup] = markupStop(s, v.tab)
		return
	}
	switch {
	case v.Whole(keyMarkup):
		m, buttons := markupOf(v, raw)
		raw[keyMarkup] = m
		if len(buttons) > 0 {
			raw[keyMarkupButtons] = buttons
		}
	case v.inside(keyMarkup):
		raw[keyMarkup+"_state"] = statePartial
	}
}

// markupStop is raw.reply_markup of a decode that stopped at the markup object itself, by the rule of mediaStop: an ID
// that was read (an unknown one, or an opaque one with its kind) is its constructor; a stop before the ID was read is
// state <the stop kind>, never an ID that is not on the wire.
func markupStop(s *tl.Stop, tab tl.Table) map[string]any {
	c, known := tab[tl.Key{Type: s.Type, ID: s.ID}]
	switch s.Kind {
	case tl.StopUnknownConstructor:
		return map[string]any{"constructor": hexID(s.ID)}
	case tl.StopOpaque:
		m := map[string]any{"constructor": hexID(s.ID)}
		if k := markupKind(c.Name); known && k != "" {
			m["kind"] = k
		}
		return m
	}
	m := map[string]any{"state": s.Kind}
	if known && s.ID != 0 { // the ID was read and known: a refusal after it (the budget)
		m["constructor"] = hexID(s.ID)
		if k := markupKind(c.Name); k != "" {
			m["kind"] = k
		}
	}
	return m
}

// markupOf is the scalar-only raw.reply_markup of a markup read whole and the capped button list of its rows.
func markupOf(v view, raw map[string]any) (map[string]any, []any) {
	id, c, ok := entryAt(v, v.tab, keyMarkup)
	m := map[string]any{"constructor": hexID(id)}
	if k := markupKind(c.Name); ok && k != "" {
		m["kind"] = k
	}
	for _, k := range []string{"resize", "single_use", "selective", "is_persistent", "force_reply"} {
		if v.True(keyMarkup + "." + k) {
			m[k] = true
		}
	}
	if b, ok := v.Str(keyMarkup + ".placeholder"); ok {
		cleanInto(m, raw, "placeholder", "reply_markup.placeholder", b, maxName)
	}
	const rows = keyMarkup + ".rows"
	n, ok := v.Len(rows)
	if !ok {
		return m, nil
	}
	m["rows_total"] = int64(n)
	var buttons []any
	total := 0
	for i := range n {
		rp := elem(rows, i) + ".buttons"
		count, _ := v.Len(rp)
		for j := range count {
			if total < maxButtons {
				buttons = append(buttons, buttonOf(v, raw, elem(rp, j), i, j))
			}
			total++
		}
	}
	m["buttons_total"] = int64(total)
	if total > maxButtons {
		m["buttons_truncated"] = true
	}
	return m, buttons
}

// buttonPath is the first path read of the button at p for the field name: on the button itself, in its type object, or
// in the mType holder of an older class ("" when none was read).
func buttonPath(v view, p, name string) string {
	for _, q := range []string{p + "." + name, p + ".type." + name, p + ".mType." + name} {
		if v.Has(q) {
			return q
		}
	}
	return ""
}

// buttonKind is the kind of the button at p of class: the label of its own class, or, for the classes that carry a type
// object, the label of that type; a plain button is "default".
func buttonKind(v view, p, class string) string {
	var label string
	switch {
	case class == "TL_keyboardButton":
		label = classLabel(v.Class(p+".type"), "TL_buttonType")
	case strings.HasPrefix(class, "TL_keyboardInlineButton"):
		label = classLabel(v.Class(p+".type"), "TL_inlineButtonType")
	default:
		label = classLabel(class, "TL_keyboardButton")
	}
	if label == "" {
		return "default"
	}
	return label
}

// buttonOf is one scalar-only entry of raw.reply_markup_buttons: the button at p, in row and column (0-based), with
// each field that was read (plan 4I4c "Raw shapes"). A bot callback's data is its length and SHA-256 only (D8, ruling
// Q8), never bytes; a profile button's user is an address and never a participant.
func buttonOf(v view, raw map[string]any, p string, row, column int) map[string]any {
	id, c, _ := entryAt(v, v.tab, p)
	m := map[string]any{"row": int64(row), "column": int64(column), "kind": buttonKind(v, p, c.Name), "constructor": hexID(id)}
	for _, t := range []struct {
		key   string
		limit int
	}{{"text", maxName}, {"fwd_text", maxName}, {"query", maxName}, {"url", maxShort}, {"copy_text", maxShort}} {
		if b, ok := v.Str(buttonPath(v, p, t.key)); ok {
			cleanInto(m, raw, t.key, keyMarkupButtons+"."+t.key, b, t.limit)
		}
	}
	if v.True(buttonPath(v, p, "same_peer")) {
		m["same_peer"] = true
	}
	if pp := buttonPath(v, p, "peer_types"); pp != "" {
		n, _ := v.Len(pp)
		var labels []string
		for i := range min(n, maxPeerTypes) {
			labels = append(labels, classLabel(v.Class(elem(pp, i)), "TL_inlineQueryPeerType"))
		}
		if len(labels) > 0 {
			m["peer_types"] = strings.Join(labels, ",")
		}
		m["peer_types_total"] = int64(n)
		if n > maxPeerTypes {
			m["peer_types_truncated"] = true // ruling T5r1: every capped list states its cut
		}
	}
	for _, k := range []string{"button_id", "max_quantity"} {
		if x, ok := v.Int(buttonPath(v, p, k)); ok {
			m[k] = x
		}
	}
	if x, ok := v.Bool(buttonPath(v, p, "quiz")); ok {
		m["quiz"] = x
	}
	if v.True(buttonPath(v, p, "requires_password")) {
		m["requires_password"] = true
	}
	if b, ok := v.Str(buttonPath(v, p, "data")); ok {
		m["data_len"], m["data_sha256"] = int64(len(b)), sha256Hex(b)
	}
	if x, ok := v.Int(buttonPath(v, p, "user_id")); ok {
		m["user"] = "user:" + strconv.FormatInt(x, 10)
	}
	if pt := buttonPath(v, p, "peer_type"); pt != "" {
		peerTypeInto(v, raw, m, pt)
	}
	for _, s := range [][2]string{{"style_primary", "bg_primary"}, {"style_danger", "bg_danger"}, {"style_success", "bg_success"}} {
		if v.True(p + ".style." + s[1]) {
			m[s[0]] = true
		}
	}
	if x, ok := v.Int(p + ".style.icon"); ok {
		m["style_icon"] = x
	}
	if n, ok := v.Named(p); ok && n.Type == typeKeyboardBtn {
		if i := strings.LastIndex(p, ".buttons["); i >= 0 && v.Class(p[:i]) == classInlineRow228 {
			m["ahead_app_drops"] = true
		}
	}
	return m
}

// peerTypeInto states a request-peer button's RequestPeerType read at pt: its label, each flag-only true set or bool
// read, the suggested name and username of a bot to create, and the admin rights asked for.
func peerTypeInto(v view, raw, m map[string]any, pt string) {
	m["peer_type"] = classLabel(v.Class(pt), "TL_requestPeerType")
	for _, f := range []string{"creator", "bot_participant", "has_username", "forum", "bot", "premium", "bot_managed"} {
		if x, ok := v.Bool(pt + "." + f); ok {
			m["peer_type_"+f] = x
		}
	}
	for _, k := range []string{"suggested_name", "suggested_username"} {
		if b, ok := v.Str(pt + "." + k); ok {
			cleanInto(m, raw, k, keyMarkupButtons+"."+k, b, maxName)
		}
	}
	for _, k := range []string{"user_admin_rights", "bot_admin_rights"} {
		if ap := pt + "." + k; v.Has(ap) {
			m[k] = adminRights(v, ap)
		}
	}
}

// adminRights is the rights of the TL_chatAdminRights read at p: the names of the flag-only trues set, in bit order,
// joined by "," ("" when none is set).
func adminRights(v view, p string) string {
	_, c, _ := entryAt(v, v.tab, p)
	var set []string
	for _, f := range c.Fields {
		if f.Kind == tl.KTrue && v.True(p+"."+f.Name) {
			set = append(set, f.Name)
		}
	}
	return strings.Join(set, ",")
}
