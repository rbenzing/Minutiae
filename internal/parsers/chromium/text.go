package chromium

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of 4F-R15.
const (
	maxURL      = 2 << 20   // bytes of a URL kept in the payload
	maxText     = 256 << 10 // bytes of a title, term or other free text kept
	maxBodyPart = 64 << 10  // bytes of one URL or title in Record.Body
	maxPath     = 4096      // bytes of a local path (the contract cap)
	maxExact    = 1 << 53   // every integer up to this magnitude is exactly a float64
)

// markText lists field in raw.text_flags (sorted): the text of that field was altered (invalid UTF-8 or NUL became
// U+FFFD, or it was cut). The original bytes stay in the artifact.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// capped is common.CleanText for a field that has no flag slot of its own in the typed payload (4F-R15): an altered
// text is listed in raw.text_flags, the original bytes in raw.<field>_raw_b64 (or _raw_sha256 and _raw_len), a cut in
// raw.<field>_total_bytes (and raw.url_truncated for the url).
func capped(raw map[string]any, field string, b []byte, limit int) string {
	return noteCleaned(raw, field, common.CleanText(b, limit))
}

// noteCleaned states in raw what cleaning field did (see capped) and returns the cleaned text. A caller that may
// drop the text (a chain entry past a cap) cleans first and notes only the entries it keeps.
func noteCleaned(raw map[string]any, field string, cl common.Cleaned) string {
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	if cl.RawB64 != "" {
		raw[field+"_raw_b64"] = cl.RawB64
	}
	if cl.RawSHA256 != "" {
		raw[field+"_raw_sha256"] = cl.RawSHA256
		raw[field+"_raw_len"] = int64(cl.RawLen)
	}
	if cl.Truncated {
		raw[field+"_total_bytes"] = int64(cl.TotalBytes)
		if field == "url" {
			raw["url_truncated"] = true
		}
	}
	return cl.Text
}

// cutText cuts s to at most limit bytes on a rune boundary; cut says whether anything was removed.
func cutText(s string, limit int) (out string, cut bool) {
	if len(s) <= limit {
		return s, false
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
