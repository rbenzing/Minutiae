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

// The sector-to-byte factor is the volume section's bytes_per_sector, never a
// constant 512.
func TestEWFAcquisitionGapsUseBytesPerSector(t *testing.T) {
	for _, bps := range []int{1024, 2048, 4096} {
		media := pattern(64 * bps)
		files := ewftest.Build(ewftest.Options{
			BytesPerSector: bps, SectorsPerChunk: 4,
			ErrorRanges: [][2]uint32{{1, 1}, {62, 10}}, // the second is clipped at the media end
		}, media)
		g := mustOpen(t, files).AcquisitionGaps()
		want := []ewf.AcquisitionGap{
			{Offset: int64(bps), Length: int64(bps)},
			{Offset: int64(62 * bps), Length: int64(2 * bps)},
		}
		if !slices.Equal(g.Kept, want) || g.Outside != 1 || g.State != ewf.GapsDeclared {
			t.Errorf("bps %d: %+v; want kept %v, 1 outside", bps, g, want)
		}
	}
}

func TestEWFAcquisitionGapsCountsEmptyRanges(t *testing.T) {
	r := mustOpen(t, ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{4, 0}, {8, 2}, {9, 0}}}, pattern(64*512)))
	g := r.AcquisitionGaps()
	if g.Empty != 2 || g.Ranges != 3 || len(g.Kept) != 1 || g.State != ewf.GapsDeclared {
		t.Fatalf("AcquisitionGaps = %+v; want 2 empty ranges counted", g)
	}
}

func TestEWFAcquisitionGapsStates(t *testing.T) {
	m := pattern(64 * 512)
	damage := func(off int) [][]byte {
		files := ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, m)
		e := section(t, files[0], "error2")
		files[0][int(e.Offset)+76+off] ^= 0xFF
		return files
	}
	for _, tc := range []struct {
		name  string
		files [][]byte
		want  ewf.GapState
	}{
		{"no error2 section", ewftest.Build(ewftest.Options{}, m), ewf.GapsAbsent},
		{"empty error2 section", ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{}}, m), ewf.GapsEmpty},
		{"declared", ewftest.Build(ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, m), ewf.GapsDeclared},
		{"damaged header", damage(3), ewf.GapsDamaged},
		{"damaged entries", damage(520), ewf.GapsDamaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mustOpen(t, tc.files).AcquisitionGaps()
			if g.State != tc.want {
				t.Fatalf("State = %v, want %v (%+v)", g.State, tc.want, g)
			}
			if tc.want == ewf.GapsDamaged && len(g.Kept) != 0 {
				t.Fatalf("damaged ranges were used: %+v", g.Kept)
			}
		})
	}
}
