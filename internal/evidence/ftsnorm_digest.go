package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ftsNormDigest is a short digest of what NormalizeText and FTSFoldDiacritics return for the probe
// corpus, computed once at start-up. It is part of FTSNormVersion, so a patched or replaced module
// that keeps its version string but normalizes differently changes the index version (R63): the
// version names the behaviour, not only the names of the pieces.
var ftsNormDigest = computeNormDigest(NormalizeText, FTSFoldDiacritics)

// computeNormDigest hashes the output of norm and of fold (applied to the normalized text, as the
// snippets do) for every probe string. The first 12 hex digits are the digest.
func computeNormDigest(norm, fold func(string) string) string {
	h := sha256.New()
	for _, p := range normProbeCorpus() {
		n := norm(p)
		f := fold(n)
		fmt.Fprintf(h, "%d:%s|%d:%s\n", len(n), n, len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// runes builds a probe from code points (so the source holds no invisible character).
func runes(rs ...rune) string { return string(rs) }

// normProbeCorpus is the pinned corpus the digest is computed over: an input for each rule of
// NormalizeText (invalid UTF-8, NUL, controls and spaces, Cf and variation selectors, NFKC,
// case folding, the dotless and dotted i, Cherokee) and for the diacritic rule, plus ordinary text,
// CJK and emoji. Changing it changes the digest: do it only with FTSPipelineVersion.
func normProbeCorpus() []string {
	return []string{
		"Hello, World 123",
		"MiXeD CaSe wITh   spaces\tand\ttabs",
		"  leading and trailing  ",
		"line one\r\nline two\nline three\rline four",
		"nul" + runes(0) + "byte",
		"bad \xff\xfe bytes \xc3",
		"truncated " + "\xe2\x82",
		runes(0xdf, ' ', 0x1e9e, ' ', 0xfb03, ' ', 0xfb01),
		runes(0x130, ' ', 0x131, ' ', 'i', 0x307, ' ', 'I', 0x307, 0x307),
		runes(0x13a0, 0x13a1, 0x13a2, ' ', 0xab70, 0xab71, ' ', 0x13f0, 0x13f5, ' ', 0x13f8, 0x13fd),
		runes(0xff21, 0xff22, 0xff23, ' ', 0xff41, 0xff42, ' ', 0xff11, 0xff12),
		runes(0x2126, ' ', 0x212a, ' ', 0x212b, ' ', 0x1e9b, 0x323),
		runes(0x3a3, 0x3b1, 0x3c3, 0x3c2, ' ', 0x3a3, ' ', 0x3b0, ' ', 0x1f88),
		runes('f', 'o', 'o', 0x200b, 'b', 'a', 'r'),
		runes('f', 'o', 'o', 0x200c, 'b', 'a', 'r'),
		runes('f', 'o', 'o', 0x200d, 'b', 'a', 'r'),
		runes('f', 'o', 'o', 0x2060, 'b', 'a', 'r'),
		runes('f', 'o', 'o', 0xad, 'b', 'a', 'r'),
		runes(0xfeff, 'b', 'o', 'm'),
		runes(0x202e, 'r', 'l', 'o', 0x202c, ' ', 0x2066, 'l', 'r', 'i', 0x2069),
		runes(0x2764, ' ', 0x2764, 0xfe0f, ' ', 0x2764, 0xfe0e),
		runes('a', 0xe0100, 'b', 0xe01ef),
		runes('a', 0x85, 'b', 0x2028, 'c', 0x2029, 'd', 0xa0, 'e', 0x3000, 'f', 0x2003, 'g'),
		runes('e', 0x301, ' ', 'a', 0x300, 0x301, ' ', 'o', 0x308, ' ', 'n', 0x303, ' ', 'u', 0x30a),
		runes(0xe9, ' ', 0xe8, ' ', 0xfc, ' ', 0xf1, ' ', 0xe5, ' ', 0xe7, ' ', 0x1ea1, ' ', 0x219),
		runes(0x345, 'a', 0x345, ' ', 0x1ab0, 'x', 0x1dc0, ' ', 0x20d0, 'y', 0xfe20),
		runes(0xe01, 0xe31, 0xe07, ' ', 0xe44, 0xe17, 0xe22, ' ', 0x1780, 0x17b6, 0x1798),
		runes(0x627, 0x644, 0x639, 0x631, 0x628, 0x64a, 0x629, ' ', 0x64b, 0x64c, 0x64d),
		runes(0x65e5, 0x672c, 0x8a9e, ' ', 0x6f22, 0x5b57, 0x3042, 0x3044, 0x30a2, 0x30a4),
		runes(0xac00, 0xac01, ' ', 0x1100, 0x1161, 0x11a8, ' ', 0x3131, 0x314f),
		runes(0x1f600, ' ', 0x1f468, 0x200d, 0x1f469, 0x200d, 0x1f467, ' ', 0x1f1fa, 0x1f1f8),
		runes(0x10400, 0x10401, ' ', 0x10428, 0x10429, ' ', 0x1e900, 0x1e922),
		runes(0x1d400, 0x1d401, ' ', 0x1d7ce, 0x1d7cf),
		runes(0xe000, ' ', 0xf8ff, ' ', 0xfdd0, ' ', 0x10ffff),
		runes(0xd7ff, ' ', 0xe000),
		runes(0x2160, 0x2161, ' ', 0x2460, 0x2461, ' ', 0x00bd, ' ', 0x2122),
		runes(0xfb00, 0xfb06, ' ', 0x1c4, 0x1c5, 0x1c6),
		runes(0x1e9e, 0xdf, 'S', 'S', 's', 's'),
		runes(0x3c, 0x62, 0x3e, 0x26, 0x22, 0x27),
		"user@example.com http://host/path?q=1&r=2 +1 (555) 010-0100 c++ a-b a_b",
		"The quick brown fox jumps over the lazy dog",
		"password pass-word PassWord PASSWORD",
		"x",
		"",
	}
}
