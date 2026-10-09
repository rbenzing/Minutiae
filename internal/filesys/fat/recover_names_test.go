package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// TestRecoverableNameWalkWorkIsBounded: a hundred deleted long-name entries in
// front of the entry are never all visited. The work counter counts the
// directory entries the walk looks at, so the bound does not depend on what the
// metadata cache absorbs.
func TestRecoverableNameWalkWorkIsBounded(t *testing.T) {
	files := make([]fattest.File, 0, 121)
	for i := 0; i < 120; i++ {
		files = append(files, fattest.File{Path: fmt.Sprintf("/F%03d.BIN", i), Data: []byte("x"), Deleted: true})
	}
	files = append(files, fattest.File{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true})
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, files)
	loc := b.Find(threeSlots, true)
	nearest := loc.LFNOffsets[len(loc.LFNOffsets)-1]
	for k := int64(1); k <= 100; k++ {
		off := loc.LFNOffsets[0] - k*32
		copy(img[off:off+32], img[nearest:nearest+32])
	}
	fsys := reopen(t, img)
	if n := fsys.NameWalkEntries(); n != 0 {
		t.Fatalf("%d entries visited before any walk", n)
	}
	basisOf(t, fsys, loc)
	if n := fsys.NameWalkEntries(); n != 20 {
		t.Errorf("the walk visited %d entries, want exactly maxLFNEntries (20)", n)
	}
}

// failingReader fails every read that overlaps [lo, hi) once armed.
type failingReader struct {
	r      io.ReaderAt
	lo, hi int64
	armed  atomic.Bool
}

var errInjected = errors.New("injected read failure")

func (f *failingReader) ReadAt(p []byte, off int64) (int, error) {
	if f.armed.Load() && off < f.hi && off+int64(len(p)) > f.lo {
		return 0, errInjected
	}
	return f.r.ReadAt(p, off)
}

// TestRecoverableUnreadableNamesAreUnknownNotShortOnly: when the walk cannot
// read the entries in front of the short entry, nothing is known about a long
// name: the marker is name=unknown and a warning is counted; name=short-only
// would claim that no long name exists.
func TestRecoverableUnreadableNamesAreUnknownNotShortOnly(t *testing.T) {
	var img []byte
	var loc fattest.EntryLoc
	for n := 0; ; n++ {
		files := make([]fattest.File, 0, n+1)
		for i := 0; i < n; i++ {
			files = append(files, fattest.File{Path: fmt.Sprintf("/F%03d.BIN", i), Data: []byte("x"), Deleted: true})
		}
		files = append(files, fattest.File{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true})
		var b *fattest.Built
		img, b = fattest.BuildLayout(fattest.Options{Type: 16, Label: "NAMES"}, files)
		loc = b.Find(threeSlots, true)
		if loc.Index >= 32 && loc.Index%16 == 0 { // the long-name entries are in a sector of their own
			break
		}
		if n > 80 {
			t.Fatal("no layout found")
		}
	}
	sector := loc.LFNOffsets[0] &^ 511
	if sector == loc.Offset&^511 || sector != loc.LFNOffsets[2]&^511 {
		t.Fatalf("fixture: long-name entries at %v, short entry at %d", loc.LFNOffsets, loc.Offset)
	}
	fr := &failingReader{r: bytes.NewReader(img), lo: sector, hi: sector + 512}
	fsys, err := fat.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	healthy := basisOf(t, reopen(t, img), loc)
	if !slices.Contains(healthy, "lfn=recovered") {
		t.Fatalf("fixture: healthy basis %v", healthy)
	}
	before := len(fsys.Info().Warnings)
	fr.armed.Store(true)
	// Recoverable itself finds the end of the directory first, which reads (and caches) every root sector;
	// the walk is called directly so that its own read of the long-name entries meets the failure.
	basis := fsys.NameBasis(0, loc.Index, img[loc.Offset:loc.Offset+32])
	if !slices.Contains(basis, "name=unknown") || slices.Contains(basis, "name=short-only") {
		t.Errorf("basis %v; want name=unknown and not name=short-only", basis)
	}
	if got := len(fsys.Info().Warnings); got != before+1 {
		t.Errorf("%d warnings after %d: want exactly one counted warning (%q)", got, before, fsys.Info().Warnings)
	}
}

// TestRecoverableNameWalkStructuralFailuresAreUnknown: a directory region that
// cannot be located (an entry past the fixed root, an entry past the end of the
// chain of a subdirectory, a cluster that is no directory) is a failed walk,
// not an absent long name.
func TestRecoverableNameWalkStructuralFailuresAreUnknown(t *testing.T) {
	for _, typ := range []int{16, 32} {
		fsys, _, b := recImage(t, fattest.Options{Type: typ}, []fattest.File{
			{Path: "/D", Dir: true}, {Path: "/D/GONE.BIN", Data: []byte("x"), Deleted: true},
		})
		sub := b.Find("/D/GONE.BIN", true)
		raw := make([]byte, 32)
		for _, c := range []struct {
			name string
			dir  uint32
			idx  int
		}{
			{"past the end of the subdirectory chain", sub.DirCluster, 1 << 20},
			{"a cluster that is no directory", b.Geometry.Clusters + 50, 5},
			{"past the fixed root", 0, 1 << 20},
		} {
			before := len(fsys.Info().Warnings)
			basis := fsys.NameBasis(c.dir, c.idx, raw)
			if !slices.Equal(basis, []string{"name=unknown"}) {
				t.Errorf("FAT%d %s: basis %v, want [name=unknown]", typ, c.name, basis)
			}
			if got := len(fsys.Info().Warnings); got != before+1 {
				t.Errorf("FAT%d %s: %d warnings after %d, want one more", typ, c.name, got, before)
			}
		}
	}
}

// deletedWithName builds a volume with one deleted file with the long name and
// returns its basis.
func deletedWithName(t *testing.T, name string) []string {
	t.Helper()
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: "/" + name, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
	})
	return basisOf(t, fsys, b.Find("/"+name, true))
}

// TestRecoverableLongNameIdentityIsConfirmedNotGuessed: the lost first byte is
// recovered from the checksum; it is accepted only when the long name confirms
// it. A first character that the 8.3 derivation does not pin down (a byte of
// 0x80 or more, anything replaced by an underscore) gives lfn=unverified and no
// long name.
func TestRecoverableLongNameIdentityIsConfirmedNotGuessed(t *testing.T) {
	for _, name := range []string{"_leading deleted name.txt", "~leading deleted name.txt", "$leading deleted name.txt", "%leading deleted name.txt"} {
		if basis := deletedWithName(t, name); !slices.Contains(basis, "lfn=recovered") {
			t.Errorf("%q: basis %v, want lfn=recovered (the first character is its own short-name byte)", name, basis)
		}
	}
	for _, name := range []string{"éclair of a deleted file.txt", "日本語 deleted file name.txt"} {
		basis := deletedWithName(t, name)
		if !slices.Contains(basis, "lfn=unverified") || slices.Contains(basis, "lfn=recovered") || slices.Contains(basis, "name=short-only") {
			t.Errorf("%q: basis %v, want lfn=unverified only", name, basis)
		}
	}
}

// TestRecoverableLongNameFirstCharacterMismatchIsUnverified: the checksum
// recovers '$' but the first long-name unit reads '%': both are in the set of
// generated first bytes, so a looser rule accepted the pair.
func TestRecoverableLongNameFirstCharacterMismatchIsUnverified(t *testing.T) {
	const name = "$leading deleted name.txt"
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/" + name, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true}})
	loc := b.Find("/"+name, true)
	nearest := loc.LFNOffsets[len(loc.LFNOffsets)-1]
	img[nearest+1] = '%'
	basis := basisOf(t, reopen(t, img), loc)
	if !slices.Contains(basis, "lfn=unverified") || slices.Contains(basis, "lfn=recovered") {
		t.Errorf("basis %v, want lfn=unverified", basis)
	}
}

// TestRecoverableNameDamageClasses pins what each kind of damage to the
// long-name entries gives.
func TestRecoverableNameDamageClasses(t *testing.T) {
	setup := func(t *testing.T) (img []byte, b *fattest.Built, loc fattest.EntryLoc) {
		_, img, b = recImage(t, fattest.Options{Type: 16}, []fattest.File{
			{Path: "/LIVE.BIN", Data: []byte("live")},
			{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
		})
		return img, b, b.Find(threeSlots, true)
	}
	t.Run("middle slot replaced", func(t *testing.T) {
		img, b, loc := setup(t)
		live := b.Find("/LIVE.BIN", false)
		copy(img[loc.LFNOffsets[1]:loc.LFNOffsets[1]+32], img[live.Offset:live.Offset+32])
		basis := basisOf(t, reopen(t, img), loc)
		if !slices.Contains(basis, "lfn=recovered") || !slices.Contains(basis, "lfn_tail=unterminated") {
			t.Errorf("basis %v, want lfn=recovered and lfn_tail=unterminated", basis)
		}
	})
	t.Run("only the nearest entry damaged", func(t *testing.T) {
		img, _, loc := setup(t)
		img[loc.LFNOffsets[2]+13] ^= 0xFF
		basis := basisOf(t, reopen(t, img), loc)
		if !slices.Contains(basis, "name=short-only") {
			t.Errorf("basis %v, want name=short-only", basis)
		}
	})
	for _, attr := range []byte{0x18, 0x28} {
		t.Run(fmt.Sprintf("attributes %#x", attr), func(t *testing.T) {
			img, _, loc := setup(t)
			img[loc.Offset+11] = attr
			cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
			if !errors.Is(err, filesys.ErrUnsupported) || cs != nil {
				t.Errorf("Recoverable = %v, %v; want nil and ErrUnsupported", cs, err)
			}
		})
	}
	t.Run("size 0 with an invalid first cluster", func(t *testing.T) {
		_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: []byte("x"), Deleted: true}})
		loc := b.Find("/GONE.BIN", true)
		first, size := uint32(0xFFF0), uint32(0)
		fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
		cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
		if err != nil || len(cs) != 1 || cs[0].Size != 0 || len(cs[0].Runs) != 0 {
			t.Errorf("Recoverable = %+v, %v; want one empty candidate", cs, err)
		}
	})
}

// TestRecoverableFATDoesNotConsultTheFATForASubdirectoryEntry: with the
// directory already listed, recovering a deleted entry of a subdirectory reads
// no FAT sector (the directory's own chain was needed to list it; the file's
// clusters are never looked up).
func TestRecoverableFATDoesNotConsultTheFATForASubdirectoryEntry(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 70)
	for _, typ := range []int{12, 16, 32} {
		img, b := fattest.BuildLayout(fattest.Options{Type: typ}, []fattest.File{
			{Path: "/D", Dir: true}, {Path: "/D/GONE.BIN", Data: data, Deleted: true},
		})
		loc := b.Find("/D/GONE.BIN", true)
		if loc.DirCluster == 0 {
			t.Fatal("fixture: the entry is in the root")
		}
		far, size := b.Geometry.Clusters+1, uint32(100) // far from the FAT sectors the listing read
		fattest.PatchEntry(img, b.Geometry, loc, &far, &size)
		fg := newFATGuard(t, img, b.Geometry)
		fsys, err := fat.Open(fg, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		list, err := fsys.ReadDir(fsys.Root())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list {
			if e.Name == "D" {
				if _, err := fsys.ReadDir(e); err != nil {
					t.Fatal(err)
				}
			}
		}
		fg.armed.Store(true)
		cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
		if err != nil || len(cs) != 1 {
			t.Fatalf("FAT%d: %v, %d candidates", typ, err, len(cs))
		}
	}
}
