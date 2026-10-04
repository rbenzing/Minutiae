package apfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// On-disk offsets used to patch images. They are written out here from the
// format reference on purpose: the tests must not share constants with the
// reader.
const (
	offMagic       = 32
	offBlockSize   = 36
	offBlockCount  = 40
	offFeatures    = 48
	offIncompat    = 64
	offUUID        = 72
	offDescBlocks  = 104
	offDataBlocks  = 108
	offDescBase    = 112
	offDataBase    = 120
	offDescIndex   = 136
	offDescLen     = 140
	offDataIndex   = 144
	offDataLen     = 148
	offSpaceman    = 152
	offOmap        = 160
	offMaxFS       = 180
	offFlags       = 1264
	offCPMFlags    = 32
	offCPMCount    = 36
	offCPMEntry    = 40
	offOType       = 24
	offOXid        = 16
	typeMap        = 0xc
	physicalFlag   = 0x40000000
	ephemeralFlag  = 0x80000000
	incompatFusion = 0x100
	incompatV1     = 0x1
)

var le = binary.LittleEndian

type image struct {
	t   *testing.T
	b   []byte
	o   apfstest.Options
	g   apfstest.Geo
	bs  int
	blk func(n uint64) []byte
}

func newImage(t *testing.T, o apfstest.Options) *image {
	t.Helper()
	im := &image{t: t, o: o, g: apfstest.Geometry(o), b: apfstest.Build(o)}
	im.bs = im.g.BlockSize
	im.blk = func(n uint64) []byte { return im.b[int(n)*im.bs : (int(n)+1)*im.bs] }
	return im
}

func (im *image) seal(n uint64) { apfstest.Seal(im.b, im.bs, n) }

// patchSupers applies fn to block 0 and to every superblock of the ring, then
// re-seals them.
func (im *image) patchSupers(fn func(b []byte)) {
	fn(im.blk(0))
	im.seal(0)
	for _, cp := range im.g.Checkpoints {
		fn(im.blk(cp.Super))
		im.seal(cp.Super)
	}
}

func (im *image) open() (*apfs.FS, error) { return apfs.Open(bytes.NewReader(im.b), int64(len(im.b))) }

func (im *image) mustOpen() *apfs.FS {
	im.t.Helper()
	f, err := im.open()
	if err != nil {
		im.t.Fatalf("Open: %v", err)
	}
	return f
}

func hasWarn(f *apfs.FS, subs ...string) bool {
	for _, w := range f.Info().Warnings {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(w, s)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestProbe(t *testing.T) {
	img := apfstest.Build(apfstest.Options{})
	if !apfs.Probe(bytes.NewReader(img), int64(len(img))) {
		t.Fatal("Probe rejects a valid container")
	}
	if !apfs.Probe(bytes.NewReader(img[:4096]), 4096) {
		t.Error("Probe needs only the first 4096 bytes")
	}
	if apfs.Probe(bytes.NewReader(img[:4095]), 4095) {
		t.Error("Probe accepts a 4095-byte image")
	}
	if apfs.Probe(bytes.NewReader(img), 100) {
		t.Error("Probe accepts a declared size below 4096")
	}
	if apfs.Probe(bytes.NewReader(make([]byte, 8192)), 8192) {
		t.Error("Probe accepts zeros")
	}
	bad := bytes.Clone(img)
	copy(bad[offMagic:], "NXSX")
	if apfs.Probe(bytes.NewReader(bad), int64(len(bad))) {
		t.Error("Probe accepts a wrong magic")
	}
	notSB := bytes.Clone(img)
	le.PutUint32(notSB[offOType:], ephemeralFlag|5) // a spaceman, not an NX_SUPERBLOCK
	if apfs.Probe(bytes.NewReader(notSB), int64(len(notSB))) {
		t.Error("Probe accepts a block 0 that is not an NX_SUPERBLOCK object")
	}
	if apfs.Probe(failReader{}, 1<<20) {
		t.Error("Probe accepts an unreadable image")
	}
}

type failReader struct{}

func (failReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("boom") }

func TestProbeRequiresValidBlockSize(t *testing.T) {
	img := apfstest.Build(apfstest.Options{})
	for _, tc := range []struct {
		bs   uint32
		want bool
	}{
		{0, false},
		{1, false},
		{512, false},
		{2048, false},
		{3000, false},
		{4095, false},
		{4096, true},
		{4097, false},
		{6144, false},
		{8192, true},
		{16384, true},
		{32768, true},
		{65536, true},
		{65537, false},
		{131072, false},
		{1 << 20, false},
		{1 << 31, false},
		{0xFFFFFFFF, false},
	} {
		b := bytes.Clone(img[:4096])
		le.PutUint32(b[offBlockSize:], tc.bs)
		if got := apfs.Probe(bytes.NewReader(b), 4096); got != tc.want {
			t.Errorf("Probe(block size %d) = %v, want %v", tc.bs, got, tc.want)
		}
	}
}

// Hand-derived vectors: the checksum covers b[8:] as little-endian u32 words
// with both sums reduced modulo 0xFFFFFFFF, then
// c1 = M - (s1+s2)%M and c2 = M - (s1+c1)%M, stored as c2<<32 | c1.
func TestFletcher64(t *testing.T) {
	words := func(ws ...uint32) []byte {
		b := make([]byte, 8+4*len(ws))
		for i, w := range ws {
			le.PutUint32(b[8+4*i:], w)
		}
		return b
	}
	for _, tc := range []struct {
		name string
		in   []byte
		want uint64
	}{
		// s1=1, s2=1: c1 = M-2, c2 = M-(1+M-2) = 1.
		{"one word", words(1), 0x00000001_FFFFFFFD},
		// s1=3, s2=4: c1 = M-7, c2 = M-(3+M-7) = 4.
		{"two words", words(1, 2), 0x00000004_FFFFFFF8},
		// s1=s2=0: c1 = c2 = M, so a zero block does not verify (stored 0).
		{"zero word", words(0), 0xFFFFFFFF_FFFFFFFF},
		// 0xFFFFFFFF reduces to 0, like zero.
		{"modulus word", words(0xFFFFFFFF), 0xFFFFFFFF_FFFFFFFF},
		// s1=s2=M-1: c1 = M-(2M-2)%M = 2, c2 = M-(M-1+2)%M = M-1.
		{"max word", words(0xFFFFFFFE), 0xFFFFFFFE_00000002},
	} {
		if got := apfs.Fletcher64(tc.in); got != tc.want {
			t.Errorf("%s: Fletcher64 = %#x, want %#x", tc.name, got, tc.want)
		}
	}
	// The first 8 bytes (the stored checksum) are not part of the input.
	a, b := words(7, 9), words(7, 9)
	le.PutUint64(b, 0xDEADBEEFDEADBEEF)
	if apfs.Fletcher64(a) != apfs.Fletcher64(b) {
		t.Error("the checksum field influences the checksum")
	}
	// A verified block stores the value; a zero block never verifies.
	blk := words(1, 2, 3, 4)
	le.PutUint64(blk, apfs.Fletcher64(blk))
	if !apfs.ChecksumOK(blk) {
		t.Error("sealed block does not verify")
	}
	blk[20] ^= 1
	if apfs.ChecksumOK(blk) {
		t.Error("a flipped bit verifies")
	}
	for _, n := range []int{0, 4, 8, 12, 4096, 65536} {
		if apfs.ChecksumOK(make([]byte, n)) {
			t.Errorf("a zero block of %d bytes verifies", n)
		}
	}
	if apfs.ChecksumOK(append(words(1), 0)) { // not a whole number of words
		t.Error("a block that is not a multiple of 4 bytes verifies")
	}
}

// The two implementations stay separate; only their outputs are compared.
func TestBuilderAndReaderFletcherAgree(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test input, not security
	sizes := []int{8, 12, 16, 40, 4096, 8192, 65536}
	for _, n := range sizes {
		for _, fill := range []string{"random", "ff", "zero"} {
			b := make([]byte, n)
			switch fill {
			case "random":
				for i := range b {
					b[i] = byte(rng.Uint32())
				}
			case "ff":
				for i := range b {
					b[i] = 0xFF
				}
			}
			if r, w := apfs.Fletcher64(b), apfstest.Fletcher64(b); r != w {
				t.Errorf("size %d %s: reader %#x, builder %#x", n, fill, r, w)
			}
		}
	}
	// Every block of a built container verifies in the reader.
	img := apfstest.Build(apfstest.Options{})
	g := apfstest.Geometry(apfstest.Options{})
	for _, cp := range g.Checkpoints {
		for _, n := range append([]uint64{cp.Super, cp.Spaceman}, cp.Maps...) {
			if !apfs.ChecksumOK(img[int(n)*4096 : (int(n)+1)*4096]) {
				t.Errorf("builder block %d does not verify in the reader", n)
			}
		}
	}
}

func TestOpenSuperblockFields(t *testing.T) {
	uuid := [16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	o := apfstest.Options{BlockSize: 8192, Blocks: 2000, UUID: uuid, CryptoSW: true, Xid: 77}
	im := newImage(t, o)
	f := im.mustOpen()
	n := f.NX()
	if n.BlockSize != 8192 || n.BlockCount != 2000 || n.UUID != uuid {
		t.Errorf("block size %d count %d uuid %x", n.BlockSize, n.BlockCount, n.UUID)
	}
	if n.Xid != 77 || n.NextXid != 78 {
		t.Errorf("xid %d next %d", n.Xid, n.NextXid)
	}
	if n.DescBlocks != 16 || n.DataBlocks != 16 || n.DescBase != im.g.DescBase || n.DataBase != im.g.DataBase {
		t.Errorf("areas: %+v", n)
	}
	if n.SpacemanOid != im.g.SpacemanOid || n.OmapOid != im.g.Omap || n.MaxFS != 1 {
		t.Errorf("oids: spaceman %d omap %d max fs %d", n.SpacemanOid, n.OmapOid, n.MaxFS)
	}
	if n.Incompat != 2 || n.Flags != 4 {
		t.Errorf("incompat %#x flags %#x", n.Incompat, n.Flags)
	}
	in := f.Info()
	if in.Type != "apfs" || in.Label != "" || in.UUID != "12345678-9abc-def0-1122-334455667788" {
		t.Errorf("Info = %+v", in)
	}
	if in.BlockSize != 8192 || in.Size != 2000*8192 {
		t.Errorf("Info size = %d x %d", in.BlockSize, in.Size)
	}
	if !contains(in.Features, "crypto-sw") || !contains(in.Features, "version2") {
		t.Errorf("Features = %v", in.Features)
	}
	if len(in.Warnings) != 0 || len(in.Volumes) != 0 || in.Encrypted {
		t.Errorf("Info = %+v, want no warnings, volumes or encryption", in)
	}
	// Without crypto-sw the feature is absent.
	if in := newImage(t, apfstest.Options{}).mustOpen().Info(); contains(in.Features, "crypto-sw") {
		t.Errorf("Features = %v", in.Features)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestEphemeralMapIsParsed(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	f := im.mustOpen()
	paddr, size, typ, ok := f.Ephemeral(im.g.SpacemanOid)
	if !ok || paddr != im.g.Checkpoints[0].Spaceman || size != 4096 || typ&0xffff != 5 {
		t.Errorf("spaceman mapping = %d %d %#x %v", paddr, size, typ, ok)
	}
	if f.EphemeralCount() != 1 {
		t.Errorf("%d ephemeral objects, want 1", f.EphemeralCount())
	}
	// A multi-block map list collects the entries of every block.
	im = newImage(t, apfstest.Options{MapBlocks: 3, Checkpoints: 2})
	if f := im.mustOpen(); f.EphemeralCount() != 1 || len(f.Info().Warnings) != 0 {
		t.Errorf("multi-block maps: %d objects, %v", f.EphemeralCount(), f.Info().Warnings)
	}
}

func TestReadObjectVerifiesChecksumAndType(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	f := im.mustOpen()
	sp := im.g.Checkpoints[0].Spaceman
	b, typ, xid, err := f.ReadObject(sp, 4096, 5)
	if err != nil || len(b) != 4096 || typ&0xffff != 5 || xid != 10 {
		t.Fatalf("ReadObject(spaceman) = %d bytes, type %#x, xid %d, %v", len(b), typ, xid, err)
	}
	if _, _, _, err := f.ReadObject(sp, 4096, apfs.AnyType); err != nil {
		t.Errorf("any type: %v", err)
	}
	if _, _, _, err := f.ReadObject(sp, 4096, 0xb); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("wrong type: %v, want CorruptError", err)
	}
	// Outside the container, zero or oversized lengths.
	for _, tc := range []struct {
		paddr uint64
		size  int
	}{{uint64(im.g.Blocks), 4096}, {1 << 62, 4096}, {sp, 0}, {sp, -1}, {sp, 4097}, {sp, 1 << 30}} {
		if _, _, _, err := f.ReadObject(tc.paddr, tc.size, apfs.AnyType); err == nil {
			t.Errorf("ReadObject(%d, %d) succeeded", tc.paddr, tc.size)
		}
	}
	// Flip a byte: checksum mismatch carrying the address.
	im.blk(sp)[100] ^= 0xFF
	f = im.mustOpen() // falls back to the older checkpoint
	_, _, _, err = f.ReadObject(sp, 4096, 5)
	if p, ok := apfs.IsChecksumError(err); !ok || p != sp {
		t.Errorf("ReadObject(corrupt) = %v, want a checksum error at %d", err, sp)
	}
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Error("a checksum error does not match ErrCorrupt")
	}
}

func TestCheckpointPicksHighestValidXid(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	// Block 0 is the stale copy of the oldest superblock.
	if x := le.Uint64(im.blk(0)[offOXid:]); x != 8 {
		t.Fatalf("block 0 xid = %d, want the stale 8", x)
	}
	f := im.mustOpen()
	if got := f.NX().Xid; got != 10 {
		t.Errorf("selected xid %d, want 10", got)
	}
	if got, want := f.SuperblockIndex(), im.g.Checkpoints[0].Index; got != want {
		t.Errorf("selected ring index %d, want %d", got, want)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings on a clean container: %v", w)
	}
	// Ring order does not matter: the newest checkpoint written first in the
	// ring is still the newest by xid.
	im = newImage(t, apfstest.Options{Checkpoints: 4, RingStart: 5, Xid: 40})
	if got := im.mustOpen().NX().Xid; got != 40 {
		t.Errorf("RingStart 5: selected xid %d, want 40", got)
	}
	// A single checkpoint is its own stale copy.
	im = newImage(t, apfstest.Options{Checkpoints: 1, Xid: 5})
	if got := im.mustOpen().NX().Xid; got != 5 {
		t.Errorf("single checkpoint: selected xid %d, want 5", got)
	}
}

func TestCheckpointFallsBackToOlderValid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(im *image)
		reason string
	}{
		{"superblock checksum", func(im *image) { im.blk(im.g.Checkpoints[0].Super)[400] ^= 0x55 }, "checksum"},
		{"map without LAST", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint32(im.blk(m)[offCPMFlags:], 0)
			im.seal(m)
		}, "CHECKPOINT_MAP_LAST"},
		{"map with another xid", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint64(im.blk(m)[offOXid:], 9)
			im.seal(m)
		}, "xid"},
		{"map of the wrong type", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint32(im.blk(m)[offOType:], physicalFlag|1)
			im.seal(m)
		}, "type"},
		{"map checksum", func(im *image) { im.blk(im.g.Checkpoints[0].Maps[0])[200] ^= 1 }, "checksum"},
		{"spaceman checksum", func(im *image) { im.blk(im.g.Checkpoints[0].Spaceman)[300] ^= 0x01 }, "spaceman"},
		{"spaceman type", func(im *image) {
			sp := im.g.Checkpoints[0].Spaceman
			le.PutUint32(im.blk(sp)[offOType:], ephemeralFlag|6)
			im.seal(sp)
		}, "spaceman"},
		{"spaceman not mapped", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint32(im.blk(m)[offCPMCount:], 0)
			im.seal(m)
		}, "spaceman"},
		{"mapping outside the data area", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint64(im.blk(m)[offCPMEntry+32:], uint64(im.g.Blocks-1))
			im.seal(m)
		}, "data area"},
		{"mapping count beyond the block", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint32(im.blk(m)[offCPMCount:], 100000)
			im.seal(m)
		}, "mapping"},
		{"mapping of size zero", func(im *image) {
			m := im.g.Checkpoints[0].Maps[0]
			le.PutUint32(im.blk(m)[offCPMEntry+8:], 0)
			im.seal(m)
		}, "size"},
		{"superblock geometry differs from block 0", func(im *image) {
			sb := im.g.Checkpoints[0].Super
			le.PutUint64(im.blk(sb)[offDescBase:], 3)
			im.seal(sb)
		}, "descriptor area"},
		{"superblock with a hostile descriptor length", func(im *image) {
			sb := im.g.Checkpoints[0].Super
			le.PutUint32(im.blk(sb)[offDescLen:], 17)
			im.seal(sb)
		}, "descriptor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := newImage(t, apfstest.Options{})
			tc.mutate(im)
			f := im.mustOpen()
			if got := f.NX().Xid; got != 9 {
				t.Fatalf("selected xid %d, want the older 9", got)
			}
			if got, want := f.SuperblockIndex(), im.g.Checkpoints[1].Index; got != want {
				t.Errorf("selected ring index %d, want %d", got, want)
			}
			if !hasWarn(f, "xid 10", tc.reason) {
				t.Errorf("no warning naming xid 10 and %q: %q", tc.reason, f.Info().Warnings)
			}
		})
	}
}

func TestCheckpointFallsBackRepeatedly(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	im.blk(im.g.Checkpoints[0].Super)[400] ^= 1
	im.blk(im.g.Checkpoints[1].Super)[400] ^= 1
	f := im.mustOpen()
	if got := f.NX().Xid; got != 8 {
		t.Fatalf("selected xid %d, want 8", got)
	}
	if !hasWarn(f, "xid 10") || !hasWarn(f, "xid 9") {
		t.Errorf("warnings = %q, want both rejected xids named", f.Info().Warnings)
	}
}

func TestCheckpointRingWrapAround(t *testing.T) {
	// One checkpoint: map blocks at ring indexes 15 and 0, superblock at 1.
	im := newImage(t, apfstest.Options{Checkpoints: 1, MapBlocks: 2, RingStart: 15, Xid: 12})
	cp := im.g.Checkpoints[0]
	if cp.Index != 1 || cp.MapIndex[0] != 15 || cp.MapIndex[1] != 0 {
		t.Fatalf("builder geometry = %+v, want the superblock at 1 and maps at 15, 0", cp)
	}
	f := im.mustOpen()
	if f.NX().Xid != 12 || f.SuperblockIndex() != 1 || len(f.Info().Warnings) != 0 {
		t.Errorf("xid %d index %d warnings %v", f.NX().Xid, f.SuperblockIndex(), f.Info().Warnings)
	}
	// Two checkpoints; the newest wraps. Breaking its wrapped map block (at
	// the end of the ring) makes it fall back to the older one.
	im = newImage(t, apfstest.Options{Checkpoints: 2, MapBlocks: 2, RingStart: 12, Xid: 20})
	newest := im.g.Checkpoints[0]
	if newest.Index != 1 || newest.MapIndex[0] != 15 {
		t.Fatalf("builder geometry = %+v", newest)
	}
	if f := im.mustOpen(); f.NX().Xid != 20 || len(f.Info().Warnings) != 0 {
		t.Errorf("clean wrapped ring: xid %d warnings %v", f.NX().Xid, f.Info().Warnings)
	}
	im.blk(newest.Maps[0])[500] ^= 1
	f = im.mustOpen()
	if f.NX().Xid != 19 || !hasWarn(f, "xid 20") {
		t.Errorf("xid %d warnings %q, want fallback to 19", f.NX().Xid, f.Info().Warnings)
	}
}

func TestCheckpointNoValidIsCorrupt(t *testing.T) {
	// Every superblock of the ring fails its checksum; block 0 alone is valid
	// but is never trusted.
	im := newImage(t, apfstest.Options{})
	for _, cp := range im.g.Checkpoints {
		im.blk(cp.Super)[400] ^= 1
	}
	assertCorrupt(t, im, "no valid checkpoint")
	// The descriptor area is empty.
	im = newImage(t, apfstest.Options{})
	clear(im.b[int(im.g.DescBase)*im.bs : int(im.g.DescBase+im.g.DescCount)*im.bs])
	assertCorrupt(t, im, "no valid checkpoint")
	// Every checkpoint is missing its spaceman.
	im = newImage(t, apfstest.Options{})
	for _, cp := range im.g.Checkpoints {
		im.blk(cp.Spaceman)[300] ^= 1
	}
	assertCorrupt(t, im, "no valid checkpoint")
}

func assertCorrupt(t *testing.T, im *image, what string) {
	t.Helper()
	f, err := im.open()
	var ce *filesys.CorruptError
	if err == nil || f != nil || !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Open = %v, %v; want a *CorruptError", f, err)
	}
	if what != "" && !strings.Contains(err.Error(), what) {
		t.Errorf("error %q does not mention %q", err, what)
	}
}

func TestBlockZeroBadChecksumWarns(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	im.blk(0)[2000] ^= 0xFF
	f := im.mustOpen()
	if f.NX().Xid != 10 {
		t.Errorf("selected xid %d, want 10", f.NX().Xid)
	}
	if !hasWarn(f, "block 0", "checksum") {
		t.Errorf("no block 0 checksum warning: %q", f.Info().Warnings)
	}
	// A block 0 without the magic is not an APFS container.
	im = newImage(t, apfstest.Options{})
	copy(im.blk(0)[offMagic:], "XXXX")
	assertCorrupt(t, im, "")
}

func TestOpenHostileGeometry(t *testing.T) {
	put32 := func(off int, v uint32) func(b []byte) { return func(b []byte) { le.PutUint32(b[off:], v) } }
	put64 := func(off int, v uint64) func(b []byte) { return func(b []byte) { le.PutUint64(b[off:], v) } }
	for _, tc := range []struct {
		name string
		fn   func(b []byte)
	}{
		{"block size 0", put32(offBlockSize, 0)},
		{"block size 512", put32(offBlockSize, 512)},
		{"block size 3000", put32(offBlockSize, 3000)},
		{"block size 1<<20", put32(offBlockSize, 1<<20)},
		{"block size not a power of two", put32(offBlockSize, 4096+512)},
		{"block count 0", put64(offBlockCount, 0)},
		{"block count overflows the byte size", put64(offBlockCount, 1<<62)},
		{"block count all ones", put64(offBlockCount, ^uint64(0))},
		{"descriptor blocks 0", put32(offDescBlocks, 0)},
		{"descriptor blocks above the cap", put32(offDescBlocks, 1<<20+1)},
		{"descriptor blocks outside the container", put32(offDescBlocks, 1<<20)},
		{"data blocks 0", put32(offDataBlocks, 0)},
		{"data blocks above the cap", put32(offDataBlocks, 1<<20+1)},
		{"data blocks outside the container", put32(offDataBlocks, 5000)},
		{"descriptor area outside the container", put64(offDescBase, 4090)},
		{"descriptor base all ones", put64(offDescBase, ^uint64(0))},
		{"data area outside the container", put64(offDataBase, 4090)},
		{"data base all ones", put64(offDataBase, ^uint64(0)-3)},
		{"areas overlap", put64(offDataBase, 10)},
		{"descriptor area covers block 0", put64(offDescBase, 0)},
		{"too many file systems", put32(offMaxFS, 101)},
		{"descriptor length 0", put32(offDescLen, 0)},
		{"descriptor length beyond the area", put32(offDescLen, 17)},
		{"descriptor length huge", put32(offDescLen, 0xFFFFFFFF)},
		{"ring index beyond the ring", put32(offDescIndex, 16)},
		{"ring index huge", put32(offDescIndex, 0xFFFFFFFF)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := newImage(t, apfstest.Options{})
			im.patchSupers(tc.fn)
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			start := time.Now()
			assertCorrupt(t, im, "")
			runtime.ReadMemStats(&m1)
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("Open took %v", d)
			}
			if a := m1.TotalAlloc - m0.TotalAlloc; a > 4<<20 {
				t.Errorf("Open allocated %d bytes for a hostile geometry", a)
			}
		})
	}
}

// Only the ring superblocks are hostile: block 0 is fine, so the area is
// found, and every candidate is rejected.
func TestHostileRingSuperblocksAreRejected(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	for _, cp := range im.g.Checkpoints {
		le.PutUint32(im.blk(cp.Super)[offDescIndex:], 99)
		im.seal(cp.Super)
	}
	assertCorrupt(t, im, "no valid checkpoint")
}

func TestNonContiguousAreaIsUnsupported(t *testing.T) {
	for _, off := range []int{offDescBlocks, offDataBlocks} {
		im := newImage(t, apfstest.Options{})
		im.patchSupers(func(b []byte) { le.PutUint32(b[off:], le.Uint32(b[off:])|0x80000000) })
		if _, err := im.open(); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("offset %d: Open = %v, want ErrUnsupported", off, err)
		}
	}
}

func TestFusionAndVersion1AreUnsupported(t *testing.T) {
	for _, bit := range []uint64{incompatFusion, incompatV1, incompatFusion | incompatV1 | 2} {
		im := newImage(t, apfstest.Options{})
		im.patchSupers(func(b []byte) { le.PutUint64(b[offIncompat:], bit) })
		if _, err := im.open(); !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("incompat %#x: Open = %v, want ErrUnsupported", bit, err)
		}
	}
	// Unknown incompatible bits only warn.
	im := newImage(t, apfstest.Options{})
	im.patchSupers(func(b []byte) { le.PutUint64(b[offIncompat:], 2|0x10000) })
	f := im.mustOpen()
	if !hasWarn(f, "incompatible", "0x10000") {
		t.Errorf("no warning for an unknown incompatible bit: %q", f.Info().Warnings)
	}
	// Optional features are named.
	im = newImage(t, apfstest.Options{})
	im.patchSupers(func(b []byte) { le.PutUint64(b[offFeatures:], 3) })
	if in := im.mustOpen().Info(); !contains(in.Features, "defrag") || !contains(in.Features, "lcfd") || len(in.Warnings) != 0 {
		t.Errorf("Info = %+v", in)
	}
}

func TestTruncatedImageIsAWarning(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	keep := int(im.g.Free) * im.bs // through the object map tree
	im.b = im.b[:keep]
	f := im.mustOpen()
	in := f.Info()
	if in.Size != int64(keep) {
		t.Errorf("Size = %d, want the clamped %d", in.Size, keep)
	}
	if !hasWarn(f, "truncated") {
		t.Errorf("no truncation warning: %q", in.Warnings)
	}
	// Truncated below every superblock: nothing to open.
	im = newImage(t, apfstest.Options{})
	im.b = im.b[:4096*2]
	assertCorrupt(t, im, "")
}

func TestIOErrorIsNotConvertedToCorruption(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	sentinel := errors.New("device error")
	r := readerFunc(func(p []byte, off int64) (int, error) {
		if off >= int64(im.g.DescBase+3)*int64(im.bs) {
			return 0, sentinel
		}
		return bytes.NewReader(im.b).ReadAt(p, off)
	})
	_, err := apfs.Open(r, int64(len(im.b)))
	if !errors.Is(err, sentinel) || errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open = %v, want the I/O error unchanged", err)
	}
}

type readerFunc func(p []byte, off int64) (int, error)

func (f readerFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

func TestInfoWarningsAreLive(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	f := im.mustOpen()
	before := f.Info().Warnings
	if len(before) != 0 {
		t.Fatalf("warnings on a clean container: %v", before)
	}
	f.Warn("late problem %q", "x")
	f.Warn("late problem %q", "x") // duplicates are recorded once
	after := f.Info().Warnings
	if len(after) != 1 || !strings.Contains(after[0], `late problem "x"`) {
		t.Errorf("warnings = %q", after)
	}
	if len(before) != 0 {
		t.Error("an earlier snapshot changed")
	}
	// Mutating the returned slice does not affect the reader.
	after[0] = "changed"
	if w := f.Info().Warnings; w[0] == "changed" {
		t.Error("Info().Warnings shares storage with the reader")
	}
}

func TestOpenRejectsTinyAndNonContainers(t *testing.T) {
	for _, n := range []int{0, 1, 100, 4095} {
		if _, err := apfs.Open(bytes.NewReader(make([]byte, n)), int64(n)); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("Open(%d bytes) = %v, want CorruptError", n, err)
		}
	}
	if _, err := apfs.Open(bytes.NewReader(make([]byte, 1<<16)), 1<<16); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open(zeros) = %v, want CorruptError", err)
	}
}

// A hostile image must never panic Open, whatever a byte holds.
func TestOpenMutatedSuperblockNeverPanics(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 64, DescBlocks: 8, DataBlocks: 8})
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input, not security
	base := bytes.Clone(im.b)
	targets := []uint64{0, im.g.Checkpoints[0].Super, im.g.Checkpoints[0].Maps[0], im.g.Checkpoints[0].Spaceman}
	for i := 0; i < 600; i++ {
		copy(im.b, base)
		blk := targets[rng.IntN(len(targets))]
		b := im.blk(blk)
		for range 1 + rng.IntN(3) {
			b[rng.IntN(1400)] = byte(rng.IntN(256))
		}
		if i%2 == 0 { // half re-sealed so the mutation reaches the parsers
			im.seal(blk)
		}
		f, err := im.open()
		if err == nil {
			_ = f.Info()
		} else if f != nil {
			t.Fatalf("Open returned an FS together with %v", err)
		}
	}
}

func ExampleFletcher64() {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[8:], 1)
	fmt.Printf("%#x\n", apfs.Fletcher64(b))
	// Output: 0x1fffffffd
}

// A forged ring full of checksum-valid superblocks cannot make Open try them
// all: at most the 256 newest candidates are considered.
func TestForgedCandidatesAreBounded(t *testing.T) {
	im := newImage(t, apfstest.Options{Blocks: 2048, DescBlocks: 1000, DataBlocks: 8})
	proto := bytes.Clone(im.blk(im.g.Checkpoints[0].Super))
	le.PutUint32(proto[offDescLen:], 900) // its "maps" are other superblocks
	for i := range uint64(1000) {
		b := im.blk(im.g.DescBase + i)
		copy(b, proto)
		le.PutUint64(b[offOXid:], 100+i)
		im.seal(im.g.DescBase + i)
	}
	copy(im.blk(0), proto)
	im.seal(0)
	start := time.Now()
	f, err := im.open()
	if err == nil || f != nil || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Open = %v, %v; want a CorruptError", f, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Open took %v", d)
	}
}

// Block 0 supplies only geometry: the checkpoint cursors, the file-system
// count and the volume oids of its (possibly stale) copy mean nothing, so a
// block 0 with nonsense there still mounts through the ring.
func TestBlockZeroSuppliesOnlyGeometry(t *testing.T) {
	im := newImage(t, apfstest.Options{})
	b0 := im.blk(0)
	le.PutUint32(b0[offDescLen:], 0)
	le.PutUint32(b0[offDescIndex:], 0xFFFFFFFF)
	le.PutUint32(b0[offMaxFS:], 101)
	le.PutUint32(b0[offDataIndex:], 0xFFFFFFFF)
	le.PutUint32(b0[offDataLen:], 0xFFFFFFFF)
	im.seal(0)
	f, err := im.open()
	if err != nil {
		t.Fatalf("Open with a nonsense block-0 cursor: %v", err)
	}
	if f.NX().Xid != 10 || f.NX().MaxFS != 1 {
		t.Errorf("selected xid %d max fs %d, want the ring's newest checkpoint", f.NX().Xid, f.NX().MaxFS)
	}
}

// The cursor claims of a checkpoint superblock (where its descriptor blocks and
// data blocks sit) are verified only against single-checkpoint containers: a
// disagreement is a warning, and a checkpoint whose checksums, xids and objects
// verify is still the one used.
func TestCheckpointCursorClaimsAreWarnings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(im *image)
		warn   string
	}{
		{"descriptor cursor disagrees with its ring position", func(im *image) {
			sb := im.g.Checkpoints[0].Super
			le.PutUint32(im.blk(sb)[offDescIndex:], uint32(im.g.Checkpoints[0].DescIndex+1))
			im.seal(sb)
		}, "descriptor cursor"},
		{"mapped object outside the data cursor range", func(im *image) {
			sb := im.g.Checkpoints[0].Super
			le.PutUint32(im.blk(sb)[offDataLen:], 0)
			im.seal(sb)
		}, "data range"},
		{"data index moved off the mapped object", func(im *image) {
			sb := im.g.Checkpoints[0].Super
			le.PutUint32(im.blk(sb)[offDataIndex:], 5)
			im.seal(sb)
		}, "data range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im := newImage(t, apfstest.Options{})
			tc.mutate(im)
			f := im.mustOpen()
			if got := f.NX().Xid; got != 10 {
				t.Fatalf("selected xid %d, want the newest (10) despite the cursor", got)
			}
			if !hasWarn(f, "xid 10", tc.warn, "accepted") {
				t.Errorf("no warning about %q: %q", tc.warn, f.Info().Warnings)
			}
		})
	}
}
