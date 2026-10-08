package ext4_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// dirOf builds an extents image with /d holding the named files a, b (b in a
// second directory block) and returns it with the image offset of /d's inode.
func dirOf(t *testing.T) (img []byte, dino int) {
	t.Helper()
	files := []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: []byte("a")},
		{Path: "/d/b", Data: []byte("b"), NewBlock: true},
	}
	img = ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	return img, inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
}

func TestZeroISizeDirectoryWithMappedBlocks(t *testing.T) {
	img, dino := dirOf(t)
	put32(img, dino+4, 0) // i_size 0 although two blocks are mapped
	f := mustOpen(t, img)
	es := readDir(t, f, "/d")
	if got := names(es); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("entries %v, want both blocks read despite i_size 0", got)
	}
	for _, e := range es {
		if !e.Deleted {
			t.Errorf("%s is live although every block lies beyond i_size", e.Name)
		}
		if v, _ := attr(e, "beyond_isize"); v != "true" {
			t.Errorf("%s attrs %v, want beyond_isize=true", e.Name, e.Attrs)
		}
	}
	if !hasWarning(f.Info(), "has i_size 0") {
		t.Errorf("no i_size 0 warning: %q", f.Info().Warnings)
	}
	if _, err := f.Lookup("/d/a"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of an entry beyond i_size: %v, want ErrNotFound", err)
	}
}

func TestZeroISizeEmptyDirectoryIsQuiet(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/d", Dir: true}})
	f := mustOpen(t, img)
	dino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	put32(img, dino+4, 0)
	// Without any mapped block there is nothing to report.
	put16(img, dino+0x28+2, 0) // eh_entries = 0
	f = mustOpen(t, img)
	if es := readDir(t, f, "/d"); len(es) != 0 {
		t.Errorf("entries %v", names(es))
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings %q", w)
	}
}

func TestEntriesBeyondISizeAreDeletedNotLive(t *testing.T) {
	img, dino := dirOf(t)
	put32(img, dino+4, 1024) // i_size covers block 0 only
	f := mustOpen(t, img)
	es := readDir(t, f, "/d")
	a, b := byName(t, es, "a"), byName(t, es, "b")
	if a.Deleted {
		t.Errorf("a lies inside i_size but is deleted: %+v", a)
	}
	if !b.Deleted {
		t.Errorf("b lies beyond i_size but is live: %+v", b)
	}
	if v, _ := attr(b, "beyond_isize"); v != "true" {
		t.Errorf("b attrs %v, want beyond_isize=true", b.Attrs)
	}
	if v, _ := attr(b, "dirent"); v == "" {
		t.Errorf("b attrs %v, want the dirent location", b.Attrs)
	}
	if _, ok := attr(b, "inode_reused"); ok {
		t.Errorf("b names a live inode that was never reused: %v", b.Attrs)
	}
	if _, err := f.Lookup("/d/b"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of an entry beyond i_size: %v, want ErrNotFound", err)
	}
	if _, err := f.Lookup("/d/a"); err != nil {
		t.Errorf("Lookup of an entry inside i_size: %v", err)
	}
	if _, err := f.Open(b); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open of an entry beyond i_size: %v, want ErrDeleted", err)
	}
	// Walk descends nowhere through it and reports it as deleted.
	var seen []string
	_ = filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, _ error) error {
		if e.Deleted {
			seen = append(seen, p)
		}
		return nil
	})
	if !slices.Equal(seen, []string{"/d/b"}) {
		t.Errorf("deleted entries in the walk: %v", seen)
	}
}

// hostileDir makes /d a 16384-block directory whose every block is a hostile
// slack: valid inode numbers (the superblock claims 2^32-1 inodes), a name_len
// of 255 at every 4-byte step and a '/' only once every 256 bytes, so each
// candidate is rejected only after scanning on average 128 name bytes.
func hostileDir(t *testing.T) *ext4.FS {
	t.Helper()
	const bs, mapped = 4096, 4096 // physical blocks; mapped 4 times = 16384 logical blocks
	files := []ext4test.File{{Path: "/d", Dir: true}, {Path: "/d/a", Data: []byte("a")}}
	img := ext4test.Build(ext4test.Options{Extents: true, BlockSize: bs, BlocksPerGroup: 8192}, files)
	f := mustOpen(t, img)
	root := rootBlock(t, f) / bs
	start := root + 1
	if (start+mapped)*bs > len(img) {
		t.Fatalf("image too small: %d blocks, hostile range ends at %d", len(img)/bs, start+mapped)
	}
	put32(img, 1024+0, 0xFFFFFFFF) // s_inodes_count: every inode number is plausible

	block := make([]byte, bs)
	put32(block, 0, 7)  // one record spanning the block: the rest is its slack
	put16(block, 4, bs) // rec_len
	block[6], block[7] = 1, 1
	block[8] = 'x'
	for i := 12; i < bs; i++ {
		switch {
		case i%256 == 253:
			block[i] = '/'
		case i%4 == 2:
			block[i] = 0xFF
		default:
			block[i] = 'a'
		}
	}
	for b := range mapped {
		copy(img[(start+b)*bs:], block)
	}

	dino := inodeOff(t, f, bs, 256, 128, int(ext4test.InodeNumber(0)))
	put32(img, dino+4, 4*mapped*bs) // i_size: 16384 blocks
	ib := dino + 0x28
	put16(img, ib+2, 4) // four extents, all onto the same physical blocks
	for i := range 4 {
		e := ib + 12 + 12*i
		put32(img, e, uint32(i*mapped))
		put16(img, e+4, mapped)
		put16(img, e+6, 0)
		put32(img, e+8, uint32(start))
	}
	return mustOpen(t, img)
}

func TestHostileSlackWorkIsLinear(t *testing.T) {
	const (
		bs        = 4096
		blocks    = 4 * 4096 // the logical blocks of the hostile directory
		slackPer  = bs - 12  // each block is one 12-byte record and its slack
		totalSize = int64(blocks) * slackPer
	)
	// The work is a count, not a time, so a loaded machine cannot fail it: one unit per 4-byte
	// candidate checked plus one per byte indexed for the name check (once per block). A scan that
	// rescanned for each candidate would cost about 1000 times more.
	read := func(slackCap int64) (work int64, scanned int64, n int) {
		f := hostileDir(t)
		f.SetSlackScanCap(slackCap)
		d, err := f.Lookup("/d")
		if err != nil {
			t.Fatal(err)
		}
		es, err := f.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		return f.SlackWork(), min(slackCap, totalSize), len(es)
	}
	for _, c := range []struct {
		name string
		cap  int64
	}{{"default cap", 16 << 20}, {"uncapped", math.MaxInt64}} {
		work, scanned, n := read(c.cap)
		t.Logf("%s: %d work units over %d slack bytes, %d entries", c.name, work, scanned, n)
		// at most scanned/4 candidates and, per started block, one index of bs+1 bytes
		limit := scanned/4 + (scanned/slackPer+1)*(bs+1)
		if work < scanned/4-int64(blocks) || work > limit {
			t.Errorf("%s: %d work units for %d slack bytes, want within [%d, %d]", c.name, work, scanned, scanned/4-int64(blocks), limit)
		}
	}
}

func TestSlackScanCap(t *testing.T) {
	f := hostileDir(t)
	f.SetSlackScanCap(1 << 20)
	d, err := f.Lookup("/d")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadDir(d); err != nil {
		t.Fatal(err)
	}
	if !hasWarning(f.Info(), "slack") {
		t.Errorf("no slack-cap warning: %q", f.Info().Warnings)
	}

	// Under the cap nothing is reported.
	img, _ := dirOf(t)
	g := mustOpen(t, img)
	g.SetSlackScanCap(1 << 20)
	readDir(t, g, "/d")
	if w := g.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings %q", w)
	}
}

func TestDotRecordsOutsideTheFirstTwoAreReported(t *testing.T) {
	h := newDirImage(t, true)
	copy(h.img[h.dirent[1]+8:], ".") // b's record becomes "."
	f, es, err := h.open(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(es); slices.Contains(got, ".") || slices.Contains(got, "..") {
		t.Errorf("a dot record is listed: %v", got)
	}
	if !hasWarning(f.Info(), `"." or ".."`) {
		t.Errorf("no warning for a misplaced dot record: %q", f.Info().Warnings)
	}

	// The regular "." and ".." do not warn.
	clean := newDirImage(t, true)
	f, _, err = clean.open(t)
	if err != nil || len(f.Info().Warnings) != 0 {
		t.Errorf("clean directory: err %v, warnings %q", err, f.Info().Warnings)
	}
}

func TestDotRecordInLaterBlockIsReported(t *testing.T) {
	img, _ := dirOf(t)
	f := mustOpen(t, img)
	d, _ := f.Lookup("/d")
	runs := dirRuns(t, f, d)
	// Block 1 starts with b's record: make it ".." (inode kept, so it is live).
	off := int(runs[len(runs)-1].Offset) + int(runs[len(runs)-1].Length) - 1024
	if len(runs) == 1 {
		off = int(runs[0].Offset) + 1024
	}
	img[off+6], img[off+8], img[off+9] = 2, '.', '.'
	f = mustOpen(t, img)
	es := readDir(t, f, "/d")
	if slices.Contains(names(es), "..") {
		t.Errorf("a dot record is listed: %v", names(es))
	}
	if !hasWarning(f.Info(), `"." or ".."`) {
		t.Errorf("no warning for a dot record in block 1: %q", f.Info().Warnings)
	}
}

func TestFlagsWithoutFeatureBitWarn(t *testing.T) {
	t.Run("extents", func(t *testing.T) {
		img, _ := dirOf(t)
		put32(img, offIncompat, le32(img, offIncompat)&^0x40) // extent feature off, inodes keep the flag
		f := mustOpen(t, img)
		es := readDir(t, f, "/d") // the flag is still honoured
		if got := names(es); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("entries %v", got)
		}
		if !hasWarning(f.Info(), "extents flag") {
			t.Errorf("no warning: %q", f.Info().Warnings)
		}
	})
	t.Run("inline data", func(t *testing.T) {
		img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true}, []ext4test.File{
			{Path: "/idir", Dir: true, Inline: true}, {Path: "/idir/x", Data: []byte("x")},
		})
		put32(img, offIncompat, le32(img, offIncompat)&^0x8000)
		f := mustOpen(t, img)
		if got := names(readDir(t, f, "/idir")); !slices.Equal(got, []string{"x"}) {
			t.Fatalf("entries %v", got)
		}
		if !hasWarning(f.Info(), "inline data flag") {
			t.Errorf("no warning: %q", f.Info().Warnings)
		}
	})
	t.Run("features present stay quiet", func(t *testing.T) {
		img, _ := dirOf(t)
		f := mustOpen(t, img)
		readDir(t, f, "/d")
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("warnings %q", w)
		}
	})
}

func TestLiveEntryOfFreedInode(t *testing.T) {
	img, _ := dirOf(t)
	f := mustOpen(t, img)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(1))) // /d/a
	put16(img, ino+0x1A, 0)                                             // i_links_count
	put32(img, ino+0x14, 1700000000)                                    // i_dtime
	f = mustOpen(t, img)
	es := readDir(t, f, "/d")
	a, b := byName(t, es, "a"), byName(t, es, "b")
	if a.Deleted {
		t.Errorf("a is a live dirent: %+v", a)
	}
	if v, _ := attr(a, "inode_freed"); v != "true" {
		t.Errorf("a attrs %v, want inode_freed=true", a.Attrs)
	}
	if _, ok := attr(b, "inode_freed"); ok {
		t.Errorf("b is flagged: %v", b.Attrs)
	}
	if !hasWarning(f.Info(), "freed") {
		t.Errorf("no warning: %q", f.Info().Warnings)
	}
}

// eofAt is a ReaderAt that behaves as if the image ended at one block: a read
// that touches it returns no bytes and io.EOF.
type eofAt struct {
	r   *bytes.Reader
	blk int64
}

func (e eofAt) ReadAt(p []byte, off int64) (int, error) {
	if off < (e.blk+1)*tBS && off+int64(len(p)) > e.blk*tBS {
		return 0, io.EOF
	}
	return e.r.ReadAt(p, off)
}

func TestBadDescriptorChecksumGroupIsSkippedWhateverItsFlags(t *testing.T) {
	for _, mode := range []string{"gdt", "meta"} {
		t.Run(mode, func(t *testing.T) {
			img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, UUID: tUUID, GDTCsum: mode == "gdt", MetadataCsum: mode == "meta"}, nil)
			// Group 1 has no BLOCK_UNINIT flag (cleared by the damage or never
			// set) and a checksum that does not match: its bitmap location may
			// be stale or forged.
			put16(img, descOff(1, 32)+0x1E, le16(img, descOff(1, 32)+0x1E)^0x5A5A)
			f := openImg(t, img)
			runs, err := f.Unallocated()
			if err != nil {
				t.Fatal(err)
			}
			got := freeSet(t, img, runs)
			for b := tStart(1); b < tStart(2); b++ {
				if got[b] {
					t.Fatalf("block %d of the group with a bad descriptor checksum reported free", b)
				}
			}
			if !got[tStart(2)+2+tITable] || !got[tStart(0)+60] {
				t.Error("blocks of the intact groups should still be free")
			}
			if !hasWarning(f.Info(), "wrong descriptor checksum") {
				t.Errorf("no skipped-group warning: %q", f.Info().Warnings)
			}
		})
	}

	t.Run("without descriptor checksums nothing is distrusted", func(t *testing.T) {
		img := ext4test.Build(ext4test.Options{Groups: 3, Extents: true, UUID: tUUID}, nil)
		put16(img, descOff(1, 32)+0x1E, 0x5A5A) // bg_checksum is meaningless here
		f := openImg(t, img)
		runs, err := f.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		if got := freeSet(t, img, runs); !got[tStart(1)+2+2+tITable] {
			t.Error("group 1 should be read from its bitmap")
		}
	})
}

func TestUnreadableDescriptorsSkipBlockUninitGroups(t *testing.T) {
	const groups, bpg = 20, 256
	img := ext4test.Build(ext4test.Options{Groups: groups, Extents: true, Bit64: true, GDTCsum: true, UUID: tUUID, BlocksPerGroup: bpg}, nil)
	// Group 2 is BLOCK_UNINIT with a valid checksum.
	put16(img, descOff(2, 64)+0x12, 0x2)
	fixDescCsum(img, 2, 64, "gdt")
	// Descriptor block 1 (groups 16-19) cannot be read.
	f, err := ext4.Open(eofAt{bytes.NewReader(img), 3}, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(f.Info(), "descriptor table truncated: 16 of 20") {
		t.Fatalf("setup: %q", f.Info().Warnings)
	}
	runs, err := f.Unallocated()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, r := range runs {
		for b := r.Offset / tBS; b < (r.Offset+r.Length)/tBS; b++ {
			got[int(b)] = true
		}
	}
	for b := 1 + 2*bpg; b < 1+3*bpg; b++ {
		if got[b] {
			t.Fatalf("block %d of a BLOCK_UNINIT group reported free although other descriptors are unreadable", b)
		}
	}
	if !hasWarning(f.Info(), "BLOCK_UNINIT") {
		t.Errorf("no warning for the skipped BLOCK_UNINIT groups: %q", f.Info().Warnings)
	}
	// A group read from its bitmap is still reported.
	if !got[1+5*bpg+bpg-1] {
		t.Error("the last block of an ordinary group should be free")
	}
}
