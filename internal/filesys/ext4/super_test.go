package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Superblock field offsets (the superblock starts at byte 1024).
const (
	offInodesCount  = 1024 + 0x0
	offBlocksLo     = 1024 + 0x4
	offFirstData    = 1024 + 0x14
	offLogBlockSize = 1024 + 0x18
	offBPG          = 1024 + 0x20
	offIPG          = 1024 + 0x28
	offMagic        = 1024 + 0x38
	offState        = 1024 + 0x3A
	offRev          = 1024 + 0x4C
	offInodeSize    = 1024 + 0x58
	offIncompat     = 1024 + 0x60
	offRoCompat     = 1024 + 0x64
	offVolName      = 1024 + 0x78
	offDescSize     = 1024 + 0xFE
	offFirstMetaBg  = 1024 + 0x104
	offBlocksHi     = 1024 + 0x150
)

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func put16(b []byte, off int, v uint16) {
	binary.LittleEndian.PutUint16(b[off:], v)
}

func put32(b []byte, off int, v uint32) {
	binary.LittleEndian.PutUint32(b[off:], v)
}

func build(o ext4test.Options) []byte { return ext4test.Build(o, nil) }

func open(b []byte) (*ext4.FS, error) { return ext4.Open(bytes.NewReader(b), int64(len(b))) }

func mustOpen(t *testing.T, b []byte) *ext4.FS {
	t.Helper()
	f, err := open(b)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func hasWarning(info filesys.Info, sub string) bool {
	for _, w := range info.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestProbe(t *testing.T) {
	for _, bs := range []int{1024, 2048, 4096} {
		img := build(ext4test.Options{BlockSize: bs})
		if !ext4.Probe(bytes.NewReader(img), int64(len(img))) {
			t.Errorf("block size %d: Probe = false on a built image", bs)
		}
	}
	zeros := make([]byte, 1<<16)
	if ext4.Probe(bytes.NewReader(zeros), int64(len(zeros))) {
		t.Error("Probe = true on zeros")
	}
	img := build(ext4test.Options{BlockSize: 1024})
	if ext4.Probe(bytes.NewReader(img[:2047]), 2047) {
		t.Error("Probe = true when the image is smaller than 2048 bytes")
	}
	if ext4.Probe(bytes.NewReader(nil), 0) {
		t.Error("Probe = true on an empty image")
	}
	bad := bytes.Clone(img)
	put32(bad, offLogBlockSize, 7)
	if ext4.Probe(bytes.NewReader(bad), int64(len(bad))) {
		t.Error("Probe = true with log_block_size 7")
	}
	bad = bytes.Clone(img)
	put16(bad, offMagic, 0xEF54)
	if ext4.Probe(bytes.NewReader(bad), int64(len(bad))) {
		t.Error("Probe = true with the wrong magic")
	}
}

func TestOpenSuperblockFields(t *testing.T) {
	uuid := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	cases := []struct {
		name     string
		opts     ext4test.Options
		wantType string
		features []string
	}{
		{"ext2", ext4test.Options{}, "ext2", []string{"filetype", "sparse_super"}},
		{"ext3", ext4test.Options{Journal: true}, "ext3", []string{"has_journal"}},
		{"ext4", ext4test.Options{Extents: true}, "ext4", []string{"extent"}},
		{"ext4 64bit csum", ext4test.Options{Bit64: true, MetadataCsum: true}, "ext4", []string{"64bit", "metadata_csum"}},
		{"ext4 4k inline encrypt", ext4test.Options{BlockSize: 4096, InlineData: true, Encrypt: true}, "ext4", []string{"inline_data", "encrypt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := c.opts
			if o.BlockSize == 0 {
				o.BlockSize = 1024
			}
			o.Label = "evidence"
			o.UUID = uuid
			o.Groups = 3
			img := build(o)
			f := mustOpen(t, img)
			info := f.Info()
			if info.Type != c.wantType {
				t.Errorf("Type = %q, want %q", info.Type, c.wantType)
			}
			if info.BlockSize != o.BlockSize {
				t.Errorf("BlockSize = %d, want %d", info.BlockSize, o.BlockSize)
			}
			if info.Label != "evidence" {
				t.Errorf("Label = %q", info.Label)
			}
			if want := "00010203-0405-0607-0809-0a0b0c0d0e0f"; info.UUID != want {
				t.Errorf("UUID = %q, want %q", info.UUID, want)
			}
			if info.Size != int64(len(img)) {
				t.Errorf("Size = %d, want %d", info.Size, len(img))
			}
			if f.GroupCount() != 3 {
				t.Errorf("GroupCount = %d, want 3", f.GroupCount())
			}
			if len(info.Warnings) != 0 {
				t.Errorf("Warnings = %q, want none", info.Warnings)
			}
			if info.Encrypted != o.Encrypt {
				t.Errorf("Encrypted = %v, want %v", info.Encrypted, o.Encrypt)
			}
			for _, want := range c.features {
				found := false
				for _, got := range info.Features {
					found = found || got == want
				}
				if !found {
					t.Errorf("Features = %q, missing %q", info.Features, want)
				}
			}
		})
	}
}

func TestOpenRev0IgnoresFeaturesAndInodeSize(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024, Extents: true})
	put32(img, offRev, 0)
	put16(img, offInodeSize, 0) // not read in revision 0
	f := mustOpen(t, img)
	if got := f.Info().Type; got != "ext2" {
		t.Errorf("Type = %q, want ext2 (features are ignored in revision 0)", got)
	}
}

func TestOpenRejectsBigalloc(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put32(img, offRoCompat, le32(img, offRoCompat)|0x200)
	_, err := open(img)
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestOpenRejectsUnknownIncompat(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put32(img, offIncompat, le32(img, offIncompat)|0x400000)
	_, err := open(img)
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "0x400000") {
		t.Errorf("error %q does not name the bit", err)
	}
	// compression (0x1) is also unknown to this reader
	img = build(ext4test.Options{BlockSize: 1024})
	put32(img, offIncompat, le32(img, offIncompat)|0x1)
	if _, err := open(img); !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("compression: err = %v, want ErrUnsupported", err)
	}
}

func TestOpenRejectsJournalDevice(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put32(img, offIncompat, le32(img, offIncompat)|0x8)
	if _, err := open(img); !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestOpenBadMagicIsCorrupt(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put16(img, offMagic, 0x1234)
	_, err := open(img)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("err = %v, want a CorruptError", err)
	}
}

func TestOpenBadSuperblockChecksumWarns(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024, MetadataCsum: true, Label: "vol"})
	if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
		t.Fatalf("clean image has warnings %q", w)
	}
	img[offVolName] ^= 0xFF
	f, err := open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !hasWarning(f.Info(), "superblock checksum") {
		t.Errorf("Warnings = %q, want a superblock checksum warning", f.Info().Warnings)
	}
}

func TestOpenStateWarnings(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put16(img, offState, 0x2) // error flag, not cleanly unmounted
	put32(img, offIncompat, le32(img, offIncompat)|0x4)
	info := mustOpen(t, img).Info()
	if !hasWarning(info, "recovery") {
		t.Errorf("Warnings = %q, want a journal recovery warning", info.Warnings)
	}
	if !hasWarning(info, "errors") {
		t.Errorf("Warnings = %q, want an errors-flag warning", info.Warnings)
	}
}

// smallGroups shrinks the group geometry (16 blocks and 8 inodes per group) so
// that a large blocks_count means a huge number of groups.
func smallGroups(b []byte) {
	put32(b, offBPG, 16)
	put32(b, offIPG, 8)
}

func TestOpenHostileGeometry(t *testing.T) {
	base := ext4test.Options{BlockSize: 1024}
	b64 := ext4test.Options{BlockSize: 1024, Bit64: true}
	cases := []struct {
		name string
		opts ext4test.Options
		mut  func(b []byte)
	}{
		{"blocks_per_group 0", base, func(b []byte) { put32(b, offBPG, 0) }},
		{"blocks_per_group beyond one bitmap", base, func(b []byte) { put32(b, offBPG, 8*1024+8) }},
		{"inodes_per_group 0", base, func(b []byte) { put32(b, offIPG, 0) }},
		{"inodes_per_group beyond one bitmap", base, func(b []byte) { put32(b, offIPG, 8*1024+8) }},
		{"inodes_count 0", base, func(b []byte) { put32(b, offInodesCount, 0) }},
		{"inode_size 0", base, func(b []byte) { put16(b, offInodeSize, 0) }},
		{"inode_size 100", base, func(b []byte) { put16(b, offInodeSize, 100) }},
		{"inode_size 192 (not a power of two)", base, func(b []byte) { put16(b, offInodeSize, 192) }},
		{"inode_size larger than block", base, func(b []byte) { put16(b, offInodeSize, 2048) }},
		{"log_block_size 7", base, func(b []byte) { put32(b, offLogBlockSize, 7) }},
		{"log_block_size max", base, func(b []byte) { put32(b, offLogBlockSize, 0xFFFFFFFF) }},
		{"inode table larger than a group", base, func(b []byte) { put32(b, offIPG, 8*1024); put32(b, offInodesCount, 8*1024) }},
		{"first_data_block beyond blocks_count", base, func(b []byte) { put32(b, offFirstData, 0xFFFFFFF0) }},
		{"blocks_count 0", base, func(b []byte) { put32(b, offBlocksLo, 0) }},
		{"desc_size 8", b64, func(b []byte) { put16(b, offDescSize, 8) }},
		{"desc_size 4096", b64, func(b []byte) { put16(b, offDescSize, 4096) }},
		{"desc_size 96 (not a power of two)", b64, func(b []byte) { put16(b, offDescSize, 96) }},
		{
			"blocks_count beyond int64", b64,
			func(b []byte) { smallGroups(b); put32(b, offBlocksLo, 0xFFFFFFFF); put32(b, offBlocksHi, 0xFFFFFFFF) },
		},
		{
			"group count x desc size overflows int64", b64,
			func(b []byte) { smallGroups(b); put32(b, offBlocksLo, 0xFFFFFFFF); put32(b, offBlocksHi, 0x7FFFFFFF) },
		},
		{
			"descriptor table far beyond the cap", b64,
			func(b []byte) { smallGroups(b); put32(b, offBlocksHi, 0x7FFF) },
		},
		{
			"descriptor table larger than the cap", b64,
			func(b []byte) { smallGroups(b); put32(b, offBlocksLo, 1<<24) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img := build(c.opts)
			c.mut(img)
			start := time.Now()
			f, err := open(img)
			if el := time.Since(start); el > 2*time.Second {
				t.Errorf("Open took %v", el)
			}
			var ce *filesys.CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("Open = (%v, %v), want a CorruptError", f, err)
			}
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("errors.Is(err, ErrCorrupt) = false for %v", err)
			}
		})
	}
}

func TestOpenTinyImages(t *testing.T) {
	for _, n := range []int{0, 1, 1024, 2047, 2048, 3000} {
		if f, err := open(make([]byte, n)); err == nil {
			t.Errorf("size %d: Open = (%v, nil), want an error", n, f)
		}
	}
}

func TestOpenTruncatedImageWarns(t *testing.T) {
	full := build(ext4test.Options{BlockSize: 1024, Groups: 2})
	keep := 1 + 1024 // first_data_block + one group, in blocks
	img := full[:keep*1024]
	f := mustOpen(t, img)
	info := f.Info()
	if !hasWarning(info, "truncated") {
		t.Errorf("Warnings = %q, want a truncation warning", info.Warnings)
	}
	if info.Size != int64(len(img)) {
		t.Errorf("Size = %d, want %d", info.Size, len(img))
	}
	if !hasWarning(info, "outside") {
		t.Errorf("Warnings = %q, want group 1 reported as outside the filesystem", info.Warnings)
	}
	if _, _, _, _, bad := f.Group(1); !bad {
		t.Error("group 1 is not flagged bad")
	}
	if _, _, _, _, bad := f.Group(0); bad {
		t.Error("group 0 is flagged bad")
	}
	// a size that is not a whole number of blocks rounds down
	f = mustOpen(t, full[:len(img)-100])
	if got, want := f.Info().Size, int64(len(img)-1024); got != want {
		t.Errorf("Size = %d, want %d", got, want)
	}
}

func TestGroupDescriptors(t *testing.T) {
	for _, bs := range []int{1024, 4096} {
		t.Run(fmt.Sprint(bs), func(t *testing.T) {
			img := build(ext4test.Options{BlockSize: bs, Groups: 3, MetadataCsum: true, Bit64: true})
			f := mustOpen(t, img)
			if f.GroupCount() != 3 {
				t.Fatalf("GroupCount = %d", f.GroupCount())
			}
			first := uint64(0)
			if bs == 1024 {
				first = 1
			}
			for g := 0; g < 3; g++ {
				bb, ib, it, flags, bad := f.Group(g)
				start := first + uint64(g)*1024
				if bad || flags != 0 {
					t.Errorf("group %d: bad=%v flags=%#x", g, bad, flags)
				}
				if ib != bb+1 || it != ib+1 {
					t.Errorf("group %d: bitmaps/table %d,%d,%d are not consecutive", g, bb, ib, it)
				}
				if bb < start || it >= start+1024 {
					t.Errorf("group %d: locations %d,%d,%d outside [%d,%d)", g, bb, ib, it, start, start+1024)
				}
			}
			// Groups 0 and 1 hold a superblock and descriptor table (sparse_super:
			// 0, 1, 3, 5, 7...), group 2 does not, so its bitmaps start 2 blocks
			// (superblock + one descriptor block) earlier.
			bb0, _, _, _, _ := f.Group(0)
			bb1, _, _, _, _ := f.Group(1)
			bb2, _, _, _, _ := f.Group(2)
			if bb1-(first+1024) != bb0-first {
				t.Errorf("group 1 (backup) bitmap offset %d != group 0's %d", bb1-(first+1024), bb0-first)
			}
			if bb2-(first+2048) != bb0-first-2 {
				t.Errorf("group 2 (no backup) bitmap offset %d, want %d", bb2-(first+2048), bb0-first-2)
			}
		})
	}
}

func TestGroupDescriptorChecksums(t *testing.T) {
	for _, o := range []ext4test.Options{
		{BlockSize: 1024, Groups: 3, MetadataCsum: true},
		{BlockSize: 1024, Groups: 3, MetadataCsum: true, Bit64: true},
		{BlockSize: 4096, Groups: 3, GDTCsum: true},
	} {
		t.Run(fmt.Sprintf("%+v", o), func(t *testing.T) {
			img := build(o)
			if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
				t.Fatalf("clean image has warnings %q", w)
			}
			gdt := 2 * 1024 // 1K blocks: first_data_block 1, so the table starts at block 2
			if o.BlockSize == 4096 {
				gdt = 4096
			}
			size := 32
			if o.Bit64 {
				size = 64
			}
			img[gdt+size+0x4] ^= 0x01 // group 1, inode bitmap location
			info := mustOpen(t, img).Info()
			if !hasWarning(info, "group descriptor checksum") {
				t.Errorf("Warnings = %q, want a group descriptor checksum warning", info.Warnings)
			}
			if o.Bit64 {
				img[gdt+size+0x4] ^= 0x01  // restore
				img[gdt+size+0x20] ^= 0x01 // a byte of the _hi half (bg_block_bitmap_hi)
				if info := mustOpen(t, img).Info(); !hasWarning(info, "group descriptor checksum") {
					t.Errorf("hi-half tamper: Warnings = %q, want a group descriptor checksum warning", info.Warnings)
				}
			}
		})
	}
}

func TestMetaBGDescriptorLocation(t *testing.T) {
	// 17 groups of 64-byte descriptors need two descriptor blocks (16 per
	// 1 KiB block). With META_BG and first_meta_bg=1 the second block lives
	// at the start of group 16 (no superblock backup there) instead of
	// right after the first.
	o := ext4test.Options{BlockSize: 1024, Bit64: true, Groups: 17, BlocksPerGroup: 256}
	img := build(o)
	plain := mustOpen(t, img)
	want := make([][3]uint64, 17)
	for g := range want {
		want[g][0], want[g][1], want[g][2], _, _ = plain.Group(g)
	}
	const bs = 1024
	second := 3 * bs // descriptor block 1 = block 3 (first_data_block 1, table at 2..3)
	moved := 1 + 16*256
	copy(img[moved*bs:(moved+1)*bs], img[second:second+bs])
	clear(img[second : second+bs])
	put32(img, offIncompat, le32(img, offIncompat)|0x10)
	put32(img, offFirstMetaBg, 1)
	f := mustOpen(t, img)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("Warnings = %q", w)
	}
	for g := range want {
		bb, ib, it, _, _ := f.Group(g)
		if [3]uint64{bb, ib, it} != want[g] {
			t.Errorf("group %d = %v, want %v", g, [3]uint64{bb, ib, it}, want[g])
		}
	}
}

func TestChecksumPrimitives(t *testing.T) {
	// CRC-16/MODBUS (init 0xFFFF, as the kernel uses for uninit_bg)
	if got := ext4.CRC16(0xFFFF, []byte("123456789")); got != 0x4B37 {
		t.Errorf("crc16 = %#04x, want 0x4b37", got)
	}
	// the kernel's crc32c has no final inversion: standard CRC-32C is ^raw(~0)
	if got := ^ext4.RawCRC32C(0xFFFFFFFF, []byte("123456789")); got != 0xE3069283 {
		t.Errorf("crc32c = %#08x, want 0xe3069283", got)
	}
	// chaining: raw(raw(s, a), b) == raw(s, a+b)
	a, b := []byte("1234"), []byte("56789")
	if x, y := ext4.RawCRC32C(ext4.RawCRC32C(7, a), b), ext4.RawCRC32C(7, []byte("123456789")); x != y {
		t.Errorf("chained crc %#x != one-shot %#x", x, y)
	}
}

func TestOpenDescriptorTableLargerThanImageOpens(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024, Bit64: true})
	smallGroups(img)
	put32(img, offBlocksLo, 1<<19) // ~32768 groups, a 2 MiB table, in a 1 MiB image
	f, err := open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !hasWarning(f.Info(), "descriptor table truncated") {
		t.Errorf("Warnings = %q", f.Info().Warnings)
	}
	if f.GroupCount() != 1<<19/16 {
		t.Errorf("GroupCount = %d, want %d (unreadable groups stay in the slice)", f.GroupCount(), 1<<19/16)
	}
}

func TestOpenHeadCaptureOfLargeFilesystem(t *testing.T) {
	// 40 groups x 64-byte descriptors = 3 descriptor blocks; keep only the
	// first 3 blocks of the image, so one descriptor block (16 groups) is readable.
	full := build(ext4test.Options{BlockSize: 1024, Bit64: true, Groups: 40})
	whole := mustOpen(t, full)
	f := mustOpen(t, full[:3*1024])
	if !hasWarning(f.Info(), "descriptor table truncated: 16 of 40 groups readable") {
		t.Errorf("Warnings = %q", f.Info().Warnings)
	}
	if f.GroupCount() != 40 {
		t.Fatalf("GroupCount = %d, want 40", f.GroupCount())
	}
	for g := 0; g < 40; g++ {
		bb, ib, it, _, bad := f.Group(g)
		wb, wi, wt, _, _ := whole.Group(g)
		if g < 16 {
			if bb != wb || ib != wi || it != wt {
				t.Errorf("group %d locations differ from the full image", g)
			}
			continue // bitmaps/table are beyond the 3 captured blocks, so bad is expected
		}
		if !bad {
			t.Errorf("group %d beyond the readable table is not flagged bad", g)
		}
	}
	if _, err := f.Inode(11); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Inode in a group beyond the capture = %v, want CorruptError", err)
	}
	if _, err := f.Inode(40 * 128); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Inode in an unreadable group = %v, want CorruptError", err)
	}

	// 64 KiB of a much larger (24 MiB) filesystem: group 0 stays fully usable.
	big := ext4test.Build(ext4test.Options{BlockSize: 1024, Groups: 24, Extents: true},
		[]ext4test.File{{Path: "/a", Mode: 0o600}})
	head := big[:64*1024]
	f = mustOpen(t, head)
	if f.Info().Size != 64*1024 {
		t.Errorf("Size = %d", f.Info().Size)
	}
	in, err := f.Inode(11)
	if err != nil {
		t.Fatalf("Inode(11) in the captured group: %v", err)
	}
	if e := ext4.ToEntry("a", nil, in); e.Mode != 0o100600 {
		t.Errorf("mode %#o", e.Mode)
	}
}

func TestMetaBGTruncatedTable(t *testing.T) {
	o := ext4test.Options{BlockSize: 1024, Bit64: true, Groups: 17, BlocksPerGroup: 256}
	img := build(o)
	put32(img, offIncompat, le32(img, offIncompat)|0x10)
	put32(img, offFirstMetaBg, 1)
	f := mustOpen(t, img[:3*1024]) // descriptor block 0 (block 2) only; block 1 is at group 16
	if !hasWarning(f.Info(), "descriptor table truncated: 16 of 17 groups readable") {
		t.Errorf("Warnings = %q", f.Info().Warnings)
	}
	if _, _, _, _, bad := f.Group(16); !bad {
		t.Error("group 16 is not flagged unreadable")
	}
}

func TestContiguousTableIgnoresFirstDataBlock(t *testing.T) {
	check := func(name string, o ext4test.Options, fdb uint32, blocks uint32) {
		t.Run(name, func(t *testing.T) {
			img := build(o)
			ref := mustOpen(t, img)
			bb, ib, it, _, _ := ref.Group(0)
			put32(img, offFirstData, fdb)
			put32(img, offBlocksLo, blocks)
			f, err := open(img)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			gb, gi, gt, _, _ := f.Group(0)
			if gb != bb || gi != ib || gt != it {
				t.Errorf("group 0 = %d,%d,%d, want %d,%d,%d (table is read from the block after the superblock block)", gb, gi, gt, bb, ib, it)
			}
			if !hasWarning(f.Info(), "first_data_block") {
				t.Errorf("Warnings = %q, want a first_data_block warning", f.Info().Warnings)
			}
		})
	}
	check("1 KiB blocks, first_data_block 0", ext4test.Options{BlockSize: 1024}, 0, 1024)
	check("4 KiB blocks, first_data_block 1", ext4test.Options{BlockSize: 4096}, 1, 1023)
}

func TestRevisionLevelWarning(t *testing.T) {
	img := build(ext4test.Options{BlockSize: 1024})
	put32(img, offRev, 7)
	if !hasWarning(mustOpen(t, img).Info(), "unknown revision level 7") {
		t.Error("no unknown-revision warning")
	}
	if w := mustOpen(t, build(ext4test.Options{BlockSize: 1024})).Info().Warnings; len(w) != 0 {
		t.Errorf("rev 1 image warns: %q", w)
	}
}

func TestFsTypeWhitelist(t *testing.T) {
	const (
		compat   = 1024 + 0x5C
		incompat = 1024 + 0x60
		ro       = 1024 + 0x64
	)
	cases := []struct {
		name string
		mut  func(b []byte)
		want string
	}{
		{"plain", func([]byte) {}, "ext2"},
		{"sparse_super2 (compat 0x200)", func(b []byte) { put32(b, compat, le32(b, compat)|0x200) }, "ext4"},
		{"project (ro 0x2000)", func(b []byte) { put32(b, ro, le32(b, ro)|0x2000) }, "ext4"},
		{"verity (ro 0x8000)", func(b []byte) { put32(b, ro, le32(b, ro)|0x8000) }, "ext4"},
		{"unknown compat bit", func(b []byte) { put32(b, compat, le32(b, compat)|0x100000) }, "ext4"},
		{"journal", func(b []byte) { put32(b, compat, le32(b, compat)|0x4) }, "ext3"},
		{"journal + recover + meta_bg", func(b []byte) {
			put32(b, compat, le32(b, compat)|0x4)
			put32(b, incompat, le32(b, incompat)|0x4|0x10)
		}, "ext3"},
		{"journal + sparse_super2", func(b []byte) { put32(b, compat, le32(b, compat)|0x4|0x200) }, "ext4"},
		{"journal + extents", func(b []byte) { put32(b, compat, le32(b, compat)|0x4); put32(b, incompat, le32(b, incompat)|0x40) }, "ext4"},
		{"journal + project", func(b []byte) { put32(b, compat, le32(b, compat)|0x4); put32(b, ro, le32(b, ro)|0x2000) }, "ext4"},
		{"recover without a journal", func(b []byte) { put32(b, incompat, le32(b, incompat)|0x4) }, "ext4"},
	}
	for _, c := range cases {
		img := build(ext4test.Options{BlockSize: 1024})
		c.mut(img)
		if got := mustOpen(t, img).Info().Type; got != c.want {
			t.Errorf("%s: Type = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuilderCapsImageSize(t *testing.T) {
	defer func() {
		r := recover()
		if s, ok := r.(string); !ok || !strings.Contains(s, "test limit") {
			t.Errorf("panic = %v, want the image size limit message", r)
		}
	}()
	ext4test.Build(ext4test.Options{BlockSize: 4096, BlocksPerGroup: 32768, Groups: 4000}, nil)
	t.Error("Build did not panic")
}

// metaBGImage returns a 17-group image (two 64-byte descriptor blocks at 1 KiB)
// switched to META_BG with the given first_meta_bg and sparse_super setting,
// with descriptor block 1 moved to where the kernel looks for it (at wantBlock).
func metaBGImage(t *testing.T, firstMetaBG uint32, sparse bool, wantBlock int) (*ext4.FS, [][3]uint64) {
	t.Helper()
	img := build(ext4test.Options{BlockSize: 1024, Bit64: true, Groups: 17, BlocksPerGroup: 256})
	plain := mustOpen(t, img)
	want := make([][3]uint64, 17)
	for g := range want {
		want[g][0], want[g][1], want[g][2], _, _ = plain.Group(g)
	}
	const bs = 1024
	copy(img[wantBlock*bs:], img[3*bs:4*bs]) // descriptor block 1 (block 3 in the contiguous layout)
	if wantBlock != 3 {
		clear(img[3*bs : 4*bs])
	}
	put32(img, offIncompat, le32(img, offIncompat)|0x10)
	put32(img, offFirstMetaBg, firstMetaBG)
	if !sparse {
		put32(img, offRoCompat, le32(img, offRoCompat)&^0x1)
	}
	f := mustOpen(t, img)
	return f, want
}

func TestMetaBGLocationRules(t *testing.T) {
	const start16 = 1 + 16*256 // first block of group 16 (first_data_block 1)
	cases := []struct {
		name        string
		firstMetaBG uint32
		sparse      bool
		block       int
	}{
		// first_meta_bg 0: block 0 stays at group 0 + 1 for the superblock
		// (block 2, the same place); block 1 moves to group 16, which has no
		// backup superblock under sparse_super.
		{"first_meta_bg 0, sparse_super", 0, true, start16},
		// without sparse_super every group has a backup, so +1.
		{"first_meta_bg 0, sparse_super cleared", 0, false, start16 + 1},
		{"first_meta_bg 1, sparse_super cleared", 1, false, start16 + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, want := metaBGImage(t, c.firstMetaBG, c.sparse, c.block)
			for g := range want {
				bb, ib, it, _, _ := f.Group(g)
				if [3]uint64{bb, ib, it} != want[g] {
					t.Errorf("group %d = %v, want %v", g, [3]uint64{bb, ib, it}, want[g])
				}
			}
		})
	}
}

func TestMetaBGOneKFirstDataBlockZeroBump(t *testing.T) {
	// 1 KiB blocks with first_data_block 0: group 0's superblock is in block 1
	// and its descriptor block is block 2, so META_BG adds one to the usual
	// "group start + has_super".
	img := build(ext4test.Options{BlockSize: 1024})
	ref := mustOpen(t, img)
	bb, ib, it, _, _ := ref.Group(0)
	put32(img, offFirstData, 0)
	put32(img, offBlocksLo, 1024)
	put32(img, offIncompat, le32(img, offIncompat)|0x10)
	put32(img, offFirstMetaBg, 0)
	f, err := open(img)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	gb, gi, gt, _, bad := f.Group(0)
	if gb != bb || gi != ib || gt != it || bad {
		t.Errorf("group 0 = %d,%d,%d bad=%v, want %d,%d,%d", gb, gi, gt, bad, bb, ib, it)
	}
}

func TestInodeOrphanNextIsNotDeletionTime(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/linked"}, {Path: "/gone", Deleted: true}})
	f := mustOpen(t, img)
	off := inodeOff(t, f, 1024, 256, 128, 11)
	put32(img, off+0x14, 12) // dtime of a linked inode = next inode on the orphan list
	f = mustOpen(t, img)
	e := entryOf(t, f, 11)
	if !e.Times.Deleted.T.IsZero() {
		t.Errorf("linked inode got Deleted time %v", e.Times.Deleted.T)
	}
	if v, _ := attr(e, "orphan_next"); v != "12" {
		t.Errorf("orphan_next = %q, want 12", v)
	}
	g := entryOf(t, f, 12)
	if g.Times.Deleted.T.IsZero() {
		t.Error("unlinked inode lost its Deleted time")
	}
	if _, ok := attr(g, "orphan_next"); ok {
		t.Error("unlinked inode has an orphan_next attr")
	}
	if _, ok := attr(entryOf(t, f, 2), "orphan_next"); ok {
		t.Error("root has an orphan_next attr")
	}
}
