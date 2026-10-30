package typedstream

import (
	"os"
	"testing"
)

// TestExtractTextOnAPublicSample reads a typedstream archive written by Apple's own NSArchiver and
// published, with the text it holds, in a general-purpose MIT-licensed format library (see
// testdata/PROVENANCE.md). It is an NSString archive, not an NSAttributedString one: the extractor
// reads the first NSString object, which is what both contain, but no real attributedBody blob
// exists in the repository (docs/limitations.md).
func TestExtractTextOnAPublicSample(t *testing.T) {
	b, err := os.ReadFile("testdata/public-sample.typedstream")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	// The text the source library's own test states for this file.
	const want = "abcdefgABCDEFG!?@[]/+*^&-_"
	mustText(t, b, want)
}
