package evidence

import (
	"strings"
	"testing"
)

// pinnedNormDigest is the digest of NormalizeText and FTSFoldDiacritics over the probe corpus. It
// changes only when the behaviour of the normalization changes: then bump FTSPipelineVersion, revisit
// the tests that pin the normalization and re-pin this constant (R63).
const pinnedNormDigest = "8c99c9c2d190"

// TestNormDigestIsPinned: a patched or replaced x/text with the same version string changes what
// NormalizeText returns for some probe, so the digest, and with it FTSNormVersion, changes (R63).
func TestNormDigestIsPinned(t *testing.T) {
	if ftsNormDigest != pinnedNormDigest {
		t.Fatalf("the normalization digest is %q, pinned %q: the behaviour of NormalizeText or FTSFoldDiacritics changed "+
			"(bump FTSPipelineVersion, revisit the normalization tests, re-pin)", ftsNormDigest, pinnedNormDigest)
	}
	if v := FTSNormVersion(); !strings.Contains(v, "/norm-"+pinnedNormDigest+"/") {
		t.Errorf("FTSNormVersion() = %q does not carry the digest %q", v, pinnedNormDigest)
	}
}

// TestNormDigestSeesEachFunction: the digest depends on NormalizeText and on FTSFoldDiacritics, and
// on the probe corpus being fed to both.
func TestNormDigestSeesEachFunction(t *testing.T) {
	same := computeNormDigest(NormalizeText, FTSFoldDiacritics)
	if same != ftsNormDigest {
		t.Fatalf("computeNormDigest is not deterministic: %q vs %q", same, ftsNormDigest)
	}
	patchedNorm := func(s string) string { return strings.ToUpper(NormalizeText(s)) }
	if computeNormDigest(patchedNorm, FTSFoldDiacritics) == same {
		t.Error("a different NormalizeText gives the same digest")
	}
	patchedFold := func(s string) string { return FTSFoldDiacritics(s) + "x" }
	if computeNormDigest(NormalizeText, patchedFold) == same {
		t.Error("a different FTSFoldDiacritics gives the same digest")
	}
	// one rune of behaviour is enough: the variation selector kept instead of dropped
	keepVS := func(s string) string { return strings.ReplaceAll(NormalizeText(s), "a", "b") }
	if computeNormDigest(keepVS, FTSFoldDiacritics) == same {
		t.Error("a one-letter change goes unseen")
	}
}

// TestNormProbeCorpusCoversTheRulesOfThePipeline: the corpus holds an input for each rule the
// pipeline has, so a change to any of them changes the digest.
func TestNormProbeCorpusCoversTheRulesOfThePipeline(t *testing.T) {
	corpus := normProbeCorpus()
	if len(corpus) < 40 {
		t.Fatalf("the probe corpus holds %d strings", len(corpus))
	}
	seen := map[string]bool{}
	for _, p := range corpus {
		if seen[p] {
			t.Errorf("duplicate probe %q", p)
		}
		seen[p] = true
	}
	for _, want := range []struct {
		name  string
		match func(string) bool
	}{
		{"zero width space", func(p string) bool { return strings.ContainsRune(p, 0x200b) }},
		{"zero width joiner", func(p string) bool { return strings.ContainsRune(p, 0x200d) }},
		{"word joiner", func(p string) bool { return strings.ContainsRune(p, 0x2060) }},
		{"variation selector 16", func(p string) bool { return strings.ContainsRune(p, 0xfe0f) }},
		{"supplementary variation selector", func(p string) bool { return strings.ContainsRune(p, 0xe0100) }},
		{"dotless i", func(p string) bool { return strings.ContainsRune(p, 0x131) }},
		{"i with a dot above", func(p string) bool { return strings.Contains(p, "i"+string(rune(0x307))) }},
		{"cherokee capital", func(p string) bool { return strings.ContainsRune(p, 0x13a0) }},
		{"cherokee small", func(p string) bool { return strings.ContainsRune(p, 0xab70) }},
		{"combining mark", func(p string) bool { return strings.ContainsRune(p, 0x301) }},
		{"CJK", func(p string) bool { return strings.ContainsRune(p, 0x65e5) }},
		{"emoji", func(p string) bool { return strings.ContainsRune(p, 0x1f600) }},
		{"CRLF", func(p string) bool { return strings.Contains(p, "\r\n") }},
		{"NUL", func(p string) bool { return strings.ContainsRune(p, 0) }},
		{"invalid UTF-8", func(p string) bool { return strings.Contains(p, "\xff") }},
		{"sharp s", func(p string) bool { return strings.ContainsRune(p, 0xdf) }},
		{"ligature", func(p string) bool { return strings.ContainsRune(p, 0xfb03) }},
		{"fullwidth", func(p string) bool { return strings.ContainsRune(p, 0xff21) }},
		{"final sigma", func(p string) bool { return strings.ContainsRune(p, 0x3c2) }},
		{"supplementary cased letter", func(p string) bool { return strings.ContainsRune(p, 0x10400) }},
	} {
		found := false
		for _, p := range corpus {
			if want.match(p) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the probe corpus has no %s", want.name)
		}
	}
}
