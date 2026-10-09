package ewf_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func TestEWFAcquisitionGapsAreMediaByteRanges(t *testing.T) {
	media := pattern(128 * 512) // 128 sectors
	files := ewftest.Build(ewftest.Options{
		// unsorted, touching ({8,8} and {12,8} overlap), one partly beyond the media
		ErrorRanges: [][2]uint32{{100, 2}, {8, 8}, {12, 8}, {120, 100}},
	}, media)
	r := mustOpen(t, files)
	g := r.AcquisitionGaps()
	want := []ewf.AcquisitionGap{
		{Offset: 8 * 512, Length: 12 * 512},
		{Offset: 100 * 512, Length: 2 * 512},
		{Offset: 120 * 512, Length: 8 * 512}, // clipped to the media
	}
	if !slices.Equal(g.Kept, want) || g.Ranges != 4 || g.Outside != 1 || g.Truncated {
		t.Fatalf("AcquisitionGaps = %+v; want kept %v, 4 ranges, 1 outside, not truncated", g, want)
	}
	// The bytes are untouched: the gap reads exactly as the builder stored it.
	if got := readAll(t, r); !slices.Equal(got, media) {
		t.Fatal("a gap changed the media bytes")
	}
}

func TestEWFAcquisitionGapsAbsent(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{}, pattern(64*512)))
	if g := r.AcquisitionGaps(); g.Ranges != 0 || len(g.Kept) != 0 || g.Truncated || g.Outside != 0 {
		t.Fatalf("AcquisitionGaps = %+v, want zero", g)
	}
}

func TestEWFAcquisitionGapsTruncatedWhenMoreThanKept(t *testing.T) {
	ranges := make([][2]uint32, ewf.MaxErrorRanges+5)
	for i := range ranges {
		ranges[i] = [2]uint32{0, 1} // the same sector again and again: cheap to build
	}
	r := mustOpen(t, ewftest.Build(ewftest.Options{ErrorRanges: ranges}, pattern(64*512)))
	g := r.AcquisitionGaps()
	if !g.Truncated || g.Ranges != uint64(ewf.MaxErrorRanges+5) || len(g.Kept) != 1 || g.Kept[0] != (ewf.AcquisitionGap{Offset: 0, Length: 512}) {
		t.Fatalf("Ranges %d, Truncated %v, Kept %v", g.Ranges, g.Truncated, g.Kept)
	}
}

func TestEWFAcquisitionGapsOverflowingRangeIsOutside(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{0xFFFFFFFF, 0xFFFFFFFF}}}, pattern(64*512)))
	g := r.AcquisitionGaps()
	if len(g.Kept) != 0 || g.Outside != 1 || g.Ranges != 1 {
		t.Fatalf("AcquisitionGaps = %+v", g)
	}
}
