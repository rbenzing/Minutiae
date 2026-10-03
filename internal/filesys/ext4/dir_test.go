package ext4_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Compile-time proof that FS is a filesys.FileSystem.
var _ filesys.FileSystem = (*ext4.FS)(nil)

func names(es []filesys.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func byName(t *testing.T, es []filesys.Entry, name string) filesys.Entry {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", name, names(es))
	return filesys.Entry{}
}

func readDir(t *testing.T, f *ext4.FS, path string) []filesys.Entry {
	t.Helper()
	d, err := f.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", path, err)
	}
	es, err := f.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	return es
}

func readAll(t *testing.T, f *ext4.FS, e filesys.Entry) []byte {
	t.Helper()
	fl, err := f.Open(e)
	if err != nil {
		t.Fatalf("Open(%q): %v", e.Name, err)
	}
	b := make([]byte, fl.Size())
	if n, err := fl.ReadAt(b, 0); n != len(b) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt(%q) = %d, %v", e.Name, n, err)
	}
	return b
}

// direntLoc parses the dirent=<block>:<offset> attribute into an image offset.
func direntLoc(t *testing.T, e filesys.Entry, bs int) int {
	t.Helper()
	v, ok := attr(e, "dirent")
	if !ok {
		t.Fatalf("entry %q has no dirent attribute: %v", e.Name, e.Attrs)
	}
	blk, off, ok := strings.Cut(v, ":")
	b, err1 := strconv.Atoi(blk)
	o, err2 := strconv.Atoi(off)
	if !ok || err1 != nil || err2 != nil {
		t.Fatalf("dirent attribute %q is not <block>:<offset>", v)
	}
	return b*bs + o
}

func TestReadDirLiveEntries(t *testing.T) {
	for _, csum := range []bool{false, true} {
		for _, ext := range []bool{false, true} {
			t.Run(fmt.Sprintf("csum=%v extents=%v", csum, ext), func(t *testing.T) {
				data := pat(3000, 1)
				files := []ext4test.File{
					{Path: "/a.txt", Data: data, Mode: 0o600, UID: 1000, GID: 100, Times: [4]int64{0, 0, 1700000100, 0}},
					{Path: "/d", Dir: true},
					{Path: "/d/x", Data: pat(10, 2)},
					{Path: "/link", Symlink: "a.txt"},
					{Path: "/longlink", Symlink: strings.Repeat("p/", 60) + "end"},
				}
				img := ext4test.Build(ext4test.Options{Extents: ext, MetadataCsum: csum}, files)
				f := mustOpen(t, img)
				root := f.Root()
				if root.ID != "inode:2" || root.Type != filesys.TypeDir {
					t.Fatalf("Root = %+v, want inode:2 dir", root)
				}
				es, err := f.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := names(es), []string{"a.txt", "d", "link", "longlink"}; !slices.Equal(got, want) {
					t.Fatalf("root entries = %v, want %v", got, want)
				}
				a := byName(t, es, "a.txt")
				if a.ID != "inode:"+strconv.Itoa(int(ext4test.InodeNumber(0))) || a.Type != filesys.TypeFile ||
					a.Size != 3000 || a.Mode&0o7777 != 0o600 || a.UID != 1000 || a.GID != 100 || a.Deleted || a.Encrypted {
					t.Errorf("a.txt = %+v", a)
				}
				if a.Times.Modified.T.Unix() != 1700000100 {
					t.Errorf("a.txt mtime = %v", a.Times.Modified.T)
				}
				if !bytes.Equal(readAll(t, f, a), data) {
					t.Error("content of a.txt differs")
				}
				if d := byName(t, es, "d"); d.Type != filesys.TypeDir {
					t.Errorf("d type = %v", d.Type)
				}
				if l := byName(t, es, "link"); l.Type != filesys.TypeSymlink || l.LinkTarget != "a.txt" {
					t.Errorf("link = %v %q", l.Type, l.LinkTarget)
				}
				if l := byName(t, es, "longlink"); l.Type != filesys.TypeSymlink || l.LinkTarget != strings.Repeat("p/", 60)+"end" {
					t.Errorf("longlink target = %q", l.LinkTarget)
				}
				if got := names(readDir(t, f, "/d")); !slices.Equal(got, []string{"x"}) {
					t.Errorf("/d entries = %v", got)
				}
				if w := f.Info().Warnings; len(w) != 0 {
					t.Errorf("warnings on a clean image: %v", w)
				}
			})
		}
	}
}

func TestReadDirFlagsDeletedEntries(t *testing.T) {
	files := []ext4test.File{
		{Path: "/keep1", Data: pat(100, 1)},
		{Path: "/gone.txt", Data: pat(2000, 2), Deleted: true},
		{Path: "/keep2", Data: pat(100, 3)},
		{Path: "/gone2", Data: pat(5, 4), Deleted: true},
		{Path: "/gone3", Data: pat(5, 5), Deleted: true},
	}
	for _, csum := range []bool{false, true} {
		img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum}, files)
		f := mustOpen(t, img)
		es := readDir(t, f, "/")
		var live, dead []string
		for _, e := range es {
			if e.Deleted {
				dead = append(dead, e.Name)
			} else {
				live = append(live, e.Name)
			}
		}
		sort.Strings(dead)
		if !slices.Equal(live, []string{"keep1", "keep2"}) || !slices.Equal(dead, []string{"gone.txt", "gone2", "gone3"}) {
			t.Fatalf("csum=%v: live %v dead %v", csum, live, dead)
		}
		g := byName(t, es, "gone.txt")
		if g.ID != "inode:"+strconv.Itoa(int(ext4test.InodeNumber(1))) || g.Type != filesys.TypeFile || g.Size != 2000 {
			t.Errorf("gone.txt = %+v", g)
		}
		if g.Times.Deleted.T.IsZero() {
			t.Error("gone.txt has no deletion time")
		}
		if _, ok := attr(g, "inode_reused"); ok {
			t.Error("a deleted inode is not reused")
		}
		if _, ok := attr(g, "dirent"); !ok {
			t.Error("no dirent attribute")
		}
		if _, err := f.Lookup("/gone.txt"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup of a deleted entry: %v, want ErrNotFound", err)
		}
		if _, err := f.Open(g); !errors.Is(err, filesys.ErrDeleted) {
			t.Errorf("Open of a deleted entry: %v, want ErrDeleted", err)
		}
		for _, n := range []string{"keep1", "keep2"} {
			if _, ok := attr(byName(t, es, n), "dirent"); ok {
				t.Errorf("live entry %s carries a dirent attribute", n)
			}
		}
	}
}

func TestDeletedEntryInodeReuseAndCandidateRules(t *testing.T) {
	files := []ext4test.File{
		{Path: "/keep1", Data: pat(100, 1)},
		{Path: "/gone", Data: pat(10, 2), Deleted: true},
		{Path: "/keep2", Data: pat(100, 3)},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	loc := direntLoc(t, byName(t, readDir(t, f, "/"), "gone"), 1024)
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(1)))

	// The inode was reallocated: links > 0 and no dtime.
	reused := bytes.Clone(img)
	put16(reused, ino+0x1A, 1)
	put32(reused, ino+0x14, 0)
	g := byName(t, readDir(t, mustOpen(t, reused), "/"), "gone")
	if v, _ := attr(g, "inode_reused"); v != "true" || !g.Deleted {
		t.Errorf("reused inode: %+v", g)
	}
	// An orphan-list inode (links > 0, dtime = next inode) is not reuse.
	orphan := bytes.Clone(img)
	put16(orphan, ino+0x1A, 1)
	if _, ok := attr(byName(t, readDir(t, mustOpen(t, orphan), "/"), "gone"), "inode_reused"); ok {
		t.Error("an orphan-list inode is flagged as reused")
	}

	// Candidates are rejected when they break the rules.
	for name, mut := range map[string]func(b []byte){
		"inode zero":         func(b []byte) { put32(b, loc, 0) },
		"inode out of range": func(b []byte) { put32(b, loc, 0x7FFFFFFF) },
		"name length zero":   func(b []byte) { b[loc+6] = 0 },
		"name has slash":     func(b []byte) { b[loc+8] = '/' },
		"name has NUL":       func(b []byte) { b[loc+9] = 0 },
		"name past slack":    func(b []byte) { b[loc+6] = 200 },
	} {
		c := bytes.Clone(img)
		mut(c)
		for _, e := range readDir(t, mustOpen(t, c), "/") {
			if e.Name == "gone" || e.Deleted {
				t.Errorf("%s: candidate was accepted: %+v", name, e)
			}
		}
	}
}

func TestReadDirDeletedFirstEntryInBlock(t *testing.T) {
	// Block 1 of the root starts with a deleted record. The kernel zeroes the
	// inode of a first record (its number is lost) and merges later deletions
	// into its rec_len, so gone2 is found by scanning the slack of that
	// zero-inode first record, and gone1 itself is reported with an unknown
	// inode.
	files := []ext4test.File{
		{Path: "/live0", Data: pat(10, 1)},
		{Path: "/gone1", Data: pat(10, 2), Deleted: true, NewBlock: true},
		{Path: "/gone2", Data: pat(10, 3), Deleted: true},
		{Path: "/live3", Data: pat(10, 4)},
	}
	for _, csum := range []bool{false, true} {
		img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum}, files)
		f := mustOpen(t, img)
		es := readDir(t, f, "/")
		if got, want := names(es), []string{"gone1", "gone2", "live0", "live3"}; !slices.Equal(got, want) {
			t.Fatalf("csum=%v: entries %v, want %v", csum, got, want)
		}
		if !byName(t, es, "gone2").Deleted || byName(t, es, "live0").Deleted || byName(t, es, "live3").Deleted {
			t.Errorf("csum=%v: wrong deleted flags: %+v", csum, es)
		}
		v, _ := attr(byName(t, es, "gone2"), "dirent")
		if !strings.HasSuffix(v, ":16") { // after gone1's 16-byte record
			t.Errorf("csum=%v: gone2 dirent = %q, want an offset of 16", csum, v)
		}
		g1 := byName(t, es, "gone1")
		blk, _, _ := strings.Cut(v, ":")
		if !g1.Deleted || g1.ID != "dirent:"+blk+":0" || g1.Type != filesys.TypeFile ||
			g1.Size != 0 || g1.Mode != 0 || !g1.Times.Modified.T.IsZero() || g1.UID != 0 {
			t.Errorf("csum=%v: gone1 = %+v, want a deleted file with ID dirent:%s:0 and no inode fields", csum, g1, blk)
		}
		if u, _ := attr(g1, "inode"); u != "unknown" {
			t.Errorf("csum=%v: gone1 attrs %v, want inode=unknown", csum, g1.Attrs)
		}
		if d, _ := attr(g1, "dirent"); d != blk+":0" {
			t.Errorf("csum=%v: gone1 dirent = %q", csum, d)
		}
		if _, err := f.Open(g1); !errors.Is(err, filesys.ErrDeleted) {
			t.Errorf("csum=%v: Open(gone1) = %v, want ErrDeleted", csum, err)
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("csum=%v: warnings %v", csum, w)
		}
	}
}

func TestDeletedFirstEntryRules(t *testing.T) {
	files := []ext4test.File{
		{Path: "/live0", Data: pat(10, 1)},
		{Path: "/gone1", Data: pat(10, 2), Deleted: true, NewBlock: true},
		{Path: "/live2", Data: pat(10, 3)},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	runs, err := f.DirRuns(f.Root())
	if err != nil || len(runs) != 1 {
		t.Fatalf("root runs %v, %v", runs, err)
	}
	b1 := int(runs[0].Offset) + 1024 // block 1 starts with the zeroed record

	// Without the filetype feature the type is unknown: clear the feature bit
	// and the file_type byte of every record.
	noft := bytes.Clone(img)
	put32(noft, 1024+0x60, le32(noft, 1024+0x60)&^0x2)
	for _, off := range []int{int(runs[0].Offset) + 7, int(runs[0].Offset) + 19, int(runs[0].Offset) + 31, b1 + 7, b1 + 16 + 7, b1 + 32 + 7} {
		noft[off] = 0
	}
	es := readDir(t, mustOpen(t, noft), "/")
	if g := byName(t, es, "gone1"); !g.Deleted || g.Type != filesys.TypeOther {
		t.Errorf("without filetype: gone1 = %+v, want a deleted entry of type other", g)
	}

	// The name must pass the same checks as any deleted record.
	for name, mut := range map[string]func(b []byte){
		"name has slash": func(b []byte) { b[b1+8] = '/' },
		"name has NUL":   func(b []byte) { b[b1+9] = 0 },
		"name_len zero":  func(b []byte) { b[b1+6] = 0 },
		"name is dot":    func(b []byte) { b[b1+6], b[b1+8] = 1, '.' },
	} {
		c := bytes.Clone(img)
		mut(c)
		for _, e := range readDir(t, mustOpen(t, c), "/") {
			if e.Deleted {
				t.Errorf("%s: accepted %+v", name, e)
			}
		}
	}

	// A hole-free but empty block (inode 0, name_len 0, spanning the block) is
	// not an entry.
	c := bytes.Clone(img)
	clear(c[b1 : b1+1024])
	put16(c, b1+4, 1024)
	for _, e := range readDir(t, mustOpen(t, c), "/") {
		if e.Deleted {
			t.Errorf("an empty block yielded %+v", e)
		}
	}
}

func TestDirRecordCap(t *testing.T) {
	files := []ext4test.File{{Path: "/d", Dir: true}}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files = append(files, ext4test.File{Path: "/d/" + n, Data: []byte(n)})
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)

	f := mustOpen(t, img)
	f.SetDirRecordCap(3)
	d, _ := f.Lookup("/d")
	es, err := f.ReadDir(d)
	if err != nil || len(es) != 3 {
		t.Fatalf("ReadDir = %d entries, %v; want 3 and no error", len(es), err)
	}
	if !hasWarning(f.Info(), "more than 3 entries") {
		t.Errorf("no cap warning in %v", f.Info().Warnings)
	}

	// Exactly at the cap is not over it.
	f = mustOpen(t, img)
	f.SetDirRecordCap(5)
	d, _ = f.Lookup("/d")
	if es, _ = f.ReadDir(d); len(es) != 5 || len(f.Info().Warnings) != 0 {
		t.Errorf("at the cap: %d entries, warnings %v", len(es), f.Info().Warnings)
	}
}

func TestHtreeDirectoryLinearScan(t *testing.T) {
	files := []ext4test.File{{Path: "/big", Dir: true, HTree: true}}
	for i := range 300 {
		files = append(files, ext4test.File{Path: fmt.Sprintf("/big/file-%03d", i), Data: []byte{byte(i)}})
	}
	for _, csum := range []bool{false, true} {
		want := map[string]bool{}
		for i := range 300 {
			want[fmt.Sprintf("file-%03d", i)] = true
		}
		img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum, Groups: 4}, files)
		f := mustOpen(t, img)
		dir, err := f.Lookup("/big")
		if err != nil {
			t.Fatal(err)
		}
		in, _ := f.Inode(ext4test.InodeNumber(0))
		if fl := in.Fields(); fl.Flags&0x1000 == 0 || fl.Size < 3*1024 {
			t.Fatalf("not an htree directory of several blocks: flags %#x size %d", fl.Flags, fl.Size)
		}
		es, err := f.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 300 {
			t.Fatalf("csum=%v: %d entries, want 300", csum, len(es))
		}
		for _, e := range es {
			if !want[e.Name] || e.Deleted {
				t.Errorf("unexpected entry %+v", e)
			}
			delete(want, e.Name)
		}
		if len(want) != 0 {
			t.Errorf("missing names: %v", want)
		}
		for _, n := range []string{"file-000", "file-150", "file-299"} {
			if _, err := f.Lookup("/big/" + n); err != nil {
				t.Errorf("Lookup(%s): %v", n, err)
			}
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("csum=%v: warnings %v", csum, w)
		}
	}
}

func TestHtreeDxBlocksAreNotScannedOrVerified(t *testing.T) {
	files := []ext4test.File{{Path: "/big", Dir: true, HTree: true}}
	for i := range 100 {
		files = append(files, ext4test.File{Path: fmt.Sprintf("/big/file-%03d", i), Data: []byte{1}})
	}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	d, err := f.Lookup("/big")
	if err != nil {
		t.Fatal(err)
	}
	root := int(dirRuns(t, f, d)[0].Offset) // block 0 is the dx root

	// A plausible dirent planted in the dx index area, and a corrupted index.
	c := bytes.Clone(img)
	put32(c, root+40, ext4test.InodeNumber(5))
	put16(c, root+44, 12)
	c[root+46], c[root+47] = 1, 1
	c[root+48] = 'z'
	cf := mustOpen(t, c)
	es := readDir(t, cf, "/big")
	if len(es) != 100 {
		t.Errorf("%d entries, want 100", len(es))
	}
	for _, e := range es {
		if e.Deleted || e.Name == "z" {
			t.Errorf("dx data was read as a directory entry: %+v", e)
		}
		if v, ok := attr(e, "checksum"); ok {
			t.Errorf("entry %s: checksum=%s, but dx blocks are not verified", e.Name, v)
		}
	}
	if hasWarning(cf.Info(), "checksum") {
		t.Errorf("warnings about a dx block: %v", cf.Info().Warnings)
	}
}

// dirRuns returns the byte runs of directory d's data.
func dirRuns(t *testing.T, f *ext4.FS, d filesys.Entry) []filesys.Run {
	t.Helper()
	runs, err := f.DirRuns(d)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestLookupNested(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a", Dir: true},
		{Path: "/a/b", Dir: true},
		{Path: "/a/b/c", Dir: true},
		{Path: "/a/b/c/file.txt", Data: []byte("hello")},
		{Path: "/a/b/same", Data: []byte("1")},
		{Path: "/same", Data: []byte("2")},
		{Path: "/ln", Symlink: "/a"},
		{Path: "/cf", Dir: true, Flags: 0x40000000},
		{Path: "/cf/File.TXT", Data: []byte("c")},
		{Path: "/plain", Dir: true},
		{Path: "/plain/File.TXT", Data: []byte("p")},
	}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)

	e, err := f.Lookup("/a/b/c/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(readAll(t, f, e)) != "hello" || e.Type != filesys.TypeFile {
		t.Errorf("entry %+v", e)
	}
	if r, err := f.Lookup("/"); err != nil || r.ID != "inode:2" {
		t.Errorf("Lookup(/) = %+v, %v", r, err)
	}
	if d, err := f.Lookup("/a//b/"); err != nil || d.Type != filesys.TypeDir {
		t.Errorf("Lookup(/a//b/) = %+v, %v", d, err)
	}
	if s, err := f.Lookup("/a/b/same"); err != nil || string(readAll(t, f, s)) != "1" {
		t.Errorf("Lookup(/a/b/same) = %+v, %v", s, err)
	}
	for _, p := range []string{"/nope", "/a/nope", "/a/b/c/file.txt/x", "/a/./b", "/a/../a", "/ln/b", "/A"} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if l, err := f.Lookup("/ln"); err != nil || l.Type != filesys.TypeSymlink || l.LinkTarget != "/a" {
		t.Errorf("symlinks are reported, not followed: %+v, %v", l, err)
	}
	// Case-insensitive directories compare with EqualFold; others are exact.
	if c, err := f.Lookup("/cf/file.txt"); err != nil || c.Name != "File.TXT" {
		t.Errorf("casefold Lookup = %+v, %v", c, err)
	}
	if _, err := f.Lookup("/plain/file.txt"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("exact directory matched a different case: %v", err)
	}
	if _, err := f.Lookup("/plain/File.TXT"); err != nil {
		t.Error(err)
	}
}

func TestReadDirRefusals(t *testing.T) {
	files := []ext4test.File{{Path: "/f", Data: []byte("x")}, {Path: "/gone", Dir: true, Deleted: true}, {Path: "/keep", Data: []byte("x")}}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	file, _ := f.Lookup("/f")
	if _, err := f.ReadDir(file); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir of a file: %v, want ErrUnsupported", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "nonsense", Type: filesys.TypeDir}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ReadDir of a bad id: %v, want ErrNotFound", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: "inode:4000000", Type: filesys.TypeDir}); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("ReadDir of an out-of-range inode: %v, want a CorruptError", err)
	}
	var del filesys.Entry
	for _, e := range readDir(t, f, "/") {
		if e.Name == "gone" {
			del = e
		}
	}
	if !del.Deleted {
		t.Fatal("the deleted directory is not listed")
	}
	if _, err := f.ReadDir(del); !errors.Is(err, filesys.ErrDeleted) {
		t.Errorf("ReadDir of a deleted directory: %v, want ErrDeleted", err)
	}
}

func TestEncryptedNames(t *testing.T) {
	cipher := string([]byte{0xFF, 0x01, 0x80, 'c', 'i', 'p', 'h', 'e', 'r', 0x9C})
	cipher2 := "plainlooking"
	files := []ext4test.File{
		{Path: "/enc/" + cipher, Data: pat(1500, 1)},
		{Path: "/enc/" + cipher2, Data: pat(10, 2)},
		{Path: "/clear", Data: pat(10, 3)},
	}
	for _, csum := range []bool{false, true} {
		img := ext4test.Build(ext4test.Options{Extents: true, Encrypt: true, MetadataCsum: csum}, files)
		f := mustOpen(t, img)
		if !f.Info().Encrypted {
			t.Error("Info.Encrypted is false")
		}
		root := readDir(t, f, "/")
		if e := byName(t, root, "enc"); !e.Encrypted || e.Type != filesys.TypeDir {
			t.Errorf("enc dir = %+v", e)
		}
		if e := byName(t, root, "clear"); e.Encrypted {
			t.Errorf("clear = %+v", e)
		}
		es := readDir(t, f, "/enc")
		if len(es) != 2 {
			t.Fatalf("%d entries in /enc", len(es))
		}
		want := "~enc~" + base64.RawURLEncoding.EncodeToString([]byte(cipher))
		e := byName(t, es, want)
		if !e.Encrypted || string(e.RawName) != cipher {
			t.Errorf("entry = %+v", e)
		}
		want2 := "~enc~" + base64.RawURLEncoding.EncodeToString([]byte(cipher2))
		if e2 := byName(t, es, want2); !e2.Encrypted || string(e2.RawName) != cipher2 {
			t.Errorf("entry2 = %+v", e2)
		}
		// The ~enc~ form round-trips through Lookup, and content is ciphertext.
		got, err := f.Lookup("/enc/" + want)
		if err != nil || got.ID != e.ID {
			t.Fatalf("Lookup(%q) = %+v, %v", want, got, err)
		}
		if !bytes.Equal(readAll(t, f, got), pat(1500, 1)) {
			t.Error("encrypted content is not returned raw")
		}
		if _, err := f.Lookup("/enc/~enc~" + base64.RawURLEncoding.EncodeToString([]byte("other"))); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup of a missing ~enc~ name: %v", err)
		}
		// A malformed ~enc~ suffix is a plain miss, not a crash.
		if _, err := f.Lookup("/enc/~enc~!!!"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup of a malformed ~enc~ name: %v", err)
		}
	}
}

func TestRawNames(t *testing.T) {
	bad := string([]byte{'a', 0xFF, 'b', 0xC3})
	files := []ext4test.File{
		{Path: "/" + bad, Data: []byte("raw")},
		{Path: "/café", Data: []byte("utf8")},
		{Path: "/~raw~literal", Data: []byte("lit")},
	}
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	es := readDir(t, f, "/")
	display := "~raw~" + base64.RawURLEncoding.EncodeToString([]byte(bad))
	e := byName(t, es, display)
	if string(e.RawName) != bad || e.Encrypted {
		t.Errorf("raw entry = %+v", e)
	}
	if u := byName(t, es, "café"); u.RawName != nil {
		t.Errorf("valid UTF-8 name carries RawName %q", u.RawName)
	}
	for _, p := range []string{"/" + display, "/" + bad} {
		got, err := f.Lookup(p)
		if err != nil || got.ID != e.ID {
			t.Errorf("Lookup(%q) = %+v, %v", p, got, err)
		}
	}
	// A real name that looks like the raw form still wins as an exact match.
	if l, err := f.Lookup("/~raw~literal"); err != nil || string(readAll(t, f, l)) != "lit" {
		t.Errorf("Lookup(~raw~literal) = %+v, %v", l, err)
	}
}

func TestInlineDirectory(t *testing.T) {
	files := []ext4test.File{{Path: "/idir", Dir: true, Inline: true}}
	var want []string
	for i := 1; i <= 7; i++ { // 4 records fit in i_block; the rest go to system.data
		n := "n" + strconv.Itoa(i)
		want = append(want, n)
		files = append(files, ext4test.File{Path: "/idir/" + n, Data: []byte(n), Deleted: i == 6})
	}
	img := ext4test.Build(ext4test.Options{Extents: true, InlineData: true}, files)
	f := mustOpen(t, img)

	es := readDir(t, f, "/idir")
	if got := names(es); !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	for _, e := range es {
		if e.Deleted != (e.Name == "n6") {
			t.Errorf("%s: Deleted = %v", e.Name, e.Deleted)
		}
	}
	if v, _ := attr(byName(t, es, "n6"), "dirent"); !strings.HasPrefix(v, "inline:") {
		t.Errorf("n6 dirent = %q, want inline:<offset>", v)
	}
	if e, err := f.Lookup("/idir/n7"); err != nil || string(readAll(t, f, e)) != "n7" {
		t.Errorf("Lookup(/idir/n7) = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/idir/n6"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup of the deleted inline entry: %v", err)
	}

	// A small inline directory that fits in i_block, and an empty one.
	img = ext4test.Build(ext4test.Options{Extents: true, InlineData: true, MetadataCsum: true}, []ext4test.File{
		{Path: "/small", Dir: true, Inline: true},
		{Path: "/small/x", Data: []byte("x")},
		{Path: "/empty", Dir: true, Inline: true},
	})
	f = mustOpen(t, img)
	if got := names(readDir(t, f, "/small")); !slices.Equal(got, []string{"x"}) {
		t.Errorf("small = %v", got)
	}
	if got := readDir(t, f, "/empty"); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}
}

// dirImage is a block-mapped image with directory /d holding names a..e, and
// the offsets needed to damage it.
type dirImage struct {
	img    []byte
	f      *ext4.FS
	blk    int // image offset of /d's first block
	dino   int // image offset of /d's inode
	dirent [5]int
}

func newDirImage(t *testing.T, extents bool) *dirImage {
	t.Helper()
	files := []ext4test.File{{Path: "/d", Dir: true}}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files = append(files, ext4test.File{Path: "/d/" + n, Data: []byte(n)})
	}
	img := ext4test.Build(ext4test.Options{Extents: extents}, files)
	f := mustOpen(t, img)
	d, err := f.Lookup("/d")
	if err != nil {
		t.Fatal(err)
	}
	h := &dirImage{img: img, f: f, dino: inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))}
	h.blk = int(dirRuns(t, f, d)[0].Offset)
	for i := range h.dirent {
		h.dirent[i] = h.blk + 24 + 12*i // ".", ".." then 12-byte records
	}
	return h
}

func (h *dirImage) open(t *testing.T) (*ext4.FS, []filesys.Entry, error) {
	t.Helper()
	f := mustOpen(t, h.img)
	d, err := f.Lookup("/d")
	if err != nil {
		return f, nil, err
	}
	done := make(chan struct{})
	var es []filesys.Entry
	var rerr error
	go func() {
		defer close(done)
		es, rerr = f.ReadDir(d)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("ReadDir did not finish")
	}
	return f, es, rerr
}

func TestDirHostile(t *testing.T) {
	cases := []struct {
		name    string
		extents bool
		mutate  func(h *dirImage)
		want    []string // live entries expected (nil: do not check)
		warning string
		wantErr bool
		maxRecs int
	}{
		{"rec_len 0", true, func(h *dirImage) { put16(h.img, h.dirent[2]+4, 0) }, []string{"a", "b"}, "rec_len", false, 5},
		{"rec_len past the block", true, func(h *dirImage) { put16(h.img, h.dirent[2]+4, 4000) }, []string{"a", "b"}, "rec_len", false, 5},
		{"rec_len not a multiple of 4", true, func(h *dirImage) { put16(h.img, h.dirent[2]+4, 13) }, []string{"a", "b"}, "rec_len", false, 5},
		{"rec_len below 12", true, func(h *dirImage) { put16(h.img, h.dirent[2]+4, 8) }, []string{"a", "b"}, "rec_len", false, 5},
		{"name_len past rec_len", true, func(h *dirImage) { h.img[h.dirent[2]+6] = 255 }, []string{"a", "b", "d", "e"}, "name_len", false, 5},
		{"2 GiB size", true, func(h *dirImage) { put32(h.img, h.dino+4, 1<<31) }, []string{"a", "b", "c", "d", "e"}, "64 MiB", false, 5},
		{"all zero block", true, func(h *dirImage) { clear(h.img[h.blk+24 : h.blk+1024]) }, []string{}, "rec_len", false, 0},
		{"duplicate block pointers", false, func(h *dirImage) {
			put32(h.img, h.dino+4, 12*1024)
			for i := range 12 {
				put32(h.img, h.dino+0x28+4*i, uint32(h.blk/1024))
			}
		}, nil, "", false, 5 * 12},
		{"indirect cycle", false, func(h *dirImage) {
			put32(h.img, h.dino+4, 400*1024) // reaches the double-indirect pointer
			clear(h.img[h.dino+0x28 : h.dino+0x28+48])
			const self = 1000 // an unused block: a double-indirect block pointing at itself
			put32(h.img, h.dino+0x28+4*13, self)
			put32(h.img, self*1024, self)
		}, nil, "", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDirImage(t, tc.extents)
			tc.mutate(h)
			f, es, err := h.open(t)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ReadDir error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("error %v is not a CorruptError", err)
			}
			if tc.want != nil {
				got := []string{}
				for _, e := range es {
					if !e.Deleted {
						got = append(got, e.Name)
					}
				}
				sort.Strings(got)
				if !slices.Equal(got, tc.want) {
					t.Errorf("entries %v, want %v", got, tc.want)
				}
			}
			if len(es) > tc.maxRecs {
				t.Errorf("%d entries, want at most %d", len(es), tc.maxRecs)
			}
			if tc.warning != "" && !hasWarning(f.Info(), tc.warning) {
				t.Errorf("no %q warning in %v", tc.warning, f.Info().Warnings)
			}
			_, _ = f.Lookup("/d/e") // Lookup survives the same damage
		})
	}
}

func TestLiveEntryWithUnreadableInode(t *testing.T) {
	h := newDirImage(t, true)
	put32(h.img, h.dirent[1], 0x7FFFFFFF) // b -> an inode beyond the table
	_, es, err := h.open(t)
	if err != nil {
		t.Fatal(err)
	}
	b := byName(t, es, "b")
	if v, _ := attr(b, "inode"); v != "unreadable" || b.Type != filesys.TypeFile || b.ID != "inode:2147483647" || b.Deleted {
		t.Errorf("b = %+v", b)
	}
	if _, ok := attr(byName(t, es, "a"), "inode"); ok {
		t.Error("a readable inode is flagged unreadable")
	}
}

// treeFiles is a small tree: directories, files, a symlink and a deleted file.
func treeFiles() []ext4test.File {
	return []ext4test.File{
		{Path: "/etc", Dir: true},
		{Path: "/etc/passwd", Data: []byte("root")},
		{Path: "/etc/conf.d", Dir: true},
		{Path: "/etc/conf.d/net", Data: []byte("net")},
		{Path: "/etc/conf.d/old", Data: []byte("old"), Deleted: true},
		{Path: "/home/u/notes.txt", Data: pat(2500, 1)},
		{Path: "/home/u/link", Symlink: "/etc"},
		{Path: "/z", Data: []byte("z")},
	}
}

func TestWalkWholeTree(t *testing.T) {
	for _, csum := range []bool{false, true} {
		f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: csum}, treeFiles()))
		var got []string
		err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
			if err != nil {
				t.Errorf("%s: %v", p, err)
				return nil
			}
			s := p + " " + e.Type.String()
			if e.Deleted {
				s += " deleted"
			}
			got = append(got, s)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"/etc dir",
			"/etc/conf.d dir",
			"/etc/conf.d/net file",
			"/etc/conf.d/old file deleted",
			"/etc/passwd file",
			"/home dir",
			"/home/u dir",
			"/home/u/link symlink",
			"/home/u/notes.txt file",
			"/z file",
		}
		if !slices.Equal(got, want) {
			t.Errorf("csum=%v walk:\n got %q\nwant %q", csum, got, want)
		}
	}
}

func TestWalkDetectsDirectoryCycle(t *testing.T) {
	files := []ext4test.File{{Path: "/a", Dir: true}, {Path: "/a/b", Dir: true}, {Path: "/a/b/loop", Dir: true}, {Path: "/a/b/x", Data: []byte("x")}}
	img := ext4test.Build(ext4test.Options{Extents: true}, files)
	f := mustOpen(t, img)
	d, err := f.Lookup("/a/b")
	if err != nil {
		t.Fatal(err)
	}
	// "loop" is the first record after "." and ".." in /a/b's block: make it
	// point back at /a.
	put32(img, int(dirRuns(t, f, d)[0].Offset)+24, ext4test.InodeNumber(0))
	f = mustOpen(t, img)
	var corrupt bool
	err = filesys.Walk(f, f.Root(), "/", func(p string, _ filesys.Entry, err error) error {
		if err != nil {
			if !errors.Is(err, filesys.ErrCorrupt) || p != "/a/b/loop" {
				t.Errorf("%s: unexpected error %v", p, err)
			}
			corrupt = true
		}
		return nil
	})
	if err != nil || !corrupt {
		t.Fatalf("Walk = %v, cycle reported = %v", err, corrupt)
	}
}

func TestDirBlockChecksum(t *testing.T) {
	files := []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/a", Data: []byte("a")},
		{Path: "/d/b", Data: []byte("b")},
		{Path: "/d/c", Data: []byte("c"), NewBlock: true},
		{Path: "/d/d", Data: []byte("d")},
		{Path: "/d/gone", Data: []byte("g"), Deleted: true},
	}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	d, _ := f.Lookup("/d")
	runs := dirRuns(t, f, d)
	if len(runs) != 1 || runs[0].Length != 2048 {
		t.Fatalf("directory runs %v, want one 2-block run", runs)
	}
	for _, e := range readDir(t, f, "/d") {
		if v, ok := attr(e, "checksum"); ok {
			t.Errorf("clean image: %s has checksum=%s", e.Name, v)
		}
	}
	if len(f.Info().Warnings) != 0 {
		t.Fatalf("warnings on a clean image: %v", f.Info().Warnings)
	}

	first := int(runs[0].Offset)
	for name, mut := range map[string]func(b []byte){
		"body byte":   func(b []byte) { b[first+1000]++ }, // slack of the last record
		"stored csum": func(b []byte) { b[first+1024-1] ^= 0x80 },
		"tail erased": func(b []byte) { clear(b[first+1024-12 : first+1024]) },
	} {
		c := bytes.Clone(img)
		mut(c)
		cf := mustOpen(t, c)
		es := readDir(t, cf, "/d")
		if !hasWarning(cf.Info(), "checksum") {
			t.Errorf("%s: no checksum warning in %v", name, cf.Info().Warnings)
		}
		for _, e := range es {
			v, bad := attr(e, "checksum")
			inFirst := e.Name == "a" || e.Name == "b"
			if bad != inFirst || (bad && v != "bad") {
				t.Errorf("%s: entry %s checksum attr = %q (present %v), want present=%v", name, e.Name, v, bad, inFirst)
			}
		}
	}
	// Corruption in the second block flags only its entries (c, d, gone).
	c := bytes.Clone(img)
	c[first+1024+500]++
	for _, e := range readDir(t, mustOpen(t, c), "/d") {
		_, bad := attr(e, "checksum")
		if bad != (e.Name == "c" || e.Name == "d" || e.Name == "gone") {
			t.Errorf("second block: entry %s flagged=%v", e.Name, bad)
		}
	}
	// Without metadata_csum nothing is verified.
	plain := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, files))
	readDir(t, plain, "/d")
	if len(plain.Info().Warnings) != 0 {
		t.Errorf("warnings without metadata_csum: %v", plain.Info().Warnings)
	}
}

func TestExtentBlockChecksum(t *testing.T) {
	data := pat(1024, 1)
	files := []ext4test.File{{Path: "/f", Pieces: []ext4test.Piece{{Block: 0, Data: data}, {Block: 2, Data: data}, {Block: 4, Data: data}, {Block: 6, Data: data}}, Scatter: true, ExtentLeaves: 2}}
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, files)
	f := mustOpen(t, img)
	checkFile(t, img, f, ext4test.InodeNumber(0), expected(1024, []ext4test.Piece{{Block: 0, Data: data}, {Block: 2, Data: data}, {Block: 4, Data: data}, {Block: 6, Data: data}}, 6*1024+1024))
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings on a clean image: %v", w)
	}
	ino := inodeOff(t, f, 1024, 256, 128, int(ext4test.InodeNumber(0)))
	leaf := int(le32(img, ino+0x28+12+4)) * 1024 // first index entry's leaf

	for name, mut := range map[string]func(b []byte){
		"stored checksum":   func(b []byte) { b[leaf+1020] ^= 1 },
		"unused entry area": func(b []byte) { b[leaf+500] = 7 },
	} {
		c := bytes.Clone(img)
		mut(c)
		cf := mustOpen(t, c)
		fl, err := tryOpenNum(cf, ext4test.InodeNumber(0))
		if err != nil {
			t.Fatalf("%s: a checksum mismatch must not fail Open: %v", name, err)
		}
		if fl.Size() != 7*1024 {
			t.Errorf("%s: size %d", name, fl.Size())
		}
		if !hasWarning(cf.Info(), "extent block") || !hasWarning(cf.Info(), "checksum") {
			t.Errorf("%s: no extent checksum warning in %v", name, cf.Info().Warnings)
		}
		// Opening again does not repeat the warning.
		before := len(cf.Info().Warnings)
		if _, err := tryOpenNum(cf, ext4test.InodeNumber(0)); err != nil {
			t.Fatal(err)
		}
		if after := len(cf.Info().Warnings); after != before {
			t.Errorf("%s: warnings grew from %d to %d on a repeat read", name, before, after)
		}
	}
	// A different generation changes the seed: a stale leaf is detected too.
	c := bytes.Clone(img)
	put32(c, ino+0x64, 99)
	if cf := mustOpen(t, c); !func() bool {
		_, _ = tryOpenNum(cf, ext4test.InodeNumber(0))
		return hasWarning(cf.Info(), "extent block")
	}() {
		t.Error("a changed generation was not noticed")
	}
}

func TestWarningsAreDedupedCappedAndConcurrencySafe(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true}, nil))
	base := len(f.Info().Warnings)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				f.Warn("w%d", i) // the same 300 messages from every goroutine
				_ = f.Info()
			}
		}()
	}
	wg.Wait()
	if got := len(f.Info().Warnings); got != base+300 {
		t.Fatalf("%d warnings, want %d (deduplicated)", got, base+300)
	}
	for i := range 2000 {
		f.Warn("extra %d", i)
	}
	ws := f.Info().Warnings
	if len(ws) != 1001 || ws[1000] != "further warnings suppressed" {
		t.Fatalf("%d warnings, last %q; want 1000 plus one suppression line", len(ws), ws[len(ws)-1])
	}
	f.Warn("late")
	if n := len(f.Info().Warnings); n != 1001 {
		t.Errorf("warnings grew past the cap: %d", n)
	}
	// Info returns a snapshot.
	ws[0] = "mutated"
	if f.Info().Warnings[0] == "mutated" {
		t.Error("Info exposes its internal warning slice")
	}
}

func TestConcurrentDirectoryReads(t *testing.T) {
	f := mustOpen(t, ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true}, treeFiles()))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := f.Lookup("/etc/conf.d/net"); err != nil {
					t.Error(err)
				}
				if _, err := f.ReadDir(f.Root()); err != nil {
					t.Error(err)
				}
				_ = f.Info()
			}
		}()
	}
	wg.Wait()
}

func TestRecLenFromDisk(t *testing.T) {
	for _, tc := range []struct {
		raw  uint16
		bs   int
		want int
	}{
		{12, 1024, 12}, {0, 1024, 0}, {0xFFFF, 4096, 65535}, // below 64 KiB the value is used as is
		{0, 65536, 65536}, {0xFFFF, 65536, 65536}, // a whole 64 KiB block
		{12, 65536, 12}, {65532, 65536, 65532}, {1, 65536, 1 << 16}, {0xFFFC | 1, 65536, 0xFFFC | 1<<16},
	} {
		if got := ext4.RecLenFromDisk(tc.raw, tc.bs); got != tc.want {
			t.Errorf("RecLenFromDisk(%#x, %d) = %d, want %d", tc.raw, tc.bs, got, tc.want)
		}
	}
}
