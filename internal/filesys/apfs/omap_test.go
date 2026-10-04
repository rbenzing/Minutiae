package apfs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// refLookup is the specification of an object map lookup: the record of oid
// with the largest xid not above the target; DELETED or none is "not found".
func refLookup(es []apfstest.OmapEntry, oid, xid uint64) (paddr uint64, ok bool) {
	var best *apfstest.OmapEntry
	for i := range es {
		e := &es[i]
		if e.Oid == oid && e.Xid <= xid && (best == nil || e.Xid > best.Xid) {
			best = e
		}
	}
	if best == nil || best.Flags&apfstest.OmapValDeleted != 0 {
		return 0, false
	}
	return best.Paddr, true
}

// Review Focus 2: the example of the format reference. Entries
// (588, 2101 -> 200), (588, 2202 -> 300), (588, 2300 -> 100).
func TestOmapLookupLargestXidNotAboveTarget(t *testing.T) {
	entries := []apfstest.OmapEntry{
		{Oid: 588, Xid: 2101, Paddr: 200},
		{Oid: 588, Xid: 2202, Paddr: 300},
		{Oid: 588, Xid: 2300, Paddr: 100},
		{Oid: 587, Xid: 2000, Paddr: 90}, // neighbours: other oids never match
		{Oid: 587, Xid: 2400, Paddr: 91},
		{Oid: 589, Xid: 1, Paddr: 92},
		{Oid: 589, Xid: 2100, Paddr: 93},
	}
	for _, maxKeys := range []int{0, 2, 3} {
		t.Run(fmt.Sprintf("maxkeys-%d", maxKeys), func(t *testing.T) {
			im := newImage(t, apfstest.Options{Omap: entries, OmapMaxKeys: maxKeys})
			f := im.mustOpen()
			for _, tc := range []struct {
				oid, xid uint64
				paddr    uint64 // 0: not found
			}{
				{588, 2300, 100},
				{588, 2301, 100},
				{588, 1 << 60, 100},
				{588, 2299, 300},
				{588, 2290, 300},
				{588, 2202, 300},
				{588, 2201, 200},
				{588, 2101, 200},
				{588, 2100, 0},
				{588, 0, 0},
				{587, 2399, 90},
				{587, 2400, 91},
				{587, 1999, 0},
				{589, 2099, 92},
				{589, 2100, 93},
				{586, 5000, 0},
				{590, 5000, 0},
				{0, 5000, 0},
				{1 << 63, 1 << 63, 0},
			} {
				paddr, size, flags, err := f.OmapLookup(0, tc.oid, tc.xid)
				if tc.paddr == 0 {
					if !errors.Is(err, filesys.ErrNotFound) {
						t.Errorf("lookup(%d, %d) = %d, %v; want ErrNotFound", tc.oid, tc.xid, paddr, err)
					}
					continue
				}
				if err != nil || paddr != tc.paddr || size != 4096 || flags != 0 {
					t.Errorf("lookup(%d, %d) = %d/%d/%d, %v; want paddr %d", tc.oid, tc.xid, paddr, size, flags, err, tc.paddr)
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %q", w)
			}
		})
	}
}

// The lookup is exact for every oid and xid whatever the node boundaries are.
func TestOmapLookupAcrossLeafBoundaries(t *testing.T) {
	var entries []apfstest.OmapEntry
	for oid := uint64(100); oid < 140; oid += 1 + oid%3 {
		for _, x := range []uint64{2, 5, 9, 12} {
			if (oid+x)%5 == 0 {
				continue
			}
			e := apfstest.OmapEntry{Oid: oid, Xid: x, Paddr: oid*1000 + x}
			if (oid*x)%7 == 3 {
				e.Flags = apfstest.OmapValDeleted
				e.Paddr = 0
			}
			entries = append(entries, e)
		}
	}
	for _, maxKeys := range []int{0, 2, 3, 4, 5, 7} {
		t.Run(fmt.Sprintf("maxkeys-%d", maxKeys), func(t *testing.T) {
			im := newImage(t, apfstest.Options{Omap: entries, OmapMaxKeys: maxKeys})
			if maxKeys == 2 && len(im.g.OmapNodes) < 8 {
				t.Fatalf("only %d tree nodes: the boundaries are not exercised", len(im.g.OmapNodes))
			}
			f := im.mustOpen()
			for oid := uint64(95); oid < 145; oid++ {
				for xid := uint64(0); xid < 16; xid++ {
					want, ok := refLookup(entries, oid, xid)
					got, _, _, err := f.OmapLookup(0, oid, xid)
					switch {
					case ok && (err != nil || got != want):
						t.Fatalf("lookup(%d, %d) = %d, %v; want %d", oid, xid, got, err, want)
					case !ok && !errors.Is(err, filesys.ErrNotFound):
						t.Fatalf("lookup(%d, %d) = %d, %v; want ErrNotFound", oid, xid, got, err)
					}
				}
			}
		})
	}
}

func TestOmapDeletedMarkerIsNotFound(t *testing.T) {
	entries := []apfstest.OmapEntry{
		{Oid: 700, Xid: 5, Paddr: 90},
		{Oid: 700, Xid: 8, Flags: apfstest.OmapValDeleted},
		{Oid: 700, Xid: 12, Paddr: 91, Flags: 0},
		{Oid: 701, Xid: 3, Flags: apfstest.OmapValDeleted},
	}
	im := newImage(t, apfstest.Options{Omap: entries, OmapMaxKeys: 2})
	f := im.mustOpen()
	if p, _, _, err := f.OmapLookup(0, 700, 7); err != nil || p != 90 {
		t.Errorf("before the deletion: %d, %v", p, err)
	}
	for _, xid := range []uint64{8, 9, 11} {
		// The deletion is the newest record at or below the target: no
		// fallback to the older live record.
		if p, _, _, err := f.OmapLookup(0, 700, xid); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("lookup(700, %d) = %d, %v; want ErrNotFound", xid, p, err)
		}
	}
	if p, _, _, err := f.OmapLookup(0, 700, 12); err != nil || p != 91 {
		t.Errorf("after re-creation: %d, %v", p, err)
	}
	if _, _, _, err := f.OmapLookup(0, 701, 100); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("only a deleted record: %v", err)
	}
	// Other flags are reported, not interpreted.
	im = newImage(t, apfstest.Options{Omap: []apfstest.OmapEntry{{Oid: 9, Xid: 1, Paddr: 44, Flags: 0x4 | 0x2}}})
	if p, _, fl, err := im.mustOpen().OmapLookup(0, 9, 1); err != nil || p != 44 || fl != 0x6 {
		t.Errorf("flags: %d/%#x, %v", p, fl, err)
	}
}

func TestOmapPhysicalAndVirtualTrees(t *testing.T) {
	var recs []apfstest.Rec
	for i := range 14 {
		recs = append(recs, fsRec(uint64(30+i/2), uint8(3+i%2), "", i))
	}
	// Pack the virtual tree first to learn its oids and blocks, map them in the
	// container object map, then build the image.
	virt := varSpec(3)
	virt.Storage = apfstest.StorageVirtual
	virt.BlockSize = 4096
	next := uint64(treeBase + 50)
	oid := uint64(9000)
	vblocks := apfstest.PackTree(virt, recs, func() (uint64, uint64) {
		a, o := next, oid
		next++
		oid++
		return a, o
	})
	var entries []apfstest.OmapEntry
	for _, b := range vblocks {
		entries = append(entries, apfstest.OmapEntry{Oid: b.Oid, Xid: 5, Paddr: b.Addr})
	}
	build := func(mutate func(es []apfstest.OmapEntry) []apfstest.OmapEntry) *treeImage {
		es := append([]apfstest.OmapEntry(nil), entries...)
		if mutate != nil {
			es = mutate(es)
		}
		ti := newTreeImage(t, apfstest.Options{Omap: es, OmapMaxKeys: 3})
		apfstest.Place(ti.b, ti.bs, vblocks)
		return ti
	}
	rootOid := vblocks[0].Oid

	ti := build(nil)
	f := ti.mustOpen()
	// The virtual tree is read through the omap.
	got, err := f.ScanTree(rootOid, virtTreeType, true, nil, 0)
	if err != nil {
		t.Fatalf("virtual tree: %v", err)
	}
	sameRecs(t, got, recs)
	// The same bytes as a PHYSICAL tree (children are oids, not addresses, so
	// only the single-node root of a small tree is the same thing): a physical
	// tree is read at the block address, not through the omap.
	small := []apfstest.Rec{fsRec(1, 3, "", 0), fsRec(2, 3, "", 1)}
	pblocks := ti.place(varSpec(0), small)
	sameRecs(t, scanAll(t, f, pblocks[0].Addr, physTreeType), small)
	// The type bits decide: the virtual root oid is not a block address, and a
	// physical root address is not an omap oid.
	if _, err := f.ScanTree(rootOid, physTreeType, false, nil, 0); !isCorrupt(err) {
		t.Errorf("virtual oid read as physical: %v", err)
	}
	if _, err := f.ScanTree(pblocks[0].Addr, virtTreeType, true, nil, 0); !isCorrupt(err) {
		t.Errorf("physical address read as virtual: %v", err)
	}
	if _, err := f.ScanTree(rootOid, virtTreeType, false, nil, 0); !isCorrupt(err) {
		t.Errorf("virtual tree without an object map: %v", err)
	}
	if _, err := f.ScanTree(rootOid, ephTreeType, true, nil, 0); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ephemeral tree: %v, want ErrUnsupported", err)
	}
	for _, typ := range []uint32{0xc0000002, 0x40000003, 0x1, 0} {
		if _, err := f.ScanTree(pblocks[0].Addr, typ, true, nil, 0); !isCorrupt(err) {
			t.Errorf("tree type %#x: %v, want a CorruptError", typ, err)
		}
	}

	// A virtual node the omap cannot resolve, resolves wrongly or hides is corrupt.
	for _, tc := range []struct {
		name   string
		mutate func(es []apfstest.OmapEntry) []apfstest.OmapEntry
		want   error // nil: a CorruptError
	}{
		{"root oid missing from the omap", func(es []apfstest.OmapEntry) []apfstest.OmapEntry { return es[1:] }, nil},
		{"child oid missing", func(es []apfstest.OmapEntry) []apfstest.OmapEntry { return es[:len(es)-1] }, nil},
		{"mapping newer than the checkpoint", func(es []apfstest.OmapEntry) []apfstest.OmapEntry {
			es[0].Xid = 11 // the newest checkpoint is xid 10
			return es
		}, nil},
		{"mapping deleted", func(es []apfstest.OmapEntry) []apfstest.OmapEntry {
			es[0].Flags = apfstest.OmapValDeleted
			return es
		}, nil},
		{"mapping of the wrong size", func(es []apfstest.OmapEntry) []apfstest.OmapEntry {
			es[2].Size = 8192
			return es
		}, nil},
		{"encrypted mapping", func(es []apfstest.OmapEntry) []apfstest.OmapEntry {
			es[1].Flags = 0x4
			return es
		}, filesys.ErrEncrypted},
		{"mapping without header", func(es []apfstest.OmapEntry) []apfstest.OmapEntry {
			es[1].Flags = 0x8
			return es
		}, filesys.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := build(tc.mutate).mustOpen()
			_, err := f.ScanTree(rootOid, virtTreeType, true, nil, 0)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Errorf("error %v, want %v", err, tc.want)
				}
				return
			}
			if !isCorrupt(err) {
				t.Errorf("error %v, want a CorruptError", err)
			}
		})
	}

	// The omap view an older xid sees an older tree: an entry at xid 5 is
	// replaced at xid 9 by another block holding a different tree.
	other := []apfstest.Rec{fsRec(77, 3, "", 0)}
	oblocks := apfstest.PackTree(func() apfstest.TreeSpec {
		s := virt
		s.MaxKeys = 0
		return s
	}(), other, func() (uint64, uint64) { return treeBase + 200, rootOid }) // same virtual oid as the old root
	es := append(append([]apfstest.OmapEntry(nil), entries...), apfstest.OmapEntry{Oid: rootOid, Xid: 9, Paddr: treeBase + 200})
	ti2 := newTreeImage(t, apfstest.Options{Omap: es, OmapMaxKeys: 3})
	apfstest.Place(ti2.b, ti2.bs, vblocks)
	apfstest.Place(ti2.b, ti2.bs, oblocks)
	sameRecs(t, scanAll2(t, ti2.mustOpen(), rootOid), other)
}

func scanAll2(t *testing.T, f *apfs.FS, root uint64) []apfs.TreeRec {
	t.Helper()
	got, err := f.ScanTree(root, virtTreeType, true, nil, 0)
	if err != nil {
		t.Fatalf("ScanTree: %v", err)
	}
	return got
}

func TestOmapSnapshotTreeParsed(t *testing.T) {
	snaps := []apfstest.OmapSnap{
		{Xid: 9, Flags: 2, Oid: 1300}, // REVERTED
		{Xid: 3, Flags: 0, Oid: 1100},
		{Xid: 5, Flags: 1, Oid: 1200}, // DELETED
		{Xid: 7, Flags: 3, Oid: 1250},
	}
	want := []apfs.OmapSnapshot{{3, 0, 1100}, {5, 1, 1200}, {7, 3, 1250}, {9, 2, 1300}}
	for _, maxKeys := range []int{0, 2} {
		im := newImage(t, apfstest.Options{OmapSnaps: snaps, OmapMaxKeys: maxKeys})
		f := im.mustOpen()
		got, err := f.OmapSnapshots(0)
		if err != nil || len(got) != len(want) {
			t.Fatalf("maxkeys %d: %v, %v", maxKeys, got, err)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("maxkeys %d: snapshot %d = %+v, want %+v", maxKeys, i, got[i], want[i])
			}
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("warnings: %q", w)
		}
	}
	// No snapshot tree: no snapshots.
	got, err := newImage(t, apfstest.Options{}).mustOpen().OmapSnapshots(0)
	if err != nil || len(got) != 0 {
		t.Errorf("no tree: %v, %v", got, err)
	}
	// Snapshot xids must increase.
	im := newImage(t, apfstest.Options{OmapSnaps: snaps})
	n := im.blk(im.g.OmapSnapNodes[0])
	keyStart := 56 + 4*len(snaps) // fixed ToC of kvoff_t entries
	copy(n[keyStart+8:keyStart+16], n[keyStart:keyStart+8])
	apfstest.SealObject(n)
	if _, err := im.mustOpen().OmapSnapshots(0); !isCorrupt(err) {
		t.Errorf("repeated snapshot xid: %v", err)
	}
}

func TestOmapPhysHostile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(im *image)
		want   string // "" corrupt error at Open; else a warning
	}{
		{"wrong object type", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint32(b[24:], physicalFlag|5)
			im.seal(im.g.Omap)
		}, ""},
		{"no tree", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint64(b[48:], 0)
			im.seal(im.g.Omap)
		}, ""},
		{"virtual tree type", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint32(b[40:], 2)
			im.seal(im.g.Omap)
		}, ""},
		{"tree root outside the container", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint64(b[48:], 1<<40)
			im.seal(im.g.Omap)
		}, ""},
		{"tree root is not a tree", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint64(b[48:], im.g.DescBase)
			im.seal(im.g.Omap)
		}, ""},
		{"tree of the wrong key size", func(im *image) {
			n := im.blk(im.g.OmapNodes[0])
			le.PutUint32(n[4096-40+8:], 8)
			im.seal(im.g.OmapNodes[0])
		}, ""},
		{"unknown flags", func(im *image) {
			b := im.blk(im.g.Omap)
			le.PutUint32(b[32:], 0x1|0x100)
			im.seal(im.g.Omap)
		}, "flag"},
		{"bad checksum", func(im *image) { im.blk(im.g.Omap)[100] ^= 1 }, "checksum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := newImage(t, apfstest.Options{Omap: []apfstest.OmapEntry{{Oid: 5, Xid: 1, Paddr: 77}}})
			tc.mutate(im)
			if tc.want == "" {
				assertCorrupt(t, im, "")
				return
			}
			f := im.mustOpen()
			if !hasWarn(f, tc.want) {
				t.Errorf("no %q warning: %q", tc.want, f.Info().Warnings)
			}
			if p, _, _, err := f.OmapLookup(0, 5, 1); err != nil || p != 77 {
				t.Errorf("lookup still works: %d, %v", p, err)
			}
		})
	}
}
