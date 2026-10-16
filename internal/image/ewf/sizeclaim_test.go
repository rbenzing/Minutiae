package ewf_test

import (
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func TestEWFSizeClaimImplausible(t *testing.T) {
	media := pattern(3 * 64 * 512)
	files := ewftest.Build(ewftest.Options{}, media)
	// Forge the volume section (checksum fixed): 1000 chunks of 16 MiB = 16 GiB
	// of media claimed by a file set of a few hundred KB.
	v := section(t, files[0], "volume")
	p := int(v.Offset) + 76
	binary.LittleEndian.PutUint32(files[0][p+4:], 1000)        // number_of_chunks
	binary.LittleEndian.PutUint32(files[0][p+8:], 32768)       // sectors_per_chunk
	binary.LittleEndian.PutUint64(files[0][p+16:], 1000*32768) // number_of_sectors
	ewftest.FixAdler(files[0], p, p+1048, p+1048)
	r := mustOpen(t, files)
	sc := r.SizeClaim()
	if !sc.Implausible || sc.Media != 1000*32768*512 || sc.Stored != int64(len(files[0])) {
		t.Fatalf("SizeClaim = %+v", sc)
	}
	if !hasWarning(r, "implausible size") || !hasWarning(r, "16777216000") {
		t.Fatalf("warnings %q do not state the claim", r.Warnings())
	}
}

func TestEWFHonestImageIsNotImplausible(t *testing.T) {
	for name, media := range map[string][]byte{
		"incompressible": pattern(10 * 64 * 512),
		"short":          pattern(3 * 512),
	} {
		t.Run(name, func(t *testing.T) {
			r := mustOpen(t, ewftest.Build(ewftest.Options{Compress: ewftest.CompressAll}, media))
			if sc := r.SizeClaim(); sc.Implausible || hasWarning(r, "implausible size") {
				t.Fatalf("honest image flagged: %+v %q", sc, r.Warnings())
			}
		})
	}
}

// All-zero media compresses about 1000:1, right under deflate's 1032:1 limit:
// the best honest case must stay clear of the line.
func TestEWFHonestZeroImageIsNotImplausible(t *testing.T) {
	const chunk = 32768 * 512 // 16 MiB chunks
	media := make([]byte, 4*chunk)
	files := ewftest.Build(ewftest.Options{SectorsPerChunk: 32768, Compress: ewftest.CompressAll}, media)
	r := mustOpen(t, files)
	var stored int64
	for _, f := range files {
		stored += int64(len(f))
	}
	if sc := r.SizeClaim(); sc.Implausible || sc.Media != int64(len(media)) || sc.Stored != stored {
		t.Fatalf("SizeClaim = %+v (stored %d); an honest zero image must not be flagged", sc, stored)
	}
}
