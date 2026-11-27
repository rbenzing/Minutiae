package androidtelegram

import (
	"strings"
	"testing"
)

// Plan 4I4b "Raw shapes" (Size entry, Labels), ruling T4a: sizeKind names every PhotoSize and VideoSize class of the
// table (the nine kinds, never "" and never the unknown fallback), and classLabel names every DocumentAttribute,
// InputStickerSet and WebPageAttribute class (never "", and one label belongs to one class name apart from its layer
// and age twins). A capital run is one word: TL_inputStickerSetID is "id".
func TestSizeKindAndClassLabelCoverTheTable(t *testing.T) {
	tab := newTable()
	kinds := map[string]bool{}
	labelOwner := map[string]map[string]string{} // type -> label -> the class name without its suffix
	prefixes := map[string]string{
		"DocumentAttribute": "TL_documentAttribute",
		"InputStickerSet":   "TL_inputStickerSet",
		"WebPageAttribute":  "TL_webPageAttribute",
	}
	for _, c := range tab {
		switch c.Type {
		case "PhotoSize", "VideoSize":
			k := sizeKind(c.Name)
			if k == "" || k == "unknown" {
				t.Errorf("sizeKind(%s) = %q", c.Name, k)
			}
			kinds[k] = true
		case "DocumentAttribute", "InputStickerSet", "WebPageAttribute":
			l := classLabel(c.Name, prefixes[c.Type])
			if l == "" {
				t.Errorf("classLabel(%s) is empty", c.Name)
				continue
			}
			base, _, _ := strings.Cut(strings.TrimPrefix(c.Name, prefixes[c.Type]), "_")
			if labelOwner[c.Type] == nil {
				labelOwner[c.Type] = map[string]string{}
			}
			if prev, dup := labelOwner[c.Type][l]; dup && prev != base {
				t.Errorf("%s: label %q names both %s and %s", c.Type, l, prev, base)
			}
			labelOwner[c.Type][l] = base
		}
	}
	for _, k := range []string{"size", "cached", "stripped", "path", "progressive", "empty", "video", "emoji_markup", "sticker_markup"} {
		if !kinds[k] {
			t.Errorf("no PhotoSize or VideoSize class has the size kind %q", k)
		}
	}
	if len(kinds) != 9 {
		t.Errorf("%d size kinds, want the nine of the plan", len(kinds))
	}
	for _, c := range []struct{ class, prefix, want string }{
		{"TL_inputStickerSetID", "TL_inputStickerSet", "id"}, // T4a: a capital run is one word
		{"TL_inputStickerSetShortName", "TL_inputStickerSet", "short_name"},
		{"TL_inputStickerSetAnimatedEmoji", "TL_inputStickerSet", "animated_emoji"},
		{"TL_inputStickerSetDice_layer111", "TL_inputStickerSet", "dice"},
		{"TL_documentAttributeImageSize", "TL_documentAttribute", "image_size"},
		{"TL_documentAttributeHasStickers", "TL_documentAttribute", "has_stickers"},
		{"TL_documentAttributeVideo_layer159", "TL_documentAttribute", "video"},
		{"TL_webPageAttributeStickerSet", "TL_webPageAttribute", "sticker_set"},
		{"TL_documentAttributeVideo", "TL_webPageAttribute", ""}, // another prefix has no label
		{"TL_messageEntityTextUrl", "TL_messageEntity", "text_url"},
	} {
		if got := classLabel(c.class, c.prefix); got != c.want {
			t.Errorf("classLabel(%q, %q) = %q, want %q", c.class, c.prefix, got, c.want)
		}
	}
	// T4a: the 4I4 media labels are unchanged by the capital-run rule (no media class carries a capital run)
	for _, c := range tab {
		if c.Type != typeMedia {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(c.Name, "TL_messageMedia"), "MessageMedia")
		name, _, _ = strings.Cut(name, "_")
		for i := 1; i < len(name); i++ {
			if name[i] >= 'A' && name[i] <= 'Z' && name[i-1] >= 'A' && name[i-1] <= 'Z' {
				t.Errorf("media class %s has a capital run: its 4I4 label would change under the T4a rule", c.Name)
			}
		}
	}
}
