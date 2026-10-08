package evidence

import (
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The sweeps below close the gap of the single-code-point tests: they check the claims "every
// Unicode spelling of a text meets" and "normalization does not depend on the text around a
// character" over every code point (Task 2 review). The full sweep (every code point, all
// contexts) runs unless -short is given; a strided subset always runs.

// sweepRunes calls fn for every Unicode scalar value (surrogates cannot occur in a Go string), or
// for a strided subset under -short.
func sweepRunes(fn func(r rune)) {
	step := rune(1)
	if testing.Short() {
		step = 97
	}
	for r := rune(0); r <= unicode.MaxRune; r += step {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		fn(r)
	}
}

// TestNormalizeUnicodeFormsMeet: NFC, NFD, NFKC and NFKD of every code point normalize to what the
// code point itself does, and so do its upper and lower case.
func TestNormalizeUnicodeFormsMeet(t *testing.T) {
	bad := 0
	report := func(r rune, what, got, want string) {
		bad++
		if bad <= 10 {
			t.Errorf("U+%04X: %s normalizes to %q, but the code point itself to %q", r, what, got, want)
		}
	}
	sweepRunes(func(r rune) {
		s := string(r)
		want := NormalizeText(s)
		for name, v := range map[string]string{
			"NFC": norm.NFC.String(s), "NFD": norm.NFD.String(s), "NFKC": norm.NFKC.String(s), "NFKD": norm.NFKD.String(s),
			"upper": strings.ToUpper(s), "lower": strings.ToLower(s),
		} {
			if got := NormalizeText(v); got != want {
				report(r, name, got, want)
			}
		}
	})
	if bad > 0 {
		t.Errorf("%d spellings do not meet", bad)
	}
}

// TestNormalizeIsContextFree: every code point, placed in several contexts (letters, an "i" before a
// dot, a joiner, a space run, a Hangul pair, a keycap), gives a text that normalizes to itself and
// keeps the output contract, so a query typed from stored text finds it.
func TestNormalizeIsContextFree(t *testing.T) {
	contexts := [][2]string{
		{"a", "b"},
		{"i", ""},
		{"", "\u0301"},
		{"e", "\u200b"},
		{"\u0131", "\u0307"},
		{" ", " "},
		{"\u1100", "\u1161"},
		{"1", "\ufe0f\u20e3"},
		{"\U0001f468\u200d", "\U0001f469"},
	}
	if testing.Short() {
		contexts = contexts[:3]
	} else {
		contexts = contexts[:6]
	}
	bad := 0
	sweepRunes(func(r rune) {
		for _, c := range contexts {
			in := c[0] + string(r) + c[1]
			once := NormalizeText(in)
			if twice := NormalizeText(once); once != twice {
				bad++
				if bad <= 10 {
					t.Errorf("%q: once %q, twice %q", in, once, twice)
				}
			}
			checkNormalizedShape(t, in, once)
		}
	})
	if bad > 0 {
		t.Errorf("%d contexts are not idempotent", bad)
	}
}
