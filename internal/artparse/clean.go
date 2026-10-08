package artparse

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cleanAuditText makes device- or parser-supplied text safe for the audit log: valid UTF-8, no NUL
// (each invalid byte and NUL becomes "?"), at most limit bytes, cut on a rune boundary and marked with
// "..." when there is room for it.
func cleanAuditText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		var b strings.Builder
		for _, r := range s {
			if r == utf8.RuneError || r == 0 {
				r = '?'
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return clipBytes(s, limit)
	}
	return clipBytes(s, limit-3) + "..."
}

// panicText renders a recovered panic value without trusting its methods: an Error or String method
// that itself panics is reported by type name.
func panicText(v any) (text string) {
	defer func() {
		if recover() != nil {
			text = fmt.Sprintf("%T (its Error or String method panicked)", v)
		}
	}()
	switch x := v.(type) {
	case nil:
		return "nil panic value"
	case string:
		return x
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(x)
	}
	return fmt.Sprintf("%T", v)
}
