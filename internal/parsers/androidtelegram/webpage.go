package androidtelegram

import "github.com/rbenzing/minutiae/internal/decode/tl"

// webPageSummary states what the web page of a TL_messageMediaWebPage holds beside its url and display_url (plan 4I4b
// "Raw shapes", Web page): its own type under webpage_type (raw.media.type is the media label), the texts, the embed
// size, the duration (no unit: the table gives none), the photo id, the link preview's document (its id, mime type and
// file name under document_file_name: a preview's document is never an attachment, spec 8.1), the attribute labels and
// the class of the cached page the decode stopped at.
func webPageSummary(v view, tab tl.Table, m, raw map[string]any) {
	const w = "media.webpage."
	for _, kk := range [][2]string{
		{"type", "webpage_type"},
		{"site_name", "site_name"},
		{"title", "title"},
		{"description", "description"},
		{"author", "author"},
		{"embed_url", "embed_url"},
		{"embed_type", "embed_type"},
	} {
		if b, ok := v.Str(w + kk[0]); ok {
			cleanInto(m, raw, kk[1], "media."+kk[1], b, maxShort)
		}
	}
	for _, k := range []string{"embed_width", "embed_height", "duration"} {
		if x, ok := v.Int(w + k); ok {
			m[k] = x
		}
	}
	if x, ok := v.Int(w + "photo.id"); ok {
		m["photo_id"] = x
	}
	if v.Has(w + "document") {
		if x, ok := v.Int(w + "document.id"); ok {
			m["document_id"] = x
		}
		if b, ok := v.Str(w + "document.mime_type"); ok {
			cleanInto(m, raw, "document_mime_type", "media.document_mime_type", b, maxShort)
		}
		documentName(v, tab, m, raw, w+"document", "document_file_name")
	}
	labelList(v, tab, m, "webpage_attributes", w+"attributes", "TL_webPageAttribute", maxLabels)
	if s := v.stopAt(w + "cached_page"); s != nil && s.ID != 0 {
		if c, ok := tab[tl.Key{Type: s.Type, ID: s.ID}]; ok {
			m["cached_page_class"] = c.Name
		}
	}
}
