package f2fs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Superblock field offsets, relative to the start of the superblock.
const (
	sbMajorVer       = 4
	sbLogSectorSize  = 8
	sbLogBlockSize   = 16
	sbLogBlocksPSeg  = 20
	sbSegsPerSec     = 24
	sbChecksumOffset = 32
	sbBlockCount     = 36
	sbSectionCount   = 44
	sbSegmentCount   = 48
	sbSegCkpt        = 52
	sbSegSIT         = 56
	sbSegNAT         = 60
	sbSegMain        = 68
	sbSeg0Addr       = 72
	sbCPAddr         = 76
	sbNATAddr        = 84
	sbSSAAddr        = 88
	sbMainAddr       = 92
	sbRootIno        = 96
	sbNodeIno        = 100
	sbVolumeName     = 124
	sbCPPayload      = 1664
	sbFeature        = 2180

	primary = 1024
	backup  = 4096 + 1024

	// Checkpoint header offsets.
	cpValidBlocks    = 16
	cpPackBlocks     = 136
	cpStartSum       = 140
	cpSITBitmapBytes = 156
	cpNATBitmapBytes = 160
	cpChecksumOffset = 164
)

var le = binary.LittleEndian

// smallOpts is the cheapest image: one main-area segment (about 18 MiB, mostly
// untouched zero pages).
func smallOpts() f2fstest.Options { return f2fstest.Options{Segments: 1} }

func open(b []byte) (*f2fs.FS, error) { return f2fs.Open(bytes.NewReader(b), int64(len(b))) }

func mustOpen(t *testing.T, b []byte) *f2fs.FS {
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

// sb32/sb64 patch a superblock field in both copies.
func sb32(img []byte, off int, v uint32) {
	le.PutUint32(img[primary+off:], v)
	le.PutUint32(img[backup+off:], v)
}

func sb64(img []byte, off int, v uint64) {
	le.PutUint64(img[primary+off:], v)
	le.PutUint64(img[backup+off:], v)
}

// cpBlock returns the first block of checkpoint pack 1 or 2.
func cpBlock(img []byte, g f2fstest.Layout, pack int) []byte {
	base := int(g.CP)
	if pack == 2 {
		base += f2fstest.BlocksPerSeg
	}
	return img[base*4096 : (base+1)*4096]
}

// cp32 patches a checkpoint header field of both packs and reseals them.
func cp32(img []byte, g f2fstest.Layout, off int, v uint32) {
	for pack := 1; pack <= 2; pack++ {
		le.PutUint32(cpBlock(img, g, pack)[off:], v)
		f2fstest.SealCheckpoint(img, pack)
	}
}

func asCorrupt(t *testing.T, err error) *filesys.CorruptError {
	t.Helper()
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("error %v is not ErrCorrupt", err)
	}
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CorruptError", err)
	}
	return ce
}

func TestProbe(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)
	if !f2fs.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Error("Probe = false on a built image")
	}
	zeros := make([]byte, 1<<16)
	if f2fs.Probe(bytes.NewReader(zeros), int64(len(zeros))) {
		t.Error("Probe = true on zeros")
	}
	if f2fs.Probe(bytes.NewReader(img[:8191]), 8191) {
		t.Error("Probe = true below 8192 bytes")
	}
	if !f2fs.Probe(bytes.NewReader(img[:8192]), 8192) {
		t.Error("Probe = false on the 8192-byte head")
	}

	// A damaged primary magic still probes through the backup, and vice versa.
	for _, off := range []int{primary, backup} {
		c := slices.Clone(img[:8192])
		le.PutUint32(c[off:], 0)
		if !f2fs.Probe(bytes.NewReader(c), int64(len(c))) {
			t.Errorf("Probe = false with the magic at %d zeroed", off)
		}
	}
	c := slices.Clone(img[:8192])
	le.PutUint32(c[primary:], 0)
	le.PutUint32(c[backup:], 0)
	if f2fs.Probe(bytes.NewReader(c), int64(len(c))) {
		t.Error("Probe = true with both magics zeroed")
	}
	// ext4's magic is not ours.
	ext := make([]byte, 8192)
	le.PutUint16(ext[1024+0x38:], 0xEF53)
	if f2fs.Probe(bytes.NewReader(ext), int64(len(ext))) {
		t.Error("Probe = true on an ext superblock")
	}
}

func TestOpenSuperblockFields(t *testing.T) {
	uuid := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	o := f2fstest.Options{
		Segments: 2, Label: "héllo \U0001F600 f2fs", UUID: uuid,
		ExtraAttr: true, InodeChksum: true, SBChksum: true, InlineXattr: true, Encrypt: true,
	}
	img := f2fstest.Build(o, nil)
	f := mustOpen(t, img)
	info := f.Info()
	g := f2fstest.Geometry(o)
	if info.Type != "f2fs" {
		t.Errorf("Type = %q", info.Type)
	}
	if info.Label != o.Label {
		t.Errorf("Label = %q, want %q", info.Label, o.Label)
	}
	if info.UUID != "01234567-89ab-cdef-fedc-ba9876543210" {
		t.Errorf("UUID = %q", info.UUID)
	}
	if info.BlockSize != 4096 {
		t.Errorf("BlockSize = %d", info.BlockSize)
	}
	if want := int64(g.BlockCount) * 4096; info.Size != want || info.Size != int64(len(img)) {
		t.Errorf("Size = %d, want %d (image %d)", info.Size, want, len(img))
	}
	wantFeat := []string{"encrypt", "extra_attr", "inode_checksum", "flexible_inline_xattr", "sb_checksum"}
	if !slices.Equal(info.Features, wantFeat) {
		t.Errorf("Features = %v, want %v", info.Features, wantFeat)
	}
	if !info.Encrypted {
		t.Error("Encrypted = false with the encrypt feature")
	}
	if len(info.Warnings) != 0 {
		t.Errorf("clean image has warnings: %q", info.Warnings)
	}

	got := f.Geometry()
	want := f2fs.Geometry{
		BlockCount: uint64(g.BlockCount), SegmentCount: g.CPSegs + g.SITSegs + g.NATSegs + g.SSASegs + g.MainSegs,
		SegCkpt: g.CPSegs, SegSIT: g.SITSegs, SegNAT: g.NATSegs, SegSSA: g.SSASegs, SegMain: g.MainSegs,
		Seg0: g.Seg0, CP: g.CP, SIT: g.SIT, NAT: g.NAT, SSA: g.SSA, Main: g.Main,
		RootIno: 3, NodeIno: 1, MetaIno: 2, CPPayload: 0, Feature: 0x1 | 0x8 | 0x20 | 0x40 | 0x800,
		Blocks: int64(g.BlockCount),
	}
	if got != want {
		t.Errorf("Geometry = %+v\nwant       %+v", got, want)
	}

	// The default options give a plain, unencrypted volume with no features.
	plain := mustOpen(t, f2fstest.Build(smallOpts(), nil)).Info()
	if plain.Encrypted || len(plain.Features) != 0 || plain.Label != "" {
		t.Errorf("plain Info = %+v", plain)
	}
	// An unknown feature bit is reported, not hidden.
	nc := f2fstest.Build(smallOpts(), nil)
	sb32(nc, sbFeature, 0x1|0x80000000)
	ni := mustOpen(t, nc).Info()
	if !slices.Equal(ni.Features, []string{"encrypt", "feature:0x80000000"}) {
		t.Errorf("Features with an unknown bit = %v", ni.Features)
	}
}

func TestLabelDecoding(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)
	// An unpaired surrogate becomes U+FFFD; a NUL ends the name.
	put := func(units ...uint16) {
		for i := range 512 {
			var u uint16
			if i < len(units) {
				u = units[i]
			}
			le.PutUint16(img[primary+sbVolumeName+2*i:], u)
		}
	}
	put('a', 0xD800, 'b', 0, 'z')
	if got := mustOpen(t, img).Info().Label; got != "a�b" {
		t.Errorf("Label = %q", got)
	}
	// A name filling all 512 units without a NUL is read in full.
	units := make([]uint16, 512)
	for i := range units {
		units[i] = 'x'
	}
	put(units...)
	if got := mustOpen(t, img).Info().Label; got != strings.Repeat("x", 512) {
		t.Errorf("full label has %d chars", len(got))
	}
}

func TestSuperblockBackupUsedWhenPrimaryBad(t *testing.T) {
	label := "backup-test"
	cases := []struct {
		name string
		opts func(*f2fstest.Options)
		hurt func(img []byte)
		why  string
	}{
		{"bad magic", nil, func(img []byte) { le.PutUint32(img[primary:], 0) }, "bad magic"},
		{"checksum", func(o *f2fstest.Options) { o.SBChksum = true }, func(img []byte) { img[primary+sbVolumeName]++ }, "checksum mismatch"},
		{"geometry", nil, func(img []byte) { le.PutUint32(img[primary+sbLogBlockSize:], 13) }, "log_blocksize"},
		{"zeroed", nil, func(img []byte) { clear(img[primary:4096]) }, "bad magic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := smallOpts()
			o.Label = label
			if tc.opts != nil {
				tc.opts(&o)
			}
			img := f2fstest.Build(o, nil)
			tc.hurt(img)
			f := mustOpen(t, img)
			info := f.Info()
			if info.Label != label {
				t.Errorf("Label = %q, want the backup's %q", info.Label, label)
			}
			if !hasWarning(info, "backup superblock") || !hasWarning(info, tc.why) {
				t.Errorf("want a backup-superblock warning mentioning %q, got %q", tc.why, info.Warnings)
			}
		})
	}

	t.Run("both bad", func(t *testing.T) {
		img := f2fstest.Build(smallOpts(), nil)
		le.PutUint32(img[primary:], 0)
		le.PutUint32(img[backup+sbLogBlockSize:], 7)
		_, err := open(img)
		ce := asCorrupt(t, err)
		if !strings.Contains(ce.Reason, "bad magic") || !strings.Contains(ce.Reason, "backup superblock") || !strings.Contains(ce.Reason, "log_blocksize") {
			t.Errorf("reason does not describe both copies: %q", ce.Reason)
		}
	})

	t.Run("backup bad is not reported", func(t *testing.T) {
		img := f2fstest.Build(smallOpts(), nil)
		clear(img[backup : backup+3072])
		if w := mustOpen(t, img).Info().Warnings; len(w) != 0 {
			t.Errorf("a damaged backup with a good primary produced warnings %q", w)
		}
	})
}

func TestCheckpointPicksHigherValidVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pack2     bool
		wantPack  int
		wantVer   uint64
		wantAddrO func(g f2fstest.Layout) uint32
	}{
		{"pack 1 newer", false, 1, 9, func(g f2fstest.Layout) uint32 { return g.CP }},
		{"pack 2 newer", true, 2, 9, func(g f2fstest.Layout) uint32 { return g.CP + 512 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := smallOpts()
			o.Version, o.Pack2Newer = 9, tc.pack2
			f := mustOpen(t, f2fstest.Build(o, nil))
			cp := f.Checkpoint()
			if cp.Pack != tc.wantPack || cp.Version != tc.wantVer || cp.Addr != tc.wantAddrO(f2fstest.Geometry(o)) {
				t.Errorf("Checkpoint = pack %d ver %d addr %d", cp.Pack, cp.Version, cp.Addr)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("unexpected warnings %q", w)
			}
			g := f2fstest.Geometry(o)
			if cp.PackBlocks != g.PackBlocks || cp.StartSum != g.StartSum {
				t.Errorf("PackBlocks/StartSum = %d/%d, want %d/%d", cp.PackBlocks, cp.StartSum, g.PackBlocks, g.StartSum)
			}
		})
	}

	// Equal versions: pack 1 wins.
	img := f2fstest.Build(smallOpts(), nil)
	g := f2fstest.Geometry(smallOpts())
	le.PutUint64(cpBlock(img, g, 2)[0:], le.Uint64(cpBlock(img, g, 1)[0:]))
	f2fstest.SealCheckpoint(img, 2)
	if cp := mustOpen(t, img).Checkpoint(); cp.Pack != 1 {
		t.Errorf("equal versions picked pack %d", cp.Pack)
	}
}

func TestCheckpointFallsBackToOlderValidPack(t *testing.T) {
	o := smallOpts()
	o.Version = 9
	g := f2fstest.Geometry(o)
	lastOf := func(pack int) int { return int(g.CP) + (pack-1)*512 + int(g.PackBlocks) - 1 }
	cases := []struct {
		name     string
		hurt     func(img []byte)
		wantPack int
		wantVer  uint64
		warn     string
	}{
		{"newer first block", func(img []byte) { cpBlock(img, g, 1)[cpValidBlocks]++ }, 2, 8, "pack 1"},
		{"newer last block", func(img []byte) { img[lastOf(1)*4096+cpValidBlocks]++ }, 2, 8, "pack 1"},
		{"newer last block version", func(img []byte) {
			blk := img[lastOf(1)*4096 : (lastOf(1)+1)*4096]
			le.PutUint64(blk[0:], 8)
			le.PutUint32(blk[4092:], f2fs.RawCRC32(0xF2F52010, blk[:4092])) // valid checksum, disagreeing version
		}, 2, 8, "in the last"},
		{"newer checksum_offset", func(img []byte) { le.PutUint32(cpBlock(img, g, 1)[cpChecksumOffset:], 100) }, 2, 8, "checksum_offset"},
		{"pack 2 hurt, pack 1 used", func(img []byte) { cpBlock(img, g, 2)[cpValidBlocks]++ }, 1, 9, "pack 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := f2fstest.Build(o, nil)
			tc.hurt(img)
			f := mustOpen(t, img)
			cp := f.Checkpoint()
			if cp.Pack != tc.wantPack || cp.Version != tc.wantVer {
				t.Errorf("used pack %d ver %d, want pack %d ver %d", cp.Pack, cp.Version, tc.wantPack, tc.wantVer)
			}
			if !hasWarning(f.Info(), tc.warn) || !hasWarning(f.Info(), "invalid") {
				t.Errorf("want a warning about %q, got %q", tc.warn, f.Info().Warnings)
			}
		})
	}

	t.Run("pack 2 newer and bad, pack 1 older", func(t *testing.T) {
		o2 := o
		o2.Pack2Newer = true
		img := f2fstest.Build(o2, nil)
		cpBlock(img, g, 2)[cpValidBlocks]++
		f := mustOpen(t, img)
		if cp := f.Checkpoint(); cp.Pack != 1 || cp.Version != 8 {
			t.Errorf("used pack %d ver %d", cp.Pack, cp.Version)
		}
		if !hasWarning(f.Info(), "pack 2") {
			t.Errorf("warnings = %q", f.Info().Warnings)
		}
	})
}

func TestCheckpointBothInvalidIsCorrupt(t *testing.T) {
	g := f2fstest.Geometry(smallOpts())
	img := f2fstest.Build(smallOpts(), nil)
	cpBlock(img, g, 1)[cpValidBlocks]++
	cpBlock(img, g, 2)[cpValidBlocks]++
	f, err := open(img)
	if f != nil {
		t.Error("Open returned a filesystem")
	}
	ce := asCorrupt(t, err)
	if !strings.Contains(ce.Reason, "pack 1") || !strings.Contains(ce.Reason, "pack 2") || !strings.Contains(ce.Reason, "checksum mismatch") {
		t.Errorf("reason = %q", ce.Reason)
	}

	// Both packs blank: also corrupt.
	img = f2fstest.Build(smallOpts(), nil)
	clear(cpBlock(img, g, 1))
	clear(cpBlock(img, g, 2))
	_, err = open(img)
	if ce = asCorrupt(t, err); !strings.Contains(ce.Reason, "never written") {
		t.Errorf("reason = %q", ce.Reason)
	}
}

func TestBlankSecondPackIsSilent(t *testing.T) {
	o := smallOpts()
	o.NoPack2 = true
	f := mustOpen(t, f2fstest.Build(o, nil))
	if cp := f.Checkpoint(); cp.Pack != 1 {
		t.Errorf("pack = %d", cp.Pack)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("mkfs-style blank second pack produced warnings %q", w)
	}
}

func TestUncleanCheckpointWarns(t *testing.T) {
	o := smallOpts()
	o.Unclean = true
	f := mustOpen(t, f2fstest.Build(o, nil))
	if !hasWarning(f.Info(), "not an unmount checkpoint") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
	if f.Checkpoint().Flags&1 != 0 {
		t.Error("CP_UMOUNT_FLAG set")
	}
	clean := mustOpen(t, f2fstest.Build(smallOpts(), nil))
	if clean.Checkpoint().Flags&1 == 0 || hasWarning(clean.Info(), "unclean") {
		t.Errorf("clean checkpoint: flags %#x warnings %q", clean.Checkpoint().Flags, clean.Info().Warnings)
	}
}

func TestCheckpointBitmapPlacement(t *testing.T) {
	pattern := func(seed byte) []byte {
		b := make([]byte, 64)
		for i := range b {
			b[i] = seed + byte(i)*3
		}
		return b
	}
	for _, tc := range []struct {
		name    string
		payload int
		large   bool
	}{
		{"inline", 0, false},
		{"payload", 1, false},
		{"large", 0, true},
		{"large with payload", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := smallOpts()
			o.CPPayload, o.LargeNATBitmap = tc.payload, tc.large
			o.NATBitmap, o.SITBitmap = pattern(1), pattern(77)
			f := mustOpen(t, f2fstest.Build(o, nil))
			cp := f.Checkpoint()
			if !bytes.Equal(cp.NATBitmap, o.NATBitmap) || !bytes.Equal(cp.SITBitmap, o.SITBitmap) {
				t.Errorf("bitmaps not read back:\nnat %x\nsit %x", cp.NATBitmap, cp.SITBitmap)
			}
			if (cp.Flags&0x400 != 0) != tc.large {
				t.Errorf("flags = %#x", cp.Flags)
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings %q", w)
			}
		})
	}
}

func TestBitOrderIsMSBFirst(t *testing.T) {
	bm := []byte{0x80, 0x01}
	for i, want := range map[uint32]bool{0: true, 1: false, 7: false, 8: false, 15: true, 16: false, 1 << 31: false} {
		if got := f2fs.TestBit(bm, i); got != want {
			t.Errorf("TestBit(%#x, %d) = %v, want %v", bm, i, got, want)
		}
	}
}

// refCRC32LE is the kernel's bit-at-a-time crc32_le.
func refCRC32LE(crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc ^= uint32(b)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xEDB88320
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func TestRawCRC32MatchesKernelAlgorithm(t *testing.T) {
	// Standard CRC-32 of "123456789" is 0xCBF43926; the raw register has no
	// inversion at either end.
	if got := f2fs.RawCRC32(0xFFFFFFFF, []byte("123456789")); got != ^uint32(0xCBF43926) {
		t.Errorf("raw crc = %#08x", got)
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	for range 50 {
		p := make([]byte, rng.IntN(300))
		for i := range p {
			p[i] = byte(rng.Uint32())
		}
		const seed = 0xF2F52010
		if got, want := f2fs.RawCRC32(seed, p), refCRC32LE(seed, p); got != want {
			t.Fatalf("len %d: %#08x, want %#08x", len(p), got, want)
		}
		cut := rng.IntN(len(p) + 1)
		if f2fs.RawCRC32(f2fs.RawCRC32(seed, p[:cut]), p[cut:]) != f2fs.RawCRC32(seed, p) {
			t.Fatalf("crc does not chain at %d/%d", cut, len(p))
		}
	}
}

func TestOpenHostileGeometry(t *testing.T) {
	base := smallOpts()
	g := f2fstest.Geometry(base)
	img := f2fstest.Build(base, nil)

	cases := []struct {
		name   string
		mutate func(img []byte)
		reason string
	}{
		{"log_blocksize 13", func(b []byte) { sb32(b, sbLogBlockSize, 13) }, "log_blocksize 13"},
		{"log_blocksize 11", func(b []byte) { sb32(b, sbLogBlockSize, 11) }, "log_blocksize 11"},
		{"log_blocksize 0", func(b []byte) { sb32(b, sbLogBlockSize, 0) }, "log_blocksize 0"},
		{"log_blocks_per_seg 8", func(b []byte) { sb32(b, sbLogBlocksPSeg, 8) }, "log_blocks_per_seg 8"},
		{"log_blocks_per_seg 10", func(b []byte) { sb32(b, sbLogBlocksPSeg, 10) }, "log_blocks_per_seg 10"},
		{"log_blocks_per_seg huge", func(b []byte) { sb32(b, sbLogBlocksPSeg, 0xFFFFFFFF) }, "log_blocks_per_seg"},
		{"log_sectorsize 13", func(b []byte) { sb32(b, sbLogSectorSize, 13) }, "log_sectorsize"},
		{"major_ver 2", func(b []byte) { le.PutUint16(b[primary+sbMajorVer:], 2); le.PutUint16(b[backup+sbMajorVer:], 2) }, "major_ver"},
		{"segs_per_sec 0", func(b []byte) { sb32(b, sbSegsPerSec, 0) }, "segs_per_sec"},
		{"section_count 0", func(b []byte) { sb32(b, sbSectionCount, 0) }, "section_count"},
		{"section_count huge", func(b []byte) { sb32(b, sbSectionCount, 0xFFFFFFFF) }, "section_count"},
		{"segment_count huge", func(b []byte) { sb32(b, sbSegmentCount, 0xFFFFFFFF) }, "segment_count"},
		{"segment_count_main huge", func(b []byte) { sb32(b, sbSegMain, 0xFFFFFFFF) }, "main area"},
		{"segment_count_main one too many", func(b []byte) { sb32(b, sbSegMain, g.MainSegs+1) }, "main area"},
		{"segment_count_sit overflows", func(b []byte) { sb32(b, sbSegSIT, 0xFFFFFFFE) }, "SIT area"},
		{"segment_count_nat overflows", func(b []byte) { sb32(b, sbSegNAT, 0xFFFFFFFE) }, "NAT area"},
		{"segment_count_nat grows into SSA", func(b []byte) { sb32(b, sbSegNAT, g.NATSegs+1) }, "SSA area"},
		{"segment_count_ckpt 1", func(b []byte) { sb32(b, sbSegCkpt, 1) }, "checkpoint area has 1 segments"},
		{"segment_count_ckpt 0", func(b []byte) { sb32(b, sbSegCkpt, 0) }, "checkpoint area"},
		{"segment_count_main 0", func(b []byte) { sb32(b, sbSegMain, 0) }, "main area has 0 segments"},
		{"nat_blkaddr = sit_blkaddr", func(b []byte) { sb32(b, sbNATAddr, g.SIT) }, "NAT area"},
		{"ssa_blkaddr before nat", func(b []byte) { sb32(b, sbSSAAddr, g.NAT) }, "SSA area"},
		{"main_blkaddr beyond", func(b []byte) { sb32(b, sbMainAddr, 0xFFFFFFF0) }, "main area"},
		{"cp_blkaddr before segment0", func(b []byte) { sb32(b, sbCPAddr, g.Seg0-1) }, "checkpoint area"},
		{"segment0_blkaddr in the superblocks", func(b []byte) { sb32(b, sbSeg0Addr, 1) }, "segment0_blkaddr"},
		{"block_count below the main area end", func(b []byte) { sb64(b, sbBlockCount, uint64(g.BlockCount)-1) }, "beyond block_count"},
		{"block_count 2^33", func(b []byte) { sb64(b, sbBlockCount, 1<<33) }, "32-bit"},
		{"block_count 2^64-1", func(b []byte) { sb64(b, sbBlockCount, ^uint64(0)) }, "32-bit"},
		{"cp_payload 511", func(b []byte) { sb32(b, sbCPPayload, 511) }, "cp_payload"},
		{"cp_payload huge", func(b []byte) { sb32(b, sbCPPayload, 0xFFFFFFFF) }, "cp_payload"},
		{"root_ino 0", func(b []byte) { sb32(b, sbRootIno, 0) }, "root_ino"},
		{"root_ino = node_ino", func(b []byte) { sb32(b, sbRootIno, 1) }, "root_ino"},
		{"node_ino 0", func(b []byte) { sb32(b, sbNodeIno, 0) }, "node_ino"},

		// Checkpoint packs that carry a good checksum but a bad field.
		{"cp_pack_total_block_count 0", func(b []byte) { cp32(b, g, cpPackBlocks, 0) }, "cp_pack_total_block_count 0"},
		{"cp_pack_total_block_count 1", func(b []byte) { cp32(b, g, cpPackBlocks, 1) }, "cp_pack_total_block_count 1"},
		{"cp_pack_total_block_count 513", func(b []byte) { cp32(b, g, cpPackBlocks, 513) }, "cp_pack_total_block_count 513"},
		{"cp_pack_total_block_count huge", func(b []byte) { cp32(b, g, cpPackBlocks, 0xFFFFFFFF) }, "cp_pack_total_block_count"},
		{"cp_pack_start_sum 0", func(b []byte) { cp32(b, g, cpStartSum, 0) }, "cp_pack_start_sum 0"},
		{"cp_pack_start_sum at the trailing copy", func(b []byte) { cp32(b, g, cpStartSum, g.PackBlocks-1) }, "cp_pack_start_sum"},
		{"cp_pack_start_sum huge", func(b []byte) { cp32(b, g, cpStartSum, 0xFFFFFFFF) }, "cp_pack_start_sum"},
		{"sit bitmap size huge", func(b []byte) { cp32(b, g, cpSITBitmapBytes, 0xFFFFFFFF) }, "sit_ver_bitmap_bytesize"},
		{"sit bitmap size +1", func(b []byte) { cp32(b, g, cpSITBitmapBytes, 65) }, "sit_ver_bitmap_bytesize 65"},
		{"nat bitmap size huge", func(b []byte) { cp32(b, g, cpNATBitmapBytes, 0xFFFFFFFF) }, "nat_ver_bitmap_bytesize"},
		{"nat bitmap size 0", func(b []byte) { cp32(b, g, cpNATBitmapBytes, 0) }, "nat_ver_bitmap_bytesize 0"},
		{"checksum_offset 191", func(b []byte) { cp32(b, g, cpChecksumOffset, 191) }, "checksum_offset 191"},
		{"checksum_offset 4093", func(b []byte) { cp32(b, g, cpChecksumOffset, 4093) }, "checksum_offset 4093"},
		{"checksum_offset 0xFFFFFFFF", func(b []byte) { cp32(b, g, cpChecksumOffset, 0xFFFFFFFF) }, "checksum_offset"},
		{"checksum_offset over the bitmaps", func(b []byte) { cp32(b, g, cpChecksumOffset, 200) }, "overlaps the checksum word"},
		{"bitmaps beyond the block", func(b []byte) {
			// a superblock that makes the checkpoint need payload blocks it lacks
			sb32(b, sbCPPayload, 1)
		}, "cp_pack_start_sum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := slices.Clone(img)
			tc.mutate(c)
			start := time.Now()
			f, err := open(c)
			if f != nil {
				t.Fatal("Open returned a filesystem")
			}
			ce := asCorrupt(t, err)
			if !strings.Contains(ce.Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", ce.Reason, tc.reason)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("took %v", d)
			}
		})
	}

	t.Run("checksum_offset beyond the superblock", func(t *testing.T) {
		o := smallOpts()
		o.SBChksum = true
		for _, off := range []uint32{3069, 3072, 4096, 0xFFFFFFFF} {
			c := f2fstest.Build(o, nil)
			sb32(c, sbChecksumOffset, off)
			_, err := open(c)
			if ce := asCorrupt(t, err); !strings.Contains(ce.Reason, "checksum_offset") {
				t.Errorf("offset %d: reason %q", off, ce.Reason)
			}
		}
	})
}

func TestTruncatedImageIsAWarning(t *testing.T) {
	o := f2fstest.Options{Segments: 3}
	g := f2fstest.Geometry(o)
	img := f2fstest.Build(o, nil)

	// The image is cut short inside the main area.
	cut := img[:(int(g.Main)+f2fstest.BlocksPerSeg+17)*4096+100]
	f := mustOpen(t, cut)
	if !hasWarning(f.Info(), "truncated image") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
	if want := int64(g.Main+f2fstest.BlocksPerSeg+17) * 4096; f.Info().Size != want {
		t.Errorf("Size = %d, want %d", f.Info().Size, want)
	}

	// block_count claims more than the image holds (but the areas still fit).
	more := slices.Clone(img)
	sb64(more, sbBlockCount, uint64(g.BlockCount)+1000)
	f = mustOpen(t, more)
	if !hasWarning(f.Info(), "truncated image") || f.Info().Size != int64(len(img)) {
		t.Errorf("warnings = %q, Size = %d", f.Info().Warnings, f.Info().Size)
	}

	// A head capture that still holds both checkpoint packs opens.
	head := img[:int(g.SIT)*4096]
	if f = mustOpen(t, head); !hasWarning(f.Info(), "truncated image") {
		t.Errorf("head capture: warnings = %q", f.Info().Warnings)
	}
}

func TestOpenTooSmall(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)
	for _, n := range []int{0, 1, 4096, 8191} {
		_, err := open(img[:n])
		_ = asCorrupt(t, err)
	}
	// Both superblocks present but no checkpoint behind them.
	_, err := open(img[:8192])
	if ce := asCorrupt(t, err); ce.Structure != "f2fs checkpoint" {
		t.Errorf("structure = %q", ce.Structure)
	}
	_, err = open(make([]byte, 8192))
	_ = asCorrupt(t, err)
}

func TestOpenMutatedNeverPanics(t *testing.T) {
	img := f2fstest.Build(smallOpts(), nil)
	g := f2fstest.Geometry(smallOpts())
	regions := [][2]int{
		{primary, 4096},
		{backup, 8192},
		{int(g.CP) * 4096, int(g.CP)*4096 + 4096},
		{(int(g.CP) + 512) * 4096, (int(g.CP)+512)*4096 + 4096},
		{(int(g.CP) + int(g.PackBlocks) - 1) * 4096, (int(g.CP) + int(g.PackBlocks)) * 4096},
	}
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input, not security
	for i := range 400 {
		r := regions[rng.IntN(len(regions))]
		type patch struct {
			off int
			old byte
		}
		var undo []patch
		for range 1 + rng.IntN(4) {
			off := r[0] + rng.IntN(r[1]-r[0])
			undo = append(undo, patch{off, img[off]})
			img[off] = byte(rng.Uint32())
		}
		if f, err := open(img); err == nil {
			_ = f.Info()
		} else if !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("iteration %d: error %v is not ErrCorrupt", i, err)
		}
		for j := len(undo) - 1; j >= 0; j-- {
			img[undo[j].off] = undo[j].old
		}
	}
}

func TestInfoWarningsAreLive(t *testing.T) {
	f := mustOpen(t, f2fstest.Build(smallOpts(), nil))
	if len(f.Info().Warnings) != 0 {
		t.Fatalf("warnings = %q", f.Info().Warnings)
	}
	f.Warn("problem %d", 1)
	f.Warn("problem %d", 1)
	f.Warn("problem %d", 2)
	if w := f.Info().Warnings; !slices.Equal(w, []string{"problem 1", "problem 2"}) {
		t.Errorf("warnings = %q", w)
	}
}
