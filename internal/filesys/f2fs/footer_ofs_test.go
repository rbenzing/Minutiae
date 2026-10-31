package f2fs

import "testing"

// The footer flag words below are copied from nodes the kernel f2fs driver wrote (the nodiscard recovery
// fixture: nid 91 is the first direct node of /direct/target.bin, nids 93/94/95 are the nodes at offsets 1, 2 and 3
// of /indirect/target.bin, an inode has offset 0). The flag keeps COLD, FSYNC and DENT in bits 0-2 and ofs_of_node
// from bit 3: an offset read from bit 7 turns every real direct node into an "inode".
func TestFooterOfsFollowsTheKernelBitLayout(t *testing.T) {
	for _, tc := range []struct {
		flag uint32
		want uint32
	}{
		{0x1, 0},  // inode, cold
		{0x9, 1},  // offset 1, cold
		{0x11, 2}, // offset 2
		{0x19, 3}, // offset 3
		{0x7, 0},  // cold, fsync and dent bits do not leak into the offset
		{0xF, 1},  // offset 1 with the three flag bits set
	} {
		if got := (footer{flag: tc.flag}).ofs(); got != tc.want {
			t.Errorf("ofs of flag %#x = %d, want %d", tc.flag, got, tc.want)
		}
	}
}
