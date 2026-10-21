package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	eaParent = 11 // /x, the first listed file
	eaInum   = 12 // the EA inode the builder creates for it: InodeNumber(len(files))
	eaName   = "user.big"
	eaFlag   = 0x200000
)

// eaValue is a recognisable value of n bytes.
func eaValue(n int) []byte {
	v := make([]byte, n)
	for i := range v {
		v[i] = byte('a' + i%23)
	}
	return v
}

// eaImage is a builder image whose /x carries user.big in an EA inode (inode 12).
type eaImage struct {
	img   []byte
	value []byte
}

func newEAImage(t *testing.T, o ext4test.Options, value []byte, mod func(*ext4test.Xattr)) *eaImage {
	t.Helper()
	o.Extents = true
	x := ext4test.Xattr{Name: eaName, Value: value, EAInode: true}
	if mod != nil {
		mod(&x)
	}
	img := ext4test.Build(o, []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{x}}})
	return &eaImage{img: img, value: value}
}

func (e *eaImage) open(t *testing.T) *ext4.FS { return mustOpen(t, e.img) }

// inode is the byte offset of inode n in the image (1 KiB blocks, 256-byte inodes, 128 per group).
func (e *eaImage) inode(t *testing.T, n int) int {
	t.Helper()
	return inodeOff(t, e.open(t), 1024, 256, 128, n)
}

func readValue(t *testing.T, fl filesys.File) []byte {
	t.Helper()
	out := make([]byte, fl.Size())
	n, err := fl.ReadAt(out, 0)
	if int64(n) != fl.Size() || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt = %d, %v for a %d-byte value", n, err, fl.Size())
	}
	return out
}

func eaWarnings(info filesys.Info) []string {
	var out []string
	for _, w := range info.Warnings {
		if strings.Contains(w, "EA inode") {
			out = append(out, w)
		}
	}
	return out
}

func TestEAInodeValueIsRead(t *testing.T) {
	t.Run("builder", func(t *testing.T) {
		for _, n := range []int{0, 1, 100, 1024, 5000, 70000} { // 70000 spans more than one hash chunk
			e := newEAImage(t, ext4test.Options{MetadataCsum: true}, eaValue(n), nil)
			f := e.open(t)
			fl, bad, err := f.EAInodeValue(eaParent, eaName)
			if err != nil {
				t.Fatalf("%d bytes: %v", n, err)
			}
			if bad {
				t.Errorf("%d bytes: the hash of an intact value failed", n)
			}
			if fl.Size() != int64(n) || !bytes.Equal(readValue(t, fl), e.value) {
				t.Errorf("%d bytes: value differs from the bytes written", n)
			}
			if w := eaWarnings(f.Info()); len(w) != 0 {
				t.Errorf("%d bytes: warnings on an intact value: %q", n, w)
			}
		}
	})
	t.Run("without metadata_csum", func(t *testing.T) {
		e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
		fl, bad, err := e.open(t).EAInodeValue(eaParent, eaName)
		if err != nil || bad || !bytes.Equal(readValue(t, fl), e.value) {
			t.Errorf("EAInodeValue = bad %v, %v", bad, err)
		}
	})
	t.Run("real mke2fs fixture", func(t *testing.T) {
		path := "testdata/ext4-xattr.img.gz"
		want := loadOracle(t, path)
		f := mustOpen(t, gunzipFixture(t, path))
		seen := 0
		for _, a := range want.Xattrs {
			if a.Storage != "ea_inode" {
				continue
			}
			seen++
			n := inodeOfPath(t, f, a.Path)
			fl, bad, err := f.EAInodeValue(uint32(n), a.Name)
			if err != nil {
				t.Fatalf("%s %s: %v", a.Path, a.Name, err)
			}
			sum := sha256.Sum256(readValue(t, fl))
			if fl.Size() != int64(a.Size) || hex.EncodeToString(sum[:]) != a.SHA256 {
				t.Errorf("%s %s: %d bytes sha256 %x, want %d bytes %s (debugfs ea_get)", a.Path, a.Name, fl.Size(), sum, a.Size, a.SHA256)
			}
			if bad {
				t.Errorf("%s %s: the stored hash of a libext2fs-written EA inode did not verify: the hash rule is wrong", a.Path, a.Name)
			}
		}
		if seen != 1 {
			t.Fatalf("the oracle holds %d EA-inode attributes, want 1", seen)
		}
		if w := eaWarnings(f.Info()); len(w) != 0 {
			t.Errorf("warnings on the fixture: %q", w)
		}
	})
}

func TestEAInodeRunsAreRealImageRuns(t *testing.T) {
	e := newEAImage(t, ext4test.Options{MetadataCsum: true}, eaValue(9000), nil)
	f := e.open(t)
	fl, _, err := f.EAInodeValue(eaParent, eaName)
	if err != nil {
		t.Fatal(err)
	}
	runs := fl.Runs()
	if len(runs) == 0 {
		t.Fatal("a streaming value has runs")
	}
	if err := filesys.CheckRuns(runs, fl.Size(), int64(len(e.img))); err != nil {
		t.Errorf("runs violate the contract: %v", err)
	}
	for _, r := range runs {
		if r.Offset < 0 {
			t.Errorf("run %+v is a hole", r)
		}
	}
	if got := runBytes(t, e.img, runs); !bytes.Equal(got, e.value) {
		t.Error("the image bytes at the runs are not the value")
	}
}

func TestEAInodeSizeMismatchIsCorrupt(t *testing.T) {
	t.Run("i_size differs from the entry", func(t *testing.T) {
		e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
		put32(e.img, e.inode(t, eaInum)+4, 3001)
		wantEACorrupt(t, e.open(t))
	})
	t.Run("the entry declares another size", func(t *testing.T) {
		e := newEAImage(t, ext4test.Options{}, eaValue(3000), func(x *ext4test.Xattr) { x.Size = 2999 })
		wantEACorrupt(t, e.open(t))
	})
	t.Run("the runs do not cover the size", func(t *testing.T) {
		e := newEAImage(t, ext4test.Options{}, eaValue(3000), func(x *ext4test.Xattr) { x.Size = 4000 })
		put32(e.img, e.inode(t, eaInum)+4, 4000) // size and entry agree; the file has blocks for 3000 bytes only
		wantEACorrupt(t, e.open(t))
	})
}

func TestEAInodeWithoutFlagIsCorrupt(t *testing.T) {
	e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
	at := e.inode(t, eaInum) + 0x20
	put32(e.img, at, le32(e.img, at)&^eaFlag)
	wantEACorrupt(t, e.open(t))
}

func TestEAInodeNotARegularFileIsCorrupt(t *testing.T) {
	for _, mode := range []uint16{0x4000 | 0o755, 0xA000 | 0o777, 0x6000} {
		e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
		put16(e.img, e.inode(t, eaInum), mode)
		wantEACorrupt(t, e.open(t))
	}
}

// A value with no blocks is all hole; the kernel allocates every block of an EA inode, so it is refused.
func TestEAInodeHoleIsCorrupt(t *testing.T) {
	e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
	put16(e.img, e.inode(t, eaInum)+0x28+2, 0) // eh_entries: no extents
	wantEACorrupt(t, e.open(t))
}

func wantEACorrupt(t *testing.T, f *ext4.FS) {
	t.Helper()
	fl, _, err := f.EAInodeValue(eaParent, eaName)
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("EAInodeValue = %v, %v: want ErrCorrupt (never an empty value)", fl, err)
	}
	if fl != nil {
		t.Error("a file was returned with the error")
	}
	// the attribute itself is still listed, with its name
	e, lerr := f.Lookup("/x")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if v, _ := attr(e, "xattrs"); v != eaName {
		t.Errorf("xattrs = %q: the attribute must stay listed", v)
	}
}

func TestEAInodeHashMismatchWarns(t *testing.T) {
	for _, at := range []int{10, 65536 + 5, 69999} { // the first, a second and the last hash chunk
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			e := newEAImage(t, ext4test.Options{MetadataCsum: true}, eaValue(70000), nil)
			fl, _, err := e.open(t).EAInodeValue(eaParent, eaName)
			if err != nil {
				t.Fatal(err)
			}
			runs := fl.Runs()
			e.img[runs[0].Offset+int64(at)] ^= 0x01 // one extent: the builder places the value contiguously
			flipped := bytes.Clone(e.value)
			flipped[at] ^= 0x01

			f := e.open(t)
			fl, hashBad, err := f.EAInodeValue(eaParent, eaName)
			if err != nil {
				t.Fatalf("a hash mismatch must not fail the read: %v", err)
			}
			if !hashBad {
				t.Error("the mismatch was not reported")
			}
			if !bytes.Equal(readValue(t, fl), flipped) {
				t.Error("the bytes of the image were not returned as they are")
			}
			w := eaWarnings(f.Info())
			if len(w) != 1 || !strings.Contains(w[0], fmt.Sprintf("EA inode %d", eaInum)) || !strings.Contains(w[0], `"user.big" of inode 11`) {
				t.Errorf("warnings = %q, want one naming the EA inode, the attribute and the parent", w)
			}
		})
	}
}

// A damaged EA inode record (its own checksum) is a warning; the value is still read and verified.
func TestEAInodeChecksumMismatchWarns(t *testing.T) {
	e := newEAImage(t, ext4test.Options{MetadataCsum: true}, eaValue(3000), nil)
	e.img[e.inode(t, eaInum)+0x2] ^= 0x01 // i_uid: covered by the inode checksum only
	f := e.open(t)
	fl, bad, err := f.EAInodeValue(eaParent, eaName)
	if err != nil || bad || !bytes.Equal(readValue(t, fl), e.value) {
		t.Fatalf("EAInodeValue = bad %v, %v", bad, err)
	}
	if w := eaWarnings(f.Info()); len(w) != 1 || !strings.Contains(w[0], "inode checksum mismatch") {
		t.Errorf("warnings = %q", w)
	}
}

// TestEAInodeCapIsEnforced: a value of exactly 16 MiB with real blocks is read, one byte more is corrupt even
// though every block is mapped. Mutation: drop the cap check, or make it >=; a row fails.
func TestEAInodeCapIsEnforced(t *testing.T) {
	for _, tc := range []struct {
		size int
		ok   bool
	}{{ext4.MaxEAValue, true}, {ext4.MaxEAValue + 1, false}} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			v := eaValue(tc.size)
			e := newEAImage(t, ext4test.Options{BlockSize: 4096, BlocksPerGroup: 8192}, v, nil)
			f := e.open(t)
			fl, bad, err := f.EAInodeValue(eaParent, eaName)
			switch {
			case tc.ok && (err != nil || bad || fl.Size() != int64(tc.size)):
				t.Errorf("EAInodeValue = bad %v, %v: want the value", bad, err)
			case !tc.ok && !errors.Is(err, filesys.ErrCorrupt):
				t.Errorf("EAInodeValue = %v, %v: want ErrCorrupt", fl, err)
			}
		})
	}
}

// TestEAInodeDeclaredSizeNeverAllocates: a declared size is never the size of an allocation. The EA inode's
// i_size agrees with the entry and no extent maps it; every size fails with ErrCorrupt (a size above the cap by
// the limit, a size at or below it because a value with no blocks is all hole), and the call allocates a few KiB.
// Mutation: allocate make([]byte, x.ValueSize) before the checks; the 0xFFFFFFFF row fails.
func TestEAInodeDeclaredSizeNeverAllocates(t *testing.T) {
	for _, size := range []uint32{0xFFFFFFFF, ext4.MaxEAValue + 1, ext4.MaxEAValue, ext4.MaxEAValue - 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			e := newEAImage(t, ext4test.Options{}, eaValue(100), func(x *ext4test.Xattr) { x.Size = size })
			ino := e.inode(t, eaInum)
			put32(e.img, ino+4, size)
			put16(e.img, ino+0x28+2, 0) // no extents
			f := e.open(t)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			fl, _, err := f.EAInodeValue(eaParent, eaName)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
				t.Fatalf("EAInodeValue = %v, %v: want ErrCorrupt", fl, err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
				t.Errorf("the call allocated %d bytes for a declared size of %d", grew, size)
			}
		})
	}
}

// TestEAInodeCycleTerminates pins existing behaviour: an EA inode that names itself, or two that name each
// other, in their own attribute areas never recurse, because the EA inode is read without its attributes.
// Mutation: make eaInodeValue read the EA inode's own EA-valued attributes by calling itself; the cycle recurses
// and the test fails (a stack overflow or the timeout).
func TestEAInodeCycleTerminates(t *testing.T) {
	v := eaValue(2000)
	cases := map[string][]ext4test.Xattr{
		"an EA inode naming itself": {
			{Name: eaName, Value: v, EAInode: true, EAXattrs: []ext4test.Xattr{{Name: "user.self", Inum: eaInum, Size: uint32(len(v))}}},
		},
		"two EA inodes naming each other": {
			{Name: eaName, Value: v, EAInode: true, EAXattrs: []ext4test.Xattr{{Name: "user.b", Inum: eaInum + 1, Size: uint32(len(v))}}},
			{Name: "user.other", Value: v, EAInode: true, EAXattrs: []ext4test.Xattr{{Name: "user.a", Inum: eaInum, Size: uint32(len(v))}}},
		},
	}
	for name, xs := range cases {
		t.Run(name, func(t *testing.T) {
			img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: xs}})
			done := make(chan error, 1)
			go func() {
				f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
				if err != nil {
					done <- err
					return
				}
				for _, x := range xs {
					fl, bad, err := f.EAInodeValue(eaParent, x.Name)
					if err != nil || bad || fl.Size() != int64(len(v)) {
						done <- fmt.Errorf("%s: bad %v, %v", x.Name, bad, err)
						return
					}
				}
				// the EA inodes' own attributes are listed without being followed
				for _, n := range []uint32{eaInum, eaInum + 1} {
					if in, err := f.Inode(n); err == nil {
						_, _ = f.Xattrs(in)
					}
				}
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reading an EA-inode value did not terminate")
			}
		})
	}
}

// recordingReader remembers the offsets read.
type recordingReader struct {
	r    io.ReaderAt
	offs []int64
}

func (r *recordingReader) ReadAt(p []byte, off int64) (int, error) {
	r.offs = append(r.offs, off)
	return r.r.ReadAt(p, off)
}

// TestSystemDataInEAInodeIsUnsupported: inline content whose system.data value is held in an EA inode is a
// layout no kernel was seen to write: it is refused as unsupported (not corrupt), warned about, flagged on the
// entry, and nothing is decoded (the EA inode is never read).
func TestSystemDataInEAInodeIsUnsupported(t *testing.T) {
	data := eaValue(150) // more than the 60 bytes of i_block
	img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true, MetadataCsum: true}, []ext4test.File{
		{Path: "/i", Data: data, Inline: true, SystemDataInEAInode: true},
	})
	probe := mustOpen(t, img)
	eaOff := int64(inodeOff(t, probe, 1024, 256, 128, 12))
	rr := &recordingReader{r: bytes.NewReader(img)}
	f, err := ext4.Open(rr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.Lookup("/i")
	if err != nil {
		t.Fatal(err)
	}
	rr.offs = nil

	fl, err := f.Open(e)
	if fl != nil || !errors.Is(err, filesys.ErrUnsupported) || errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open = %v, %v: want ErrUnsupported and not ErrCorrupt", fl, err)
	}
	if !hasWarning(f.Info(), "system.data") || !hasWarning(f.Info(), "inode 11") {
		t.Errorf("no warning naming the inode and system.data: %v", f.Info().Warnings)
	}
	root, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, re := range root {
		if re.Name == "i" {
			found = true
			if v, ok := attr(re, "system_data"); !ok || v != "unsupported" {
				t.Errorf("system_data attribute = %q (present %v), want unsupported", v, ok)
			}
		}
	}
	if !found {
		t.Fatal("/i is not listed")
	}
	for _, off := range rr.offs {
		if off >= eaOff && off < eaOff+256 {
			t.Errorf("the EA inode was read (offset %d): no byte of an unsupported layout is decoded", off)
		}
	}
}

// An EA inode holds its value in blocks; the inline-data flag on it is refused.
func TestEAInodeWithInlineDataFlagIsCorrupt(t *testing.T) {
	e := newEAImage(t, ext4test.Options{}, eaValue(3000), nil)
	at := e.inode(t, eaInum) + 0x20
	put32(e.img, at, le32(e.img, at)|0x10000000)
	wantEACorrupt(t, e.open(t))
}
