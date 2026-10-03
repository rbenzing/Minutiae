package f2fs_test

import (
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

const (
	natEntSize = 9
	// Offsets in the checkpoint header block (no cp_payload): the SIT version
	// bitmap (64 bytes) is at 192 and the NAT bitmap follows it.
	cpNATBitmapOff = 192 + 64
)

// fileInode is a minimal regular-file inode block for nid.
func fileInode(o f2fstest.Options, nid uint32, size uint64) []byte {
	return f2fstest.InodeBlock(o, f2fstest.Inode{NID: nid, Mode: 0o100644, Links: 1, Size: size})
}

func natNode(o f2fstest.Options, nid, size uint64) f2fstest.Node {
	return f2fstest.Node{NID: uint32(nid), Block: fileInode(o, uint32(nid), size)}
}

func wantNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("error %v, want one wrapping ErrNotFound", err)
	}
	if errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("a free nid must not be reported as corruption: %v", err)
	}
}

func TestNATBlockAndBitmapCopy(t *testing.T) {
	o := smallOpts()
	l := f2fstest.Geometry(o)
	// Three nids in three NAT blocks: block 0 (bit 0 set: second copy),
	// block 1 (bit clear: first copy) and block 511, the last one of the
	// area (bit 511 set: second copy of the last segment's blocks).
	const nidA, nidB, nidC = 5, f2fstest.NATPerBlock + 9, 511*f2fstest.NATPerBlock + 7
	bm := make([]byte, 64)
	bm[0] = 0x80 // bit 0, most significant bit first
	bm[63] = 0x01
	o.NATBitmap = bm
	o.Nodes = []f2fstest.Node{natNode(o, nidA, 1), natNode(o, nidB, 2), natNode(o, nidC, 3)}
	img := f2fstest.Build(o, nil)
	f := mustOpen(t, img)

	for i, nid := range []uint32{nidA, nidB, nidC} {
		got, err := f.NATLookup(nid)
		if err != nil {
			t.Fatalf("NATLookup(%d): %v", nid, err)
		}
		if want := l.Main + uint32(i); got != want {
			t.Errorf("NATLookup(%d) = %d, want %d", nid, got, want)
		}
		v, _, err := f.Inode(nid)
		if err != nil || v.Size != int64(i+1) {
			t.Errorf("Inode(%d) = size %d, %v; want size %d", nid, v.Size, err, i+1)
		}
	}

	// The builder put the entries in the copies the bitmap names and left the
	// other copies zero, so a reader that ignored the bitmap would see free nids.
	entry := func(blk uint32, nid uint32) uint32 {
		return le.Uint32(img[int(blk)*4096+int(nid%f2fstest.NATPerBlock)*natEntSize+5:])
	}
	if entry(l.NATBlock(0, true), nidA) != l.Main || entry(l.NATBlock(0, false), nidA) != 0 {
		t.Error("nid A is not only in the second copy of NAT block 0")
	}
	if entry(l.NATBlock(1, false), nidB) != l.Main+1 || entry(l.NATBlock(1, true), nidB) != 0 {
		t.Error("nid B is not only in the first copy of NAT block 1")
	}
	if entry(l.NATBlock(511, true), nidC) != l.Main+2 || entry(l.NATBlock(511, false), nidC) != 0 {
		t.Error("nid C is not only in the second copy of NAT block 511")
	}

	// Flip bit 0 in the checkpoint: the reader must now look at the first copy
	// (all zero) and find nid A free.
	img2 := append([]byte(nil), img...)
	cpBlock(img2, l, 1)[cpNATBitmapOff] &^= 0x80
	f2fstest.SealCheckpoint(img2, 1)
	_, err := mustOpen(t, img2).NATLookup(nidA)
	wantNotFound(t, err)

	// Setting bit 1 sends nid B to its (empty) second copy.
	img3 := append([]byte(nil), img...)
	cpBlock(img3, l, 1)[cpNATBitmapOff] |= 0x40
	f2fstest.SealCheckpoint(img3, 1)
	_, err = mustOpen(t, img3).NATLookup(nidB)
	wantNotFound(t, err)
}

func TestNATJournalOverridesBlock(t *testing.T) {
	for _, compact := range []bool{false, true} {
		name := "full summary"
		if compact {
			name = "compact summary"
		}
		t.Run(name, func(t *testing.T) {
			o := smallOpts()
			o.CompactSum = compact
			l := f2fstest.Geometry(o)
			// nid 5: the NAT block says Main+0 (size 1) but the journal says
			// Main+1 (size 2). nid 6: the block maps it, the journal frees it.
			// nid 9 exists only in the journal.
			stale := natNode(o, 5, 1)
			fresh := natNode(o, 5, 2)
			fresh.NoNAT = true
			fresh.Addr = l.Main + 1
			stale.Addr = l.Main
			six := natNode(o, 6, 6)
			six.Addr = l.Main + 2
			nine := natNode(o, 9, 9)
			nine.NoNAT = true
			nine.Addr = l.Main + 3
			o.Nodes = []f2fstest.Node{stale, fresh, six, nine}

			// Control: without a journal the block wins.
			f := mustOpen(t, f2fstest.Build(o, nil))
			if got, err := f.NATLookup(5); err != nil || got != l.Main {
				t.Fatalf("without a journal NATLookup(5) = %d, %v; want %d", got, err, l.Main)
			}
			if _, err := f.NATLookup(9); err == nil {
				t.Fatal("nid 9 resolves without a journal entry or NAT entry")
			}

			o.NATJournal = []f2fstest.NATEntry{
				{NID: 5, Ino: 5, Addr: l.Main + 1},
				{NID: 6, Ino: 6, Addr: 0},
				{NID: 9, Ino: 9, Addr: l.Main + 3},
			}
			f = mustOpen(t, f2fstest.Build(o, nil))
			if got, err := f.NATLookup(5); err != nil || got != l.Main+1 {
				t.Fatalf("NATLookup(5) = %d, %v; want the journal's %d", got, err, l.Main+1)
			}
			if v, _, err := f.Inode(5); err != nil || v.Size != 2 {
				t.Fatalf("Inode(5) size = %d, %v; want 2 (the journalled block)", v.Size, err)
			}
			if got, err := f.NATLookup(9); err != nil || got != l.Main+3 {
				t.Fatalf("journal-only nid 9: %d, %v", got, err)
			}
			_, err := f.NATLookup(6)
			wantNotFound(t, err)
			// A nid the journal does not mention still comes from the block.
			if got, err := f.NATLookup(7); err == nil {
				t.Fatalf("unmapped nid 7 resolved to %d", got)
			}
		})
	}
}

func TestNATJournalFullAndOverfull(t *testing.T) {
	o := smallOpts()
	l := f2fstest.Geometry(o)
	for i := range 38 {
		o.NATJournal = append(o.NATJournal, f2fstest.NATEntry{NID: uint32(100 + i), Ino: uint32(100 + i), Addr: l.Main + uint32(i)})
	}
	img := f2fstest.Build(o, nil)
	f := mustOpen(t, img)
	for _, i := range []int{0, 37} {
		if got, err := f.NATLookup(uint32(100 + i)); err != nil || got != l.Main+uint32(i) {
			t.Fatalf("journal entry %d: %d, %v", i, got, err)
		}
	}

	// A count beyond the in-block capacity is capped (with a warning) and
	// never reads past the journal.
	jb := (int(l.CP+l.StartSum)*4096 + 3584)
	le.PutUint16(img[jb:], 60000)
	f = mustOpen(t, img)
	if got, err := f.NATLookup(137); err != nil || got != l.Main+37 {
		t.Fatalf("capped journal: %d, %v", got, err)
	}
	if !hasWarning(f.Info(), "NAT journal claims 60000") {
		t.Errorf("no cap warning: %q", f.Info().Warnings)
	}
}

func TestNATFreeNid(t *testing.T) {
	o := smallOpts()
	o.Nodes = []f2fstest.Node{natNode(o, 5, 1)}
	l := f2fstest.Geometry(o)
	f := mustOpen(t, f2fstest.Build(o, nil))

	for _, nid := range []uint32{3, 4, 6, 4000, l.NATCapacity() - 1} {
		_, err := f.NATLookup(nid)
		wantNotFound(t, err)
		_, err = f.Node(nid)
		wantNotFound(t, err)
		_, _, err = f.Inode(nid)
		wantNotFound(t, err)
	}
}

func TestNATLookupRefusesInvalidNids(t *testing.T) {
	o := smallOpts()
	l := f2fstest.Geometry(o)
	f := mustOpen(t, f2fstest.Build(o, nil))
	for _, nid := range []uint32{0, f2fstest.NodeIno, f2fstest.MetaIno, l.NATCapacity(), l.NATCapacity() + 1, math.MaxUint32} {
		_, err := f.NATLookup(nid)
		_ = asCorrupt(t, err)
		if errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("nid %d: invalid nid reported as merely free", nid)
		}
	}
}

func TestNodeFooterMismatchIsCorrupt(t *testing.T) {
	o := smallOpts()
	// The NAT maps nid 6 to a block whose footer says it is nid 5.
	wrong := f2fstest.Node{NID: 6, Block: fileInode(o, 5, 1)}
	right := natNode(o, 5, 1)
	o.Nodes = []f2fstest.Node{wrong, right}
	f := mustOpen(t, f2fstest.Build(o, nil))

	if _, err := f.Node(5); err != nil {
		t.Fatalf("Node(5): %v", err)
	}
	_, err := f.Node(6)
	ce := asCorrupt(t, err)
	if ce.Structure != "f2fs node" {
		t.Errorf("structure %q", ce.Structure)
	}
	if _, _, err := f.Inode(6); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Inode(6): %v, want ErrCorrupt", err)
	}
}

func TestNodeAddressMustBeInMainArea(t *testing.T) {
	o := smallOpts()
	l := f2fstest.Geometry(o)
	for name, addr := range map[string]uint32{
		"first block of the SSA":  l.SSA,
		"just below main":         l.Main - 1,
		"first block past main":   l.BlockCount,
		"far beyond the image":    l.BlockCount + 100000,
		"largest block address":   math.MaxUint32,
		"checkpoint area":         l.CP,
		"inside the NAT area":     l.NAT + 3,
		"inside the segment zero": 1,
	} {
		t.Run(name, func(t *testing.T) {
			oo := o
			oo.Nodes = []f2fstest.Node{{NID: 5, Addr: addr, Block: fileInode(o, 5, 1)}}
			f := mustOpen(t, f2fstest.Build(oo, nil))
			_, err := f.Node(5)
			_ = asCorrupt(t, err)
		})
	}
}
