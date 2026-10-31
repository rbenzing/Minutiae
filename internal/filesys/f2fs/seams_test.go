package f2fs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// These tests pin the unexported seams that F2FS recovery builds on (3D
// Task 3): each is a pure extraction of what the reader already did, so they
// check the new function against the old behaviour, not new behaviour.

func openImg(t *testing.T, img []byte) *FS {
	t.Helper()
	f, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func blockAt(img []byte, addr uint32) []byte {
	return img[int(addr)*blockSize : (int(addr)+1)*blockSize]
}

func TestParseFooterFields(t *testing.T) {
	o := f2fstest.Options{Segments: 2}
	l := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)
	want := f2fstest.Footer{NID: 0x01020304, Ino: 0x0a0b0c0d, Flag: 5<<3 | 3, CPVer: 1<<40 | 77, NextBlk: 0x55667788}
	f2fstest.SetFooter(img, l.Main, want)
	got := parseFooter(blockAt(img, l.Main))
	if got.nid != want.NID || got.ino != want.Ino || got.flag != want.Flag || got.cpVer != want.CPVer || got.nextBlk != want.NextBlk {
		t.Errorf("parseFooter = %+v, want %+v", got, want)
	}
	if got.ofs() != 5 {
		t.Errorf("ofs = %d, want 5", got.ofs())
	}
}

func TestParseInodeBlockEqualsInode(t *testing.T) {
	o := f2fstest.Options{Segments: 4, ExtraAttr: true, InodeChksum: true}
	files := []f2fstest.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: bytes.Repeat([]byte{7}, 3*blockSize)},
		{Path: "/d/inl", Data: []byte("inline"), Inline: true},
		{Path: "/d/sym", Symlink: "/d/a"},
	}
	img, tree := f2fstest.BuildTree(o, files)
	f := openImg(t, img)
	checked := 0
	for path, nid := range tree.NID {
		want, err := f.inode(nid)
		if err != nil {
			t.Fatalf("inode(%d) %s: %v", nid, path, err)
		}
		raw, err := f.node(nid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.parseInodeBlock(raw, nid)
		if err != nil || got == nil {
			t.Fatalf("parseInodeBlock(%s) = %v, %v", path, got, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseInodeBlock(%s) differs from inode(%d):\n got  %+v\n want %+v", path, nid, got, want)
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("only %d inodes checked", checked)
	}
	// A block whose footer names another owner is not that inode: the same
	// typed error as inode().
	nid := tree.NID["/d/a"]
	raw, err := f.node(nid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.parseInodeBlock(raw, nid+1)
	var ni *notInodeError
	if !errors.As(err, &ni) {
		t.Errorf("parseInodeBlock with a foreign ino: err = %v, want *notInodeError", err)
	}
}

func TestCheckpointCursorsAndVerLow(t *testing.T) {
	t.Run("node cursors and low version", func(t *testing.T) {
		o := f2fstest.Options{Segments: 4, Version: 7, CurNode: [3]f2fstest.CurSeg{{Seg: 1, Off: 5}, {Seg: 2, Off: 7}, {Seg: 3, Off: 9}}}
		f := openImg(t, f2fstest.Build(o, nil))
		for i, w := range o.CurNode {
			if got := f.cp.curNode[i]; got.seg != w.Seg || got.off != w.Off {
				t.Errorf("curNode[%d] = %+v, want %+v", i, got, w)
			}
		}
		if v, ok := f.cp.verLow(); v != 7 || !ok {
			t.Errorf("verLow = %d, %v; want 7, true", v, ok)
		}
	})
	t.Run("high half set", func(t *testing.T) {
		o := f2fstest.Options{Segments: 4, Version: 1<<32 | 5}
		f := openImg(t, f2fstest.Build(o, nil))
		if _, ok := f.cp.verLow(); ok {
			t.Error("verLow reported ok for a version above 32 bits")
		}
	})
	t.Run("data cursors", func(t *testing.T) {
		// cur_data_segno[3] at 84 and cur_data_blkoff[3] at 116 (from memory of
		// struct f2fs_checkpoint); the builder writes only the node cursors.
		o := f2fstest.Options{Segments: 4}
		l := f2fstest.Geometry(o)
		img := f2fstest.Build(o, nil)
		for pack, base := range map[int]uint32{1: l.CP, 2: l.CP + blocksPerSeg} {
			h := blockAt(img, base)
			for i := range 3 {
				binary.LittleEndian.PutUint32(h[84+4*i:], uint32(10+i))
				binary.LittleEndian.PutUint16(h[116+2*i:], uint16(20+i))
			}
			f2fstest.SealCheckpoint(img, pack)
		}
		f := openImg(t, img)
		for i := range 3 {
			if got := f.cp.curData[i]; got.seg != uint32(10+i) || got.off != uint16(20+i) {
				t.Errorf("curData[%d] = %+v, want {%d %d}", i, got, 10+i, 20+i)
			}
		}
	})
}

// seamFault fails reads that touch [lo, hi) once armed.
type seamFault struct {
	r      *bytes.Reader
	lo, hi int64
	armed  bool
}

func (s *seamFault) ReadAt(p []byte, off int64) (int, error) {
	if s.armed && off < s.hi && off+int64(len(p)) > s.lo {
		return 0, errors.New("device unplugged")
	}
	return s.r.ReadAt(p, off)
}

// freeFromSIT derives the free runs from forEachSIT alone: the clear valid
// bits of every segment whose count is not above its map (a count above the
// map makes the whole segment unknown), and none for an unreadable block.
func freeFromSIT(t *testing.T, f *FS) []filesys.Run {
	t.Helper()
	limit := min(f.sb.mainEnd(), uint64(f.sb.blocks))
	var rs []filesys.Run
	add := func(a uint64) {
		off := int64(a) * blockSize
		if n := len(rs); n > 0 && rs[n-1].Offset+rs[n-1].Length == off {
			rs[n-1].Length += blockSize
			return
		}
		rs = append(rs, filesys.Run{Offset: off, Length: blockSize})
	}
	_, err := f.forEachSIT(func(seg uint32, raw []byte) bool {
		if raw == nil {
			return true
		}
		vmap := raw[sitMapOff : sitMapOff+sitMapSize]
		set := 0
		for i := range blocksPerSeg {
			if sitValid(vmap, i) {
				set++
			}
		}
		if int(binary.LittleEndian.Uint16(raw)&sitVBlocksMask) > set {
			return true
		}
		base := uint64(f.sb.mainAddr) + uint64(seg)<<segShift
		for i := range blocksPerSeg {
			if a := base + uint64(i); a < limit && !sitValid(vmap, i) {
				add(a)
			}
		}
		return true
	})
	if err != nil {
		t.Fatalf("forEachSIT: %v", err)
	}
	return rs
}

func TestForEachSITMatchesUnallocated(t *testing.T) {
	one := uint16(1)
	cases := map[string]f2fstest.Options{
		"entries with gaps": {
			Segments: 3, NoSIT: true,
			SIT: []f2fstest.SITEntry{
				{Segno: 0, Valid: []int{0, 1, 5, 300, 511}},
				{Segno: 2, Valid: []int{9}},
			},
		},
		"journal overrides the block": {
			Segments: 3, NoSIT: true,
			SIT: []f2fstest.SITEntry{{Segno: 0, Valid: []int{1, 2, 3}}},
			SITJournal: []f2fstest.SITEntry{
				{Segno: 0, Valid: []int{100}},
				{Segno: 1, Valid: []int{4}},
			},
		},
		"bitmap selects the second copy": {
			Segments: 3, SITBitmap: []byte{0x80}, SITDecoy: true,
			SIT: []f2fstest.SITEntry{{Segno: 1, Valid: []int{2, 3}}},
		},
		"count above the map": {
			Segments: 3, NoSIT: true,
			SIT: []f2fstest.SITEntry{
				{Segno: 0, Valid: []int{1}, VBlocks: &one},
				{Segno: 1, Valid: []int{1, 2}, VBlocks: &one},
			},
		},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			f := openImg(t, f2fstest.Build(o, nil))
			want, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 {
				t.Fatal("test bug: nothing is free")
			}
			if got := freeFromSIT(t, f); !slices.Equal(got, want) {
				t.Errorf("free runs from forEachSIT differ from Unallocated:\n got  %v\n want %v", got, want)
			}
		})
	}

	t.Run("unreadable SIT block", func(t *testing.T) {
		o := f2fstest.Options{Segments: 58, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 56, Valid: []int{0}}}}
		l := f2fstest.Geometry(o)
		img := f2fstest.Build(o, nil)
		r := &seamFault{r: bytes.NewReader(img), lo: int64(l.SIT) * blockSize, hi: int64(l.SIT+1) * blockSize}
		f, err := Open(r, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		var nilSegs, okSegs []uint32
		unreadable, err := f.forEachSIT(func(seg uint32, raw []byte) bool {
			if raw == nil {
				nilSegs = append(nilSegs, seg)
			} else {
				okSegs = append(okSegs, seg)
			}
			return true
		})
		if err != nil {
			t.Fatalf("forEachSIT: %v", err)
		}
		if !slices.Equal(unreadable, []uint32{0}) {
			t.Errorf("unreadable = %v, want [0]", unreadable)
		}
		if len(nilSegs) != 55 || nilSegs[0] != 0 || nilSegs[54] != 54 || !slices.Equal(okSegs, []uint32{55, 56, 57}) {
			t.Errorf("nil segments %v, readable segments %v; want 0..54 and 55..57", nilSegs, okSegs)
		}
		if !hasWarn(f, "SIT block 0") || !hasWarn(f, "device unplugged") {
			t.Errorf("no warning naming the unreadable SIT block: %v", f.Info().Warnings)
		}
	})
	t.Run("unreadable journal", func(t *testing.T) {
		o := f2fstest.Options{Segments: 4}
		img := f2fstest.Build(o, nil)
		probe := openImg(t, img)
		lo := int64(probe.cp.addr+probe.cp.startSum) * blockSize
		r := &seamFault{r: bytes.NewReader(img), lo: lo, hi: lo + 3*blockSize}
		f, err := Open(r, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		r.armed = true
		visited := 0
		_, err = f.forEachSIT(func(uint32, []byte) bool { visited++; return true })
		if err == nil || visited != 0 {
			t.Errorf("forEachSIT = %v after %d visits; want an error and no visit", err, visited)
		}
	})
	t.Run("visit false stops", func(t *testing.T) {
		f := openImg(t, f2fstest.Build(f2fstest.Options{Segments: 4}, nil))
		n := 0
		if _, err := f.forEachSIT(func(uint32, []byte) bool { n++; return n < 2 }); err != nil || n != 2 {
			t.Errorf("visits = %d, err = %v; want 2, nil", n, err)
		}
	})
}

func hasWarn(f *FS, sub string) bool {
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestForEachSITStopsAtTruncatedImage(t *testing.T) {
	o := f2fstest.Options{Segments: 6}
	l := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)
	cut := int64(l.Main)*blockSize + (3*blocksPerSeg+100)*blockSize // in the middle of main segment 3
	f := openImg(t, img[:cut])
	var segs []uint32
	if _, err := f.forEachSIT(func(seg uint32, raw []byte) bool {
		if raw == nil {
			t.Errorf("segment %d: unreadable", seg)
		}
		segs = append(segs, seg)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(segs, []uint32{0, 1, 2, 3}) {
		t.Errorf("visited segments %v, want 0..3", segs)
	}
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	if last := runs[len(runs)-1]; last.Offset+last.Length != cut {
		t.Errorf("Unallocated ends at %d, want the cut at %d", last.Offset+last.Length, cut)
	}
}

func TestSITValidBitOrder(t *testing.T) {
	vmap := make([]byte, sitMapSize)
	vmap[0], vmap[1] = 0x80, 0x40
	for i, want := range map[int]bool{0: true, 1: false, 7: false, 8: false, 9: true, 10: false} {
		if got := sitValid(vmap, i); got != want {
			t.Errorf("sitValid(%d) = %v, want %v", i, got, want)
		}
	}
	// Unallocated gives the runs the MSB-first order implies.
	o := f2fstest.Options{Segments: 2, NoSIT: true, SIT: []f2fstest.SITEntry{{Segno: 0, Map: []byte{0x80, 0x40}}}}
	l := f2fstest.Geometry(o)
	f := openImg(t, f2fstest.Build(o, nil))
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	at := func(b uint32) int64 { return int64(l.Main+b) * blockSize }
	want := []filesys.Run{{Offset: at(1), Length: 8 * blockSize}, {Offset: at(10), Length: (2*blocksPerSeg - 10) * blockSize}}
	if !slices.Equal(runs, want) {
		t.Errorf("Unallocated = %v, want %v", runs, want)
	}
}

func TestSSAEntryAndType(t *testing.T) {
	o := f2fstest.Options{Segments: 4}
	l := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)
	f2fstest.SetSSAEntry(img, o, l.Main+blocksPerSeg+5, 4242) // segment 1, entry 5
	f2fstest.SetSSAType(img, o, 1, sumTypeNode)
	f := openImg(t, img)
	b, err := f.ssaBlock(1)
	if err != nil || len(b) != blockSize {
		t.Fatalf("ssaBlock(1) = %d bytes, %v", len(b), err)
	}
	if got := ssaEntryNID(b, 5); got != 4242 {
		t.Errorf("ssaEntryNID(5) = %d, want 4242", got)
	}
	if got := ssaEntryNID(b, 4); got != 0 {
		t.Errorf("ssaEntryNID(4) = %d, want 0", got)
	}
	if got := ssaType(b); got != sumTypeNode {
		t.Errorf("ssaType = %d, want %d", got, sumTypeNode)
	}
	b0, err := f.ssaBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ssaType(b0); got != 0 {
		t.Errorf("ssaType of segment 0 = %d, want 0", got)
	}
	if _, err := f.ssaBlock(f.sb.segMain); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("ssaBlock past the main area: err = %v, want a corruption error", err)
	}
}

func TestNATPrevAddr(t *testing.T) {
	put := func(img []byte, blk uint32, nid, addr uint32) {
		off := int(nid%natEntriesPerBlock) * natEntrySize
		b := blockAt(img, blk)
		b[off] = 1
		binary.LittleEndian.PutUint32(b[off+1:], nid)
		binary.LittleEndian.PutUint32(b[off+5:], addr)
	}
	for name, tc := range map[string]struct {
		bitmap []byte
		older  func(l f2fstest.Layout) uint32 // block that holds the older copy
	}{
		"second copy selected": {[]byte{0x80}, func(l f2fstest.Layout) uint32 { return l.NATBlock(0, false) }},
		"first copy selected":  {nil, func(l f2fstest.Layout) uint32 { return l.NATBlock(0, true) }},
	} {
		t.Run(name, func(t *testing.T) {
			o := f2fstest.Options{Segments: 4, NATBitmap: tc.bitmap, Nodes: []f2fstest.Node{{NID: 4, Addr: 0}}}
			l := f2fstest.Geometry(o)
			img := f2fstest.Build(o, nil)
			put(img, tc.older(l), 4, l.Main+3)
			f := openImg(t, img)
			if got, err := f.natPrevAddr(4); err != nil || got != l.Main+3 {
				t.Errorf("natPrevAddr(4) = %d, %v; want %d", got, err, l.Main+3)
			}
			if cur, err := f.natLookup(4); err != nil || cur != l.Main {
				t.Errorf("natLookup(4) = %d, %v; the current copy must stay %d", cur, err, l.Main)
			}
			if got, err := f.natPrevAddr(5); err != nil || got != 0 {
				t.Errorf("natPrevAddr(5) = %d, %v; want 0 (none)", got, err)
			}
			if _, err := f.natPrevAddr(0); err == nil {
				t.Error("natPrevAddr(0) succeeded")
			}
			if _, err := f.natPrevAddr(uint32(f.sb.natCapacity())); !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("natPrevAddr past the NAT: err = %v, want a corruption error", err)
			}
		})
	}
}

func TestAreaAtInlineAndBlock(t *testing.T) {
	o := f2fstest.Options{Segments: 4}
	files := []f2fstest.File{{Path: "/inl", Dir: true, Inline: true}, {Path: "/big", Dir: true}}
	for i := range 24 { // 150-byte names take 19 slots: 11 per block, so three blocks
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/big/%s%02d", strings.Repeat("n", 148), i), Inline: true})
	}
	img, tree := f2fstest.BuildTree(o, files)
	f := openImg(t, img)

	inl, err := f.inode(tree.NID["/inl"])
	if err != nil {
		t.Fatal(err)
	}
	a, first, err := f.areaAt(inl, 0)
	if err != nil || !first || !a.inlined || a.max < 1 || len(a.b) == 0 {
		t.Fatalf("areaAt(inline, 0) = %+v, %v, %v", a, first, err)
	}
	if _, _, err := f.areaAt(inl, 1); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("areaAt(inline, 1): err = %v, want ErrNotFound", err)
	}

	big, err := f.inode(tree.NID["/big"])
	if err != nil {
		t.Fatal(err)
	}
	blocks := tree.DirBlocks["/big"]
	if len(blocks) < 2 {
		t.Fatalf("test bug: /big has %d dentry blocks", len(blocks))
	}
	for i, addr := range blocks {
		a, first, err := f.areaAt(big, int64(i))
		if err != nil {
			t.Fatalf("areaAt(big, %d): %v", i, err)
		}
		if first != (i == 0) || a.inlined || a.max != nrDentryInBlock || a.ents != dentriesOff || a.names != filenamesOff {
			t.Errorf("areaAt(big, %d) = %+v, first %v", i, a, first)
		}
		if !bytes.Equal(a.b, blockAt(img, addr)) {
			t.Errorf("areaAt(big, %d) holds other bytes than the dentry block at %d", i, addr)
		}
	}
	if _, _, err := f.areaAt(big, int64(len(blocks))); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("areaAt past the size: err = %v, want ErrNotFound", err)
	}
	if _, _, err := f.areaAt(big, -1); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("areaAt(-1): err = %v, want ErrNotFound", err)
	}

	// A hole in the middle of the directory: NULL_ADDR in i_addr[1].
	holed := slices.Clone(img)
	binary.LittleEndian.PutUint32(blockAt(holed, tree.Addr[tree.NID["/big"]])[big.addrStart+4:], 0)
	fh := openImg(t, holed)
	bigH, err := fh.inode(tree.NID["/big"])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fh.areaAt(bigH, 1); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("areaAt(hole): err = %v, want ErrNotFound", err)
	}
	if _, _, err := fh.areaAt(bigH, 0); err != nil {
		t.Errorf("areaAt(0) beside a hole: %v", err)
	}
}
