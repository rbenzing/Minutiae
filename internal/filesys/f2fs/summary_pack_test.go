package f2fs_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Non-compact data summaries are located from the END of the checkpoint pack,
// as the kernel does (sum_blk_addr): total-7+type when the pack also holds the
// node summaries (an unmount checkpoint), total-4+type when it does not. A
// forged cp_pack_start_sum, which the kernel never consults for them, changes
// nothing.
func TestSummariesLocatedFromPackEnd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unclean     bool   // no CP_UMOUNT_FLAG: no node summaries in the pack
		packBlocks  uint32 // 0 = keep the builder's pack length
		forgedStart uint32 // cp_pack_start_sum written into the header
	}{
		{"umount pack, start_sum pointing at the warm summary", false, 0, 2},
		{"umount pack, start_sum pointing at the cold summary", false, 0, 3},
		// 1 header + 3 data summaries + the trailing header copy: no node summaries.
		{"pack without node summaries", true, 5, 1},
		{"pack without node summaries, forged start_sum", true, 5, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := smallOpts()
			o.Segments = 3
			o.NoSIT = true
			o.Unclean = tc.unclean
			l := f2fstest.Geometry(o)
			nine := natNode(o, 9, 9)
			nine.NoNAT = true
			nine.Addr = l.Main + 3
			o.Nodes = []f2fstest.Node{nine}
			o.NATJournal = []f2fstest.NATEntry{{NID: 9, Ino: 9, Addr: l.Main + 3}}
			o.SITJournal = []f2fstest.SITEntry{{Segno: 0, Valid: offsets(0, 10)}}

			img := f2fstest.Build(o, nil)
			hdr := cpBlock(img, l, 1)
			if tc.packBlocks != 0 {
				le.PutUint32(hdr[136:], tc.packBlocks)
			}
			le.PutUint32(hdr[140:], tc.forgedStart)
			f2fstest.SealCheckpoint(img, 1)

			f := mustOpen(t, img)
			if got, err := f.NATLookup(9); err != nil || got != l.Main+3 {
				t.Errorf("NATLookup(9) = %d, %v; want the hot summary's journal entry %d", got, err, l.Main+3)
			}
			valid := map[uint32]bool{}
			segBlocks(l, valid, 0, offsets(0, 10))
			checkFree(t, unalloc(t, f), freeOf(l, valid))
		})
	}
}
