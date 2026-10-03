package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// escapeText makes s safe to print on a terminal: every rune that is not
// printable (control characters, escape sequences, bidi and other format
// characters) and every invalid UTF-8 byte is replaced by a \xNN, \uNNNN or
// \UNNNNNNNN escape. Newlines are escaped too, so the result is always one
// line and filesystem- or container-supplied text can never forge a separate
// warning/status line. Use it for single-line warnings and notes that may
// carry such text; printable (image.go) quotes single names instead.
func escapeText(s string) string { return escapeKeeping(s, false) }

// escapeMultiline is escapeText but keeps newlines, so the multi-line error
// printed by Run stays readable. Use it only for that top-level error.
func escapeMultiline(s string) string { return escapeKeeping(s, true) }

func escapeKeeping(s string, keepNewline bool) string {
	clean := func(r rune) bool { return (keepNewline && r == '\n') || unicode.IsPrint(r) }
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !clean(r) }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case clean(r):
			b.WriteString(s[i : i+n])
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += n
	}
	return b.String()
}
