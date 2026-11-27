package androidtelegram

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// classLabel is the type label of a constructor class (plan 4I4b "Raw shapes", Labels): the class name after prefix up
// to the first "_" (a layer or age suffix), in snake case, a run of capitals one word (ruling T4a:
// TL_inputStickerSetID is "id", TL_documentAttributeImageSize "image_size"). A class of another prefix has no label
// ("").
func classLabel(class, prefix string) string {
	rest, ok := strings.CutPrefix(class, prefix)
	if !ok {
		return ""
	}
	rest, _, _ = strings.Cut(rest, "_")
	var sb strings.Builder
	var prev rune
	for i, r := range rest {
		orig := r
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
				sb.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		sb.WriteRune(r)
		prev = orig
	}
	return sb.String()
}

// sizeKind is the kind of a PhotoSize or VideoSize class (plan 4I4b "Raw shapes", Size entry): the class name without
// its layer suffix, one of nine words; "unknown" for a class that is none of them.
func sizeKind(class string) string {
	base, _, _ := strings.Cut(class, "_layer")
	switch base {
	case "TL_photoSize":
		return "size"
	case "TL_photoCachedSize":
		return "cached"
	case "TL_photoStrippedSize":
		return "stripped"
	case "TL_photoPathSize":
		return "path"
	case "TL_photoSizeProgressive":
		return "progressive"
	case "TL_photoSizeEmpty":
		return "empty"
	case "TL_videoSize":
		return "video"
	case "TL_videoSizeEmojiMarkup":
		return "emoji_markup"
	case "TL_videoSizeStickerMarkup":
		return "sticker_markup"
	}
	return "unknown"
}

// className is the table class name of the object read at path ("" when no object was read there).
func className(v view, tab tl.Table, path string) string {
	_, c, _ := entryAt(v, tab, path)
	return c.Name
}

// elem is the path of element i of the list at path.
func elem(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }

// finiteInto states the number x at m[key]; a double that is not finite is m[key+"_state"] "out-of-range" and the
// stored IEEE-754 bits m[key+"_bits"] (the coordinate rule without a range): never shown as a number.
func finiteInto(m map[string]any, key string, x any) {
	if f, ok := x.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
		m[key+"_state"] = stateOutOfRange
		m[key+"_bits"] = fmt.Sprintf("0x%016x", math.Float64bits(f))
		return
	}
	m[key] = x
}

// number is the integer or double read at path.
func number(v view, path string) (any, bool) {
	n, ok := v.Named(path)
	if !ok {
		return nil, false
	}
	if n.Kind == tl.KDouble {
		return n.Float, true
	}
	if x, ok := v.Int(path); ok {
		return x, true
	}
	return nil, false
}

// putList states the list at path in m: entry(element path) for each of the first limit elements (an element it
// declines, and an empty list, is absent), m[key+"_total"] the count read and m[key+"_truncated"] when the cap cut it,
// when the list was read whole; m[key+"_state"] "partial" and no entry when the decode stopped at or inside it (P3: a
// list a stop cut is not the list). A list the decode never reached states nothing.
func putList(v view, m map[string]any, key, path string, limit int, entry func(p string) (any, bool)) {
	switch {
	case v.Whole(path):
		n, ok := v.Len(path)
		if !ok {
			return
		}
		out := []any{}
		for i := range min(n, limit) {
			if e, ok := entry(elem(path, i)); ok {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			m[key] = out
		}
		m[key+"_total"] = int64(n)
		if n > limit {
			m[key+"_truncated"] = true
		}
	case v.inside(path):
		m[key+"_state"] = statePartial
	}
}

// labelList states the class labels (prefix) of the list at path, as putList states a list.
func labelList(v view, tab tl.Table, m map[string]any, key, path, prefix string, limit int) {
	putList(v, m, key, path, limit, func(p string) (any, bool) {
		return classLabel(className(v, tab, p), prefix), true
	})
}

// sizeEntry is one scalar-only size entry (plan 4I4b "Raw shapes", Size entry) of the PhotoSize or VideoSize read at
// p; flag names its texts in raw.text_flags. The thumbnail bytes are a length and a digest (D11), a file location is
// its class name only (R7, D12): never a volume, a local id or a secret.
func sizeEntry(v view, tab tl.Table, raw map[string]any, p, flag string) (any, bool) {
	class := className(v, tab, p)
	if class == "" {
		return nil, false
	}
	e := map[string]any{"kind": sizeKind(class)}
	if b, ok := v.Str(p + ".type"); ok {
		cleanInto(e, raw, "type", flag+".type", b, maxTiny)
	}
	for _, k := range []string{"w", "h", "size", "emoji_id", "sticker_id"} {
		if x, ok := v.Int(p + "." + k); ok {
			e[k] = x
		}
	}
	if b, ok := v.Str(p + ".bytes"); ok {
		e["bytes_len"], e["bytes_sha256"] = int64(len(b)), sha256Hex(b)
	}
	for _, kk := range [][2]string{{"sizes", "progressive_total"}, {"background_colors", "background_colors_total"}} {
		if n, ok := v.Len(p + "." + kk[0]); ok {
			e[kk[1]] = int64(n)
		}
	}
	if c := className(v, tab, p+".location"); c != "" {
		e["location_class"] = c
	}
	if x, ok := number(v, p+".video_start_ts"); ok {
		finiteInto(e, "video_start_ts", x)
	}
	if c := className(v, tab, p+".stickerset"); c != "" {
		e["stickerset_kind"] = classLabel(c, "TL_inputStickerSet")
	}
	return e, true
}

// fieldKind is the kind the constructor of the object read at the parent of path gives the field named by path's last
// element (0 when that object was not read or has no such field).
func fieldKind(v view, tab tl.Table, path string) tl.Kind {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return 0
	}
	_, c, ok := entryAt(v, tab, path[:i])
	if !ok {
		return 0
	}
	for _, f := range c.Fields {
		if f.Name == path[i+1:] {
			return f.Kind
		}
	}
	return 0
}

// sizeList states raw[key] (B1: at the top of raw) for the sizes at path: a vector, or the single PhotoSize an old
// class carries as one entry (plan 4I4b "Raw shapes"); as putList states a list.
func sizeList(v view, tab tl.Table, path, key string, raw map[string]any) {
	switch fieldKind(v, tab, path) {
	case tl.KVector:
		putList(v, raw, key, path, maxSizes, func(p string) (any, bool) { return sizeEntry(v, tab, raw, p, key) })
	case tl.KObject:
		switch {
		case v.Whole(path):
			if e, ok := sizeEntry(v, tab, raw, path, key); ok {
				raw[key] = []any{e}
				raw[key+"_total"] = int64(1)
			}
		case v.inside(path):
			raw[key+"_state"] = statePartial
		}
	}
}
