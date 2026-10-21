package ext4_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// wantIncomplete checks the typed incomplete-scan error: it wraps filesys.ErrIncomplete, is ErrLimit for the
// record cap and ErrCorrupt for the other causes, and is never "not found", ambiguous or undecided.
func wantIncomplete(t *testing.T, what string, err error, limit bool) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: no error", what)
	case !errors.Is(err, filesys.ErrIncomplete):
		t.Errorf("%s: %v does not wrap ErrIncomplete", what, err)
	case limit && !errors.Is(err, filesys.ErrLimit), !limit && !errors.Is(err, filesys.ErrCorrupt):
		t.Errorf("%s: %v has the wrong kind (limit %v)", what, err, limit)
	case errors.Is(err, filesys.ErrNotFound), errors.Is(err, filesys.ErrAmbiguous), errors.Is(err, filesys.ErrUndecided):
		t.Errorf("%s: %v is also not-found, ambiguous or undecided", what, err)
	}
}

// dFiles is /d with the named empty files; names after a "|" element start a new directory block.
func dFiles(names ...string) []ext4test.File {
	files := []ext4test.File{{Path: "/d", Dir: true}}
	nb := false
	for _, n := range names {
		if n == "|" {
			nb = true
			continue
		}
		files = append(files, ext4test.File{Path: "/d/" + n, Data: []byte(n), NewBlock: nb})
		nb = false
	}
	return files
}

func dirBlocks(t *testing.T, f *ext4.FS, p string) (first int, d filesys.Entry) {
	t.Helper()
	d, err := f.Lookup(p)
	if err != nil {
		t.Fatal(err)
	}
	runs := dirRuns(t, f, d)
	if len(runs) == 0 || runs[0].Length < 2048 {
		t.Fatalf("directory %s runs %+v: want at least two blocks in the first run", p, runs)
	}
	return int(runs[0].Offset), d
}

func TestLookupAfterCapIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b", "c", "d", "e"))
	f := mustOpen(t, img)
	f.SetDirRecordCap(3)
	_, err := f.Lookup("/d/e")
	wantIncomplete(t, "an entry beyond the cap", err, true)
	if _, err := f.Lookup("/d/missing"); err == nil || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a missing name after the cap = %v, want the incomplete error", err)
	}
	for _, n := range []string{"a", "c"} {
		if e, err := f.Lookup("/d/" + n); err != nil || e.Name != n {
			t.Errorf("Lookup(%s) before the cap = %+v, %v", n, e, err)
		}
	}
}

// Exactly at the cap is not over it (pins existing behaviour); one more entry is. Mutation: set the cut when
// the count REACHES the cap instead of when a record beyond it is seen.
func TestLookupAtExactlyTheCapIsNotCut(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b", "c", "d", "e")))
	f.SetDirRecordCap(5)
	if _, err := f.Lookup("/d/missing"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a missing name in a directory of exactly cap entries = %v, want not found", err)
	}
	g := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b", "c", "d", "e", "f")))
	g.SetDirRecordCap(5)
	_, err := g.Lookup("/d/missing")
	wantIncomplete(t, "cap+1 entries", err, true)
}

func TestLookupInDamagedBlockIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
	f := mustOpen(t, img)
	blk, _ := dirBlocks(t, f, "/d")
	put16(img, blk+1024+4, 0) // rec_len of the first record of block 1 (a block after the first has no "." and "..")
	f = mustOpen(t, img)
	if e, err := f.Lookup("/d/a"); err != nil || e.Name != "a" {
		t.Errorf("a name of the intact block = %+v, %v", e, err)
	}
	for _, n := range []string{"b", "missing"} {
		_, err := f.Lookup("/d/" + n)
		wantIncomplete(t, "Lookup("+n+") with block 1 damaged", err, false)
	}
	if !hasWarning(f.Info(), "rec_len") {
		t.Errorf("no rec_len warning: %v", f.Info().Warnings)
	}
}

// failingReader returns io.ErrUnexpectedEOF for reads that touch [from, to).
type failingReader struct {
	r        io.ReaderAt
	from, to int64
}

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off < f.to && off+int64(len(p)) > f.from {
		return 0, io.ErrUnexpectedEOF
	}
	return f.r.ReadAt(p, off)
}

func TestLookupAfterUnreadableBlockIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
	blk, _ := dirBlocks(t, mustOpen(t, img), "/d")
	r := failingReader{bytes.NewReader(img), int64(blk) + 1024, int64(blk) + 2048}
	f, err := ext4.Open(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if e, err := f.Lookup("/d/a"); err != nil || e.Name != "a" {
		t.Errorf("a name of the readable block = %+v, %v", e, err)
	}
	for _, n := range []string{"b", "missing"} {
		_, err := f.Lookup("/d/" + n)
		wantIncomplete(t, "Lookup("+n+") with block 1 unreadable", err, false)
	}
	if !hasWarning(f.Info(), "is unreadable") {
		t.Errorf("no unreadable-block warning: %v", f.Info().Warnings)
	}
}

func TestLookupAfterDamagedMapIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
	f := mustOpen(t, img)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	ib := ino + 0x28
	put16(img, ib+12+4, 1)        // the first extent covers block 0 only
	put16(img, ib+2, 2)           // two extents
	put32(img, ib+24, 1)          // ee_block
	put16(img, ib+24+4, 1)        // ee_len
	put16(img, ib+24+6, 0)        // ee_start_hi
	put32(img, ib+24+8, 0xFFFFF0) // ee_start_lo: far outside the filesystem
	f = mustOpen(t, img)
	if e, err := f.Lookup("/d/a"); err != nil || e.Name != "a" {
		t.Errorf("a name before the damage = %+v, %v", e, err)
	}
	for _, n := range []string{"b", "missing"} {
		_, err := f.Lookup("/d/" + n)
		wantIncomplete(t, "Lookup("+n+") after a damaged map", err, false)
	}
}

func TestLookupInSparseDirectoryIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b", "|", "c"))
	f := mustOpen(t, img)
	blk, _ := dirBlocks(t, f, "/d")
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	ib := ino + 0x28
	start := uint32(blk / 1024)
	// two extents: block 0 and block 2; logical block 1 becomes a hole inside i_size
	put16(img, ib+2, 2)
	put32(img, ib+12, 0)
	put16(img, ib+12+4, 1)
	put16(img, ib+12+6, 0)
	put32(img, ib+12+8, start)
	put32(img, ib+24, 2)
	put16(img, ib+24+4, 1)
	put16(img, ib+24+6, 0)
	put32(img, ib+24+8, start+2)
	f = mustOpen(t, img)
	for _, n := range []string{"a", "c"} {
		if e, err := f.Lookup("/d/" + n); err != nil || e.Name != n {
			t.Errorf("Lookup(%s) around the hole = %+v, %v", n, e, err)
		}
	}
	_, err := f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name in a sparse directory", err, false)
	if !hasWarning(f.Info(), "sparse") {
		t.Errorf("no sparse warning: %v", f.Info().Warnings)
	}
}

func TestLookupAfterBadNameLenIsAnError(t *testing.T) {
	h := newDirImage(t, true)
	h.img[h.dirent[2]+6] = 255 // c: name_len does not fit rec_len, the record is skipped
	f := mustOpen(t, h.img)
	if e, err := f.Lookup("/d/e"); err != nil || e.Name != "e" {
		t.Errorf("a name after the skipped record = %+v, %v", e, err)
	}
	for _, n := range []string{"c", "missing"} {
		_, err := f.Lookup("/d/" + n)
		wantIncomplete(t, "Lookup("+n+") with a skipped record", err, false)
	}
}

// Ruling Z14: i_size above the 64 MiB read limit is a cut, whose text names the limit.
func TestLookupInOversizeDirectoryIsAnError(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b"))
	f := mustOpen(t, img)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	put32(img, ino+4, 1<<27)
	f = mustOpen(t, img)
	if e, err := f.Lookup("/d/b"); err != nil || e.Name != "b" {
		t.Errorf("a present name = %+v, %v", e, err)
	}
	_, err := f.Lookup("/d/missing")
	wantIncomplete(t, "a missing name in an oversize directory", err, false)
	if err != nil && !strings.Contains(err.Error(), "64 MiB") {
		t.Errorf("the error does not name the 64 MiB limit: %v", err)
	}
}

// A block with a wrong checksum is read in full, so a name missing from it is NOT in doubt (pins existing
// behaviour). Mutation: set the cut for a checksum-bad block.
func TestLookupBadChecksumBlockIsStillSearched(t *testing.T) {
	files := append(dFiles("a", "|", "b"), ext4test.File{Path: "/e", Dir: true}, ext4test.File{Path: "/e/x", Data: []byte("x")})
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	blk, _ := dirBlocks(t, f, "/d")
	img[blk+1023] ^= 0xFF // the stored checksum of block 0
	f = mustOpen(t, img)
	e, err := f.Lookup("/d/a")
	if err != nil || e.Name != "a" {
		t.Fatalf("a name in the checksum-bad block = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/d/nowhere"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a name that exists nowhere = %v, want not found (a checksum-bad block is fully examined)", err)
	}
	// the same missing name in a directory whose block has a forged rec_len IS in doubt
	f2 := mustOpen(t, img)
	d, err := f2.Lookup("/e")
	if err != nil {
		t.Fatal(err)
	}
	eb := int(dirRuns(t, f2, d)[0].Offset)
	put16(img, eb+24+4, 0)
	f2 = mustOpen(t, img)
	_, err = f2.Lookup("/e/nowhere")
	wantIncomplete(t, "a missing name beside a forged rec_len", err, false)
}

// ONE filesystem: the cut is a property of one scan, not remembered. Mutation: store it on the FS.
func TestLookupErrorIsNotCached(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "b", "c", "d", "e")))
	f.SetDirRecordCap(3)
	_, err := f.Lookup("/d/e")
	wantIncomplete(t, "beyond the cap", err, true)
	f.SetDirRecordCap(10)
	if e, err := f.Lookup("/d/e"); err != nil || e.Name != "e" {
		t.Errorf("the same lookup after raising the cap = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/d/nowhere"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("a clean miss after raising the cap = %v, want not found", err)
	}
}

// Ruling Z18: a fold decision is made only from a complete scan. The cap cuts the scan after exactly one fold
// hit and before the exact name: the answer is the incomplete error, not the fold hit and not not-found.
func TestCasefoldIncompleteScanBeatsFoldDecision(t *testing.T) {
	f := cfFS(t, 1, "Stra"+sharpS+"e", "pad1", "pad2", "STRASSE")
	f.SetDirRecordCap(3)
	_, err := f.Lookup("/cf/STRASSE")
	wantIncomplete(t, "the exact name beyond the cap with one fold hit before it", err, true)
	// a second fold hit beyond the cut would make it ambiguous: still the incomplete error
	g := cfFS(t, 1, "Stra"+sharpS+"e", "pad1", "pad2", "strasse")
	g.SetDirRecordCap(3)
	_, err = g.Lookup("/cf/STRASSE")
	wantIncomplete(t, "a possible second hit beyond the cap", err, true)
	// an undecidable name before the cut does not turn the incomplete error into an undecided one
	h := cfFS(t, 1, "x"+string([]byte{0xFF})+"y", "pad1", "pad2", "z")
	h.SetDirRecordCap(3)
	_, err = h.Lookup("/cf/missing")
	wantIncomplete(t, "an undecidable name before the cut", err, true)
	// no cut: the same shapes decide as before
	if e, err := cfFS(t, 1, "Stra"+sharpS+"e").Lookup("/cf/STRASSE"); err != nil || e.Name != "Stra"+sharpS+"e" {
		t.Errorf("a complete scan with one fold hit = %+v, %v", e, err)
	}
}

// An exact (or alias) name that comes BEFORE the cut stands (the scan ends at the exact hit).
func TestCasefoldExactBeforeCutStillWins(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	f := cfFS(t, 1, "STRASSE", bad, "p1", "p2", "p3", "p4")
	f.SetDirRecordCap(3)
	if e, err := f.Lookup("/cf/STRASSE"); err != nil || e.Name != "STRASSE" {
		t.Errorf("the exact name before the cut = %+v, %v", e, err)
	}
	if e, err := f.Lookup("/cf/" + alias); err != nil || string(e.RawName) != bad {
		t.Errorf("the alias name before the cut = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/cf/p4"); err == nil {
		t.Error("a name beyond the cap resolved")
	}
}

// Ruling Z20: an exact (or alias) name that lies AFTER the cut is still returned once the scan reaches it:
// the cut is BEFORE the match (block 0 has a forged rec_len, the name is in block 1). Mutation: decide the
// cut before the exact/alias decision; this test must fail.
func TestCasefoldExactAndAliasAfterTheCutStillWin(t *testing.T) {
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	for _, c := range []struct{ name, query, want string }{
		{"exact", "Exact.TXT", "Exact.TXT"},
		{"alias", alias, bad},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := []ext4test.File{
				{Path: "/cf", Dir: true, Flags: casefoldFlag},
				{Path: "/cf/first"},
				{Path: "/cf/second"},
				{Path: "/cf/" + c.want, NewBlock: true},
			}
			img := ext4test.Build(ext4test.Options{Extents: true, Encoding: 1}, files)
			f := mustOpen(t, img)
			blk, _ := dirBlocks(t, f, "/cf")
			put16(img, blk+24+4, 0) // block 0 is cut after "." and ".."
			f = mustOpen(t, img)
			e, err := f.Lookup("/cf/" + c.query)
			if err != nil || !bytes.Equal(rawName(e), []byte(c.want)) {
				t.Fatalf("Lookup(%q) = %+v, %v; want %q found after the cut", c.query, e, err, c.want)
			}
			// ... while a name that is not there is in doubt, not absent
			_, err = f.Lookup("/cf/nowhere")
			wantIncomplete(t, "a missing name beside the cut", err, false)
		})
	}
}

func TestLookupCutErrorNamesTheCause(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, dFiles("a", "|", "b"))
	f := mustOpen(t, img)
	blk, _ := dirBlocks(t, f, "/d")
	put16(img, blk+1024+4, 0)
	f = mustOpen(t, img)
	_, err := f.Lookup("/d/missing")
	if err == nil || !strings.Contains(err.Error(), "cannot be decided") || !strings.Contains(err.Error(), "rec_len") {
		t.Errorf("error %v does not say why the lookup cannot be decided", err)
	}
}
