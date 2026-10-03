package ext4_test

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

var direntIDRe = regexp.MustCompile(`^dirent:\d+:\d+$`)

// Every deleted entry is identified by where its record lies, so Deleted can
// be derived from the ID; the inode it names stays an attribute.
func TestDeletedEntryIDIsTheRecordLocation(t *testing.T) {
	files := []ext4test.File{
		{Path: "/keep", Data: []byte("keep")},
		{Path: "/gone", Data: []byte("gone"), Deleted: true},
		{Path: "/gone2", Data: []byte("gone2"), Deleted: true},
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	es := readDir(t, f, "/")
	seen := map[string]string{}
	for _, e := range es {
		if !e.Deleted {
			if want := "inode:" + strconv.Itoa(int(ext4test.InodeNumber(0))); e.Name == "keep" && e.ID != want {
				t.Errorf("live entry ID %q, want %q", e.ID, want)
			}
			continue
		}
		if !direntIDRe.MatchString(e.ID) {
			t.Errorf("%s: ID %q, want dirent:<block>:<offset>", e.Name, e.ID)
		}
		if other, dup := seen[e.ID]; dup {
			t.Errorf("%s and %s share the ID %q", e.Name, other, e.ID)
		}
		seen[e.ID] = e.Name
		if v, ok := attr(e, "inode"); !ok || v == "unknown" {
			t.Errorf("%s: attr inode = %q, want the inode number", e.Name, v)
		}
		if v, _ := attr(e, "dirent"); e.ID != "dirent:"+v {
			t.Errorf("%s: ID %q does not match attr dirent %q", e.Name, e.ID, v)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("deleted entries %v, want gone and gone2", seen)
	}
	g := byName(t, es, "gone")
	if v, _ := attr(g, "inode"); v != strconv.Itoa(int(ext4test.InodeNumber(1))) {
		t.Errorf("gone: inode attr %q, want %d", v, ext4test.InodeNumber(1))
	}
}

func TestDeletedEntryIDInlineDirectory(t *testing.T) {
	files := []ext4test.File{{Path: "/idir", Dir: true, Inline: true}}
	for i := 1; i <= 7; i++ {
		n := "n" + strconv.Itoa(i)
		files = append(files, ext4test.File{Path: "/idir/" + n, Data: []byte(n), Deleted: i == 6})
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, InlineData: true}, files))
	n6 := byName(t, readDir(t, f, "/idir"), "n6")
	off, _ := attr(n6, "dirent")
	off = strings.TrimPrefix(off, "inline:")
	want := "dirent:inline:" + strconv.Itoa(int(ext4test.InodeNumber(0))) + ":" + off
	if n6.ID != want || !n6.Deleted {
		t.Errorf("n6 ID = %q (deleted %v), want %q", n6.ID, n6.Deleted, want)
	}
	if v, _ := attr(n6, "inode"); v != strconv.Itoa(int(ext4test.InodeNumber(6))) {
		t.Errorf("n6 inode attr %q", v)
	}
	if _, err := f.Open(n6); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(inline deleted): %v, want ErrDeleted", err)
	}
}

// Deleted-ness is derived from the ID and the disk, never from the Deleted,
// Type or Size fields the caller passes back.
func TestOpenAndReadDirIgnoreForgedEntryFields(t *testing.T) {
	files := []ext4test.File{
		{Path: "/live", Data: []byte("live content")},
		{Path: "/gone", Data: []byte("gone content"), Deleted: true},
		{Path: "/dir", Dir: true},
		{Path: "/dir/x", Data: []byte("x")},
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	es := readDir(t, f, "/")
	gone, live, dir := byName(t, es, "gone"), byName(t, es, "live"), byName(t, es, "dir")
	if !gone.Deleted || gone.ID == "" || !strings.HasPrefix(gone.ID, "dirent:") {
		t.Fatalf("gone = %+v", gone)
	}

	// Forged Deleted=false (and a plausible Type/Size) on a deleted entry.
	forged := gone
	forged.Deleted, forged.Type, forged.Size = false, filesys.TypeFile, 5
	if _, err := f.Open(forged); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(deleted entry, Deleted forged false) = %v, want ErrDeleted", err)
	}
	// A deleted entry whose inode number is rewritten into a live-looking ID
	// is a different entry; the ID is the only identity.
	if _, err := f.Open(filesys.Entry{ID: gone.ID, Name: "x"}); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(bare dirent ID) = %v, want ErrDeleted", err)
	}

	// Forged Size and Type on a live file entry: the content is the inode's.
	lf := live
	lf.Size, lf.Type, lf.Deleted = 3, filesys.TypeDir, true
	if got := string(readAll(t, f, lf)); got != "live content" {
		t.Errorf("Open(live entry, forged fields) read %q, want the inode's content", got)
	}
	fl, err := f.Open(lf)
	if err != nil || fl.Size() != int64(len("live content")) {
		t.Errorf("forged Size changed the file: %v, %v", fl, err)
	}

	// ReadDir: a deleted directory entry is refused even with Deleted forged
	// false; a live directory still lists with a forged Type/Deleted.
	delDir := gone
	delDir.Type, delDir.Deleted = filesys.TypeDir, false
	if _, err := f.ReadDir(delDir); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("ReadDir(deleted entry, Deleted forged false) = %v, want ErrDeleted", err)
	}
	ld := dir
	ld.Type, ld.Deleted = filesys.TypeFile, true
	if got, err := f.ReadDir(ld); err != nil || len(got) != 1 || got[0].Name != "x" {
		t.Errorf("ReadDir(live dir, forged Type/Deleted) = %v, %v", got, err)
	}
	// A regular file passed to ReadDir is still refused, by its inode's mode.
	lfd := live
	lfd.Type = filesys.TypeDir
	if _, err := f.ReadDir(lfd); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(regular file typed as dir) = %v, want ErrUnsupported", err)
	}
}

// Entries found in blocks beyond i_size and stale copies are deleted entries
// too: location IDs, and Open refuses them.
func TestBeyondISizeAndStaleCopyEntriesHaveLocationIDs(t *testing.T) {
	img, dino := dirOf(t)
	put32(img, dino+4, 1024) // i_size covers block 0 only
	f := mustOpen(t, img)
	es := readDir(t, f, "/d")
	b := byName(t, es, "b")
	if !b.Deleted || !direntIDRe.MatchString(b.ID) {
		t.Fatalf("b = %+v, want a deleted entry with a dirent ID", b)
	}
	if v, _ := attr(b, "inode"); v != strconv.Itoa(int(ext4test.InodeNumber(2))) {
		t.Errorf("b inode attr %q", v)
	}
	forged := b
	forged.Deleted = false
	if _, err := f.Open(forged); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("Open(beyond-i_size entry, forged live) = %v, want ErrDeleted", err)
	}
	if a := byName(t, es, "a"); a.Deleted || !strings.HasPrefix(a.ID, "inode:") {
		t.Errorf("a = %+v, want live with an inode ID", a)
	}

	// Stale copy (same setup as TestStaleCopyOfLiveRecord).
	files := []ext4test.File{
		{Path: "/a", Data: []byte("a")},
		{Path: "/b", Data: []byte("b")},
		{Path: "/c", Data: []byte("c")},
		{Path: "/gone", Data: []byte("g"), Deleted: true},
	}
	img = ext4test.Build(ext4test.Options{Extents: true}, files)
	fs2 := mustOpen(t, img)
	blk := rootBlock(t, fs2)
	copy(img[blk+76:blk+88], img[blk+36:blk+48])
	staleSeen := 0
	for _, e := range readDir(t, mustOpen(t, img), "/") {
		if _, stale := attr(e, "stale_copy"); stale {
			staleSeen++
			if !e.Deleted || !direntIDRe.MatchString(e.ID) {
				t.Errorf("stale copy %+v, want a deleted entry with a dirent ID", e)
			}
			if v, _ := attr(e, "inode"); v != strconv.Itoa(int(ext4test.InodeNumber(1))) {
				t.Errorf("stale copy of b: inode attr %q", v)
			}
		}
	}
	if staleSeen != 1 {
		t.Errorf("%d stale copies found, want 1", staleSeen)
	}
}
