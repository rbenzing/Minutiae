package evidence

import (
	"strings"
	"testing"
	"unicode"
	"unsafe"
)

// cjkRunes builds text from code points so the source holds no CJK character.
func cjkRunes(rs ...rune) string { return string(rs) }

// TestNormalizeSegmentsCJKLetters: every Han, Hiragana and Katakana letter is its own token, one
// space between two of them, after a combining mark that follows a letter (pipeline step 8, M7).
func TestNormalizeSegmentsCJKLetters(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"mixed run", cjkRunes(0x65e5, 0x672c, 0x8a9e, 0x306e, 0x30c6, 0x30ad, 0x30b9, 0x30c8), cjkRunes(0x65e5, ' ', 0x672c, ' ', 0x8a9e, ' ', 0x306e, ' ', 0x30c6, ' ', 0x30ad, ' ', 0x30b9, ' ', 0x30c8)},
		{"latin after", cjkRunes(0x6771, 0x4eac) + "tower", cjkRunes(0x6771, ' ', 0x4eac) + "tower"},
		{"latin before", "tower" + cjkRunes(0x6771, 0x4eac), "tower" + cjkRunes(0x6771, ' ', 0x4eac)},
		{"existing space", cjkRunes(0x65e5, 0x672c, ' ', 0x8a9e), cjkRunes(0x65e5, ' ', 0x672c, ' ', 0x8a9e)},
		{"ideographic space", cjkRunes(0x65e5, 0x3000, 0x672c), cjkRunes(0x65e5, ' ', 0x672c)},
		{"hangul unchanged", cjkRunes(0xd55c, 0xad6d, 0xc5b4), cjkRunes(0xd55c, 0xad6d, 0xc5b4)},
		{"halfwidth katakana composes", cjkRunes(0xff76, 0xff9e, 0xff77, 0xff9e), cjkRunes(0x30ac, ' ', 0x30ae)},
		{"squared katakana expands", cjkRunes(0x3300), cjkRunes(0x30a2, ' ', 0x30d1, ' ', 0x30fc, ' ', 0x30c8)},
		{"combining mark stays with its letter", cjkRunes(0x30a2, 0x309a, 0x30a4), cjkRunes(0x30a2, 0x309a, ' ', 0x30a4)},
		{"iteration mark", cjkRunes(0x4eba, 0x3005), cjkRunes(0x4eba, ' ', 0x3005)},
		{"ideographic zero", cjkRunes(0x3007, 0x3007), cjkRunes(0x3007, ' ', 0x3007)},
		{"radicals unchanged", cjkRunes(0x2e80, 0x2e80), cjkRunes(0x2e80, 0x2e80)},
		{"latin unchanged", "ab", "ab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeText(c.in); got != c.want {
				t.Errorf("NormalizeText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSegmentUnigramsIsAFreeNoOpWithoutCJK (M17a): text without a unigram rune comes back as the
// same string, with no allocation.
func TestSegmentUnigramsIsAFreeNoOpWithoutCJK(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("plain ascii ", 50),
		"caf" + cjkRunes(0xe9) + " Stra" + cjkRunes(0xdf) + "e " + cjkRunes(0x130) + "stanbul",
		cjkRunes(0xd55c, 0xad6d, 0xc5b4, ' ', 0xd14d, 0xc2a4, 0xd2b8),
	} {
		var out string
		if n := testing.AllocsPerRun(200, func() { out = segmentUnigrams(s) }); n != 0 {
			t.Errorf("segmentUnigrams(%.20q) allocates %v times", s, n)
		}
		if unsafe.StringData(out) != unsafe.StringData(s) {
			t.Errorf("segmentUnigrams(%.20q) did not return its input", s)
		}
	}
}

func TestFTSPipelineVersionIs4(t *testing.T) {
	if FTSPipelineVersion != 4 || !strings.HasPrefix(FTSNormVersion(), "fts4/") {
		t.Errorf("FTSPipelineVersion = %d, FTSNormVersion() = %q, want pipeline 4", FTSPipelineVersion, FTSNormVersion())
	}
}

func TestIsFTSUnigram(t *testing.T) {
	for _, r := range []rune{0x65e5, 0x3042, 0x30a2, 0x30fc, 0x3005, 0x3007} {
		if !IsFTSUnigram(r) {
			t.Errorf("IsFTSUnigram(U+%04X) = false", r)
		}
	}
	for _, r := range []rune{0xac00, 0x0e01, 0x2e80, 0x3000, 0x309a, 'a'} {
		if IsFTSUnigram(r) {
			t.Errorf("IsFTSUnigram(U+%04X) = true", r)
		}
	}
}

// TestIsFTSUnigramEarlyOutIsExact (M24): the cheap early-out below U+3005 hides no member of the
// set defined by the full formula.
func TestIsFTSUnigramEarlyOutIsExact(t *testing.T) {
	full := func(r rune) bool {
		return (unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) && (unicode.IsLetter(r) || unicode.IsNumber(r))) || r == 0x30fc
	}
	for r := rune(0); r < 0x3005; r++ {
		if full(r) {
			t.Errorf("U+%04X is a unigram by the full formula, but the early-out excludes it", r)
		}
	}
	if !IsFTSUnigram(0x3005) {
		t.Error("U+3005 must be a unigram")
	}
}
