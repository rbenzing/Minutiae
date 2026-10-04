package hfsplus_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

var be = binary.BigEndian

func open(t testing.TB, img []byte) *hfsplus.FS {
	t.Helper()
	f, err := hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

func probe(img []byte) bool { return hfsplus.Probe(bytes.NewReader(img), int64(len(img))) }

func build(t testing.TB, o hfsplustest.Options) ([]byte, *hfsplustest.Layout) {
	t.Helper()
	return hfsplustest.BuildLayout(o, nil)
}

// patchHeaders applies fn to both copies of the volume header (their bytes,
// 512 each); rel offsets inside it are the on-disk header offsets.
func patchHeaders(img []byte, lay *hfsplustest.Layout, fn func(vh []byte)) {
	fn(img[lay.PrimaryVH : lay.PrimaryVH+512])
	fn(img[lay.AltVH : lay.AltVH+512])
}

func hasFeature(info filesys.Info, f string) bool { return slices.Contains(info.Features, f) }

func wantCorrupt(t *testing.T, err error) {
	t.Helper()
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("err = %v (%T), want a *filesys.CorruptError", err, err)
	}
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
	plain, _ := build(t, hfsplustest.Options{})
	hfsx, _ := build(t, hfsplustest.Options{HFSX: true, CaseSensitive: true})
	wrapped, _ := build(t, hfsplustest.Options{Wrapper: true})
	wrappedX, _ := build(t, hfsplustest.Options{Wrapper: true, HFSX: true})
	small, _ := build(t, hfsplustest.Options{BlockSize: 512, Blocks: 512})
	for name, img := range map[string][]byte{"hfs+": plain, "hfsx": hfsx, "wrapped": wrapped, "wrapped hfsx": wrappedX, "512-byte blocks": small} {
		if !probe(img) {
			t.Errorf("%s: Probe = false", name)
		}
	}

	no := map[string][]byte{
		"short":          plain[:1535],
		"zeros":          make([]byte, 64<<10),
		"ext4":           ext4test.Build(ext4test.Options{Extents: true}, nil),
		"exfat":          exfattest.Build(exfattest.Options{}, nil),
		"fat12":          fattest.Build(fattest.Options{Type: 12}, nil),
		"fat16":          fattest.Build(fattest.Options{Type: 16}, nil),
		"fat32":          fattest.Build(fattest.Options{Type: 32}, nil),
		"empty":          nil,
		"signature only": append(append(make([]byte, 1024), 'H', '+'), make([]byte, 3000)...),
	}
	for name, img := range no {
		if probe(img) {
			t.Errorf("%s: Probe = true", name)
		}
	}
	// Probe reads nothing from an image too short for a header.
	if hfsplus.Probe(failReader{t}, 1535) {
		t.Error("Probe on a 1535-byte image is true")
	}
}

type failReader struct{ t *testing.T }

func (r failReader) ReadAt([]byte, int64) (int, error) {
	r.t.Error("unexpected read")
	return 0, io.EOF
}

// Header field offsets used to forge headers.
const (
	vSig        = 0
	vVersion    = 2
	vAttributes = 4
	vJournal    = 12
	vBlockSize  = 40
	vTotal      = 44
	vNextCNID   = 64
	vFinderInfo = 80
	vAlloc      = 112
	vExtents    = 192
	vCatalog    = 272
	vAttrs      = 352
	vStartup    = 432

	// Inside a fork data record.
	fLogical = 0
	fBlocks  = 12
	fExtent0 = 16 // each extent is 8 bytes: start, count
)

func TestProbeRejectsBadHeaderSanity(t *testing.T) {
	cases := []struct {
		name string
		fn   func(vh []byte)
	}{
		{"signature H+ with version 5", func(v []byte) { be.PutUint16(v[vVersion:], 5) }},
		{"signature HX with version 4", func(v []byte) { be.PutUint16(v[vSig:], 0x4858); be.PutUint16(v[vVersion:], 4) }},
		{"unknown signature", func(v []byte) { be.PutUint16(v[vSig:], 0x1234) }},
		{"version 0", func(v []byte) { be.PutUint16(v[vVersion:], 0) }},
		{"block size 0", func(v []byte) { be.PutUint32(v[vBlockSize:], 0) }},
		{"block size 513", func(v []byte) { be.PutUint32(v[vBlockSize:], 513) }},
		{"block size 256", func(v []byte) { be.PutUint32(v[vBlockSize:], 256) }},
		{"block size above 1<<30", func(v []byte) { be.PutUint32(v[vBlockSize:], 1<<30+512) }},
		{"totalBlocks 0", func(v []byte) { be.PutUint32(v[vTotal:], 0) }},
		{"nextCatalogID 15", func(v []byte) { be.PutUint32(v[vNextCNID:], 15) }},
		{"nextCatalogID 0", func(v []byte) { be.PutUint32(v[vNextCNID:], 0) }},
		{"catalog fork 0 blocks", func(v []byte) { be.PutUint32(v[vCatalog+fBlocks:], 0) }},
		{"catalog first extent 0 blocks", func(v []byte) { be.PutUint32(v[vCatalog+fExtent0+4:], 0) }},
		{"catalog first extent starts at totalBlocks", func(v []byte) { be.PutUint32(v[vCatalog+fExtent0:], 256) }},
		{"catalog first extent overruns the volume", func(v []byte) { be.PutUint32(v[vCatalog+fExtent0+4:], 1<<31) }},
		{"catalog first extent start+count overflows 32 bits", func(v []byte) {
			be.PutUint32(v[vCatalog+fExtent0:], 0xFFFFFFFF)
			be.PutUint32(v[vCatalog+fExtent0+4:], 0xFFFFFFFF)
		}},
		{"allocation fork 0 blocks", func(v []byte) { be.PutUint32(v[vAlloc+fBlocks:], 0) }},
		{"allocation first extent 0 blocks", func(v []byte) { be.PutUint32(v[vAlloc+fExtent0+4:], 0) }},
		{"allocation first extent outside", func(v []byte) { be.PutUint32(v[vAlloc+fExtent0:], 1000) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, lay := build(t, hfsplustest.Options{})
			tc.fn(img[lay.PrimaryVH : lay.PrimaryVH+512])
			if probe(img) {
				t.Error("Probe = true for a primary header that fails the sanity checks")
			}
			// With both copies bad Open reports a corrupt volume header.
			tc.fn(img[lay.AltVH : lay.AltVH+512])
			_, err := hfsplus.Open(bytes.NewReader(img), int64(len(img)))
			wantCorrupt(t, err)
		})
	}
}

func TestOpenVolumeHeaderFields(t *testing.T) {
	t.Run("hfs+", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{})
		info := open(t, img).Info()
		if info.Type != "hfsplus" || info.BlockSize != 4096 || info.Size != 256*4096 || info.UUID != "0123456789abcdef" {
			t.Errorf("Info = %+v", info)
		}
		if !slices.Equal(info.Features, []string{"case-insensitive"}) {
			t.Errorf("Features = %q", info.Features)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("Warnings = %q", info.Warnings)
		}
		if info.Encrypted || info.Label != "" || len(info.Volumes) != 0 {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("hfsx case-sensitive", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{HFSX: true, CaseSensitive: true})
		info := open(t, img).Info()
		if info.Type != "hfsx" || !slices.Equal(info.Features, []string{"case-sensitive"}) || len(info.Warnings) != 0 {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("hfsx case-insensitive", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{HFSX: true})
		info := open(t, img).Info()
		if info.Type != "hfsx" || !slices.Equal(info.Features, []string{"case-insensitive"}) || len(info.Warnings) != 0 {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("journaled wrapped cnids-reused", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{Journaled: true, Wrapper: true})
		patchHeaders(img, lay, func(v []byte) { be.PutUint32(v[vAttributes:], be.Uint32(v[vAttributes:])|1<<12) })
		info := open(t, img).Info()
		want := []string{"case-insensitive", "journaled", "hfs-wrapper", "cnids-reused"}
		if !slices.Equal(info.Features, want) {
			t.Errorf("Features = %q, want %q", info.Features, want)
		}
		if len(info.Warnings) != 0 {
			t.Errorf("Warnings = %q", info.Warnings)
		}
	})
	t.Run("zero volume id", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{})
		patchHeaders(img, lay, func(v []byte) { clear(v[vFinderInfo+24 : vFinderInfo+32]) })
		if u := open(t, img).Info().UUID; u != "" {
			t.Errorf("UUID = %q, want empty", u)
		}
	})
	t.Run("one id word", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{})
		patchHeaders(img, lay, func(v []byte) {
			be.PutUint32(v[vFinderInfo+24:], 0)
			be.PutUint32(v[vFinderInfo+28:], 0xAB)
		})
		if u := open(t, img).Info().UUID; u != "00000000000000ab" {
			t.Errorf("UUID = %q", u)
		}
	})
	t.Run("512-byte blocks", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{BlockSize: 512, Blocks: 1024, NodeSize: 512, ExtentsNodeSize: 512})
		info := open(t, img).Info()
		if info.BlockSize != 512 || info.Size != 1024*512 || len(info.Warnings) != 0 {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("image longer than the volume", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{ImageExtra: 3000})
		info := open(t, img).Info()
		if info.Size != 256*4096 || len(info.Warnings) != 0 {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("software lock, inconsistent", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{})
		patchHeaders(img, lay, func(v []byte) { be.PutUint32(v[vAttributes:], be.Uint32(v[vAttributes:])|1<<15|1<<14) })
		info := open(t, img).Info()
		if !hasFeature(info, "software-locked") || !hasWarning(info, "inconsistent") {
			t.Errorf("Info = %+v", info)
		}
	})
	t.Run("unknown keyCompareType", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{})
		img[int64(lay.CatalogBlock)*int64(lay.BlockSize)+14+37] = 0x42
		info := open(t, img).Info()
		if len(info.Features) != 0 || !hasWarning(info, "keyCompareType") {
			t.Errorf("Info = %+v", info)
		}
	})
}

func TestAltVolumeHeaderUsedWhenPrimaryBad(t *testing.T) {
	for _, o := range []hfsplustest.Options{
		{PrimaryBad: true},
		{PrimaryBad: true, HFSX: true},
		{PrimaryBad: true, Wrapper: true},
		{PrimaryBad: true, BlockSize: 512, Blocks: 1024, NodeSize: 512, ExtentsNodeSize: 512},
	} {
		img, _ := build(t, o)
		if probe(img) {
			t.Errorf("%+v: Probe claims a volume with a destroyed primary header", o)
		}
		f := open(t, img)
		info := f.Info()
		if !hasWarning(info, "alternate volume header") {
			t.Errorf("%+v: Warnings = %q, want one about the alternate header", o, info.Warnings)
		}
		wantBS := 4096
		if o.BlockSize != 0 {
			wantBS = int(o.BlockSize)
		}
		if info.BlockSize != wantBS {
			t.Errorf("%+v: BlockSize = %d", o, info.BlockSize)
		}
		if info.UUID != "0123456789abcdef" {
			t.Errorf("%+v: UUID = %q", o, info.UUID)
		}
	}
}

func TestBothVolumeHeadersBadIsCorrupt(t *testing.T) {
	img, lay := build(t, hfsplustest.Options{PrimaryBad: true})
	clear(img[lay.AltVH : lay.AltVH+2])
	_, err := hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	wantCorrupt(t, err)

	// A wrapped volume too.
	img, lay = build(t, hfsplustest.Options{PrimaryBad: true, Wrapper: true})
	clear(img[lay.AltVH : lay.AltVH+2])
	_, err = hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	wantCorrupt(t, err)

	// Nothing at all.
	_, err = hfsplus.Open(bytes.NewReader(make([]byte, 64<<10)), 64<<10)
	wantCorrupt(t, err)
	_, err = hfsplus.Open(bytes.NewReader(make([]byte, 100)), 100)
	wantCorrupt(t, err)

	// A bad primary with an alternate that fails its geometry is corrupt too.
	img, lay = build(t, hfsplustest.Options{PrimaryBad: true})
	be.PutUint64(img[lay.AltVH+vCatalog+fLogical:], 1<<40)
	_, err = hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	wantCorrupt(t, err)
}

func TestOpenReadErrorIsNotCorrupt(t *testing.T) {
	boom := errors.New("boom")
	_, err := hfsplus.Open(errReader{boom}, 1<<20)
	if !errors.Is(err, boom) || errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Open = %v, want the read error, not a corrupt volume", err)
	}
}

type errReader struct{ err error }

func (r errReader) ReadAt([]byte, int64) (int, error) { return 0, r.err }

func TestWrapperOffsetFound(t *testing.T) {
	img, lay := build(t, hfsplustest.Options{Wrapper: true, ImageExtra: 700})
	if lay.Base != hfsplustest.WrapperBase || lay.Base == 0 || lay.Base%4096 == 0 {
		t.Fatalf("Base = %d, the test needs a base that is not block aligned", lay.Base)
	}
	f := open(t, img)
	if f.Base() != hfsplustest.WrapperBase {
		t.Errorf("Base = %d, want %d", f.Base(), hfsplustest.WrapperBase)
	}
	info := f.Info()
	if want := hfsplustest.WrapperBase + lay.VolumeBytes; info.Size != want {
		t.Errorf("Size = %d, want base + volume = %d", info.Size, want)
	}
	if !hasFeature(info, "hfs-wrapper") || len(info.Warnings) != 0 || info.Type != "hfsplus" {
		t.Errorf("Info = %+v", info)
	}
	// Reads are relative to the volume start.
	var sig [2]byte
	if n, err := f.ReadVolume(sig[:], 1024); n != 2 || err != nil || string(sig[:]) != "H+" {
		t.Errorf("ReadVolume(1024) = %q, %d, %v", sig[:], n, err)
	}
	var raw [64]byte
	if _, err := f.ReadVolume(raw[:], 1024+280); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[:], img[lay.Base+1024+280:][:64]) {
		t.Error("ReadVolume is not relative to the embedded volume")
	}
	// HFSX in a wrapper.
	imgX, _ := build(t, hfsplustest.Options{Wrapper: true, HFSX: true, CaseSensitive: true})
	fx := open(t, imgX)
	if ix := fx.Info(); ix.Type != "hfsx" || !hasFeature(ix, "case-sensitive") {
		t.Errorf("Info = %+v", ix)
	}
}

func TestWrapperPlainHFSIsUnsupported(t *testing.T) {
	img, lay := build(t, hfsplustest.Options{Wrapper: true})
	be.PutUint16(img[1024+124:], 0) // drEmbedSigWord: no embedded volume
	_, err := hfsplus.Open(bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("Open = %v, want ErrUnsupported", err)
	}
	if probe(img) {
		t.Error("Probe claims a classic HFS volume")
	}
	_ = lay
}

func TestOpenHostileGeometry(t *testing.T) {
	type tc struct {
		name string
		opt  hfsplustest.Options
		fn   func(img []byte, lay *hfsplustest.Layout)
	}
	both := func(fn func(v []byte)) func([]byte, *hfsplustest.Layout) {
		return func(img []byte, lay *hfsplustest.Layout) { patchHeaders(img, lay, fn) }
	}
	ext := func(fork, i int, start, count uint32) func(v []byte) {
		return func(v []byte) {
			be.PutUint32(v[fork+fExtent0+8*i:], start)
			be.PutUint32(v[fork+fExtent0+8*i+4:], count)
		}
	}
	cases := []tc{
		{"allocation extent beyond the volume", hfsplustest.Options{}, both(ext(vAlloc, 1, 200, 100))},
		{"allocation second extent overflows", hfsplustest.Options{}, both(ext(vAlloc, 1, 0xFFFFFFFF, 2))},
		{"catalog second extent beyond the volume", hfsplustest.Options{}, both(ext(vCatalog, 1, 255, 5))},
		{"extents file extent beyond the volume", hfsplustest.Options{}, both(ext(vExtents, 0, 255, 2))},
		{"attributes extent beyond the volume", hfsplustest.Options{}, both(func(v []byte) {
			be.PutUint32(v[vAttrs+fBlocks:], 1)
			ext(vAttrs, 0, 256, 1)(v)
		})},
		{"startup extent beyond the volume", hfsplustest.Options{}, both(func(v []byte) {
			be.PutUint32(v[vStartup+fBlocks:], 4)
			ext(vStartup, 0, 0, 0xFFFFFFFF)(v)
		})},
		{"catalog logical size beyond its blocks", hfsplustest.Options{}, both(func(v []byte) { be.PutUint64(v[vCatalog+fLogical:], 1<<40) })},
		{"allocation logical size beyond its blocks", hfsplustest.Options{}, both(func(v []byte) { be.PutUint64(v[vAlloc+fLogical:], 1<<63) })},
		{"extents logical size beyond its blocks", hfsplustest.Options{}, both(func(v []byte) { be.PutUint64(v[vExtents+fLogical:], 4096*1+1) })},
		{"fork claims more blocks than the volume", hfsplustest.Options{}, both(func(v []byte) { be.PutUint32(v[vCatalog+fBlocks:], 1000) })},
		{"extents file blocks exceed its inline extents", hfsplustest.Options{}, both(func(v []byte) { be.PutUint32(v[vExtents+fBlocks:], 2) })},
		{"extents file inline extents exceed its blocks", hfsplustest.Options{}, both(ext(vExtents, 1, 20, 3))},
		{"catalog inline extents exceed its blocks", hfsplustest.Options{}, both(ext(vCatalog, 1, 100, 100))},
		{"attributes blocks without extents", hfsplustest.Options{}, both(func(v []byte) { be.PutUint32(v[vAttrs+fBlocks:], 5) })},
		{"wrapper embed beyond the image", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint16(img[1024+128:], 0xFFFF)
		}},
		{"wrapper embed start beyond the image", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint16(img[1024+126:], 0xFFFF)
		}},
		{"wrapper block size 0", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint32(img[1024+20:], 0)
		}},
		{"wrapper block size not a multiple of 512", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint32(img[1024+20:], 513)
		}},
		{"wrapper huge block size", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint32(img[1024+20:], 0xFFFFFE00)
		}},
		{"wrapper embed too small for a header", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint16(img[1024+128:], 1)
		}},
		{"wrapper embed empty", hfsplustest.Options{Wrapper: true}, func(img []byte, _ *hfsplustest.Layout) {
			be.PutUint16(img[1024+128:], 0)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := build(t, c.opt)
			c.fn(img, lay)
			cr := &countingReader{r: bytes.NewReader(img)}
			_, err := hfsplus.Open(cr, int64(len(img)))
			wantCorrupt(t, err)
			if cr.n > 64<<10 {
				t.Errorf("Open read %d bytes before failing", cr.n)
			}
		})
	}

	t.Run("volume larger than the image is a warning", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{})
		img = img[:64*4096]
		f := open(t, img)
		info := f.Info()
		if !hasWarning(info, "truncated") {
			t.Errorf("Warnings = %q", info.Warnings)
		}
		if info.Size != int64(len(img)) {
			t.Errorf("Size = %d, want the image size %d", info.Size, len(img))
		}
	})
	t.Run("image cut inside the catalog header", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{})
		img = img[:int64(lay.CatalogBlock)*4096+10]
		f := open(t, img)
		if !hasWarning(f.Info(), "catalog B-tree header") {
			t.Errorf("Warnings = %q", f.Info().Warnings)
		}
	})
	t.Run("truncated wrapped volume", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{Wrapper: true})
		img = img[:len(img)-8192]
		_, err := hfsplus.Open(bytes.NewReader(img), int64(len(img)))
		wantCorrupt(t, err) // the wrapper's embed extent no longer fits
	})
}

type countingReader struct {
	r io.ReaderAt
	n int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

func TestJournalStateWarnings(t *testing.T) {
	t.Run("clean journal", func(t *testing.T) {
		for _, le := range []bool{false, true} {
			img, _ := build(t, hfsplustest.Options{Journaled: true, JournalLittleEndian: le})
			f := open(t, img)
			info := f.Info()
			if !hasFeature(info, "journaled") || len(info.Warnings) != 0 || f.JournalState() != "clean" {
				t.Errorf("le=%v: Info = %+v state %s", le, info, f.JournalState())
			}
		}
	})
	t.Run("pending journal", func(t *testing.T) {
		for _, le := range []bool{false, true} {
			img, _ := build(t, hfsplustest.Options{JournalPending: true, JournalLittleEndian: le})
			f := open(t, img)
			info := f.Info()
			if !hasFeature(info, "journaled") || !hasWarning(info, "pending transactions") || f.JournalState() != "pending" {
				t.Errorf("le=%v: Info = %+v state %s", le, info, f.JournalState())
			}
			if len(info.Warnings) != 1 {
				t.Errorf("le=%v: Warnings = %q, want only the pending one", le, info.Warnings)
			}
		}
	})
	t.Run("not journaled", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{})
		if f := open(t, img); f.JournalState() != "none" {
			t.Errorf("state = %s", f.JournalState())
		}
	})
	t.Run("never initialised journal", func(t *testing.T) {
		img, lay := build(t, hfsplustest.Options{Journaled: true})
		jib := int64(lay.JournalInfoBlock) * int64(lay.BlockSize)
		be.PutUint32(img[jib:], 1|4) // in the file system, needs init
		clear(img[lay.JournalOffset : lay.JournalOffset+64])
		f := open(t, img)
		if len(f.Info().Warnings) != 0 || f.JournalState() != "clean" {
			t.Errorf("Info = %+v state %s", f.Info(), f.JournalState())
		}
	})
	unparsable := []struct {
		name string
		fn   func(img []byte, lay *hfsplustest.Layout)
	}{
		{"journal info block 0", func(img []byte, lay *hfsplustest.Layout) {
			patchHeaders(img, lay, func(v []byte) { be.PutUint32(v[vJournal:], 0) })
		}},
		{"journal info block outside the volume", func(img []byte, lay *hfsplustest.Layout) {
			patchHeaders(img, lay, func(v []byte) { be.PutUint32(v[vJournal:], 256) })
		}},
		{"journal on another device", func(img []byte, lay *hfsplustest.Layout) {
			be.PutUint32(img[int64(lay.JournalInfoBlock)*int64(lay.BlockSize):], 2)
		}},
		{"journal offset beyond the volume", func(img []byte, lay *hfsplustest.Layout) {
			be.PutUint64(img[int64(lay.JournalInfoBlock)*int64(lay.BlockSize)+36:], 1<<62)
		}},
		{"journal size overflows", func(img []byte, lay *hfsplustest.Layout) {
			be.PutUint64(img[int64(lay.JournalInfoBlock)*int64(lay.BlockSize)+44:], ^uint64(0))
		}},
		{"journal header magic", func(img []byte, lay *hfsplustest.Layout) { img[lay.JournalOffset] ^= 0xFF }},
		{"journal header byte-order marker", func(img []byte, lay *hfsplustest.Layout) { img[lay.JournalOffset+4] ^= 0xFF }},
		{"journal start beyond its size", func(img []byte, lay *hfsplustest.Layout) {
			be.PutUint64(img[lay.JournalOffset+8:], 1<<40)
		}},
		{"journal info block beyond the image", func(img []byte, lay *hfsplustest.Layout) {
			// leave the volume intact but make the journal header unreadable
			be.PutUint64(img[int64(lay.JournalInfoBlock)*int64(lay.BlockSize)+36:], uint64(lay.VolumeBytes-8))
			be.PutUint64(img[int64(lay.JournalInfoBlock)*int64(lay.BlockSize)+44:], 8)
		}},
	}
	for _, c := range unparsable {
		t.Run("unparsable: "+c.name, func(t *testing.T) {
			img, lay := build(t, hfsplustest.Options{Journaled: true})
			c.fn(img, lay)
			f := open(t, img)
			info := f.Info()
			if f.JournalState() != "unknown" || len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "journal") {
				t.Errorf("state %s, Warnings = %q", f.JournalState(), info.Warnings)
			}
			if !hasFeature(info, "journaled") {
				t.Errorf("Features = %q", info.Features)
			}
		})
	}
	t.Run("not journaled and not unmounted", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{NotUnmounted: true})
		f := open(t, img) // a warning only
		info := f.Info()
		if len(info.Warnings) != 1 || !hasWarning(info, "not cleanly unmounted") {
			t.Errorf("Warnings = %q", info.Warnings)
		}
	})
	t.Run("journaled, clean journal, unmounted bit clear", func(t *testing.T) {
		img, _ := build(t, hfsplustest.Options{Journaled: true, NotUnmounted: true})
		if w := open(t, img).Info().Warnings; len(w) != 0 {
			t.Errorf("Warnings = %q: the journal covers a volume that was mounted", w)
		}
	})
}

func TestInfoWarningsAreLive(t *testing.T) {
	img, _ := build(t, hfsplustest.Options{})
	f := open(t, img)
	if len(f.Info().Warnings) != 0 {
		t.Fatal("warnings at open")
	}
	f.Warn("later %q", "x")
	f.Warn("later %q", "x")
	if w := f.Info().Warnings; len(w) != 1 || w[0] != `later "x"` {
		t.Errorf("Warnings = %q", w)
	}
}

func TestBuilderVolumeIsSelfConsistent(t *testing.T) {
	// freeBlocks, the bitmap and the layout of the builder agree: count the
	// clear bits among the first totalBlocks and compare with freeBlocks.
	for _, o := range []hfsplustest.Options{{}, {Journaled: true}, {BlockSize: 512, Blocks: 1024, NodeSize: 512, ExtentsNodeSize: 512}, {BlockSize: 1024, Blocks: 300}, {BlockSize: 8192, Blocks: 100, NodeSize: 8192, ExtentsNodeSize: 4096}} {
		img, lay := build(t, o)
		vh := img[lay.PrimaryVH:]
		total := be.Uint32(vh[vTotal:])
		free := be.Uint32(vh[48:])
		bm := img[uint64(lay.AllocBlock)*uint64(lay.BlockSize):]
		var clearBits uint32
		for n := uint32(0); n < total; n++ {
			if bm[n/8]&(0x80>>(n%8)) == 0 {
				clearBits++
			}
		}
		if clearBits != free {
			t.Errorf("%+v: %d clear bits, freeBlocks %d", o, clearBits, free)
		}
		for n := total; n < lay.AllocBlocks*lay.BlockSize*8; n++ {
			if bm[n/8]&(0x80>>(n%8)) == 0 {
				t.Errorf("%+v: padding bit %d is clear", o, n)
				break
			}
		}
		if !bytes.Equal(img[lay.PrimaryVH:lay.PrimaryVH+512], img[lay.AltVH:lay.AltVH+512]) {
			t.Errorf("%+v: the two headers differ", o)
		}
		open(t, img)
	}
}
