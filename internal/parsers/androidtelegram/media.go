package androidtelegram

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// The caps of the media summary (spec 8.5, 12): texts are cut and flagged, identifiers never are.
const (
	maxShort = 4 << 10  // a media text (mime type, title, address, url) and the reply copy's text
	maxName  = 1 << 10  // a name (a contact's first and last name, a forward's from_name and post_author)
	maxVcard = 64 << 10 // a contact's vcard
)

// mediaLabel is the type label of a MessageMedia class (spec 6.6, 8.5): the class name between TL_messageMedia (or
// MessageMedia) and the first "_" (a layer or age suffix), in snake case: TL_messageMediaGeoLive_layer119 "geo_live",
// TL_messageMediaWebPage "web_page". A name of another form has no label ("").
func mediaLabel(class string) string {
	rest, ok := strings.CutPrefix(class, "TL_messageMedia")
	if !ok {
		if rest, ok = strings.CutPrefix(class, "MessageMedia"); !ok {
			return ""
		}
	}
	rest, _, _ = strings.Cut(rest, "_")
	var sb strings.Builder
	for i, r := range rest {
		if unicode.IsUpper(r) {
			if i > 0 {
				sb.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// mediaSummary states the media of a message blob whose top constructor c was read (spec 8.5, raw only; T3: the
// sub-types beyond the summary are 4I4b): raw.media {type, constructor, ...} for a media object that was read, or one
// the decode stopped at (mediaStop). The
// summary holds what was read of the class's fields: document id, date, mime_type, size, dc_id; photo id, date; geo
// and venue lat, long (finite and within +-90/+-180, else <k>_state "out-of-range" and the stored bits <k>_bits);
// venue title, address; contact phone_number (an identifier, never cut), first_name, last_name, vcard, user_id;
// webpage url, display_url; access_hash_present, file_reference_present, key_present, iv_present (never the bytes,
// spec 9). It
// returns a document's attachment, {mime, size} each only when read and valid: never a name, never a source_ref
// (call 5).
func mediaSummary(v view, tab tl.Table, c tl.Constructor, raw map[string]any) []message.Attachment {
	if !hasField(c, "media") {
		return nil
	}
	if s := v.stopAt("media"); s != nil {
		raw["media"] = mediaStop(s, tab)
		return nil
	}
	id, ok := v.ID("media")
	if !ok {
		return nil
	}
	class := tab[id].Name
	m := map[string]any{"type": mediaLabel(class), "constructor": hexID(id)}
	raw["media"] = m
	switch {
	case strings.HasPrefix(class, "TL_messageMediaPhoto"):
		photoSummary(v, m)
	case strings.HasPrefix(class, "TL_messageMediaDocument"):
		return documentSummary(v, m, raw)
	case strings.HasPrefix(class, "TL_messageMediaGeo"), strings.HasPrefix(class, "TL_messageMediaVenue"):
		coordinate(v, m, "lat", "media.geo.lat", 90)
		coordinate(v, m, "long", "media.geo._long", 180)
		if strings.HasPrefix(class, "TL_messageMediaVenue") {
			mediaText(v, m, raw, "title", "media.title", maxShort)
			mediaText(v, m, raw, "address", "media.address", maxShort)
		}
	case strings.HasPrefix(class, "TL_messageMediaContact"):
		if b, ok := v.Str("media.phone_number"); ok {
			if s, ok := ident(m, "phone_number", b, maxIdent); ok && s != "" {
				m["phone_number"] = s
				if s != string(b) {
					markText(raw, "media.phone_number")
				}
			}
		}
		mediaText(v, m, raw, "first_name", "media.first_name", maxName)
		mediaText(v, m, raw, "last_name", "media.last_name", maxName)
		mediaText(v, m, raw, "vcard", "media.vcard", maxVcard)
		if x, ok := v.Int("media.user_id"); ok {
			m["user_id"] = x
		}
	case strings.HasPrefix(class, "TL_messageMediaWebPage"):
		mediaText(v, m, raw, "url", "media.webpage.url", maxShort)
		mediaText(v, m, raw, "display_url", "media.webpage.display_url", maxShort)
	}
	return nil
}

// mediaStop is raw.media of a decode that stopped at the media object itself: an ID that was read (one the table
// lacks, or an opaque one with its class's label) is its constructor; a stop before the ID was read or at it for
// another reason (the blob ends inside the ID, the depth bound) is state <the stop kind>, never an ID that is not on
// the wire (review I1; data_stop_path says where).
func mediaStop(s *tl.Stop, tab tl.Table) map[string]any {
	switch s.Kind {
	case tl.StopUnknownConstructor:
		return map[string]any{"constructor": hexID(s.ID)}
	case tl.StopOpaque:
		m := map[string]any{"constructor": hexID(s.ID)}
		if mc, ok := tab[s.ID]; ok {
			m["type"] = mediaLabel(mc.Name)
		}
		return m
	}
	m := map[string]any{"state": s.Kind}
	if mc, ok := tab[s.ID]; ok && s.ID != 0 { // the ID was read and known: a refusal after it (the budget)
		m["constructor"], m["type"] = hexID(s.ID), mediaLabel(mc.Name)
	}
	return m
}

// photoSummary states a photo's id and date and whether its access hash and file reference were read.
func photoSummary(v view, m map[string]any) {
	if !v.Has("media.photo") {
		return
	}
	for _, k := range []string{"id", "date"} {
		if x, ok := v.Int("media.photo." + k); ok {
			m[k] = x
		}
	}
	present(v, m, "media.photo.")
}

// documentSummary states a document's id, date, size, dc_id and mime type, whether its access hash, file reference and
// key were read, and returns its attachment {mime, size} (each only when read and valid; none when neither is).
func documentSummary(v view, m, raw map[string]any) []message.Attachment {
	if !v.Has("media.document") {
		return nil
	}
	const d = "media.document."
	for _, k := range []string{"id", "date", "size", "dc_id"} {
		if x, ok := v.Int(d + k); ok {
			m[k] = x
		}
	}
	mime := mediaText(v, m, raw, "mime_type", d+"mime_type", maxShort)
	present(v, m, d)
	for _, k := range []string{"key", "iv"} {
		if v.Has(d + k) {
			m[k+"_present"] = true // a KSecret: its length only was read, never the bytes (spec 9)
		}
	}
	var a message.Attachment
	a.Mime = mime
	if size, ok := v.Int(d + "size"); ok && size >= 0 {
		a.Size = &size
	}
	if a.Mime == "" && a.Size == nil {
		return nil
	}
	return []message.Attachment{a}
}

// present states access_hash_present and file_reference_present of the object at prefix when the field was read.
func present(v view, m map[string]any, prefix string) {
	if v.Has(prefix + "access_hash") {
		m["access_hash_present"] = true
	}
	if v.Has(prefix + "file_reference") {
		m["file_reference_present"] = true
	}
}

// coordinate states one coordinate of a geo point read at path: within +-limit and finite as read, else key_state
// "out-of-range" and the stored IEEE-754 bits key_bits (a value that is no coordinate is never shown as one).
func coordinate(v view, m map[string]any, key, path string, limit float64) {
	n, ok := v.Named(path)
	if !ok || n.Kind != tl.KDouble {
		return
	}
	x := n.Float
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) > limit {
		m[key+"_state"] = stateOutOfRange
		m[key+"_bits"] = fmt.Sprintf("0x%016x", math.Float64bits(x))
		return
	}
	m[key] = x
}

// mediaText states the text read at path in m[key], cleaned at limit (C4) and absent when empty; an altered or cut
// text is named media.<key> in raw.text_flags. It returns the text stated.
func mediaText(v view, m, raw map[string]any, key, path string, limit int) string {
	b, ok := v.Str(path)
	if !ok {
		return ""
	}
	return cleanInto(m, raw, key, "media."+key, b, limit)
}

// cleanInto states b cleaned at limit (C4) in m[key], absent when empty; an altered or cut text is named flag in
// raw.text_flags. It returns the text stated.
func cleanInto(m, raw map[string]any, key, flag string, b []byte, limit int) string {
	cl := common.CleanText(b, limit)
	if len(cl.Flags) > 0 {
		markText(raw, flag)
	}
	if cl.Text == "" {
		return ""
	}
	m[key] = cl.Text
	return cl.Text
}

// markText names a text CleanText altered or cut in raw.text_flags (the plan's shared vocabulary: one comma-joined
// string of names, sorted, B8).
func markText(raw map[string]any, name string) {
	var names []string
	if s, ok := raw["text_flags"].(string); ok && s != "" {
		names = strings.Split(s, ",")
	}
	if slices.Contains(names, name) {
		return
	}
	names = append(names, name)
	slices.Sort(names)
	raw["text_flags"] = strings.Join(names, ",")
}
