package ext4_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// rootBlock returns the image offset of the first block of the root directory.
func rootBlock(t *testing.T, f *ext4.FS) int {
	t.Helper()
	runs, err := f.DirRuns(f.Root())
	if err != nil || len(runs) == 0 {
		t.Fatalf("root runs %v, %v", runs, err)
	}
	return int(runs[0].Offset)
}

func TestStaleCopyOfLiveRecord(t *testing.T) {
	// An htree leaf split leaves a copy of the moved records in the slack of
	// the old block: same inode and name as a live entry.
	files := []ext4test.File{
		{Path: "/a", Data: []byte("a")},
		{Path: "/b", Data: []byte("b")},
		{Path: "/c", Data: []byte("c")},
		{Path: "/gone", Data: []byte("g"), Deleted: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	blk := rootBlock(t, f)
	// Records: . .. a b c (12 bytes each from 24), then the deleted "gone" (16)
	// merged into c; the slack of c starts at 60. Put a copy of b at 76.
	copy(img[blk+76:blk+88], img[blk+36:blk+48])
	es := readDir(t, mustOpen(t, img), "/")
	var stale, other int
	for _, e := range es {
		_, isStale := attr(e, "stale_copy")
		switch {
		case e.Deleted && e.Name == "b":
			if !isStale {
				t.Errorf("the copy of b is not marked stale_copy: %+v", e)
			}
			stale++
		case e.Deleted:
			if isStale {
				t.Errorf("%s is marked stale_copy but is no copy of a live entry", e.Name)
			}
			other++
		case isStale:
			t.Errorf("live entry %s is marked stale_copy", e.Name)
		}
	}
	if stale != 1 || other != 1 {
		t.Errorf("deleted entries: %d stale copies and %d others, want 1 and 1 (%v)", stale, other, names(es))
	}
}

func TestEncryptedDirDeletedNamesMayHoldAnyByte(t *testing.T) {
	files := []ext4test.File{
		{Path: "/enc/live", Data: []byte("l")},
		{Path: "/enc/go\x00ne", Data: []byte("1"), Deleted: true},
		{Path: "/enc/gone2", Data: []byte("2"), Deleted: true},
		{Path: "/plain/live", Data: []byte("l")},
		{Path: "/plain/go\x00ne", Data: []byte("1"), Deleted: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, Encrypt: true}, files)
	f := mustOpen(t, img)
	// Put a '/' in the name of gone2 (not plausible as plaintext; as
	// ciphertext it is fine).
	loc := 0
	for _, e := range readDir(t, f, "/enc") {
		if e.Deleted && string(e.RawName) == "gone2" {
			loc = direntLoc(t, e, 1024)
		}
	}
	if loc == 0 {
		t.Fatal("gone2 not found before patching")
	}
	img[loc+8+2] = '/'
	f = mustOpen(t, img)

	var dead []string
	for _, e := range readDir(t, f, "/enc") {
		if e.Deleted {
			dead = append(dead, string(e.RawName))
			if !e.Encrypted || !strings.HasPrefix(e.Name, "~enc~") {
				t.Errorf("deleted ciphertext name shown as %+v", e)
			}
		}
	}
	sort.Strings(dead)
	if want := []string{"go\x00ne", "go/e2"}; !slices.Equal(dead, want) {
		t.Errorf("deleted names in the encrypted directory = %q, want %q", dead, want)
	}
	for _, e := range readDir(t, f, "/plain") {
		if e.Deleted {
			t.Errorf("a NUL in a plaintext name was accepted: %+v", e)
		}
	}
}

func TestHtreeIndexWithoutCsumSlot(t *testing.T) {
	files := []ext4test.File{{Path: "/big", Dir: true, HTree: true}}
	for i := range 100 {
		files = append(files, ext4test.File{Path: fmt.Sprintf("/big/file-%03d", i), Data: []byte{1}})
	}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	d, _ := f.Lookup("/big")
	root := int(dirRuns(t, f, d)[0].Offset)
	// An index made before metadata_csum was enabled has no dx_tail slot: its
	// limit is one larger. It is still an index block.
	put16(img, root+32, le16(img, root+32)+1)
	put32(img, root+40, ext4test.InodeNumber(5)) // a plausible record in the index area
	put16(img, root+44, 12)
	img[root+46], img[root+47], img[root+48] = 1, 1, 'z'
	cf := mustOpen(t, img)
	es := readDir(t, cf, "/big")
	if len(es) != 100 {
		t.Errorf("%d entries, want 100", len(es))
	}
	for _, e := range es {
		if e.Deleted || e.Name == "z" {
			t.Errorf("index data read as a record: %+v", e)
		}
	}
	if hasWarning(cf.Info(), "checksum") {
		t.Errorf("warnings: %v", cf.Info().Warnings)
	}
}

func TestLookupAliasPreference(t *testing.T) {
	cipher := "~enc~QQ" // ciphertext that looks like a display form of "A"
	bad := string([]byte{'a', 0xFF})
	alias := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	files := []ext4test.File{
		{Path: "/enc/" + cipher, Data: []byte("literal")},
		{Path: "/enc/A", Data: []byte("A")},
		{Path: "/" + bad, Data: []byte("bad")},
		{Path: "/" + alias, Data: []byte("real")},
		{Path: "/cf", Dir: true, Flags: 0x40000000},
		{Path: "/cf/FILE", Data: []byte("f")},
		{Path: "/cf/" + bad, Data: []byte("b")},
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, Encrypt: true}, files))

	// Encrypted directory: only the ~enc~ form matches, never raw bytes.
	e, err := f.Lookup("/enc/~enc~QQ")
	if err != nil || string(e.RawName) != "A" {
		t.Errorf("Lookup(~enc~QQ) = %+v, %v; want the ciphertext \"A\"", e, err)
	}
	lit := "~enc~" + base64.RawURLEncoding.EncodeToString([]byte(cipher))
	if e, err = f.Lookup("/enc/" + lit); err != nil || string(e.RawName) != cipher {
		t.Errorf("Lookup(%s) = %+v, %v; want the literal ciphertext", lit, e, err)
	}
	if _, err = f.Lookup("/enc/A"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("raw ciphertext bytes matched in an encrypted directory: %v", err)
	}

	// A real name that looks like a ~raw~ display form beats the alias, though
	// the aliased entry comes first on disk.
	e, err = f.Lookup("/" + alias)
	if err != nil || e.Name != alias || string(readAll(t, f, e)) != "real" {
		t.Errorf("Lookup(%s) = %+v, %v; want the real file", alias, e, err)
	}
	if e, err = f.Lookup("/" + bad); err != nil || string(readAll(t, f, e)) != "bad" {
		t.Errorf("Lookup(bad bytes) = %+v, %v", e, err)
	}

	// Case folding needs valid UTF-8 on both sides.
	if e, err = f.Lookup("/cf/file"); err != nil || e.Name != "FILE" {
		t.Errorf("casefold Lookup = %+v, %v", e, err)
	}
	if _, err = f.Lookup("/cf/A\xff"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("non-UTF-8 component folded: %v", err)
	}
	if _, err = f.Lookup("/cf/�x"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("U+FFFD matched an invalid byte: %v", err)
	}
	if e, err = f.Lookup("/cf/" + bad); err != nil || string(readAll(t, f, e)) != "b" {
		t.Errorf("exact match in a casefold directory = %+v, %v", e, err)
	}
}

func TestSlackOfZeroInodeRecordInTheMiddle(t *testing.T) {
	files := []ext4test.File{
		{Path: "/x", Data: []byte("x")},
		{Path: "/y", Data: []byte("y")},
		{Path: "/gone", Data: []byte("g"), Deleted: true},
		{Path: "/z", Data: []byte("z")},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	blk := rootBlock(t, mustOpen(t, img))
	put32(img, blk+36, 0) // y's record (. .. x y): its inode is zeroed, gone is merged into it
	var live, dead []string
	for _, e := range readDir(t, mustOpen(t, img), "/") {
		if e.Deleted {
			dead = append(dead, e.Name)
		} else {
			live = append(live, e.Name)
		}
	}
	sort.Strings(live)
	if !slices.Equal(live, []string{"x", "z"}) || !slices.Equal(dead, []string{"gone"}) {
		t.Errorf("live %v dead %v, want [x z] and [gone]", live, dead)
	}
}

func TestBlocksBeyondISize(t *testing.T) {
	files := []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: []byte("a")},
		{Path: "/d/b", Data: []byte("b"), NewBlock: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	put32(img, ino+4, 1024) // i_size covers block 0 only
	f = mustOpen(t, img)
	es := readDir(t, f, "/d")
	if got := names(es); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("entries %v", got)
	}
	if _, ok := attr(byName(t, es, "a"), "beyond_isize"); ok {
		t.Error("a lies inside i_size but is flagged")
	}
	if v, _ := attr(byName(t, es, "b"), "beyond_isize"); v != "true" {
		t.Errorf("b attrs %v, want beyond_isize=true", byName(t, es, "b").Attrs)
	}
	if !hasWarning(f.Info(), "beyond i_size") {
		t.Errorf("no beyond i_size warning: %v", f.Info().Warnings)
	}
}

func TestPartialDirectoryAfterBadExtent(t *testing.T) {
	files := []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: []byte("a")},
		{Path: "/d/b", Data: []byte("b"), NewBlock: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	ib := ino + 0x28
	// The directory has one extent of two blocks. Cut it to block 0 and add a
	// second extent for block 1 that points beyond the filesystem.
	put16(img, ib+12+4, 1)
	put16(img, ib+2, 2)
	put32(img, ib+24, 1)          // ee_block
	put16(img, ib+24+4, 1)        // ee_len
	put16(img, ib+24+6, 0)        // ee_start_hi
	put32(img, ib+24+8, 0xFFFFF0) // ee_start_lo: far outside
	f = mustOpen(t, img)
	d, _ := f.Lookup("/d")
	es, err := f.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir failed instead of listing what it can read: %v", err)
	}
	if got := names(es); !slices.Equal(got, []string{"a"}) {
		t.Errorf("entries %v, want [a]", got)
	}
	if !hasWarning(f.Info(), "block map is damaged") {
		t.Errorf("no damage warning: %v", f.Info().Warnings)
	}
}

func TestSharedSymlinkTarget(t *testing.T) {
	target := strings.Repeat("t/", 40) + "end"
	files := []ext4test.File{{Path: "/l1", Symlink: target}, {Path: "/l2", Data: []byte("x")}}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	blk := rootBlock(t, mustOpen(t, img))
	put32(img, blk+24+12, ext4test.InodeNumber(0)) // l2 becomes a second name of the symlink
	es := readDir(t, mustOpen(t, img), "/")
	for _, n := range []string{"l1", "l2"} {
		if e := byName(t, es, n); e.LinkTarget != target {
			t.Errorf("%s target = %q", n, e.LinkTarget)
		}
	}
}
