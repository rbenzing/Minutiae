package records

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// TestFoldIndexSegmentsLikeNormalizeText (M11): the fold index of a text holds exactly what the
// index holds, the space between two CJK letters included.
func TestFoldIndexSegmentsLikeNormalizeText(t *testing.T) {
	for _, s := range []string{
		string([]rune{0x65e5, 0x672c, 0x8a9e}),
		string([]rune{0x6771, 0x4eac}) + "tower",
		string([]rune{0x30a2, 0x309a, 0x30a4}),
		string([]rune{0x3300}),
		"x " + string([]rune{0x4eba, 0x3005, ' ', 0x65e5}) + " y",
	} {
		if got, want := buildFoldIndex(s).cat, evidence.FTSFoldDiacritics(evidence.NormalizeText(s)); got != want {
			t.Errorf("buildFoldIndex(%q).cat = %q, want %q", s, got, want)
		}
	}
}
