package safari

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of 4F-R15, in bytes.
const (
	maxURL      = 2 << 20   // a URL in the payload
	maxText     = 256 << 10 // a title
	maxBodyPart = 64 << 10  // the URL or the title in Record.Body
)

// markText lists field in raw.text_flags (sorted): its text was altered (invalid UTF-8 or NUL became
// U+FFFD, or it was cut). The original bytes stay in the artifact; nothing is altered without a flag.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	if slices.Contains(cur, any(field)) {
		return
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// cleanText is common.CleanText for a text field: invalid UTF-8 and NUL become U+FFFD (the original is kept
// under raw.<field>_raw_b64, or _raw_sha256 and _raw_len above 64 KiB), a text above limit is cut on a rune
// boundary (raw.<field>_total_bytes, and raw.url_truncated for the URL), and an altered field is listed in
// raw.text_flags.
func cleanText(raw map[string]any, field string, b []byte, limit int) string {
	c := common.CleanText(b, limit)
	if len(c.Flags) > 0 {
		markText(raw, field)
	}
	if c.RawB64 != "" {
		raw[field+"_raw_b64"] = c.RawB64
	}
	if c.RawSHA256 != "" {
		raw[field+"_raw_sha256"] = c.RawSHA256
		raw[field+"_raw_len"] = int64(c.RawLen)
	}
	if c.Truncated {
		if field == "url" {
			raw["url_truncated"] = true
		}
		raw[field+"_total_bytes"] = int64(c.TotalBytes)
	}
	return c.Text
}

// cutPart cuts s to at most limit bytes on a rune boundary.
func cutPart(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// bodyOf is the searchable text: the URL, then the title on a line of its own, each part cut at 64 KiB
// (raw.body_truncated and raw.body_total_bytes, the uncut length, say so).
func bodyOf(raw map[string]any, url, title string) string {
	u, t := cutPart(url, maxBodyPart), cutPart(title, maxBodyPart)
	body := u
	if t != "" {
		body += "\n" + t
	}
	if u != url || t != title {
		total := len(url)
		if title != "" {
			total += 1 + len(title)
		}
		raw["body_truncated"] = true
		raw["body_total_bytes"] = int64(total)
	}
	return body
}
