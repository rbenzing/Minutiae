package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// xent is the entry of inode n, carrying nothing but its ID (the only field the reader may use).
func xent(n int) filesys.Entry { return filesys.Entry{ID: fmt.Sprintf("inode:%d", n)} }

func xattrByName(xs []filesys.Xattr, name string) (filesys.Xattr, bool) {
	i := slices.IndexFunc(xs, func(x filesys.Xattr) bool { return x.Name == name })
	if i < 0 {
		return filesys.Xattr{}, false
	}
	return xs[i], true
}

func mustInode(t *testing.T, f *ext4.FS, n uint32) *ext4.Inode {
	t.Helper()
	in, err := f.Inode(n)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// Every storage of the real libext2fs-written ext4-xattr image, against the debugfs ea_list oracle: the listed
// names, sizes and storage kinds, and the value of each by sha256 (in the inode, in the xattr block, in an EA inode).
func TestXattrReaderListsAllStorages(t *testing.T) {
	const path = "testdata/ext4-xattr.img.gz"
	want := loadOracle(t, path)
	f := mustOpen(t, gunzipFixture(t, path))
	byPath := map[string][]oracleXattr{}
	for _, a := range want.Xattrs {
		byPath[a.Path] = append(byPath[a.Path], a)
	}
	if len(byPath) == 0 {
		t.Fatal("the oracle lists no attributes")
	}
	seen := map[string]int{}
	for p, attrs := range byPath {
		n := inodeOfPath(t, f, p)
		xs, err := f.Xattrs(xent(n))
		if err != nil {
			t.Fatalf("%s: Xattrs: %v", p, err)
		}
		if len(xs) != len(attrs) {
			t.Errorf("%s: %d attributes listed, the oracle has %d: %v", p, len(xs), len(attrs), xs)
		}
		for _, a := range attrs {
			x, ok := xattrByName(xs, a.Name)
			if !ok {
				t.Errorf("%s: %s is not listed", p, a.Name)
				continue
			}
			wantStorage := map[string]string{"inode": filesys.XattrInline, "block": filesys.XattrInline, "ea_inode": filesys.XattrStream}[a.Storage]
			if wantStorage == "" {
				t.Fatalf("%s: the oracle storage %q is not one this test knows", a.Name, a.Storage)
			}
			if x.Storage != wantStorage || x.Size != int64(a.Size) || x.Integrity != "" {
				t.Errorf("%s %s: listed %+v, want storage %s size %d and no mark (%s)", p, a.Name, x, wantStorage, a.Size, a.Storage)
			}
			fl, err := f.OpenXattr(xent(n), a.Name)
			if err != nil {
				t.Errorf("%s %s: OpenXattr: %v", p, a.Name, err)
				continue
			}
			val := readValue(t, fl)
			sum := sha256.Sum256(val)
			if len(val) != a.Size || hex.EncodeToString(sum[:]) != a.SHA256 {
				t.Errorf("%s %s: value is %d bytes sha256 %x, want %d bytes %s", p, a.Name, len(val), sum, a.Size, a.SHA256)
			}
			if (a.Storage == "ea_inode") != (fl.Runs() != nil) {
				t.Errorf("%s %s: runs %v for storage %s: only an EA-inode value has runs", p, a.Name, fl.Runs(), a.Storage)
			}
			seen[a.Storage]++
		}
	}
	for _, s := range []string{"inode", "block", "ea_inode"} {
		if seen[s] == 0 {
			t.Errorf("the fixture exercised no %s attribute", s)
		}
	}
}

// The runs of an EA-inode value are the real image runs: the bytes at them are the value, and they satisfy the
// File.Runs contract.
func TestXattrReaderStreamRunsAreRealRuns(t *testing.T) {
	e := newEAImage(t, ext4test.Options{MetadataCsum: true}, eaValue(5000), nil)
	f := e.open(t)
	fl, err := f.OpenXattr(xent(eaParent), eaName)
	if err != nil {
		t.Fatal(err)
	}
	runs := fl.Runs()
	if len(runs) == 0 {
		t.Fatal("an EA-inode value reports no runs")
	}
	if err := filesys.CheckRuns(runs, fl.Size(), f.Info().Size); err != nil {
		t.Fatalf("runs violate the File.Runs contract: %v", err)
	}
	var got []byte
	for _, r := range runs {
		if r.Offset < 0 {
			t.Fatalf("hole run %+v in an EA-inode value", r)
		}
		got = append(got, e.img[r.Offset:r.Offset+r.Length]...)
	}
	if !bytes.Equal(got[:fl.Size()], e.value) {
		t.Error("the image bytes at the reported runs are not the value")
	}
	// an inline value reports none
	g := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{{Name: "user.a", Value: []byte("v")}}}}))
	in, err := g.OpenXattr(xent(11), "user.a")
	if err != nil || in.Runs() != nil {
		t.Errorf("inline value = %v, %v; want a value without runs", in, err)
	}
}

// A name that is not valid UTF-8 or holds a NUL is the shared ~raw~ alias of its whole stored name (prefix and
// name), spelled by hand here ("user.\xffz" and "user.a\x00b"), and OpenXattr finds it by that name only.
func TestXattrReaderRawNameAlias(t *testing.T) {
	files := []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{
		{Name: "user.\xffz", Value: []byte("one")},
		{Name: "user.a\x00b", Value: []byte("two")},
		{Name: "user.plain", Value: []byte("three")},
	}}}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	xs, err := f.Xattrs(xent(11))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(xs))
	for i, x := range xs {
		names[i] = x.Name
	}
	slices.Sort(names)
	if want := []string{"user.plain", "~raw~dXNlci5hAGI", "~raw~dXNlci7_eg"}; !slices.Equal(names, want) {
		t.Fatalf("names %q, want %q", names, want)
	}
	for name, val := range map[string]string{"~raw~dXNlci7_eg": "one", "~raw~dXNlci5hAGI": "two", "user.plain": "three"} {
		fl, err := f.OpenXattr(xent(11), name)
		if err != nil || string(readValue(t, fl)) != val {
			t.Errorf("OpenXattr(%q) = %v, %v; want %q", name, fl, err, val)
		}
	}
	if _, err := f.OpenXattr(xent(11), "user.\xffz"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("OpenXattr of the raw bytes = %v; want ErrNotFound (the display name is the alias)", err)
	}
}

// A table that cannot be read is an error and a nil list: neither an empty list (the entry has none) nor the
// attributes before the damage (a prefix is not the set). OpenXattr gives the same error, never a value.
func TestXattrReaderCorruptTableIsAnError(t *testing.T) {
	t.Run("block magic", func(t *testing.T) {
		x := newXattrImage(t, ext4test.Options{})
		put32(x.img, x.start, 0)
		f := x.reopen(t)
		xs, err := f.Xattrs(xent(11))
		if !errors.Is(err, filesys.ErrCorrupt) || xs != nil {
			t.Errorf("Xattrs = %v, %v; want nil and ErrCorrupt", xs, err)
		}
		if fl, err := f.OpenXattr(xent(11), "user.alpha"); !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
			t.Errorf("OpenXattr = %v, %v; want nil and ErrCorrupt", fl, err)
		}
	})
	t.Run("entries before the damage are dropped", func(t *testing.T) {
		x := newXattrImage(t, ext4test.Options{})
		// the second entry's value lies far outside the block: the first entry parses, the table does not
		second := x.start + 32 + 24 // header, then "user.alpha" (index 1, name "alpha": 16+5 rounded up to 24)
		put16(x.img, second+2, 0xfff0)
		f := x.reopen(t)
		raw, rerr := f.RawXattrs(mustInode(t, f, 11))
		if rerr == nil || len(raw) == 0 {
			t.Fatalf("setup: the raw table read = %v, %v; want a partial list and an error", raw, rerr)
		}
		xs, err := f.Xattrs(xent(11))
		if !errors.Is(err, filesys.ErrCorrupt) || xs != nil {
			t.Errorf("Xattrs = %v, %v; want nil and ErrCorrupt", xs, err)
		}
		if fl, err := f.OpenXattr(xent(11), "user.alpha"); !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
			t.Errorf("OpenXattr of an attribute before the damage = %v, %v; want nil and ErrCorrupt", fl, err)
		}
	})
}

// eaImage2 is /x (inode 11) with an inline attribute and two EA-inode attributes, user.good and user.bad.
func eaPairImage(t *testing.T) (img []byte, good, bad []byte) {
	t.Helper()
	good, bad = eaValue(3000), eaValue(2000)
	files := []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{
		{Name: "user.inline", Value: []byte("small")},
		{Name: "user.good", Value: good, EAInode: true},
		{Name: "user.bad", Value: bad, EAInode: true},
	}}}
	return ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files), good, bad
}

// One damaged EA inode spoils its own attribute only: it is listed with size -1 and fails with ErrCorrupt, while
// the attributes beside it list and open.
func TestXattrReaderDamagedValueFailsOnlyThatAttribute(t *testing.T) {
	img, good, _ := eaPairImage(t)
	f := mustOpen(t, img)
	raw, err := f.RawXattrs(mustInode(t, f, 11))
	if err != nil {
		t.Fatal(err)
	}
	var badInum uint32
	for _, x := range raw {
		if x.Name == "user.bad" {
			badInum = x.Inum
		}
	}
	if badInum == 0 {
		t.Fatal("the builder wrote no EA inode for user.bad")
	}
	at := inodeOff(t, f, 1024, 256, 128, int(badInum)) + 0x20
	put32(img, at, le32(img, at)&^eaFlag) // the EA-inode flag is gone: the record is damaged
	g := mustOpen(t, img)
	xs, err := g.Xattrs(xent(11))
	if err != nil || len(xs) != 3 {
		t.Fatalf("Xattrs = %v, %v; want all three listed", xs, err)
	}
	for name, want := range map[string]filesys.Xattr{
		"user.inline": {Name: "user.inline", Size: 5, Storage: filesys.XattrInline},
		"user.good":   {Name: "user.good", Size: 3000, Storage: filesys.XattrStream},
		"user.bad":    {Name: "user.bad", Size: -1, Storage: filesys.XattrStream},
	} {
		if x, _ := xattrByName(xs, name); x != want {
			t.Errorf("%s listed as %+v, want %+v", name, x, want)
		}
	}
	if fl, err := g.OpenXattr(xent(11), "user.bad"); !errors.Is(err, filesys.ErrCorrupt) || fl != nil {
		t.Errorf("OpenXattr(user.bad) = %v, %v; want nil and ErrCorrupt", fl, err)
	}
	for name, val := range map[string][]byte{"user.good": good, "user.inline": []byte("small")} {
		if fl, err := g.OpenXattr(xent(11), name); err != nil || !bytes.Equal(readValue(t, fl), val) {
			t.Errorf("OpenXattr(%s) = %v, %v; want its value", name, fl, err)
		}
	}
}

// The hash mark (Z65) is on the attribute whose EA inode fails it, and on no other (an inline neighbour and an
// attribute whose EA inode is intact stay unmarked). The bytes are still returned, with one warning.
// Mutation: mark every EA-inode attribute; the user.good check fails.
func TestXattrReaderHashBadMarksOnlyThatAttribute(t *testing.T) {
	img, _, bad := eaPairImage(t)
	fl, err := mustOpen(t, img).OpenXattr(xent(11), "user.bad")
	if err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(bad)
	flipped[100] ^= 0x01
	img[fl.Runs()[0].Offset+100] ^= 0x01
	f := mustOpen(t, img)
	xs, err := f.Xattrs(xent(11))
	if err != nil {
		t.Fatal(err)
	}
	for name, mark := range map[string]string{"user.inline": "", "user.good": "", "user.bad": filesys.XattrHashBad} {
		if x, ok := xattrByName(xs, name); !ok || x.Integrity != mark || x.Size < 0 {
			t.Errorf("%s listed as %+v (found %v); want mark %q and its size", name, x, ok, mark)
		}
	}
	vfl, err := f.OpenXattr(xent(11), "user.bad")
	if err != nil || !bytes.Equal(readValue(t, vfl), flipped) {
		t.Errorf("OpenXattr(user.bad) = %v, %v; want the image bytes as they are", vfl, err)
	}
	if w := eaWarnings(f.Info()); len(w) != 1 || !strings.Contains(w[0], `"user.bad"`) {
		t.Errorf("warnings = %q; want one, for user.bad", w)
	}
}

// countingReader counts the bytes read inside the given ranges of the image.
type countingReader struct {
	r      io.ReaderAt
	ranges []filesys.Run
	n      atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	for _, r := range c.ranges {
		lo, hi := max(off, r.Offset), min(off+int64(len(p)), r.Offset+r.Length)
		if hi > lo {
			c.n.Add(hi - lo)
		}
	}
	return c.r.ReadAt(p, off)
}

// Ruling Z66: the hash verdict is per EA inode. Thirty entries that name one 70000-byte EA inode (more than one
// hash chunk) cost one read of the value, however often they are listed and opened. Mutation: drop the cache
// lookup in eaHashVerdict; the reads multiply.
func TestXattrReaderManyEntriesOnOneEAInodeReadItOnce(t *testing.T) {
	const refs = 30
	v := eaValue(70000)
	xs := []ext4test.Xattr{{Name: "user.big", Value: v, EAInode: true}}
	for i := range refs {
		xs = append(xs, ext4test.Xattr{Name: fmt.Sprintf("user.ref%02d", i), Inum: eaInum, Size: uint32(len(v))})
	}
	for _, flip := range []bool{false, true} {
		t.Run(fmt.Sprintf("hash bad %v", flip), func(t *testing.T) {
			img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, []ext4test.File{{Path: "/x", Data: []byte("x"), XattrBlock: true, Xattrs: xs}})
			probe, err := mustOpen(t, img).OpenXattr(xent(eaParent), "user.big")
			if err != nil {
				t.Fatal(err)
			}
			runs := probe.Runs()
			if flip {
				img[runs[0].Offset+10] ^= 0x01
			}
			cr := &countingReader{r: bytes.NewReader(img), ranges: runs}
			f, err := ext4.Open(cr, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			for round := range 2 {
				list, err := f.Xattrs(xent(eaParent))
				if err != nil || len(list) != refs+1 {
					t.Fatalf("round %d: Xattrs = %d attributes, %v", round, len(list), err)
				}
				for _, x := range list {
					if x.Size != int64(len(v)) || x.Storage != filesys.XattrStream || (x.Integrity == filesys.XattrHashBad) != flip {
						t.Fatalf("round %d: %+v; want size %d, stream, hash bad %v", round, x, len(v), flip)
					}
					if _, err := f.OpenXattr(xent(eaParent), x.Name); err != nil {
						t.Fatalf("round %d: OpenXattr(%s): %v", round, x.Name, err)
					}
				}
			}
			if got := cr.n.Load(); got != int64(len(v)) {
				t.Errorf("%d bytes of the value read for %d entries listed and opened twice; want exactly one read of %d", got, refs+1, len(v))
			}
			if w := eaWarnings(f.Info()); len(w) != map[bool]int{false: 0, true: 1}[flip] {
				t.Errorf("warnings = %q (hash bad %v); want one when bad, else none", w, flip)
			}
		})
	}
}

// The hash work is bounded per filesystem: past the bound a value is listed with the unchecked mark and refused
// with ErrLimit, never passed unverified, and the attribute beside it that fits is unaffected.
func TestXattrReaderHashWorkIsBounded(t *testing.T) {
	a, b := eaValue(40000), eaValue(30000)
	files := []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{
		{Name: "user.a", Value: a, EAInode: true},
		{Name: "user.b", Value: b, EAInode: true},
	}}}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	f.SetEAHashCap(int64(len(a)) + int64(len(b)) - 1) // the second value does not fit
	xs, err := f.Xattrs(xent(11))
	if err != nil {
		t.Fatal(err)
	}
	xa, _ := xattrByName(xs, "user.a")
	xb, _ := xattrByName(xs, "user.b")
	if xa.Integrity != "" || xa.Size != int64(len(a)) {
		t.Errorf("user.a listed as %+v; want it checked", xa)
	}
	if xb.Integrity != filesys.XattrHashUnchecked || xb.Size != -1 {
		t.Errorf("user.b listed as %+v; want the unchecked mark and no size", xb)
	}
	if fl, err := f.OpenXattr(xent(11), "user.b"); !errors.Is(err, filesys.ErrLimit) || fl != nil {
		t.Errorf("OpenXattr(user.b) = %v, %v; want ErrLimit", fl, err)
	}
	if fl, err := f.OpenXattr(xent(11), "user.a"); err != nil || !bytes.Equal(readValue(t, fl), a) {
		t.Errorf("OpenXattr(user.a) = %v, %v; want its value", fl, err)
	}
	// the default bound admits the largest value the format allows
	if ext4.MaxEAHashBytes < ext4.MaxEAValue {
		t.Errorf("the hash bound %d is below one maximal value %d", ext4.MaxEAHashBytes, ext4.MaxEAValue)
	}
}

// Two attributes of one name are ambiguous, never the first.
func TestXattrReaderDuplicateNameIsAmbiguous(t *testing.T) {
	files := []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{
		{Name: "user.dup", Value: []byte("first")},
		{Name: "user.dup", Value: []byte("second")},
		{Name: "user.once", Value: []byte("o")},
	}}}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	xs, err := f.Xattrs(xent(11))
	if err != nil || len(xs) != 3 {
		t.Fatalf("Xattrs = %v, %v; want all three, the duplicate twice", xs, err)
	}
	if fl, err := f.OpenXattr(xent(11), "user.dup"); !errors.Is(err, filesys.ErrAmbiguous) || fl != nil {
		t.Errorf("OpenXattr(user.dup) = %v, %v; want ErrAmbiguous", fl, err)
	}
	if fl, err := f.OpenXattr(xent(11), "user.once"); err != nil || string(readValue(t, fl)) != "o" {
		t.Errorf("OpenXattr(user.once) = %v, %v", fl, err)
	}
	if _, err := f.OpenXattr(xent(11), "user.nope"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("OpenXattr of an unknown name = %v; want ErrNotFound", err)
	}
}

// More attributes than the bound is the typed cap error, not a partial list.
func TestXattrReaderCapIsTyped(t *testing.T) {
	files := []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{
		{Name: "user.a", Value: []byte("1")}, {Name: "user.b", Value: []byte("2")}, {Name: "user.c", Value: []byte("3")},
	}}}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	f.SetXattrCap(3)
	if xs, err := f.Xattrs(xent(11)); err != nil || len(xs) != 3 {
		t.Fatalf("at the bound: %v, %v", xs, err)
	}
	f.SetXattrCap(2)
	xs, err := f.Xattrs(xent(11))
	if !errors.Is(err, filesys.ErrXattrCap) || !errors.Is(err, filesys.ErrUnsupported) || xs != nil {
		t.Errorf("above the bound: %v, %v; want nil and ErrXattrCap", xs, err)
	}
}

// A deleted entry's ID names no inode: ErrDeleted from both methods; a malformed ID is an error, never a list.
func TestXattrReaderDeletedIDIsErrDeleted(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/x", Data: []byte("x")}}))
	e := filesys.Entry{ID: "dirent:1:24", Deleted: true}
	if xs, err := f.Xattrs(e); !errors.Is(err, filesys.ErrDeleted) || xs != nil {
		t.Errorf("Xattrs = %v, %v; want ErrDeleted", xs, err)
	}
	if fl, err := f.OpenXattr(e, "user.a"); !errors.Is(err, filesys.ErrDeleted) || fl != nil {
		t.Errorf("OpenXattr = %v, %v; want ErrDeleted", fl, err)
	}
	for _, id := range []string{"", "inode:0", "inode:011", "x:11", "inode:99999999"} {
		if xs, err := f.Xattrs(filesys.Entry{ID: id}); err == nil || xs != nil {
			t.Errorf("Xattrs(%q) = %v, %v; want an error", id, xs, err)
		}
	}
}

// Only e.ID counts: a forged Size, Type, Attrs, Name or Deleted on the entry changes nothing.
func TestXattrReaderIgnoresForgedEntryFields(t *testing.T) {
	files := []ext4test.File{
		{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{{Name: "user.a", Value: []byte("real")}}},
		{Path: "/y", Data: []byte("y")},
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	plain, err := f.Xattrs(xent(11))
	if err != nil || len(plain) != 1 {
		t.Fatal(plain, err)
	}
	forged := filesys.Entry{
		ID: "inode:11", Name: "y", Size: 1 << 40, Type: filesys.TypeDir,
		Attrs: []filesys.KV{{Key: "xattrs", Value: "user.zzz"}, {Key: "inode", Value: "12"}},
	}
	got, err := f.Xattrs(forged)
	if err != nil || !slices.Equal(got, plain) {
		t.Errorf("Xattrs(forged) = %v, %v; want %v", got, err, plain)
	}
	if fl, err := f.OpenXattr(forged, "user.a"); err != nil || string(readValue(t, fl)) != "real" {
		t.Errorf("OpenXattr(forged) = %v, %v", fl, err)
	}
	// a forged Deleted flag does not turn a live ID into a deleted one
	forged.Deleted = true
	if _, err := f.Xattrs(forged); err != nil {
		t.Errorf("Xattrs(Deleted forged) = %v", err)
	}
	// the other inode has none: an empty list, no error
	if xs, err := f.Xattrs(xent(12)); err != nil || len(xs) != 0 {
		t.Errorf("Xattrs(inode 12) = %v, %v; want an empty list and no error", xs, err)
	}
}

// bareFS hides every optional interface of the filesystem it wraps (it embeds only filesys.FileSystem) and
// forwards by Underlying().
type bareFS struct{ filesys.FileSystem }

func (w *bareFS) Underlying() filesys.FileSystem { return w.FileSystem }

// filesys.As finds the reader on the filesystem itself and through wrappers that forward by Underlying().
func TestXattrReaderViaFilesysAs(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/x", Data: []byte("x"), Xattrs: []ext4test.Xattr{{Name: "user.a", Value: []byte("v")}}}}))
	if _, ok := any(&bareFS{FileSystem: f}).(filesys.XattrReader); ok {
		t.Fatal("the wrapper itself must not implement XattrReader for this test to mean anything")
	}
	for name, fsys := range map[string]filesys.FileSystem{"direct": f, "wrapped": &bareFS{FileSystem: f}} {
		xr, ok := filesys.As[filesys.XattrReader](fsys)
		if !ok {
			t.Fatalf("%s: As[XattrReader] found nothing", name)
		}
		xs, err := xr.Xattrs(xent(11))
		if err != nil || len(xs) != 1 || xs[0].Name != "user.a" {
			t.Errorf("%s: Xattrs = %v, %v", name, xs, err)
		}
	}
}
