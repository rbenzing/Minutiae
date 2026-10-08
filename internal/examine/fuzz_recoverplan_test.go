package examine

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// FuzzCutAtFirstNonFree decodes the input as a 256-byte free bitmap followed by (offset, length) byte
// pairs and holds the planner to the bitmap model of TestCutAtFirstNonFreeMatchesBitmapModel.
func FuzzCutAtFirstNonFree(f *testing.F) {
	seed := make([]byte, 256)
	for i := 100; i < 150; i++ {
		seed[i] = 1
	}
	f.Add(append(slices.Clone(seed), 108, 80, 18, 5), false)
	f.Add(append(slices.Clone(seed), 8, 4, 108, 50), true)
	f.Add(append(slices.Clone(seed), 128, 10, 108, 40, 255, 255), false)
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, data []byte, unknown bool) {
		const space = 256
		bm := make([]bool, space)
		for i := 0; i < space && i < len(data); i++ {
			bm[i] = data[i]&1 == 1
		}
		var in []evidence.Run
		if len(data) > space {
			rest := data[space:]
			for i := 0; i+1 < len(rest) && len(in) < 16; i += 2 {
				in = append(in, evidence.Run{Offset: int64(rest[i]) - 8, Length: int64(rest[i+1])})
			}
		}
		checkCutAgainstBitmap(t, bm, in, unknown)
	})
}
