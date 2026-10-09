package ewf_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// dropChunk2 returns a patch that forges table and table2 (checksums fixed
// by patchBoth) as if chunk 2 had never been stored: entries 2..8 point one
// chunk further and the last entry moves tail bytes into the last chunk. The
// entries still start at the payload start, strictly increase and lie inside
// the payload, so rules (a)-(d) hold. Both copies are forged identically, so
// the selection logic takes the "valid copies that agree" path.
func dropChunk2(tail uint32) func(p []byte) {
	return func(p []byte) {
		for i := 2; i < 9; i++ {
			setEntry(p, i, entryAt(p, i+1))
		}
		setEntry(p, 9, entryAt(p, 9)+tail)
	}
}

func TestEWFDroppedCompressedChunkIsFoundOnRead(t *testing.T) {
	const cs = 64 * 512
	media := pattern(10 * cs)
	files := ewftest.Build(ewftest.Options{ChunksPerTable: 10, Compress: ewftest.CompressAll}, media)
	// The last entry moves 1000 bytes into the last chunk. Rules (a)-(d) hold, so
	// the table opens; only decode can tell it forged.
	patchBoth(t, files[0], 0, dropChunk2(1000))
	r := mustOpen(t, files)
	// The chunk BEFORE the drop spans two stored streams: the second one is
	// recognized as a skipped chunk.
	p := make([]byte, cs)
	n, err := r.ReadAt(p, 1*cs)
	if n != 0 || !errors.Is(err, ewf.ErrChunkCorrupt) || !strings.Contains(err.Error(), "another zlib stream") {
		t.Fatalf("ReadAt(chunk 1) = %d, %v", n, err)
	}
	res, err := r.Verify(context.Background(), nil)
	if err != nil || res.BadChunk != 1 || res.Result() != "unverified" {
		t.Fatalf("Verify = %+v, %v; want unverified at chunk 1", res, err)
	}
}

func TestEWFZlibStartsAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"default level header and a body", []byte{0x78, 0x9c, 1, 2, 3, 4, 5, 6}, true},
		{"fast level header", []byte{0x78, 0x01, 1, 2, 3, 4, 5, 6}, true},
		{"best level header", []byte{0x78, 0xda, 1, 2, 3, 4, 5, 6}, true},
		{"padding of zeros", []byte{0, 0, 0, 0, 0, 0, 0, 0}, false},
		{"three padding bytes", []byte{0xDE, 0xAD, 0xBE}, false},
		{"header check bits wrong", []byte{0x78, 0x9d, 1, 2, 3, 4, 5, 6}, false},
		{"preset dictionary flag", []byte{0x78, 0xbb, 1, 2, 3, 4, 5, 6}, false},
		{"method not deflate", []byte{0x79, 0x9c, 1, 2, 3, 4, 5, 6}, false},
		{"too short to hold a stream", []byte{0x78, 0x9c, 1}, false},
		{"empty", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ewf.ZlibStartsAt(tc.b); got != tc.want {
				t.Fatalf("ZlibStartsAt(% x) = %v, want %v", tc.b, got, tc.want)
			}
		})
	}
}

// Every layout the builder can write, and so every legal acquirer layout it
// models, opens with no warning (the stored-chunk heuristic of decode never fires), and the media reads back exactly.
func TestEWFDecodeAcceptsEveryBuilderLayout(t *testing.T) {
	comps := map[string]ewftest.Compress{"none": ewftest.CompressNone, "all": ewftest.CompressAll, "mixed": ewftest.CompressMixed}
	bases := map[string]ewftest.BaseMode{"descriptor": ewftest.BaseSectorsDescriptor, "zero": ewftest.BaseZero}
	for cname, comp := range comps {
		for _, bps := range []int{512, 4096} {
			for _, spc := range []int{8, 64} {
				for bname, base := range bases {
					for _, noFooter := range []bool{false, true} {
						for _, perSeg := range []int{0, 3} {
							name := fmt.Sprintf("%s/bps%d/spc%d/%s/nofooter=%v/perseg%d", cname, bps, spc, bname, noFooter, perSeg)
							t.Run(name, func(t *testing.T) {
								chunk := bps * spc
								media := mixedMedia(9*chunk + bps) // a short last chunk too
								files := ewftest.Build(ewftest.Options{
									BytesPerSector: bps, SectorsPerChunk: spc, Compress: comp, Base: base,
									NoTableFooter: noFooter, ChunksPerSegment: perSeg, ChunksPerTable: 4,
								}, media)
								r := mustOpen(t, files)
								if w := r.Warnings(); len(w) != 0 {
									t.Fatalf("warnings on a legal layout: %q", w)
								}
								if got := readAll(t, r); string(got) != string(media) {
									t.Fatal("media differs")
								}
							})
						}
					}
				}
			}
		}
	}
}
