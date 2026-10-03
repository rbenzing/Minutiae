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
// \UNNNNNNNN escape. Newlines are kept so multi-line errors stay readable.
// Use it for error and warning text that may carry filesystem-supplied names;
// printable (image.go) quotes single names instead.
func escapeText(s string) string {
	clean := func(r rune) bool { return r == '\n' || unicode.IsPrint(r) }
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
