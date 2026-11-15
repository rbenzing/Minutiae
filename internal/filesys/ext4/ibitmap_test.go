package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const testInodeUninit = 0x1

func inodeNum(t *testing.T, id string) uint32 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "inode:"), 10, 32)
	if err != nil {
		t.Fatalf("id %q: %v", id, err)
	}
	return uint32(n)
}

// The committed ext4-4k-csum image has 64-byte descriptors, so it proves the high-half branch of the inode bitmap
// checksum (3C-R18, 3C-R26). The oracle's live files must read used and its deleted entries free.
func TestInodeBitmapChecksumMatchesRealImages(t *testing.T) {
	p := filepath.Join("testdata", "ext4-4k-csum.img.gz")
	want := loadOracle(t, p)
	f := openImg(t, gunzipFixture(t, p))

	verified := 0
	for g := range f.GroupCount() {
		if _, _, _, flags, _ := f.Group(g); flags&testInodeUninit != 0 {
			continue
		}
		stored, computed, hasHi := f.GroupInodeBitmapCsum(g)
		if !hasHi {
			t.Fatalf("group %d: no high half; the image must have 64-byte descriptors", g)
		}
		if stored != computed {
			t.Errorf("group %d: stored %#x, computed %#x", g, stored, computed)
		}
		verified++
	}
	if verified == 0 {
		t.Fatal("no group verified, the test would pass vacuously")
	}

	used := 0
	for _, of := range want.Files {
		if of.Path == "/" {
			continue
		}
		e, err := f.Lookup(of.Path)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", of.Path, err)
		}
		state, err := f.InodeAllocated(inodeNum(t, e.ID))
		if err != nil || state != "used" {
			t.Errorf("%s (%s): state %q, %v; want used", of.Path, e.ID, state, err)
		}
		used++
	}
	if used == 0 {
		t.Fatal("no live file checked")
	}

	free := 0
	for _, dp := range want.Deleted {
		dir, base := path.Split(dp)
		found := false
		for _, e := range readDir(t, f, dir) {
			if !e.Deleted || e.Name != base {
				continue
			}
			v, _ := attr(e, "inode")
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				continue // an unknown inode number names nothing to look up
			}
			found = true
			state, err := f.InodeAllocated(uint32(n))
			if err != nil || state != "free" {
				t.Errorf("%s (inode %d): state %q, %v; want free", dp, n, state, err)
			}
			free++
		}
		if !found {
			t.Errorf("deleted entry %s not listed with an inode number", dp)
		}
	}
	if free == 0 {
		t.Fatal("no deleted inode checked")
	}
}

// gdtImg is a two-group GDT-checksum image (32 inodes per group) holding one live file in group 0.
func gdtImg(o ext4test.Options) ([]byte, *ext4test.Layout) {
	o.Extents, o.Groups, o.InodesPerGroup = true, 2, 32
	return ext4test.BuildLayout(o, []ext4test.File{{Path: "/a", Data: []byte("a")}})
}

func TestInodeStateUninitAndUnused(t *testing.T) {
	img, l := gdtImg(ext4test.Options{GDTCsum: true})
	a := l.File("/a").Inode // group 0
	f := openImg(t, img)
	if st, err := f.InodeAllocated(a); err != nil || st != "used" {
		t.Fatalf("baseline inode %d: %q, %v", a, st, err)
	}
	if st, err := f.InodeAllocated(40); err != nil || st != "free" { // group 1, nothing in it
		t.Fatalf("baseline inode 40: %q, %v", st, err)
	}

	// INODE_UNINIT on group 1: its bitmap was never written.
	img, l = gdtImg(ext4test.Options{GDTCsum: true})
	l.PatchDescriptor(img, 1, func(d []byte) { binary.LittleEndian.PutUint16(d[0x12:], testInodeUninit) })
	f = openImg(t, img)
	st, err := f.InodeAllocated(40)
	if err != nil || !strings.HasPrefix(st, "unknown") || !strings.Contains(st, "INODE_UNINIT") {
		t.Errorf("inode in an INODE_UNINIT group: %q, %v; want unknown naming INODE_UNINIT", st, err)
	}
	if st, err := f.InodeAllocated(a); err != nil || st != "used" {
		t.Errorf("inode in group 0 after patching group 1: %q, %v", st, err)
	}

	// bg_itable_unused: indexes >= 32-20 = 12 of group 0 are in the never-used tail.
	img, l = gdtImg(ext4test.Options{GDTCsum: true})
	l.PatchDescriptor(img, 0, func(d []byte) { binary.LittleEndian.PutUint16(d[0x1C:], 20) })
	f = openImg(t, img)
	if st, err := f.InodeAllocated(a); err != nil || st != "used" { // index 10, below the tail
		t.Errorf("inode below the unused tail: %q, %v", st, err)
	}
	if st, err := f.InodeAllocated(12); err != nil || strings.HasPrefix(st, "unknown") { // index 11
		t.Errorf("inode 12 just below the tail: %q, %v", st, err)
	}
	for _, n := range []uint32{13, 20, 32} { // indexes 12, 19, 31
		st, err := f.InodeAllocated(n)
		if err != nil || !strings.HasPrefix(st, "unknown") || !strings.Contains(st, "unused") {
			t.Errorf("inode %d in the unused tail: %q, %v; want unknown naming the unused tail", n, st, err)
		}
	}

	// An unused count larger than the group is not believed: the whole group is unknown.
	img, l = gdtImg(ext4test.Options{GDTCsum: true})
	l.PatchDescriptor(img, 0, func(d []byte) { binary.LittleEndian.PutUint16(d[0x1C:], 33) })
	f = openImg(t, img)
	if st, err := f.InodeAllocated(a); err != nil || !strings.HasPrefix(st, "unknown") {
		t.Errorf("itable_unused > inodes per group: %q, %v; want unknown", st, err)
	}

	// Without group checksums the flag and the count mean nothing, so the bitmap decides.
	img, l = gdtImg(ext4test.Options{})
	l.PatchDescriptor(img, 0, func(d []byte) {
		binary.LittleEndian.PutUint16(d[0x12:], testInodeUninit)
		binary.LittleEndian.PutUint16(d[0x1C:], 20)
	})
	f = openImg(t, img)
	if st, err := f.InodeAllocated(l.File("/a").Inode); err != nil || st != "used" {
		t.Errorf("no csum feature: %q, %v; want used", st, err)
	}
}

// The high half of bg_itable_unused (descriptors of 0x34 bytes or more) counts: a value above the group size makes the
// group unknown.
func TestInodeStateItableUnusedHighHalf(t *testing.T) {
	img, l := gdtImg(ext4test.Options{MetadataCsum: true, Bit64: true})
	l.PatchDescriptor(img, 0, func(d []byte) { binary.LittleEndian.PutUint16(d[0x32:], 1) })
	f := openImg(t, img)
	if st, err := f.InodeAllocated(l.File("/a").Inode); err != nil || !strings.HasPrefix(st, "unknown") {
		t.Errorf("itable_unused_hi set: %q, %v; want unknown", st, err)
	}
}

func TestInodeStateBadChecksumIsUnknown(t *testing.T) {
	files := []ext4test.File{{Path: "/a", Data: []byte("a")}, {Path: "/gone", Data: []byte("g"), Deleted: true}}
	for _, o := range []ext4test.Options{
		{Extents: true, MetadataCsum: true, Groups: 2, InodesPerGroup: 32},
		{Extents: true, MetadataCsum: true, Bit64: true, Groups: 2, InodesPerGroup: 32},
	} {
		img, l := ext4test.BuildLayout(o, files)
		l.Unlink(img, "/gone", ext4test.DebugfsRm)
		a, gone := l.File("/a").Inode, l.File("/gone").Inode
		f := openImg(t, img)
		if st, err := f.InodeAllocated(a); err != nil || st != "used" {
			t.Fatalf("baseline: %q, %v", st, err)
		}
		if st, err := f.InodeAllocated(gone); err != nil || st != "free" {
			t.Fatalf("baseline gone: %q, %v", st, err)
		}
		// Flip the bit of an unrelated inode directly, without refreshing the checksum.
		img[l.InodeBitmap[0]*l.BlockSize+3] ^= 0x01
		f = openImg(t, img)
		for _, n := range []uint32{a, gone} {
			st, err := f.InodeAllocated(n)
			if err != nil || !strings.HasPrefix(st, "unknown") || !strings.Contains(st, "checksum") {
				t.Errorf("Bit64=%v inode %d with a bad bitmap checksum: %q, %v; want unknown naming the checksum", o.Bit64, n, st, err)
			}
		}
		// Group 1's bitmap is intact.
		if st, err := f.InodeAllocated(40); err != nil || st != "free" {
			t.Errorf("Bit64=%v inode in the intact group: %q, %v", o.Bit64, st, err)
		}
	}
}

// The low-half-only branch (metadata_csum without 64bit: 32-byte descriptors) has no real image among the committed
// fixtures (3C-R26): it is covered by the builder only, and docs/limitations.md lists it.
func TestInodeBitmapChecksumLoOnlyBranch(t *testing.T) {
	files := []ext4test.File{{Path: "/a", Data: []byte("a")}, {Path: "/gone", Data: []byte("g"), Deleted: true}}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true, MetadataCsum: true, Groups: 2, InodesPerGroup: 32}, files)
	l.Unlink(img, "/gone", ext4test.DebugfsRm)
	f := openImg(t, img)
	for g := range f.GroupCount() {
		stored, computed, hasHi := f.GroupInodeBitmapCsum(g)
		if hasHi {
			t.Fatalf("group %d: 32-byte descriptors must have no high half", g)
		}
		if stored != computed || stored > 0xFFFF {
			t.Errorf("group %d: stored %#x computed %#x", g, stored, computed)
		}
	}
	if st, err := f.InodeAllocated(l.File("/a").Inode); err != nil || st != "used" {
		t.Errorf("used: %q, %v", st, err)
	}
	if st, err := f.InodeAllocated(l.File("/gone").Inode); err != nil || st != "free" {
		t.Errorf("free: %q, %v", st, err)
	}
	// A flipped bit with the checksum left alone is unknown.
	img[l.InodeBitmap[0]*l.BlockSize+2] ^= 0x80
	f = openImg(t, img)
	if st, err := f.InodeAllocated(l.File("/a").Inode); err != nil || !strings.HasPrefix(st, "unknown") {
		t.Errorf("flipped bit: %q, %v; want unknown", st, err)
	}
}

// A bitmap that lies past the end of the image cannot be read: the answer is unknown, never free.
func TestInodeStateUnreadableBitmapIsUnknown(t *testing.T) {
	img, l := gdtImg(ext4test.Options{MetadataCsum: true})
	f := openImg(t, img[:l.InodeBitmap[1]*l.BlockSize+2]) // group 1's bitmap is cut short
	st, err := f.InodeAllocated(40)
	if err != nil || !strings.HasPrefix(st, "unknown") {
		t.Errorf("cut bitmap: %q, %v; want unknown", st, err)
	}
}

// A group descriptor whose checksum is wrong cannot vouch for its bitmap location or flags: unknown, not free.
func TestInodeStateBadDescriptorChecksumIsUnknown(t *testing.T) {
	for _, o := range []ext4test.Options{{MetadataCsum: true}, {GDTCsum: true}} {
		img, l := gdtImg(o)
		var d0 []byte
		l.PatchDescriptor(img, 0, func(d []byte) {
			if d0 == nil {
				d0 = d
			}
		})
		d0[0x1E] ^= 0xFF // the primary copy's bg_checksum
		f := openImg(t, img)
		st, err := f.InodeAllocated(l.File("/a").Inode)
		if err != nil || !strings.HasPrefix(st, "unknown") || !strings.Contains(st, "descriptor") {
			t.Errorf("%+v: %q, %v; want unknown naming the descriptor", o, st, err)
		}
	}
}

// failRange fails every read that touches [lo, hi) with err.
type failRange struct {
	r      io.ReaderAt
	lo, hi int64
	err    error
}

func (f failRange) ReadAt(p []byte, off int64) (int, error) {
	if off < f.hi && off+int64(len(p)) > f.lo {
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

// A bitmap the image cannot supply (short read) is unknown, never free; a failing reader is an I/O error, never
// folded into an answer.
func TestInodeStateReadFailures(t *testing.T) {
	img, l := gdtImg(ext4test.Options{MetadataCsum: true})
	lo := int64(l.InodeBitmap[1] * l.BlockSize)
	open := func(err error) *ext4.FS {
		f, oerr := ext4.Open(failRange{bytes.NewReader(img), lo, lo + int64(l.BlockSize), err}, int64(len(img)))
		if oerr != nil {
			t.Fatal(oerr)
		}
		return f
	}
	st, err := open(io.ErrUnexpectedEOF).InodeAllocated(40)
	if err != nil || !strings.HasPrefix(st, "unknown") || !strings.Contains(st, "beyond the end") {
		t.Errorf("short read: %q, %v; want unknown", st, err)
	}
	boom := errors.New("device gone")
	st, err = open(boom).InodeAllocated(40)
	if !errors.Is(err, boom) {
		t.Errorf("failing read: %q, %v; want the I/O error", st, err)
	}
}
