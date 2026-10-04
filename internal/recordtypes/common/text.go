// Package common holds what every record type package shares: text hygiene (the
// one place invalid UTF-8 and NUL are handled), the shape-only address
// classifier, and the payload schema checker the per-type validators are built
// from. It is a pure package: it imports no other Minutiae package and has no
// package-level state.
package common

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Flags CleanText sets.
const (
	FlagInvalidUTF8 = "invalid_utf8"
	FlagNUL         = "nul"
	FlagTruncated   = "truncated"
)

// MaxLimit is the largest limit CleanText and Summarize honour; a larger one is
// clamped to it (1 GiB, above any text a record holds), so no arithmetic on a limit
// can overflow.
const MaxLimit = 1 << 30

func clampLimit(limit int) int { return max(0, min(limit, MaxLimit)) }

// MaxRawInline is the largest original (bytes) kept inline as base64; a larger
// one is recorded by SHA-256 and length instead.
const MaxRawInline = 64 << 10

// Cleaned is the result of CleanText.
type Cleaned struct {
	Text       string   // valid UTF-8, no NUL; at most limit bytes, cut on a rune boundary
	Flags      []string // subset of "invalid_utf8", "nul", "truncated"
	RawB64     string   // the original bytes (base64) when a flag other than truncated applies and len(raw) <= 64 KiB
	RawSHA256  string   // set instead of RawB64 when the original is larger (hex)
	RawLen     int      // len(raw)
	Truncated  bool
	TotalBytes int // len(raw) of what the cut text came from (set when Truncated)
}

// CleanText turns raw device text into something the records package accepts:
// every invalid UTF-8 byte and every NUL becomes U+FFFD and the flags say so;
// the original is kept (base64, or its SHA-256 and length when it exceeds 64
// KiB) whenever a flag other than truncated applies; a result longer than limit
// bytes is cut on a rune boundary with the flag truncated, Truncated and
// TotalBytes. Nothing changes without a flag. A limit above MaxLimit is treated as
// MaxLimit (so arithmetic on a limit cannot overflow), one of 0 or less as 0:
// the text is empty and, for a non-empty input, Truncated. A U+FFFD that was
// already valid in the input is kept and raises no flag.
func CleanText(raw []byte, limit int) Cleaned {
	limit = clampLimit(limit)
	c := Cleaned{RawLen: len(raw)}
	var invalid, nul bool
	var b strings.Builder
	b.Grow(min(len(raw), limit+utf8.UTFMax))
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			invalid = true
			b.WriteRune(utf8.RuneError)
		case r == 0:
			nul = true
			b.WriteRune(utf8.RuneError)
		default:
			b.Write(raw[i : i+size])
		}
		i += size
		if b.Len() > limit+utf8.UTFMax {
			// the rest cannot be part of the text; it only has to be scanned for flags
			for j := i; j < len(raw); {
				r2, s2 := utf8.DecodeRune(raw[j:])
				if r2 == utf8.RuneError && s2 == 1 {
					invalid = true
				} else if r2 == 0 {
					nul = true
				}
				j += s2
			}
			break
		}
	}
	text := b.String()
	if len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
		c.Truncated = true
		c.TotalBytes = len(raw)
	}
	c.Text = text
	if invalid {
		c.Flags = append(c.Flags, FlagInvalidUTF8)
	}
	if nul {
		c.Flags = append(c.Flags, FlagNUL)
	}
	if c.Truncated {
		c.Flags = append(c.Flags, FlagTruncated)
	}
	if invalid || nul {
		if len(raw) <= MaxRawInline {
			c.RawB64 = base64.StdEncoding.EncodeToString(raw)
		} else {
			sum := sha256.Sum256(raw)
			c.RawSHA256 = fmt.Sprintf("%x", sum)
		}
	}
	return c
}

// PayloadFields returns the payload fields the cleaning calls for, in the value
// types the records package canonicalizes (string, bool, int64, []any and
// map[string]any): body_flags, body_truncated and body_total_bytes, and
// body_raw_b64 or body_raw_sha256 with body_raw_len. A field that does not
// apply is absent (never false, 0 or ""), so the result is empty for clean text.
func (c Cleaned) PayloadFields() map[string]any {
	m := map[string]any{}
	if len(c.Flags) > 0 {
		flags := make([]any, len(c.Flags))
		for i, f := range c.Flags {
			flags[i] = f
		}
		m["body_flags"] = flags
	}
	if c.Truncated {
		m["body_truncated"] = true
		m["body_total_bytes"] = int64(c.TotalBytes)
	}
	switch {
	case c.RawB64 != "":
		m["body_raw_b64"] = c.RawB64
	case c.RawSHA256 != "":
		m["body_raw_sha256"] = c.RawSHA256
		m["body_raw_len"] = int64(c.RawLen)
	}
	return m
}

// Summarize builds the one-line summary of s, at most limit BYTES long: every run
// of whitespace and control characters (NUL included) becomes one space, the ends
// are trimmed, invalid UTF-8 becomes U+FFFD, and every format character (category
// Cf: bidi overrides, embeddings and isolates, directional marks, zero-width
// characters, the soft hyphen, the byte order mark) is shown as its code point,
// "<U+200B>", so a summary can neither reorder nor hide what it shows and "Bank"
// and "Ba<ZWSP>nk" differ (the stored text keeps the characters themselves). A
// result longer than limit is cut on a character boundary (an escape is never cut
// in half) and ends in "..."; the whole result stays within limit bytes (a limit
// under 3 gets only as many dots as fit, one of 0 or less gets ""). Text that
// already fits is returned as collapsed.
func Summarize(s string, limit int) string { return summarize(s, limit, false) }

// SummarizeChars is Summarize with the limit counted in characters (runes of the
// result: an escape counts as the characters it shows) instead of bytes, so text in
// any script keeps the same number of characters. The result is at most 4 bytes per
// character plus the dots.
func SummarizeChars(s string, limit int) string { return summarize(s, limit, true) }

func summarize(s string, limit int, chars bool) string {
	if limit = clampLimit(limit); limit <= 0 {
		return ""
	}
	var b strings.Builder
	cost := 0   // length of b in the unit of the limit
	cutLen := 0 // length of the longest prefix of b whose cost leaves room for the dots
	add := func(unit string) bool {
		b.WriteString(unit)
		if chars {
			cost += utf8.RuneCountInString(unit)
		} else {
			cost += len(unit)
		}
		if cost <= limit-3 {
			cutLen = b.Len()
		}
		return cost > limit
	}
	pendingSpace := false
	over := false
	for _, r := range s { // an invalid byte ranges as U+FFFD
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			pendingSpace = b.Len() > 0
			continue
		}
		if pendingSpace {
			pendingSpace = false
			if add(" ") {
				over = true
				break
			}
		}
		unit := string(r)
		if unicode.Is(unicode.Cf, r) {
			unit = escapeRune(r) // shown, never silently dropped: a bidi override or zero-width character must not hide
		}
		if add(unit) {
			over = true
			break // longer than limit for certain; the rest cannot matter
		}
	}
	if !over {
		return b.String()
	}
	if limit < 3 {
		return "..."[:limit]
	}
	return strings.TrimRight(b.String()[:cutLen], " ") + "..."
}

// escapeRune renders a format character as <U+XXXX> (at least four upper-case hex digits).
func escapeRune(r rune) string {
	h := strings.ToUpper(strconv.FormatInt(int64(r), 16))
	return "<U+" + strings.Repeat("0", max(0, 4-len(h))) + h + ">"
}
