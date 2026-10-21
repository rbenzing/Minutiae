package ext4_test

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

const (
	offXattrCsum   = 0x10         // h_checksum in the xattr block header
	offChecksumTyp = 1024 + 0x175 // s_checksum_type
	offUUID        = 1024 + 0x68
)

// xattrImage is a builder image whose /x (inode 11) carries two attributes in an xattr block.
type xattrImage struct {
	img   []byte
	blk   int // the xattr block number
	bs    int
	start int // byte offset of the block
}

func newXattrImage(t *testing.T, o ext4test.Options) *xattrImage {
	t.Helper()
	o.Extents = true
	files := []ext4test.File{
		{Path: "/x", Data: []byte("x"), XattrBlock: true, Xattrs: []ext4test.Xattr{
			{Name: "user.alpha", Value: []byte("first value")},
			{Name: "user.omega", Value: []byte("second value, the last one in the block")},
		}},
	}
	img := ext4test.Build(o, files)
	f := mustOpen(t, img)
	in, err := f.Inode(11)
	if err != nil {
		t.Fatal(err)
	}
	blk := int(in.Fields().FileACL)
	if blk == 0 {
		t.Fatal("the builder wrote no xattr block")
	}
	bs := f.Info().BlockSize
	return &xattrImage{img: img, blk: blk, bs: bs, start: blk * bs}
}

// seed is the filesystem checksum seed, computed here from the UUID in the image (not from the reader).
func (x *xattrImage) seed() uint32 {
	return ext4.RawCRC32C(0xFFFFFFFF, x.img[offUUID:offUUID+16])
}

// want is the checksum the kernel computes for the block as it stands: crc32c over the little-endian 64-bit
// block number, then the block with h_checksum read as zero.
func (x *xattrImage) want() uint32 {
	var nr [8]byte
	binary.LittleEndian.PutUint64(nr[:], uint64(x.blk))
	blk := append([]byte(nil), x.img[x.start:x.start+x.bs]...)
	clear(blk[offXattrCsum : offXattrCsum+4])
	return ext4.RawCRC32C(ext4.RawCRC32C(x.seed(), nr[:]), blk)
}

func (x *xattrImage) stored() uint32 { return le32(x.img, x.start+offXattrCsum) }

func (x *xattrImage) reopen(t *testing.T) *ext4.FS { return mustOpen(t, x.img) }

func xattrBlockWarnings(info filesys.Info) []string {
	var out []string
	for _, w := range info.Warnings {
		if strings.Contains(w, "xattr block") {
			out = append(out, w)
		}
	}
	return out
}

func wantXattrCsum(t *testing.T, what string, e filesys.Entry, bad bool) {
	t.Helper()
	v, ok := attr(e, "xattr_csum")
	switch {
	case bad && (!ok || v != "bad"):
		t.Errorf("%s: xattr_csum attribute = %q (present %v), want bad", what, v, ok)
	case !bad && ok:
		t.Errorf("%s: xattr_csum = %q on a block that verifies", what, v)
	}
}

func TestXattrBlockChecksumMismatchWarns(t *testing.T) {
	cases := []struct {
		name   string
		damage func(x *xattrImage)
	}{
		{"a value byte flipped, checksum untouched", func(x *xattrImage) { x.img[x.start+x.bs-1] ^= 0x55 }},
		{"h_checksum zeroed", func(x *xattrImage) { put32(x.img, x.start+offXattrCsum, 0) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newXattrImage(t, ext4test.Options{MetadataCsum: true})
			if x.stored() != x.want() {
				t.Fatalf("the builder block does not verify before the damage: stored %#x, independent computation %#x", x.stored(), x.want())
			}
			tc.damage(x)
			stored, computed := x.stored(), x.want()
			if stored == computed {
				t.Fatal("the damage left the checksum valid")
			}
			f := x.reopen(t)
			e, err := f.Lookup("/x")
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("xattr block %d: checksum mismatch (stored %#08x, computed %#08x)", x.blk, stored, computed)
			if got := xattrBlockWarnings(f.Info()); len(got) != 1 || got[0] != want {
				t.Errorf("warnings = %q, want exactly %q", got, want)
			}
			if v, _ := attr(e, "xattrs"); v != "user.alpha,user.omega" {
				t.Errorf("xattrs = %q: the names must still be listed", v)
			}
			wantXattrCsum(t, "damaged block", e, true)
			if v, ok := attr(e, "checksum"); ok {
				t.Errorf("checksum = %q: the xattr block verdict must not borrow the inode checksum key", v)
			}
			in, err := f.Inode(11)
			if err != nil {
				t.Fatal(err)
			}
			xs, err := f.Xattrs(in)
			if err != nil || len(xs) != 2 || xs[0].Name != "user.alpha" || string(xs[0].Value) != "first value" {
				t.Errorf("Xattrs = %+v, %v: the attributes are still returned", xs, err)
			}
		})
	}
}

// Builder only: libext2fs never shares an xattr block between inodes (the fixture generator cannot make one), so
// the second inode's i_file_acl is patched to the first inode's block.
func TestXattrBlockChecksumSharedBuilderBlockWarnsOnce(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a", Data: []byte("a"), XattrBlock: true, Xattrs: []ext4test.Xattr{{Name: "user.k", Value: []byte("shared value")}}},
		{Path: "/b", Data: []byte("b"), XattrBlock: true, Xattrs: []ext4test.Xattr{{Name: "user.k", Value: []byte("other value!")}}},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	a, err := f.Inode(11)
	if err != nil {
		t.Fatal(err)
	}
	blk := int(a.Fields().FileACL)
	b := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(1)))
	put32(img, b+0x68, uint32(blk)) // i_file_acl_lo of /b
	bs := f.Info().BlockSize
	img[blk*bs+bs-1] ^= 0x55
	f = mustOpen(t, img)
	for _, p := range []string{"/a", "/b"} {
		e, err := f.Lookup(p)
		if err != nil {
			t.Fatal(err)
		}
		wantXattrCsum(t, p, e, true)
	}
	if got := xattrBlockWarnings(f.Info()); len(got) != 1 {
		t.Errorf("a block shared by two inodes warned %d times: %q", len(got), got)
	}
}

// Pins existing behaviour (no warning when the checksum is right). Mutation: compute with a wrong seed; the
// builder and fixture cases must fail.
func TestXattrBlockChecksumVerified(t *testing.T) {
	t.Run("builder", func(t *testing.T) {
		x := newXattrImage(t, ext4test.Options{MetadataCsum: true})
		f := x.reopen(t)
		e, err := f.Lookup("/x")
		if err != nil {
			t.Fatal(err)
		}
		if got := xattrBlockWarnings(f.Info()); len(got) != 0 {
			t.Errorf("warnings on an intact block: %q", got)
		}
		wantXattrCsum(t, "intact block", e, false)
		if v, _ := attr(e, "xattrs"); v != "user.alpha,user.omega" {
			t.Errorf("xattrs = %q", v)
		}
	})
	t.Run("real mke2fs fixture", func(t *testing.T) {
		path := "testdata/ext4-xattr.img.gz"
		want := loadOracle(t, path)
		img := gunzipFixture(t, path)
		f := mustOpen(t, img)
		if got := xattrBlockWarnings(f.Info()); len(got) != 0 {
			t.Errorf("warnings on the kernel-format fixture: %q", got)
		}
		blocks := 0
		for _, p := range []string{"/file1", "/file2", "/dir/file3"} {
			e, err := f.Lookup(p)
			if err != nil {
				t.Fatal(err)
			}
			wantXattrCsum(t, p, e, false)
			var names []string
			for _, a := range want.Xattrs {
				if a.Path == p {
					names = append(names, a.Name)
					if a.Storage == "block" {
						blocks++
					}
				}
			}
			if v, _ := attr(e, "xattrs"); v != strings.Join(names, ",") {
				t.Errorf("%s: xattrs = %q, want (ea_list) %q", p, v, strings.Join(names, ","))
			}
		}
		if blocks != 1 {
			t.Fatalf("the oracle holds %d block-stored attributes, want 1: the fixture would not exercise the verification", blocks)
		}
		if got := xattrBlockWarnings(f.Info()); len(got) != 0 {
			t.Errorf("warnings after reading every entry: %q", got)
		}
	})
	t.Run("a damaged real block warns", func(t *testing.T) {
		path := "testdata/ext4-xattr.img.gz"
		img := gunzipFixture(t, path)
		f := mustOpen(t, img)
		in, err := f.Inode(uint32(inodeOfPath(t, f, "/file1")))
		if err != nil {
			t.Fatal(err)
		}
		blk := int(in.Fields().FileACL)
		bs := f.Info().BlockSize
		img[blk*bs+bs-1] ^= 0x01 // the last byte of the block is the end of the 300-byte value
		g := mustOpen(t, img)
		e, err := g.Lookup("/file1")
		if err != nil {
			t.Fatal(err)
		}
		wantXattrCsum(t, "damaged fixture block", e, true)
		if got := xattrBlockWarnings(g.Info()); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("xattr block %d: checksum mismatch", blk)) {
			t.Errorf("warnings = %q", got)
		}
	})
}

func inodeOfPath(t *testing.T, f *ext4.FS, p string) int {
	t.Helper()
	e, err := f.Lookup(p)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if _, err := fmt.Sscanf(e.ID, "inode:%d", &n); err != nil {
		t.Fatalf("entry id %q: %v", e.ID, err)
	}
	return n
}

// Pins existing behaviour: without metadata_csum the h_checksum field is not a checksum. Mutation: verify
// regardless of the feature.
func TestXattrBlockChecksumIgnoredWithoutMetadataCsum(t *testing.T) {
	x := newXattrImage(t, ext4test.Options{})
	put32(x.img, x.start+offXattrCsum, 0xDEADBEEF)
	f := x.reopen(t)
	e, err := f.Lookup("/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := xattrBlockWarnings(f.Info()); len(got) != 0 {
		t.Errorf("warnings on a filesystem without metadata_csum: %q", got)
	}
	wantXattrCsum(t, "no metadata_csum", e, false)
}

// Pins existing behaviour: metadata_csum with a checksum type other than crc32c invents no per-block verdict.
// Mutation: verify with an unknown type.
func TestXattrBlockChecksumUnsupportedType(t *testing.T) {
	x := newXattrImage(t, ext4test.Options{MetadataCsum: true})
	x.img[offChecksumTyp] = 2
	x.img[x.start+x.bs-1] ^= 0x55
	f := x.reopen(t)
	if !hasWarning(f.Info(), "unsupported superblock checksum type") {
		t.Fatalf("the image is not the unsupported-type case: %v", f.Info().Warnings)
	}
	e, err := f.Lookup("/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := xattrBlockWarnings(f.Info()); len(got) != 0 {
		t.Errorf("warnings with an unsupported checksum type: %q", got)
	}
	wantXattrCsum(t, "unsupported checksum type", e, false)
}

func TestXattrBlockForgedHeaderNeverPanics(t *testing.T) {
	for _, csum := range []bool{false, true} {
		for _, v := range []uint32{0, 1, 0x7FFFFFFF, 0x80000000, 0xFFFFFFFF} {
			for _, off := range []int{0x4, 0x8, 0xC} { // h_refcount, h_blocks, h_hash
				x := newXattrImage(t, ext4test.Options{MetadataCsum: csum})
				put32(x.img, x.start+off, v)
				f := x.reopen(t)
				e, err := f.Lookup("/x")
				if err != nil {
					t.Fatalf("csum=%v off=%#x v=%#x: %v", csum, off, v, err)
				}
				// the header fields are not part of any identity: the names are listed, and with metadata_csum
				// the changed header no longer verifies
				if n, _ := attr(e, "xattrs"); n != "user.alpha,user.omega" {
					t.Errorf("csum=%v off=%#x v=%#x: xattrs = %q", csum, off, v, n)
				}
				bad := csum && x.stored() != x.want()
				wantXattrCsum(t, fmt.Sprintf("csum=%v off=%#x v=%#x", csum, off, v), e, bad)
				if in, err := f.Inode(11); err == nil {
					_, _ = f.Xattrs(in)
				}
			}
		}
	}
}
