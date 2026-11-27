package androidtelegram

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// The file name of a document (design D10, rulings R6 and B6): taken from exactly the document's own filename
// attributes (or the inline file_name of an old class), never from another document the media holds (alt documents, a
// live photo's document, a link preview's document are named by their own keys).
const (
	nameAbsent      = "absent"       // the attributes were read whole and none is a filename
	nameEmpty       = "empty"        // the one name has no stored bytes: no name
	nameUnprintable = "unprintable"  // the one name has stored bytes the cleaning reduces to nothing (T5a)
	nameConflict    = "conflict"     // two different names: neither is chosen, the names are raw
	nameUndecided   = stateUndecided // the attribute list was not read whole
)

// fileName is the name of the document read at doc: its text (cleaned at maxName; flag names it in raw.text_flags) when
// every filename attribute holds the same bytes, else state absent (none), empty, conflict (names: at most maxFileNames
// distinct names in wire order, total the number of distinct names) or undecided (the attribute list was not read
// whole). No state and no name: the document was not read, or its class has neither attributes nor a file_name.
func fileName(v view, tab tl.Table, doc, flag string, raw map[string]any) (name, state string, names []string, total int) {
	_, c, ok := entryAt(v, tab, doc)
	if !ok {
		return "", "", nil, 0
	}
	var values [][]byte
	switch {
	case hasField(c, "file_name"):
		b, ok := v.Str(doc + ".file_name")
		if !ok {
			return "", nameUndecided, nil, 0
		}
		values = [][]byte{b}
	case hasField(c, "attributes"):
		p := doc + ".attributes"
		if !v.Whole(p) {
			return "", nameUndecided, nil, 0
		}
		n, _ := v.Len(p)
		for i := range n {
			if className(v, tab, elem(p, i)) != "TL_documentAttributeFilename" {
				continue
			}
			if b, ok := v.Str(elem(p, i) + ".file_name"); ok {
				values = append(values, b)
			}
		}
	default:
		return "", "", nil, 0
	}
	distinct := distinctBytes(values)
	switch {
	case len(distinct) == 0:
		return "", nameAbsent, nil, 0
	case len(distinct) > 1:
		for _, b := range distinct[:min(len(distinct), maxFileNames)] {
			names = append(names, cleaned(raw, flag, b, maxName))
		}
		return "", nameConflict, names, len(distinct)
	}
	if name = cleaned(raw, flag, distinct[0], maxName); name == "" {
		return "", noNameState(distinct[0]), nil, len(distinct[0]) // total: the stored bytes of an unprintable name
	}
	return name, "", nil, 0
}

// documentName states the name of the document at doc under key (m[key], m[key+"_state"], m[key+"s"] and
// m[key+"s_total"]) and returns the name stated.
func documentName(v view, tab tl.Table, m, raw map[string]any, doc, key string) string {
	name, state, names, total := fileName(v, tab, doc, "media."+key, raw)
	switch {
	case name != "":
		m[key] = name
	case state != "":
		m[key+"_state"] = state
	}
	if state == nameUnprintable {
		m[key+"_len"] = int64(total)
	}
	if state == nameConflict {
		out := make([]any, len(names))
		for i, n := range names {
			out[i] = n
		}
		m[key+"s"] = out
		m[key+"s_total"] = int64(total)
	}
	return name
}

// carrier is one attribute's value of a key: its comparable form and its value.
type carrier struct {
	canon string
	val   any
}

// canonOf is the form two values compare by: numbers by value (an integer and a double of the same value agree), a
// double that is not finite by its bits, bytes and strings by content.
func canonOf(x any) string {
	switch x := x.(type) {
	case nil:
		return "o:none"
	case int64:
		return "n:" + strconv.FormatInt(x, 10)
	case float64:
		switch {
		case math.IsNaN(x) || math.IsInf(x, 0):
			return fmt.Sprintf("f:%016x", math.Float64bits(x))
		case x == 0:
			return "n:0"
		case x == math.Trunc(x) && math.Abs(x) < 1e21:
			return "n:" + strconv.FormatFloat(x, 'f', -1, 64)
		}
		return "n:" + strconv.FormatFloat(x, 'g', -1, 64)
	case []byte:
		return "o:" + string(x)
	case string:
		return "o:" + x
	case bool:
		return "o:" + strconv.FormatBool(x)
	}
	return "o:?"
}

// attributesSummary states the attributes of the document read at doc into m (and the size lists into raw): their
// labels in wire order, and, when the list was read whole, their scalars (P5).
func attributesSummary(v view, tab tl.Table, doc string, m, raw map[string]any) {
	p := doc + ".attributes"
	labelList(v, tab, m, "attributes", p, "TL_documentAttribute", maxLabels)
	if v.Whole(p) {
		attributeScalars(v, tab, m, raw, p)
	}
}

// attributeScalars states the scalars of the attributes at path (read whole): a key every attribute that carries it
// holds the same decoded value of is stated; carriers that disagree give <key>_state "conflict" and no value (P5). A
// boolean is true when any carrier sets it. A thumbnail-like content (the waveform) is a length and a digest (D11).
func attributeScalars(v view, tab tl.Table, m, raw map[string]any, path string) {
	vals := map[string][]carrier{}
	bools := map[string]bool{}
	add := func(k string, x any) { vals[k] = append(vals[k], carrier{canonOf(x), x}) }
	boolFields := map[string][]string{"video": {"round_message", "supports_streaming", "nosound"}, "audio": {"voice"}, "sticker": {"mask"}}
	scalarFields := map[string][]string{
		"image_size": {"w", "h"},
		"video":      {"duration", "w", "h", "preload_prefix_size", "video_start_ts", "video_codec"},
		"audio":      {"duration", "title", "performer", "waveform"},
	}
	n, _ := v.Len(path)
	for i := range n {
		a := elem(path, i)
		class := className(v, tab, a)
		if class == "" {
			continue
		}
		lab := classLabel(class, "TL_documentAttribute")
		for _, f := range boolFields[lab] {
			if v.True(a + "." + f) {
				bools[ownKey(lab, f)] = true // T5c
			}
		}
		for _, f := range scalarFields[lab] {
			if x, ok := scalar(v, a+"."+f); ok {
				add(f, x)
			}
		}
		if lab != "sticker" && lab != "custom_emoji" {
			continue
		}
		if b, ok := v.Str(a + ".alt"); ok {
			add("sticker_alt", b)
		}
		if s := className(v, tab, a+".stickerset"); s != "" {
			sp := a + ".stickerset"
			add("stickerset_kind", classLabel(s, "TL_inputStickerSet"))
			if x, ok := v.Int(sp + ".id"); ok {
				add("stickerset_id", x)
			}
			if b, ok := v.Str(sp + ".short_name"); ok {
				add("stickerset_short_name", b)
			}
			if b, ok := v.Str(sp + ".emoticon"); ok {
				add("stickerset_emoticon", b)
			}
			if v.Has(sp + ".access_hash") {
				add("stickerset_access_hash_present", true)
			}
		}
		if v.Has(a + ".mask_coords") {
			coords := make([]any, 4)
			key := ""
			for j, f := range []string{"n", "x", "y", "zoom"} {
				if x, ok := number(v, a+".mask_coords."+f); ok {
					coords[j] = x
				}
				key += canonOf(coords[j]) + "|"
			}
			vals["mask_coords"] = append(vals["mask_coords"], carrier{key, coords})
		}
	}
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		list := vals[k]
		if slices.ContainsFunc(list, func(c carrier) bool { return c.canon != list[0].canon }) {
			m[k+"_state"] = "conflict"
			continue
		}
		stateScalar(m, raw, k, list[0].val)
	}
	for _, f := range slices.Sorted(maps.Keys(bools)) {
		m[f] = true
	}
}

// stateScalar states one decoded attribute value x under k by its kind: numbers (a double that is not finite as
// out-of-range), texts cleaned at their cap, an identifier never cut, the waveform as a length and a digest.
func stateScalar(m, raw map[string]any, k string, x any) {
	switch k {
	case "duration":
		finiteInto(m, k, x)
		m["duration_unit"] = "unestablished"
	case "video_start_ts":
		finiteInto(m, k, x)
	case "title", "performer", "sticker_alt":
		b, _ := x.([]byte)
		cleanInto(m, raw, k, "media."+k, b, maxName)
	case "video_codec", "stickerset_emoticon":
		b, _ := x.([]byte)
		cleanInto(m, raw, k, "media."+k, b, maxTiny)
	case "stickerset_short_name":
		b, _ := x.([]byte)
		identInto(m, raw, k, "media."+k, b, maxIdent)
	case "waveform":
		b, _ := x.([]byte)
		m["waveform_len"], m["waveform_sha256"] = int64(len(b)), sha256Hex(b) // the content never (D11)
	case "mask_coords":
		coords, _ := x.([]any)
		mc := map[string]any{}
		for j, f := range []string{"n", "x", "y", "zoom"} {
			if c := coords[j]; c != nil {
				finiteInto(mc, f, c)
			}
		}
		m[k] = mc
	default:
		m[k] = x
	}
}

// scalar is the value read at path as a comparable Go value: int64, float64, []byte, bool.
func scalar(v view, path string) (any, bool) {
	n, ok := v.Named(path)
	if !ok {
		return nil, false
	}
	switch n.Kind {
	case tl.KDouble:
		return n.Float, true
	case tl.KString, tl.KBytes:
		return n.Bytes, true
	case tl.KBool, tl.KTrue:
		return n.Bool, true
	}
	return v.Int(path)
}

// identInto states the identifier b at m[key], never cut: over limit it is absent with m[key+"_state"] too-long, _len
// and _sha256 (ident); a text the cleaning altered is named flag in raw.text_flags.
func identInto(m, raw map[string]any, key, flag string, b []byte, limit int) {
	if s, ok := ident(m, key, b, limit); ok && s != "" {
		m[key] = s
		if s != string(b) {
			markText(raw, flag)
		}
	}
}

// noNameState is the state of a stored name that cleaned to no text (T5a).
func noNameState(stored []byte) string {
	if len(stored) == 0 {
		return nameEmpty
	}
	return nameUnprintable
}

// distinctBytes is the values without repeats, in first-occurrence order (T5b).
func distinctBytes(values [][]byte) [][]byte {
	seen := make(map[string]struct{}, len(values))
	var out [][]byte
	for _, b := range values {
		if _, ok := seen[string(b)]; ok {
			continue
		}
		seen[string(b)] = struct{}{}
		out = append(out, b)
	}
	return out
}

// ownKey is the raw key of an attribute's boolean flag: the audio attribute's voice and the video attribute's
// round_message keep their own keys beside the wrapper's voice and round (T5c); the others keep their name.
func ownKey(label, flag string) string {
	switch {
	case label == "audio" && flag == "voice":
		return "audio_voice"
	case label == "video" && flag == "round_message":
		return "video_round_message"
	}
	return flag
}
