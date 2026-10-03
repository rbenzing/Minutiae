package ext4

import "testing"

func TestBaseMetaBlocks(t *testing.T) {
	check := func(name string, sb *superblock, cases map[uint64]uint64) {
		t.Helper()
		for group, want := range cases {
			if got := sb.baseMetaBlocks(group); got != want {
				t.Errorf("%s: group %d: %d blocks, want %d", name, group, got, want)
			}
		}
	}

	// 4 KiB blocks and 32-byte descriptors: 128 descriptors per block, so 300
	// groups need 3 descriptor blocks.
	check("sparse_super", &superblock{blockSize: 4096, descSize: 32, groups: 300, reservedGDT: 7, roCompat: roSparseSuper},
		map[uint64]uint64{
			0:  1 + 3 + 7, // superblock, descriptor blocks, reserved GDT blocks
			1:  1 + 3 + 7,
			2:  0, // no backup
			3:  1 + 3 + 7,
			9:  1 + 3 + 7,
			10: 0,
		})
	check("no sparse_super", &superblock{blockSize: 4096, descSize: 32, groups: 300, reservedGDT: 7},
		map[uint64]uint64{2: 1 + 3 + 7, 10: 1 + 3 + 7})

	// META_BG from metagroup 1 on (groups 0..127 keep the old layout). In a later
	// metagroup the first, second and last group carry one descriptor block, a
	// backup group also the superblock, and nobody the reserved GDT blocks.
	check("meta_bg", &superblock{
		blockSize: 4096, descSize: 32, groups: 400, reservedGDT: 7, firstMetaBG: 1,
		incompat: incompatMetaBG, roCompat: roSparseSuper,
	},
		map[uint64]uint64{
			0:   1 + 4 + 7, // old layout: 400 groups need 4 descriptor blocks
			81:  1 + 4 + 7,
			125: 1 + 4 + 7,
			127: 0,
			128: 1, // first of metagroup 1
			129: 1, // second
			130: 0, // middle
			243: 1, // middle, 3^5: only the backup superblock
			255: 1, // last
			256: 1,
			343: 1, // 7^3, middle
			384: 1,
		})
}
