package ext4

import (
	"testing"
	"time"
)

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
			0:   1 + 1 + 7, // old layout: only s_first_meta_bg descriptor blocks (the kernel's ext4_bg_num_gdb_nometa)
			81:  1 + 1 + 7,
			125: 1 + 1 + 7,
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

	// 1 KiB blocks without first_data_block: group 0 starts at the boot block, so
	// its superblock and descriptors sit one block later than the group start.
	check("1k boot block", &superblock{blockSize: 1024, descSize: 32, groups: 8, reservedGDT: 3, roCompat: roSparseSuper},
		map[uint64]uint64{0: 1 + 1 + 1 + 3, 1: 1 + 1 + 3, 2: 0})
	check("1k with first_data_block", &superblock{blockSize: 1024, descSize: 32, groups: 8, reservedGDT: 3, roCompat: roSparseSuper, firstDataBlock: 1},
		map[uint64]uint64{0: 1 + 1 + 3})
}

// zeroReader is an io.ReaderAt over an endless run of zero bytes.
type zeroReader struct{}

func (zeroReader) ReadAt(p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}

// uninitFS builds an FS whose groups are described directly, so tests can use
// geometries the image builder cannot produce.
func uninitFS(groups []groupDesc, bpg, itable int) *FS {
	return &FS{
		sb: &superblock{
			blockSize: 1024, descSize: 32, groups: int64(len(groups)),
			blocksPerGroup: uint32(bpg), blocksCount: int64(len(groups)) * int64(bpg), itableBlocks: int64(itable),
			roCompat: roGDTCsum | roSparseSuper,
		},
		groups: groups,
		r:      zeroReader{},
		data:   zeroReader{},
		size:   int64(len(groups)) * int64(bpg) * 1024,
	}
}

// TestUnallocatedManyDescriptorsPointingIntoUninitGroup: a hostile descriptor
// table must not make the foreign-metadata pass cost descriptors x group size.
func TestUnallocatedManyDescriptorsPointingIntoUninitGroup(t *testing.T) {
	const bpg, itable, n = 8192, 8192, 100000
	groups := make([]groupDesc, n)
	target := uint64(2 * bpg) // group 2 is BLOCK_UNINIT; every descriptor points its tables into it
	for i := range groups {
		groups[i] = groupDesc{blockBitmap: target, inodeBitmap: target + 1, inodeTable: target}
	}
	groups[2].flags = bgBlockUninit
	f := uninitFS(groups, bpg, itable)

	start := time.Now()
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Unallocated took %v for %d descriptors aimed at one group; want < 2s", d, n)
	}
	// The whole of group 2 is metadata (the inode table spans it): none free.
	for _, r := range runs {
		if r.Offset < int64(3*bpg)*1024 && r.Offset+r.Length > int64(2*bpg)*1024 {
			t.Fatalf("run %+v overlaps the metadata of uninit group 2", r)
		}
	}
}

// TestUnallocatedBadDescriptorStillProtectsInRangeLocations: a descriptor with
// one location outside the filesystem is skipped itself, but its other
// locations that lie inside a BLOCK_UNINIT group are still allocated.
func TestUnallocatedBadDescriptorStillProtectsInRangeLocations(t *testing.T) {
	const bpg, itable = 1024, 8
	groups := make([]groupDesc, 3)
	groups[2].flags = bgBlockUninit
	start2 := uint64(2 * bpg)
	// group 0: block bitmap outside the fs (bad), inode table inside group 2.
	groups[0] = groupDesc{blockBitmap: 1 << 40, inodeBitmap: start2 + 500, inodeTable: start2 + 600, bad: true}
	// group 1 is a normal good group with its bitmaps in group 1.
	groups[1] = groupDesc{blockBitmap: bpg, inodeBitmap: bpg + 1, inodeTable: bpg + 2}
	groups[2].blockBitmap, groups[2].inodeBitmap, groups[2].inodeTable = start2, start2+1, start2+2
	f := uninitFS(groups, bpg, itable)
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	free := func(blk uint64) bool {
		for _, r := range runs {
			if int64(blk)*1024 >= r.Offset && int64(blk)*1024 < r.Offset+r.Length {
				return true
			}
		}
		return false
	}
	for _, blk := range []uint64{start2 + 500, start2 + 600, start2 + 607} {
		if free(blk) {
			t.Errorf("block %d (a location of a bad descriptor inside group 2) reported free", blk)
		}
	}
	if !free(start2 + 700) {
		t.Error("an unrelated block of the uninit group should be free")
	}
	for blk := uint64(0); blk < bpg; blk++ {
		if free(blk) {
			t.Fatalf("block %d of the skipped bad group 0 reported free", blk)
		}
	}
}
