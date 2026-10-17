package examine

import "testing"

// TestCleanTextEscapesInsteadOfRepairing (F76): invalid UTF-8 and NUL are shown as escapes, never replaced
// by U+FFFD; valid text, controls included, is untouched by cleanText.
func TestCleanTextEscapesInsteadOfRepairing(t *testing.T) {
	bsl := string(rune(92))
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"bad " + string([]byte{0xff, 0xfe}) + " end", "bad " + bsl + "xff" + bsl + "xfe end"},
		{"nul " + string([]byte{0}) + " end", "nul " + bsl + "x00 end"},
		{"e" + string(rune(0xe9)) + string([]byte{0xc3}), "e" + string(rune(0xe9)) + bsl + "xc3"}, // a truncated sequence
		{"tab" + string(rune(9)) + "ok", "tab" + string(rune(9)) + "ok"},
	} {
		if got := cleanText(c.in, 256); got != c.want {
			t.Errorf("cleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := escapeText("a"+string(rune(0x1b))+"b"+string(rune(0x85)), true); got != "a"+bsl+"x1bb"+bsl+"u0085" {
		t.Errorf("escapeText with controls = %q", got)
	}
}
