package examine_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// uncleanImage builds an ext4 image with two deleted, intact files, /whole.bin and /cut.bin; the second has one data
// block allocated again. unclean sets needs_recovery in the superblock.
func uncleanImage(t *testing.T, unclean bool) (img []byte, whole, cut []byte) {
	t.Helper()
	whole, cut = pattern(4*1024, 41), pattern(4*1024, 42)
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true, MetadataCsum: true, Journal: true}, []ext4test.File{
		{Path: "/whole.bin", Data: whole, Deleted: true},
		{Path: "/cut.bin", Data: cut, Deleted: true},
	})
	l.Unlink(img, "/whole.bin", ext4test.DebugfsRm)
	l.Unlink(img, "/cut.bin", ext4test.DebugfsRm)
	l.SetBlockFree(img, l.File("/cut.bin").Data[2], false) // a block a new file took
	if unclean {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	return img, whole, cut
}

// The same two files on a clean image are written at 80 (intact) and 20 (cut); on an image that needs recovery the
// intact one is capped at 40 and the cut one stays at 20 (the lowest cap wins).
func TestRecoverExt4UncleanImageCapsAt40(t *testing.T) {
	for _, tc := range []struct {
		unclean         bool
		wantWhole, want int
	}{{false, 80, 20}, {true, 40, 20}} {
		img, whole, cut := uncleanImage(t, tc.unclean)
		s, _ := imageSession(t, img)
		sum := recoverAll(t, s)
		if len(sum.Artifacts) != 2 {
			t.Fatalf("unclean=%v: %d artifacts, want 2 (%v)", tc.unclean, len(sum.Artifacts), sum.SkippedBy)
		}
		for _, rec := range sum.Artifacts {
			rv := recoveryOf(rec)
			got := readArtifact(t, s.Case, rec)
			switch {
			case bytes.Equal(got, whole):
				if *rv.Confidence != tc.wantWhole || rec.Incomplete {
					t.Errorf("unclean=%v: intact file confidence %d (incomplete %v), want %d", tc.unclean, *rv.Confidence, rec.Incomplete, tc.wantWhole)
				}
				if hasAssumption(rv, "journal-not-replayed") != tc.unclean {
					t.Errorf("unclean=%v: journal-not-replayed = %v", tc.unclean, !tc.unclean)
				}
			case bytes.Equal(got, cut[:2*1024]):
				if *rv.Confidence != tc.want || !rec.Incomplete {
					t.Errorf("unclean=%v: cut file confidence %d (incomplete %v), want %d", tc.unclean, *rv.Confidence, rec.Incomplete, tc.want)
				}
			default:
				t.Errorf("unclean=%v: an artifact of %d bytes is neither file", tc.unclean, len(got))
			}
		}
		verifyOK(t, s.Case)
	}
}
