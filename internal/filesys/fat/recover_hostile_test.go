package fat_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

func u32(v uint32) *uint32 { return &v }

// TestRecoverableHostileEntries: every damaged deleted entry is a typed corrupt error and no candidate.
func TestRecoverableHostileEntries(t *testing.T) {
	data := bytes.Repeat([]byte{9}, 2*512) // a two-cluster deleted file, patched below
	for _, c := range []struct {
		name  string
		typ   int
		patch func(g fattest.Geometry) (first, size *uint32) // g.Clusters is CountOfClusters: the last cluster is g.Clusters+1
	}{
		{"first cluster 0 with a size", 16, func(fattest.Geometry) (*uint32, *uint32) { return u32(0), u32(4096) }},
		{"first cluster 1", 16, func(fattest.Geometry) (*uint32, *uint32) { return u32(1), u32(512) }},
		{"first cluster past the last", 16, func(g fattest.Geometry) (*uint32, *uint32) { return u32(g.Clusters + 2), u32(512) }},
		{"size larger than the volume", 16, func(fattest.Geometry) (*uint32, *uint32) { return nil, u32(0xFFFFFFFF) }},
		{"range runs past the last cluster", 16, func(g fattest.Geometry) (*uint32, *uint32) { return u32(g.Clusters + 1), u32(3 * 512) }},
		{"FAT32 first with a high half outside", 32, func(fattest.Geometry) (*uint32, *uint32) { return u32(0x0FFF0000), u32(512) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, img, b := recImage(t, fattest.Options{Type: c.typ}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
			loc := b.Find("/GONE.BIN", true)
			first, size := c.patch(b.Geometry)
			fattest.PatchEntry(img, b.Geometry, loc, first, size)
			fsys, err := fat.Open(bytes.NewReader(img), int64(len(img))) // reopen: the entry changed under the reader
			if err != nil {
				t.Fatal(err)
			}
			cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
			var ce *filesys.CorruptError
			if !errors.Is(err, filesys.ErrCorrupt) || !errors.As(err, &ce) || ce.Structure != "recovery map" || cs != nil {
				t.Fatalf("Recoverable = %v, %v; want nil and a *CorruptError of structure \"recovery map\"", cs, err)
			}
		})
	}
}

// TestRecoverableTruncatedImageIsCorruptNotClamped: Open clamps the cluster
// count to a cut image; a file whose range now leaves the volume is corrupt, a
// file wholly inside still maps, and no run reaches past the volume.
func TestRecoverableTruncatedImageIsCorruptNotClamped(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: "/INSIDE.BIN", Data: bytes.Repeat([]byte{4}, 600), Deleted: true},
		{Path: "/CUT.BIN", Data: bytes.Repeat([]byte{5}, 3*512), Deleted: true},
	})
	inside, cut := b.Find("/INSIDE.BIN", true), b.Find("/CUT.BIN", true)
	short := img[:b.Geometry.ClusterOffset(cut.FirstCluster)+512+100] // the middle of the second cluster of CUT.BIN
	fsys, err := fat.Open(bytes.NewReader(short), int64(len(short)))
	if err != nil {
		t.Fatalf("a truncated image does not open: %v", err)
	}
	if cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(cut)}); !errors.Is(err, filesys.ErrCorrupt) || cs != nil {
		t.Fatalf("file leaving the volume: %v, %v", cs, err)
	}
	cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(inside)})
	if err != nil || len(cs) != 1 {
		t.Fatalf("file wholly inside: %v, %d candidates", err, len(cs))
	}
	for _, c := range cs {
		for _, r := range c.Runs {
			if r.Offset < 0 || r.Length < 0 || r.Offset+r.Length > fsys.Info().Size {
				t.Errorf("run %+v is past the %d-byte volume", r, fsys.Info().Size)
			}
		}
	}
}

const longDeleted = "/a long deleted name.txt" // 23 units: two long-name entries

func basisOf(t *testing.T, fsys *fat.FS, loc fattest.EntryLoc) []string {
	t.Helper()
	cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
	if err != nil || len(cs) != 1 {
		t.Fatalf("Recoverable: %v, %d candidates", err, len(cs))
	}
	return cs[0].Basis
}

func TestRecoverableLongNameBasis(t *testing.T) {
	fsys, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: "/LIVE.BIN", Data: []byte("live")},
		{Path: longDeleted, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
	})
	loc := b.Find(longDeleted, true)
	basis := basisOf(t, fsys, loc)
	if !slices.Contains(basis, "lfn=recovered") || slices.Contains(basis, "lfn_tail=unterminated") {
		t.Errorf("complete set: basis %v", basis)
	}
	for _, s := range basis {
		if strings.Contains(s, "long") || strings.Contains(s, "name.txt") {
			t.Errorf("basis %q carries name text", s)
		}
	}
	// Slot reuse: the first on-disk entry (the one with the terminator) is now a live short entry.
	live := b.Find("/LIVE.BIN", false)
	copy(img[loc.LFNOffsets[0]:loc.LFNOffsets[0]+32], img[live.Offset:live.Offset+32])
	basis = basisOf(t, reopen(t, img), loc)
	if !slices.Contains(basis, "lfn=recovered") || !slices.Contains(basis, "lfn_tail=unterminated") {
		t.Errorf("reused slot: basis %v; want lfn=recovered and lfn_tail=unterminated", basis)
	}
}

func TestRecoverableOrphanLongNameIsNotVerified(t *testing.T) {
	const other = "/yet another long file.dat"
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: longDeleted, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
		{Path: other, Data: bytes.Repeat([]byte{2}, 700), Deleted: true, LongName: true},
	})
	checkShort := func(name string, img []byte, loc fattest.EntryLoc) {
		t.Helper()
		basis := basisOf(t, reopen(t, img), loc)
		if !slices.Contains(basis, "name=short-only") {
			t.Errorf("%s: basis %v lacks name=short-only", name, basis)
		}
		for _, s := range basis {
			if strings.HasPrefix(s, "lfn") {
				t.Errorf("%s: basis %v carries %q", name, basis, s)
			}
		}
	}
	a, c := b.Find(longDeleted, true), b.Find(other, true)
	// The checksum byte of every long-name entry of A is flipped.
	broken := slices.Clone(img)
	for _, off := range a.LFNOffsets {
		broken[off+13] ^= 0xFF
	}
	checkShort("flipped checksum", broken, a)
	// C's long-name entries are replaced by those of A: another file's set.
	swapped := slices.Clone(img)
	if len(a.LFNOffsets) != len(c.LFNOffsets) {
		t.Fatalf("fixture: %d and %d long-name entries", len(a.LFNOffsets), len(c.LFNOffsets))
	}
	for i := range a.LFNOffsets {
		copy(swapped[c.LFNOffsets[i]:c.LFNOffsets[i]+32], img[a.LFNOffsets[i]:a.LFNOffsets[i]+32])
	}
	checkShort("another file's set", swapped, c)
}

func TestRecoverableBasisIsBoundedAndClean(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: longDeleted, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
	})
	loc := b.Find(longDeleted, true)
	// Hostile units in the nearest entry (ordinal 1), keeping its first unit so that the set still verifies:
	// controls, a lone surrogate, a bidirectional override and a DEL.
	near := loc.LFNOffsets[len(loc.LFNOffsets)-1]
	pos := []int64{3, 5, 7, 9, 14}
	for i, u := range []uint16{0x0001, 0xD800, 0x202E, 0x007F, 0x0007} {
		img[near+pos[i]] = byte(u)
		img[near+pos[i]+1] = byte(u >> 8)
	}
	basis := basisOf(t, reopen(t, img), loc)
	if !slices.Contains(basis, "lfn=recovered") {
		t.Fatalf("the hostile set was not taken: %v", basis)
	}
	if len(basis) > 64 {
		t.Errorf("%d basis strings", len(basis))
	}
	for _, s := range basis {
		if len(s) > 256 || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 || strings.ContainsRune(s, 0x202E) {
			t.Errorf("basis string %q is unbounded or unclean", s)
		}
	}
}

const threeSlots = "/a deleted file with a long name.txt" // 36 units: three long-name entries

func TestRecoverableReadsFewBlocksForAKnownDirectory(t *testing.T) {
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
	})
	loc := b.Find(threeSlots, true)
	if len(loc.LFNOffsets) != 3 {
		t.Fatalf("fixture: %d long-name entries, want 3", len(loc.LFNOffsets))
	}
	cr := &countReader{r: bytes.NewReader(img)}
	fsys, err := fat.Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(fsys.Root()); err != nil { // teaches the reader its directories
		t.Fatal(err)
	}
	warnings := slices.Clone(fsys.Info().Warnings)
	before := cr.n.Load()
	basis := basisOf(t, fsys, loc)
	// The entry's sector, the sector before it, and one for a cluster boundary:
	// maxLFNEntries*32/512 + 1 = 2 sectors for the names, plus the entry.
	if n := cr.n.Load() - before; n > 3 {
		t.Errorf("%d reads, want at most 3", n)
	}
	if !slices.Contains(basis, "lfn=recovered") {
		t.Errorf("basis %v", basis)
	}
	if got := fsys.Info().Warnings; !reflect.DeepEqual(got, warnings) {
		t.Errorf("warnings changed: %q -> %q", warnings, got)
	}
}

// TestRecoverableNameWalkIsBoundedByMaxLFNEntries: a hundred deleted long-name
// entries in front of the entry give a verdict from the nearest maxLFNEntries.
// (The metadata cache holds the whole directory by then, so the reads of the
// walk cannot be seen from the underlying reader; the byte bound is a guard
// against a read that bypasses the cache.)
func TestRecoverableNameWalkIsBoundedByMaxLFNEntries(t *testing.T) {
	files := make([]fattest.File, 0, 121)
	for i := 0; i < 120; i++ {
		files = append(files, fattest.File{Path: "/F" + string(rune('A'+i/26)) + string(rune('A'+i%26)) + ".BIN", Data: []byte("x"), Deleted: true})
	}
	files = append(files, fattest.File{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true})
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, files)
	loc := b.Find(threeSlots, true)
	nearest := loc.LFNOffsets[len(loc.LFNOffsets)-1]
	for k := int64(1); k <= 100; k++ { // 100 copies of the nearest entry, right before the first one
		off := loc.LFNOffsets[0] - k*32
		copy(img[off:off+32], img[nearest:nearest+32])
	}
	cr := &byteReader{r: bytes.NewReader(img)}
	fsys, err := fat.Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	before := cr.n
	basis := basisOf(t, fsys, loc)
	if n := cr.n - before; n > (20+1)*32 {
		t.Errorf("%d bytes read, want at most %d (maxLFNEntries entries and the entry)", n, 21*32)
	}
	if !slices.Contains(basis, "lfn=recovered") || !slices.Contains(basis, "lfn_tail=unterminated") {
		t.Errorf("basis %v; the capped walk must stay unterminated", basis)
	}
}

// byteReader counts the bytes asked of the underlying reader.
type byteReader struct {
	r *bytes.Reader
	n int64
}

func (b *byteReader) ReadAt(p []byte, off int64) (int, error) {
	b.n += int64(len(p))
	return b.r.ReadAt(p, off)
}

// TestRecoverableNameWalkStopsAtALiveLongNameEntry: a live long-name entry
// (first byte not 0xE5) belongs to no deleted name: the walk stops before it.
func TestRecoverableNameWalkStopsAtALiveLongNameEntry(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: threeSlots, Data: bytes.Repeat([]byte{1}, 700), Deleted: true, LongName: true},
	})
	loc := b.Find(threeSlots, true)
	img[loc.LFNOffsets[0]] = 0x43 // the entry with the terminator is now live
	basis := basisOf(t, reopen(t, img), loc)
	if !slices.Contains(basis, "lfn=recovered") || !slices.Contains(basis, "lfn_tail=unterminated") {
		t.Errorf("basis %v; the terminating entry is live, so the deleted set is unterminated", basis)
	}
}
