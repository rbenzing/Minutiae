package apfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// withTimeout runs fn and fails the test (instead of hanging) when it does not
// return in time.
func withTimeout(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("did not return: a read loops without progress")
	}
}

// An empty name is a legal record (a name length of 1 is the lone NUL): it is
// listed, opened and read like any other, and never loops.
func TestXattrEmptyNameIsListedOpenedAndRead(t *testing.T) {
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{
		Path: "/e", Data: []byte("x"),
		Xattrs: []apfstest.Xattr{{Name: "", Value: []byte("hello")}, {Name: "other", Value: []byte("o")}},
	})))
	e := mustLookup(t, f, "/Data/e")
	withTimeout(t, func() {
		xs, err := f.Xattrs(e)
		if err != nil || len(xs) != 2 {
			t.Errorf("Xattrs = %+v, %v; want 2", xs, err)
			return
		}
		var listed bool
		for _, x := range xs {
			if x.Name == "" && x.Size == 5 && x.Storage == filesys.XattrInline {
				listed = true
			}
		}
		if !listed {
			t.Errorf("the empty-named attribute is not listed with its size: %+v", xs)
		}
		fl, err := f.OpenXattr(e, "")
		if err != nil {
			t.Errorf("OpenXattr(empty): %v", err)
			return
		}
		b, err := io.ReadAll(io.NewSectionReader(fl, 0, fl.Size()))
		if err != nil || string(b) != "hello" {
			t.Errorf("read = %q, %v; want hello", b, err)
		}
		if got := readXattr(t, f, e, "other"); string(got) != "o" {
			t.Errorf("other = %q", got)
		}
	})
}

// Two records with one display name (a raw name's alias written literally next
// to the raw name itself) are an ambiguity: both are listed, and opening that
// name is a typed error, never the first hit.
func TestOpenXattrDuplicateDisplayNameIsAmbiguous(t *testing.T) {
	alias := filesys.RawNameAlias([]byte("b\xff"))
	f, _ := openOpts(t, volOpts(dataVolume(apfstest.File{
		Path: "/d", Data: []byte("x"),
		Xattrs: []apfstest.Xattr{{Name: "b\xff", Value: []byte("raw")}, {Name: alias, Value: []byte("lit")}, {Name: "solo", Value: []byte("s")}},
	})))
	e := mustLookup(t, f, "/Data/d")
	xs, err := f.Xattrs(e)
	if err != nil || len(xs) != 3 {
		t.Fatalf("Xattrs = %+v, %v; want all 3 listed", xs, err)
	}
	var same int
	for _, x := range xs {
		if x.Name == alias {
			same++
		}
	}
	if same != 2 {
		t.Errorf("%d records listed under %q, want 2", same, alias)
	}
	fl, err := f.OpenXattr(e, alias)
	if err == nil || fl != nil || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("OpenXattr(%q) = %v, %v; want a typed ambiguity error and no file", alias, fl, err)
	}
	if !errors.Is(err, filesys.ErrCorrupt) || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("ambiguity error = %v; want ErrCorrupt naming 'more than one'", err)
	}
	if got := readXattr(t, f, e, "solo"); string(got) != "s" {
		t.Errorf("solo = %q", got)
	}
}

func fsNodesHolding(im *image, hdr uint64) []uint64 {
	var out []uint64
	var key [8]byte
	binary.LittleEndian.PutUint64(key[:], hdr)
	for _, n := range im.g.Volumes[0].FsNodes[1:] {
		if bytes.Contains(im.blk(n), key[:]) {
			out = append(out, n)
		}
	}
	return out
}

// Y30 pinned on both remaining paths: the inode record's leaf is intact and only
// a leaf holding the entry's xattr records is damaged (the attribute scan's own
// error), and the fs-tree root is damaged (the tree load's error). Each is an
// error and no list, never an empty list.
func TestXattrsScanAndRootDamageAreErrorsNotEmpty(t *testing.T) {
	var xs []apfstest.Xattr
	for i := range 9 {
		xs = append(xs, apfstest.Xattr{Name: fmt.Sprintf("n%d", i), Value: []byte("v")})
	}
	build := func(t *testing.T) (*image, filesys.Entry) {
		v := dataVolume(apfstest.File{Path: "/tagged", Data: []byte("t"), Xattrs: xs})
		v.TreeMaxKeys = 3
		im := newImage(t, volOpts(v))
		ino := im.g.Volumes[0].Inodes["/tagged"]
		if ino == 0 || len(im.g.Volumes[0].FsNodes) < 3 {
			t.Fatalf("fixture: inode %d, %d fs nodes", ino, len(im.g.Volumes[0].FsNodes))
		}
		return im, filesys.Entry{ID: apfs.NodeID(0, 0, ino)}
	}
	t.Run("only the xattr leaf is damaged", func(t *testing.T) {
		im, e := build(t)
		ino := im.g.Volumes[0].Inodes["/tagged"]
		inodeNodes := fsNodesHolding(im, ino|3<<60)
		var victim uint64
		for _, n := range fsNodesHolding(im, ino|4<<60) {
			if !slices.Contains(inodeNodes, n) {
				victim = n
			}
		}
		if victim == 0 || len(inodeNodes) == 0 {
			t.Fatalf("fixture: inode leaves %v, victim %d", inodeNodes, victim)
		}
		im.blk(victim)[40] ^= 0xFF
		g, err := im.open()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := g.Xattrs(e); err == nil || got != nil {
			t.Errorf("Xattrs with a damaged xattr leaf = %+v, %v; want an error and no list", got, err)
		}
		if fl, err := g.OpenXattr(e, "n0"); err == nil || fl != nil {
			t.Errorf("OpenXattr with a damaged xattr leaf = %v, %v; want an error", fl, err)
		}
	})
	t.Run("the fs-tree root is damaged", func(t *testing.T) {
		im, e := build(t)
		im.blk(im.g.Volumes[0].FsNodes[0])[40] ^= 0xFF
		g, err := im.open()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := g.Xattrs(e); err == nil || got != nil {
			t.Errorf("Xattrs with a damaged root = %+v, %v; want an error and no list", got, err)
		}
	})
}

func totalAlloc(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Nothing allocates from a declared size: a stream declared as large as the
// container (and one byte more) with no extents is a typed corruption and costs
// no more than the scan, never a buffer of the declared size.
func TestOpenXattrDeclaredSizeAllocatesNothing(t *testing.T) {
	const ino = 40
	for _, plus := range []uint64{0, 1} {
		t.Run(fmt.Sprintf("container size + %d", plus), func(t *testing.T) {
			v := dataVolume(apfstest.File{Path: "/h", Ino: ino, Data: []byte("x")})
			probe, _ := openOpts(t, volOpts(v))
			size := uint64(probe.Info().Size) + plus
			v.Extra = []apfstest.FSRecord{{ID: ino, Type: apfstest.TypeXattr, Key: apfstest.XattrKey("v"), Val: apfstest.XattrStreamVal(0x5000, size)}}
			f, _ := openOpts(t, volOpts(v))
			e := mustLookup(t, f, "/Data/h")
			_, _ = f.OpenXattr(e, "v") // warm up
			var fl filesys.File
			var err error
			a := totalAlloc(func() { fl, err = f.OpenXattr(e, "v") })
			if !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
				t.Errorf("OpenXattr = %v, %v; want ErrCorrupt and no file", fl, err)
			}
			if a > 64<<10 {
				t.Errorf("OpenXattr of a %d-byte declared stream allocated %d bytes", size, a)
			}
		})
	}
}

// Listing does not copy values: its cost is the same for 1800-byte and 3-byte
// values laid out in the same number of nodes (two records per node).
func TestXattrsListingCostIsIndependentOfValueSize(t *testing.T) {
	cost := func(n int) uint64 {
		var xs []apfstest.Xattr
		for i := range 40 {
			xs = append(xs, apfstest.Xattr{Name: fmt.Sprintf("n%03d", i), Value: bytes.Repeat([]byte{byte(i)}, n)})
		}
		v := dataVolume(apfstest.File{Path: "/many", Data: []byte("m"), Xattrs: xs})
		v.TreeMaxKeys = 2
		f, _ := openOpts(t, volOpts(v))
		e := mustLookup(t, f, "/Data/many")
		_, _ = f.Xattrs(e)
		a := totalAlloc(func() {
			if got, err := f.Xattrs(e); err != nil || len(got) != 40 {
				t.Errorf("Xattrs = %d records, %v; want 40", len(got), err)
			}
		})
		return a
	}
	small, big := cost(3), cost(1800)
	// Cloning every value would add 40*1800 = 72 KB.
	if big > small+small/50+8<<10 {
		t.Errorf("listing 1800-byte values allocated %d bytes against %d for 3-byte values", big, small)
	}
}

// Defence in depth: an inline file whose bytes are fewer than its size is a
// typed error, never a loop without progress and never a panic.
func TestInlineFileShorterThanItsSizeIsAnError(t *testing.T) {
	fl := apfs.ShortInlineFile()
	withTimeout(t, func() {
		p := make([]byte, 5)
		if _, err := fl.ReadAt(p, 0); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadAt = %v, want ErrCorrupt", err)
		}
		if _, err := fl.ReadAt(p[:2], 3); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadAt past the bytes = %v, want ErrCorrupt", err)
		}
	})
}
