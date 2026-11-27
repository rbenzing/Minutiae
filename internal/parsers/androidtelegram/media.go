package androidtelegram

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// The caps of the media summary (spec 8.5, 12): texts are cut and flagged, identifiers never are.
const (
	maxShort = 4 << 10  // a media text (mime type, title, address, url) and the reply copy's text
	maxName  = 1 << 10  // a name (a contact's first and last name, a forward's from_name and post_author)
	maxVcard = 64 << 10 // a contact's vcard

	// The caps of the 4I4b media lists and texts (plan 4I4b "Raw shapes"): a list is cut at its cap and states
	// <key>_total and <key>_truncated, a text is cut and flagged, an identifier is never cut.
	maxSizes     = 16 // entries of media_sizes, media_video_sizes, media_thumbs and media_video_thumbs
	maxLabels    = 32 // attribute labels (a document's attributes, a web page's attributes)
	maxFileNames = 8  // distinct filenames a conflict states
	maxTiny      = 64 // an emoticon, a size type, video_codec, a sticker set emoticon
)

// mediaLabel is the type label of a MessageMedia class (spec 6.6, 8.5): classLabel of the class name between
// TL_messageMedia (or MessageMedia) and the first "_" (a layer or age suffix): TL_messageMediaGeoLive_layer119
// "geo_live", TL_messageMediaWebPage "web_page". A name of another form has no label ("").
func mediaLabel(class string) string {
	if l := classLabel(class, "TL_messageMedia"); l != "" {
		return l
	}
	return classLabel(class, "MessageMedia")
}

// mediaSummary states the media of a message blob whose top constructor c was read (spec 8.5, raw only): raw.media
// {type, constructor, ...} for a media object that was read, or one the decode stopped at (mediaStop), and the size
// lists at the top of raw (B1). It states what was read of the class's fields: the wrapper's flags, ttl_seconds and
// legacy caption; a document's id, date, mime_type, size, dc_id, file name (D10), attribute labels and scalars (P5) and
// thumbnails; a photo's id, date, dc_id, sizes and live-photo document; a legacy video or audio; geo and venue lat,
// long (finite and within +-90/+-180, else <k>_state "out-of-range" and the stored bits <k>_bits); venue title,
// address; a contact's phone_number (an identifier, never cut), first_name, last_name, vcard, user_id; a web page
// (webPageSummary); a dice; and whether an access hash, file reference, key and iv was read (never the bytes, spec 9,
// R7). It returns the attachment of a document or a legacy video or audio: {name, mime, size}, each only when read and
// valid, the name only from the document's own filename attributes (D10), never a source_ref (call 5).
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
	wrapperSummary(v, m, raw)
	var atts []message.Attachment
	switch {
	case strings.HasPrefix(class, "TL_messageMediaPhoto"):
		photoSummary(v, tab, m, raw)
		if v.Has("media.document") { // P9: a live photo's document names no attachment
			if x, ok := v.Int("media.document.id"); ok {
				m["live_photo_document_id"] = x
			}
			mediaText(v, m, raw, "live_photo_mime_type", "media.document.mime_type", maxShort)
		}
	case strings.HasPrefix(class, "TL_messageMediaDocument"):
		atts = documentSummary(v, tab, m, raw)
		altDocuments(v, m)
		if x, ok := v.Int("media.video_cover.id"); ok {
			m["video_cover_photo_id"] = x
		}
		if x, ok := v.Int("media.video_timestamp"); ok {
			m["video_timestamp"] = x
		}
	case strings.HasPrefix(class, "TL_messageMediaVideo"):
		atts = legacyMedia(v, tab, "media.video_unused", m, raw)
	case strings.HasPrefix(class, "TL_messageMediaAudio"):
		atts = legacyMedia(v, tab, "media.audio_unused", m, raw)
	case strings.HasPrefix(class, "TL_messageMediaGeo"), strings.HasPrefix(class, "TL_messageMediaVenue"):
		coordinate(v, m, "lat", "media.geo.lat", 90)
		coordinate(v, m, "long", "media.geo._long", 180)
		if strings.HasPrefix(class, "TL_messageMediaVenue") {
			mediaText(v, m, raw, "title", "media.title", maxShort)
			mediaText(v, m, raw, "address", "media.address", maxShort)
		}
	case strings.HasPrefix(class, "TL_messageMediaContact"):
		if b, ok := v.Str("media.phone_number"); ok {
			identInto(m, raw, "phone_number", "media.phone_number", b, maxIdent)
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
		webPageSummary(v, tab, m, raw)
	case strings.HasPrefix(class, "TL_messageMediaDice"):
		diceSummary(v, m, raw)
	}
	return atts
}

// altDocuments states how many alt documents the wrapper holds (a vector, or the single document of an older class),
// when they were read whole: their own filenames are never read (D10).
func altDocuments(v view, m map[string]any) {
	const p = "media.alt_documents"
	if !v.Whole(p) {
		return
	}
	if n, ok := v.Len(p); ok {
		m["alt_documents_total"] = int64(n)
	} else if _, ok := v.ID(p); ok {
		m["alt_documents_total"] = int64(1)
	}
}

// wrapperSummary states what every MessageMedia wrapper may carry: its flag-only trues, ttl_seconds and a legacy
// caption (captionLegacy, or the caption of a legacy photo or video: raw only, never a Body, Q9).
func wrapperSummary(v view, m, raw map[string]any) {
	for _, f := range []string{"spoiler", "video", "round", "voice", "nopremium", "live_photo"} {
		if v.True("media." + f) {
			m[f] = true
		}
	}
	if x, ok := v.Int("media.ttl_seconds"); ok {
		m["ttl_seconds"] = x
	}
	if v.Has("media.captionLegacy") {
		mediaText(v, m, raw, "caption", "media.captionLegacy", maxShort)
		return
	}
	for _, p := range []string{"media.photo.caption", "media.video_unused.caption"} {
		if v.Has(p) {
			mediaText(v, m, raw, "caption", p, maxShort)
			return
		}
	}
}

// diceSummary states a dice: its emoticon, value and the outcome of a game roll (the stakes, and the length of the
// seed: the seed itself never, D11).
func diceSummary(v view, m, raw map[string]any) {
	mediaText(v, m, raw, "emoticon", "media.emoticon", maxTiny)
	if x, ok := v.Int("media.value"); ok {
		m["value"] = x
	}
	if !v.Has("media.game_outcome") {
		return
	}
	g := map[string]any{}
	for _, k := range []string{"stake_ton_amount", "ton_amount"} {
		if x, ok := v.Int("media.game_outcome." + k); ok {
			g[k] = x
		}
	}
	if b, ok := v.Str("media.game_outcome.seed"); ok {
		g["seed_len"] = int64(len(b))
	}
	m["game_outcome"] = g
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

// photoSummary states a photo's id, date, dc_id and (legacy) user_id, whether its access hash and file reference was
// read, has_stickers, the coordinates of its geo point, and its size lists (raw.media_sizes, raw.media_video_sizes).
func photoSummary(v view, tab tl.Table, m, raw map[string]any) {
	if !v.Has("media.photo") {
		return
	}
	const p = "media.photo."
	for _, k := range []string{"id", "date", "dc_id", "user_id"} {
		if x, ok := v.Int(p + k); ok {
			m[k] = x
		}
	}
	present(v, m, p)
	if v.True(p + "has_stickers") {
		m["has_stickers"] = true
	}
	coordinate(v, m, "geo_lat", p+"geo.lat", 90)
	coordinate(v, m, "geo_long", p+"geo._long", 180)
	sizeList(v, tab, p+"sizes", "media_sizes", raw)
	sizeList(v, tab, p+"video_sizes", "media_video_sizes", raw)
}

// documentSummary states a document's id, date, size, dc_id and mime type, whether its access hash, file reference, key
// and iv were read, its file name (D10), attribute labels and scalars, and thumbnails (raw.media_thumbs,
// raw.media_video_thumbs), and returns its attachment {name, mime, size} (each only when read and valid; none when
// none is).
func documentSummary(v view, tab tl.Table, m, raw map[string]any) []message.Attachment {
	if !v.Has("media.document") {
		return nil
	}
	const d = "media.document"
	for _, k := range []string{"id", "date", "size", "dc_id"} {
		if x, ok := v.Int(d + "." + k); ok {
			m[k] = x
		}
	}
	mime := mediaText(v, m, raw, "mime_type", d+".mime_type", maxShort)
	present(v, m, d+".")
	name := documentName(v, tab, m, raw, d, "file_name")
	attributesSummary(v, tab, d, m, raw)
	sizeList(v, tab, d+".thumbs", "media_thumbs", raw)
	sizeList(v, tab, d+".video_thumbs", "media_video_thumbs", raw)
	return attachment(name, mime, v, d+".size")
}

// legacyMedia states a legacy video or audio (layer 45 and older: the media object at path) and returns its attachment
// {mime, size}: no name, the legacy classes carry none. Its duration has no unit (the table gives none), its thumbnail
// is one entry of raw.media_thumbs.
func legacyMedia(v view, tab tl.Table, path string, m, raw map[string]any) []message.Attachment {
	if !v.Has(path) {
		return nil
	}
	p := path + "."
	for _, k := range []string{"id", "date", "size", "dc_id", "w", "h", "user_id"} {
		if x, ok := v.Int(p + k); ok {
			m[k] = x
		}
	}
	if x, ok := number(v, p+"duration"); ok {
		m["duration"], m["duration_unit"] = x, "unestablished"
	}
	mime := mediaText(v, m, raw, "mime_type", p+"mime_type", maxShort)
	present(v, m, p)
	sizeList(v, tab, p+"thumb", "media_thumbs", raw)
	return attachment("", mime, v, p+"size")
}

// attachment is the attachment of a media object: the name (read from a filename attribute), the mime type, and the
// size when read and not negative; none when none of them is.
func attachment(name, mime string, v view, sizePath string) []message.Attachment {
	a := message.Attachment{Name: name, Mime: mime}
	if size, ok := v.Int(sizePath); ok && size >= 0 {
		a.Size = &size
	}
	if a.Name == "" && a.Mime == "" && a.Size == nil {
		return nil
	}
	return []message.Attachment{a}
}

// present states access_hash_present, file_reference_present, key_present and iv_present of the object at prefix when
// the field was read: a KSecret's length only was read, never the bytes (spec 9, R7).
func present(v view, m map[string]any, prefix string) {
	for _, k := range []string{"access_hash", "file_reference", "key", "iv"} {
		if v.Has(prefix + k) {
			m[k+"_present"] = true
		}
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

// cleaned is b cleaned at limit (C4): its text ("" when empty); an altered or cut text is named flag in
// raw.text_flags.
func cleaned(raw map[string]any, flag string, b []byte, limit int) string {
	cl := common.CleanText(b, limit)
	if len(cl.Flags) > 0 {
		markText(raw, flag)
	}
	return cl.Text
}
