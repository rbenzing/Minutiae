package parsertest

import (
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/parse"
)

// The host's note rules (artparse maxNoteKey, maxNoteValue and cleanAuditText), repeated here so a
// parser sees the same notes under the harness as under the host; TestHarnessNotesFollowTheHostRules
// pins the observable behaviour.
const (
	maxNoteKey   = 64
	maxNoteValue = 1 << 10
)

// cleanNoteText makes text valid UTF-8 without NUL (each invalid byte and NUL becomes "?"), cuts it
// to limit bytes on a rune boundary and marks the cut with "..." when there is room.
func cleanNoteText(s string, limit int) string {
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
	return clipNote(s, limit-3) + "..."
}

func clipNote(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// noteCap is the number of distinct notes the host keeps for this input.
func noteCap(in *parse.Input) int {
	if in != nil && in.Limits.MaxNotes > 0 {
		return in.Limits.MaxNotes
	}
	return parse.DefaultLimits().MaxNotes
}
