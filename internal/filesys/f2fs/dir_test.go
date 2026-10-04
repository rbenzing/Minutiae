package f2fs_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

func treeOpts() f2fstest.Options { return f2fstest.Options{Segments: 2} }

// treeFS builds an image from files and opens it.
func treeFS(t *testing.T, o f2fstest.Options, files []f2fstest.File) (*f2fs.FS, []byte, *f2fstest.Tree) {
	t.Helper()
	img, tree := f2fstest.BuildTree(o, files)
	return mustOpen(t, img), img, tree
}

func nidID(n uint32) string { return fmt.Sprintf("nid:%d", n) }

func entryNames(es []filesys.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func entryByName(t *testing.T, es []filesys.Entry, name string) filesys.Entry {
	t.Helper()
	for _, e := range es {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %q in %q", name, entryNames(es))
	return filesys.Entry{}
}

func attrOf(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

func readDirPath(t *testing.T, f *f2fs.FS, p string) []filesys.Entry {
	t.Helper()
	d, err := f.Lookup(p)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", p, err)
	}
	es, err := f.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", p, err)
	}
	return es
}

func put32(img []byte, off int, v uint32) { binary.LittleEndian.PutUint32(img[off:], v) }

// setDentryBlock overwrites dentry block n of directory dir with blk.
func setDentryBlock(t *testing.T, img []byte, tree *f2fstest.Tree, dir string, n int, blk []byte) {
	t.Helper()
	addrs := tree.DirBlocks[dir]
	if n >= len(addrs) {
		t.Fatalf("directory %q has %d dentry blocks, not %d", dir, len(addrs), n+1)
	}
	copy(img[int(addrs[n])*bs:], blk)
}

// dotBlock returns a dentry block that starts with "." and "..".
func dotBlock() ([]byte, f2fstest.DentryArea) {
	blk := make([]byte, bs)
	area := f2fstest.BlockArea(blk)
	area.Put(0, f2fstest.Dentry{Name: []byte("."), Ino: 3, Type: f2fstest.FTDir})
	area.Put(1, f2fstest.Dentry{Name: []byte(".."), Ino: 3, Type: f2fstest.FTDir})
	return blk, area
}

func TestReadDirLiveEntries(t *testing.T) {
	o := f2fstest.Options{Segments: 2, ExtraAttr: true, InodeCrtime: true, InodeChksum: true, UUID: [16]byte{1, 2, 3}}
	files := []f2fstest.File{
		{Path: "/a.txt", Data: []byte("hello"), Mode: 0o640, UID: 1000, GID: 1001, Times: [4]int64{100, 200, 300, 400}},
		{Path: "/sub", Dir: true, Mode: 0o750},
		{Path: "/sub/inner", Data: []byte("inner")},
		{Path: "/ln", Symlink: "a.txt", Inline: true},
		{Path: "/fifo", Mode: 0o010600},
		{Path: "/héllo ✓", Data: []byte("x")},
	}
	f, _, tree := treeFS(t, o, files)

	root := f.Root()
	if root.ID != "nid:3" || root.Type != filesys.TypeDir || root.Name != "" {
		t.Fatalf("Root = %+v", root)
	}
	es, err := f.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt", "sub", "ln", "fifo", "héllo ✓"}; !slices.Equal(entryNames(es), want) {
		t.Fatalf("names = %q, want %q", entryNames(es), want)
	}
	a := entryByName(t, es, "a.txt")
	if a.ID != nidID(tree.NID["/a.txt"]) || a.Type != filesys.TypeFile || a.Size != 5 || a.Mode != 0o100640 || a.UID != 1000 || a.GID != 1001 {
		t.Errorf("a.txt = %+v", a)
	}
	for name, got := range map[string]filesys.Timestamp{"mtime": a.Times.Modified, "atime": a.Times.Accessed, "ctime": a.Times.Changed, "crtime": a.Times.Created} {
		want := map[string]int64{"mtime": 300, "atime": 100, "ctime": 200, "crtime": 400}[name]
		if got.T.Unix() != want || !got.ZoneKnown {
			t.Errorf("%s = %+v, want %d", name, got, want)
		}
	}
	if a.Deleted || a.Encrypted || a.RawName != nil {
		t.Errorf("a.txt flags: %+v", a)
	}
	if v, ok := attrOf(a, "checksum"); ok {
		t.Errorf("checksum attr %q on a good inode", v)
	}
	sub := entryByName(t, es, "sub")
	if sub.Type != filesys.TypeDir || sub.ID != nidID(tree.NID["/sub"]) {
		t.Errorf("sub = %+v", sub)
	}
	if ln := entryByName(t, es, "ln"); ln.Type != filesys.TypeSymlink || ln.Size != int64(len("a.txt")) {
		t.Errorf("ln = %+v", ln)
	}
	if fifo := entryByName(t, es, "fifo"); fifo.Type != filesys.TypeOther || fifo.Mode != 0o010600 {
		t.Errorf("fifo = %+v", fifo)
	}
	inner, err := f.ReadDir(sub)
	if err != nil || len(inner) != 1 || inner[0].Name != "inner" || inner[0].Size != 5 {
		t.Errorf("ReadDir(sub) = %+v, %v", inner, err)
	}
	// Every listed entry opens by its ID alone.
	fl, err := f.Open(filesys.Entry{ID: a.ID})
	if err != nil || fl.Size() != 5 {
		t.Errorf("Open(a.txt) = %v, %v", fl, err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestReadDirFlagsDeletedEntries(t *testing.T) {
	longName := "a-long-deleted-name-0123456789-abcdefgh" // 40 bytes: 5 slots
	files := []f2fstest.File{
		{Path: "/keep", Data: []byte("keep")},                       // slot 2
		{Path: "/gone.txt", Data: []byte("gone"), Deleted: true},    // slot 3
		{Path: "/" + longName, Data: []byte("L"), Deleted: true},    // slots 4..8
		{Path: "/tail", Data: []byte("tail")},                       // slot 9
		{Path: "/orphan", Deleted: true, NoInode: true},             // slot 10
		{Path: "/reused", Data: []byte("new"), Deleted: true},       // slot 11
		{Path: "/bad\x00name", Data: []byte("z"), Deleted: true},    // implausible name: slot 12, never listed
		{Path: "/slash", RawName: []byte("a/b"), Deleted: true},     // implausible name: slot 13
		{Path: "/last", Data: []byte("last")},                       // slot 14
		{Path: "/sub", Dir: true},                                   // slot 15
		{Path: "/sub/deep", Data: []byte("deep"), Deleted: true},    // sub slot 2
		{Path: "/sub/deep2", Data: []byte("deep2"), Deleted: false}, // sub slot 3
	}
	_, img, tree := treeFS(t, treeOpts(), files)
	// The inode of "reused" now belongs to another directory.
	put32(img, int(tree.Addr[tree.NID["/reused"]])*bs+84, 99) // i_pino
	f := mustOpen(t, img)

	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"keep", "gone.txt", longName, "tail", "orphan", "reused", "last", "sub"}; !slices.Equal(entryNames(es), want) {
		t.Fatalf("names = %q, want %q", entryNames(es), want)
	}
	deleted := map[string]string{"gone.txt": "dentry:3:0:3", longName: "dentry:3:0:4", "orphan": "dentry:3:0:10", "reused": "dentry:3:0:11"}
	seen := map[string]bool{}
	for _, e := range es {
		want, isDel := deleted[e.Name]
		if e.Deleted != isDel {
			t.Errorf("%s: Deleted = %v", e.Name, e.Deleted)
		}
		if isDel && e.ID != want {
			t.Errorf("%s: ID %q, want %q", e.Name, e.ID, want)
		}
		if seen[e.ID] {
			t.Errorf("duplicate ID %q", e.ID)
		}
		seen[e.ID] = true
	}
	gone := entryByName(t, es, "gone.txt")
	if gone.Type != filesys.TypeFile || gone.Size != 4 {
		t.Errorf("gone.txt not described by its inode: %+v", gone)
	}
	if v, _ := attrOf(gone, "inode"); v != fmt.Sprint(tree.NID["/gone.txt"]) {
		t.Errorf("gone.txt attr inode = %q", v)
	}
	if _, ok := attrOf(gone, "inode_reused"); ok {
		t.Errorf("gone.txt marked reused: %+v", gone.Attrs)
	}
	if v, _ := attrOf(entryByName(t, es, "reused"), "inode_reused"); v != "true" {
		t.Errorf("reused: attrs %+v", entryByName(t, es, "reused").Attrs)
	}
	orphan := entryByName(t, es, "orphan")
	if v, _ := attrOf(orphan, "inode_unreadable"); v != "true" || orphan.Type != filesys.TypeFile || orphan.Size != 0 || orphan.Mode != 0 {
		t.Errorf("orphan (inode not in the NAT) = %+v", orphan)
	}
	if v, _ := attrOf(orphan, "inode"); v != fmt.Sprint(tree.NID["/orphan"]) {
		t.Errorf("orphan attr inode = %q", v)
	}

	// A deleted entry is never found by path and never opens.
	for _, p := range []string{"/gone.txt", "/orphan", "/" + longName} {
		if _, err := f.Lookup(p); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("Lookup(%q) = %v, want ErrNotFound", p, err)
		}
	}
	for _, e := range []filesys.Entry{gone, {ID: gone.ID, Type: filesys.TypeFile, Deleted: false, Size: 4}, {ID: "dentry:3:0:99"}} {
		if _, err := f.Open(e); !errors.Is(err, filesys.ErrDeleted) {
			t.Errorf("Open(%+v) = %v, want ErrDeleted", e, err)
		}
		if _, err := f.ReadDir(e); !errors.Is(err, filesys.ErrDeleted) {
			t.Errorf("ReadDir(%+v) = %v, want ErrDeleted", e, err)
		}
	}
	// The live neighbours are unaffected.
	if _, err := f.Lookup("/tail"); err != nil {
		t.Errorf("Lookup(/tail): %v", err)
	}
	sub := readDirPath(t, f, "/sub")
	if deep := entryByName(t, sub, "deep"); !deep.Deleted || deep.ID != fmt.Sprintf("dentry:%d:0:2", tree.NID["/sub"]) {
		t.Errorf("sub/deep = %+v", deep)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

// A deleted name that spans several slots is skipped as a whole: the dentry
// structs of its continuation slots may be stale leftovers that look plausible.
func TestDeletedSlotSkipsPastName(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), []f2fstest.File{{Path: "/x", Data: []byte("x")}})
	blk, area := dotBlock()
	x := tree.NID["/x"]
	long := []byte("deleted-name-spanning-five-slots-!!!!!!!") // 40 bytes
	area.Put(2, f2fstest.Dentry{Name: long, Ino: x, Type: f2fstest.FTReg, Deleted: true})
	// A stale, plausible dentry struct inside the long name's slots.
	binary.LittleEndian.PutUint32(blk[area.Entries+4*11+4:], x)
	binary.LittleEndian.PutUint16(blk[area.Entries+4*11+8:], 3)
	blk[area.Entries+4*11+10] = f2fstest.FTReg
	setDentryBlock(t, img, tree, "/", 0, blk)
	f := mustOpen(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != string(long) || !es[0].Deleted || es[0].ID != "dentry:3:0:2" {
		t.Fatalf("entries = %+v", es)
	}
}

func TestMultiBlockDirectory(t *testing.T) {
	files := []f2fstest.File{{Path: "/big", Dir: true}}
	var want []string
	for i := range 600 {
		n := fmt.Sprintf("f%03d", i)
		files = append(files, f2fstest.File{Path: "/big/" + n})
		want = append(want, n)
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	if n := len(tree.DirBlocks["/big"]); n < 3 {
		t.Fatalf("builder made %d dentry blocks, want >= 3", n)
	}
	es := readDirPath(t, f, "/big")
	if !slices.Equal(entryNames(es), want) {
		t.Fatalf("listing differs: %d entries, first %q", len(es), entryNames(es)[:min(3, len(es))])
	}
	for _, p := range []string{"/big/f000", "/big/f213", "/big/f214", "/big/f599"} {
		e, err := f.Lookup(p)
		if err != nil || e.ID != nidID(tree.NID[p]) {
			t.Errorf("Lookup(%q) = %+v, %v", p, e, err)
		}
	}
	if _, err := f.Lookup("/big/f600"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(f600) = %v", err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestInlineDentryDirectory(t *testing.T) {
	// Kernel constants for a 4 KiB inode with the default 50-word inline xattr
	// reservation: 873 data slots, MAX_INLINE_DATA 3488, 182 dentries, a
	// 23-byte bitmap and 7 reserved bytes.
	if nr, bm, rs := f2fstest.InlineDentryGeometry(873); nr != 182 || bm != 23 || rs != 7 {
		t.Fatalf("builder geometry = %d/%d/%d, want 182/23/7", nr, bm, rs)
	}
	files := []f2fstest.File{
		{Path: "/idir", Dir: true, Inline: true},
		{Path: "/idir/one", Data: []byte("1")},
		{Path: "/idir/name-of-three-slot", Data: []byte("22")}, // 18 bytes: slots 3..5
		{Path: "/idir/gone", Data: []byte("333"), Deleted: true},
		{Path: "/idir/sub", Dir: true, Inline: true},
		{Path: "/idir/sub/leaf", Data: []byte("leaf")},
		{Path: "/full", Dir: true, Inline: true},
	}
	var full []string
	for i := range 180 { // plus "." and ".." fills all 182 slots
		n := fmt.Sprintf("n%03d", i)
		files = append(files, f2fstest.File{Path: "/full/" + n})
		full = append(full, n)
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	es := readDirPath(t, f, "/idir")
	if want := []string{"one", "name-of-three-slot", "gone", "sub"}; !slices.Equal(entryNames(es), want) {
		t.Fatalf("idir = %q, want %q", entryNames(es), want)
	}
	gone := entryByName(t, es, "gone")
	if !gone.Deleted || gone.ID != fmt.Sprintf("dentry:%d:0:6", tree.NID["/idir"]) || gone.Size != 3 {
		t.Errorf("gone = %+v", gone)
	}
	if v, _ := attrOf(entryByName(t, es, "one"), "inline"); v != "" {
		t.Errorf("file attr inline = %q", v)
	}
	if v, _ := attrOf(entryByName(t, es, "sub"), "inline"); v != "dentry" {
		t.Errorf("sub attr inline = %q", v)
	}
	if leaf, err := f.Lookup("/idir/sub/leaf"); err != nil || leaf.Size != 4 {
		t.Errorf("Lookup(leaf) = %+v, %v", leaf, err)
	}
	if got := entryNames(readDirPath(t, f, "/full")); !slices.Equal(got, full) {
		t.Errorf("full inline directory: %d entries, want %d", len(got), len(full))
	}
	if _, err := f.Lookup("/full/n179"); err != nil {
		t.Errorf("Lookup of the entry in the last slot: %v", err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

// The inline area is the address area minus the extra header and the inline
// xattr reservation; each volume variant is filled to capacity.
func TestInlineDentryGeometryVariants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     f2fstest.Options
		xattr uint16
		slots int // data slots of the directory inode
	}{
		{"plain", f2fstest.Options{Segments: 2}, 0, 923 - 50},
		{"extra attr, default xattr", f2fstest.Options{Segments: 2, ExtraAttr: true}, 0, 923 - 9 - 50},
		{"flexible, size 0", f2fstest.Options{Segments: 2, ExtraAttr: true, InlineXattr: true}, 0, 923 - 9},
		{"flexible, size 24", f2fstest.Options{Segments: 2, ExtraAttr: true, InlineXattr: true}, 24, 923 - 9 - 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nr, _, _ := f2fstest.InlineDentryGeometry(tc.slots)
			files := []f2fstest.File{{Path: "/d", Dir: true, Inline: true, InlineXattrSize: tc.xattr}}
			var want []string
			for i := range nr - 2 {
				n := fmt.Sprintf("e%03d", i)
				files = append(files, f2fstest.File{Path: "/d/" + n})
				want = append(want, n)
			}
			f, _, _ := treeFS(t, tc.o, files)
			if got := entryNames(readDirPath(t, f, "/d")); !slices.Equal(got, want) {
				t.Errorf("%d entries (capacity %d), want %d", len(got), nr, len(want))
			}
		})
	}
}

func TestLookupNested(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/a", Dir: true},
		{Path: "/a/b", Dir: true},
		{Path: "/a/b/c.txt", Data: []byte("ccc")},
		{Path: "/CF", Dir: true, Casefold: true},
		{Path: "/CF/Readme.TXT", Data: []byte("r")},
		{Path: "/Plain", Dir: true},
		{Path: "/Plain/File", Data: []byte("p")},
		{Path: "/bin", RawName: []byte{0xff, 'a', 'b'}, Data: []byte("b")},
		{Path: "/slashed", RawName: []byte("x/y"), Data: []byte("s")},
		{Path: "/dotdot", RawName: []byte(".."), Data: []byte("d")},
		{Path: "/nul", RawName: []byte("n\x00l"), Data: []byte("n")},
		{Path: "/gone", Data: []byte("g"), Deleted: true},
		{Path: "/sym", Symlink: "/a/b", Inline: true},
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	b64 := base64.RawURLEncoding.EncodeToString
	for _, tc := range []struct {
		path string
		want string // path in tree.NID, "" = not found
	}{
		{"/", "/"},
		{"", "/"},
		{"/a", "/a"},
		{"/a/b/c.txt", "/a/b/c.txt"},
		{"//a///b/c.txt/", "/a/b/c.txt"},
		{"/a/b/c.txt/more", ""},
		{"/a/B", ""},
		{"/a/b/../b/c.txt", ""},
		{"/a/./b", ""},
		{"/missing", ""},
		{"/gone", ""},
		{"/CF/readme.txt", "/CF/Readme.TXT"},
		{"/CF/README.TXT", "/CF/Readme.TXT"},
		{"/Plain/file", ""},
		{"/plain/File", ""},
		{"/~raw~" + b64([]byte{0xff, 'a', 'b'}), "/bin"},
		{"/~raw~" + b64([]byte("x/y")), "/slashed"},
		{"/~raw~" + b64([]byte("..")), "/dotdot"},
		{"/~raw~" + b64([]byte("n\x00l")), "/nul"},
		{"/~enc~" + b64([]byte("x/y")), ""}, // not an encrypted directory
		{"/sym/c.txt", ""},                  // symlinks are not followed
	} {
		e, err := f.Lookup(tc.path)
		if tc.want == "" {
			if !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("Lookup(%q) = %+v, %v; want ErrNotFound", tc.path, e, err)
			}
			continue
		}
		if err != nil || e.ID != nidID(tree.NID[tc.want]) {
			t.Errorf("Lookup(%q) = %+v, %v; want %s", tc.path, e, err, tc.want)
		}
	}
	// The display forms of ReadDir and the raw name round-trip.
	es, _ := f.ReadDir(f.Root())
	for name, raw := range map[string][]byte{
		"~raw~" + b64([]byte{0xff, 'a', 'b'}): {0xff, 'a', 'b'},
		"~raw~" + b64([]byte("x/y")):          []byte("x/y"),
		"~raw~" + b64([]byte("..")):           []byte(".."),
		"~raw~" + b64([]byte("n\x00l")):       []byte("n\x00l"),
	} {
		e := entryByName(t, es, name)
		if !bytes.Equal(e.RawName, raw) {
			t.Errorf("%q: RawName = %q, want %q", name, e.RawName, raw)
		}
	}
	// A valid name never carries a RawName.
	if e := entryByName(t, es, "a"); e.RawName != nil {
		t.Errorf("RawName on a plain name: %q", e.RawName)
	}
}

// Ciphertext names (Android file-based encryption) are shown as ~enc~ +
// base64url, keep the raw bytes, and are found again by that display form.
func TestEncryptedDentryNames(t *testing.T) {
	c1 := []byte{0x9a, '/', 0x00, 0xff, 0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0xa0, 0xb0, 0xc0}
	c2 := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef, '.'}, 8) // 40 bytes
	c3 := []byte{'.', '.', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n'}
	cd := []byte{0x01, '/', 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	files := []f2fstest.File{
		{Path: "/enc", Dir: true, Encrypted: true},
		{Path: "/enc/1", RawName: c1, Data: []byte("cipher-1"), Encrypted: true},
		{Path: "/enc/2", RawName: c2, Dir: true, Encrypted: true},
		{Path: "/enc/2/inner", RawName: c3, Data: []byte("cipher-inner"), Encrypted: true},
		{Path: "/enc/3", RawName: c3, Symlink: "cipher-link", Inline: true, Encrypted: true},
		{Path: "/enc/d", RawName: cd, Data: []byte("deleted"), Encrypted: true, Deleted: true},
		{Path: "/clear", Data: []byte("clear")},
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	b64 := base64.RawURLEncoding.EncodeToString
	es := readDirPath(t, f, "/enc")
	want := []string{"~enc~" + b64(c1), "~enc~" + b64(c2), "~enc~" + b64(c3), "~enc~" + b64(cd)}
	if !slices.Equal(entryNames(es), want) {
		t.Fatalf("names = %q\nwant %q", entryNames(es), want)
	}
	for i, raw := range [][]byte{c1, c2, c3, cd} {
		e := es[i]
		if !bytes.Equal(e.RawName, raw) || !e.Encrypted {
			t.Errorf("%s: RawName %x Encrypted %v", e.Name, e.RawName, e.Encrypted)
		}
	}
	if !es[3].Deleted || !strings.HasPrefix(es[3].ID, "dentry:") {
		t.Errorf("deleted ciphertext name (contains '/'): %+v", es[3])
	}
	// Lookup accepts the displayed form, not the raw bytes.
	e, err := f.Lookup("/enc/" + want[0])
	if err != nil || e.ID != nidID(tree.NID["/enc/1"]) {
		t.Fatalf("Lookup(~enc~ form) = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/enc/" + string(c3)); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(raw ciphertext) = %v, want ErrNotFound", err)
	}
	if e, err := f.Lookup("/enc/" + want[1] + "/" + want[2]); err != nil || e.ID != nidID(tree.NID["/enc/2/inner"]) {
		t.Errorf("nested ~enc~ Lookup = %+v, %v", e, err)
	}
	if _, err := f.Lookup("/enc/" + want[3]); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("Lookup(deleted ciphertext) = %v", err)
	}
	// Content is the stored ciphertext.
	fl, err := f.Open(es[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhole(t, fl); string(got) != "cipher-1" {
		t.Errorf("content = %q", got)
	}
	if top := readDirPath(t, f, "/"); entryByName(t, top, "clear").Encrypted {
		t.Error("plain entry marked encrypted")
	}
}

func TestDentryNameLenPastBlockIsCorrupt(t *testing.T) {
	_, img, tree := treeFS(t, treeOpts(), []f2fstest.File{{Path: "/ok", Data: []byte("o")}, {Path: "/edge", Data: []byte("e")}})
	ok, edge := tree.NID["/ok"], tree.NID["/edge"]
	blk, area := dotBlock()
	area.Put(2, f2fstest.Dentry{Name: []byte("ok"), Ino: ok, Type: f2fstest.FTReg})
	// A 64-byte name in the last 8 slots ends exactly at the end: legal.
	area.Put(206, f2fstest.Dentry{Name: bytes.Repeat([]byte("e"), 64), Ino: edge, Type: f2fstest.FTReg})
	setDentryBlock(t, img, tree, "/", 0, blk)
	f := mustOpen(t, img)
	es, err := f.ReadDir(f.Root())
	if err != nil || len(es) != 2 || es[1].Name != strings.Repeat("e", 64) {
		t.Fatalf("legal last-slot name: %v, %v", entryNames(es), err)
	}
	if len(f.Info().Warnings) != 0 {
		t.Errorf("warnings on a legal block: %v", f.Info().Warnings)
	}

	// name_len claims 5 slots from slot 212 and 255 bytes from slot 213: both
	// run past the filename area. They are not listed, the damage is a warning,
	// and the entries before them are.
	blk, area = dotBlock()
	area.Put(2, f2fstest.Dentry{Name: []byte("ok"), Ino: ok, Type: f2fstest.FTReg})
	area.Put(212, f2fstest.Dentry{Name: []byte("abcdefgh"), Ino: edge, Type: f2fstest.FTReg, NameLen: 40})
	area.Put(213, f2fstest.Dentry{Name: []byte("zzzzzzzz"), Ino: edge, Type: f2fstest.FTReg, NameLen: 255})
	setDentryBlock(t, img, tree, "/", 0, blk)
	f = mustOpen(t, img)
	es, err = f.ReadDir(f.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if !slices.Equal(entryNames(es), []string{"ok"}) {
		t.Errorf("entries = %q, want [ok]", entryNames(es))
	}
	if !hasWarning(f.Info(), "name_len") {
		t.Errorf("no warning about name_len: %v", f.Info().Warnings)
	}
}

func TestDirHostile(t *testing.T) {
	files := []f2fstest.File{{Path: "/f", Data: []byte("f")}, {Path: "/d", Dir: true}, {Path: "/d/x", Data: []byte("x")}}
	listD := func(t *testing.T, f *f2fs.FS) ([]filesys.Entry, error) {
		t.Helper()
		return f.ReadDir(entryByName(t, readDirPath(t, f, "/"), "d"))
	}

	t.Run("garbage block with a full bitmap", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		blk := make([]byte, bs)
		x := uint32(0x9e3779b9)
		for i := range blk {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			blk[i] = byte(x)
		}
		for i := range 27 {
			blk[i] = 0xff
		}
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(es) > f2fstest.NrDentryInBlock {
			t.Errorf("%d entries from one block", len(es))
		}
		_, _ = f.Lookup("/d/anything")
	})

	t.Run("bitmap clear, garbage everywhere", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		blk := bytes.Repeat([]byte{0xa5, 0x5a, 0x01}, bs)[:bs]
		clear(blk[:27])
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		if _, err := listD(t, f); err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
	})

	t.Run("name_len 0 and 65535, file_type 99", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		blk := make([]byte, bs)
		area := f2fstest.BlockArea(blk)
		fi := tree.NID["/f"]
		area.Put(0, f2fstest.Dentry{Name: []byte("zero"), Ino: fi, Type: f2fstest.FTReg})
		binary.LittleEndian.PutUint16(blk[area.Entries+8:], 0)
		area.Put(1, f2fstest.Dentry{Name: []byte("huge"), Ino: fi, Type: f2fstest.FTReg})
		binary.LittleEndian.PutUint16(blk[area.Entries+11+8:], 65535)
		area.Put(2, f2fstest.Dentry{Name: []byte("type"), Ino: fi, Type: 99})
		area.Put(3, f2fstest.Dentry{Name: []byte("zero-ino"), Ino: 0, Type: f2fstest.FTReg})
		area.Put(4, f2fstest.Dentry{Name: []byte("fine"), Ino: fi, Type: f2fstest.FTReg})
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil || !slices.Equal(entryNames(es), []string{"fine"}) {
			t.Fatalf("entries %q, %v; want only [fine]", entryNames(es), err)
		}
		if len(f.Info().Warnings) == 0 {
			t.Error("damage not reported")
		}
	})

	t.Run("free nid, nid beyond the NAT and a reserved inode", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		blk := make([]byte, bs)
		area := f2fstest.BlockArea(blk)
		area.Put(0, f2fstest.Dentry{Name: []byte("free"), Ino: tree.NID["/d/x"] + 1000, Type: f2fstest.FTReg})
		area.Put(1, f2fstest.Dentry{Name: []byte("beyond"), Ino: 0xfffffff0, Type: f2fstest.FTReg})
		area.Put(2, f2fstest.Dentry{Name: []byte("node-ino"), Ino: 1, Type: f2fstest.FTReg})
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil || len(es) != 3 {
			t.Fatalf("entries %q, %v", entryNames(es), err)
		}
		for _, e := range es {
			if v, _ := attrOf(e, "inode"); v != "unreadable" || e.Type != filesys.TypeFile || e.Deleted {
				t.Errorf("%s: %+v", e.Name, e)
			}
		}
	})

	t.Run("ino naming a direct node", func(t *testing.T) {
		o := treeOpts()
		o.Segments = 3
		big := append(slices.Clone(files), f2fstest.File{Path: "/big", Data: bytes.Repeat([]byte{7}, 1000*bs)})
		_, img, tree := treeFS(t, o, big)
		inodes := map[uint32]bool{}
		for _, n := range tree.NID {
			inodes[n] = true
		}
		var direct uint32
		for nid := range tree.Addr {
			if !inodes[nid] {
				direct = nid
			}
		}
		if direct == 0 {
			t.Fatal("no node block found")
		}
		blk := make([]byte, bs)
		f2fstest.BlockArea(blk).Put(0, f2fstest.Dentry{Name: []byte("notinode"), Ino: direct, Type: f2fstest.FTReg})
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil || len(es) != 1 {
			t.Fatalf("entries %q, %v", entryNames(es), err)
		}
		if v, _ := attrOf(es[0], "inode"); v != "unreadable" {
			t.Errorf("attrs %+v", es[0].Attrs)
		}
	})

	t.Run("huge directory size is bounded", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		put32(img, int(tree.Addr[tree.NID["/d"]])*bs+16+4, 1<<20) // i_size ~ 2^52
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if !slices.Equal(entryNames(es), []string{"x"}) {
			t.Errorf("entries %q", entryNames(es))
		}
		if !hasWarning(f.Info(), "64 MiB") {
			t.Errorf("no truncation warning: %v", f.Info().Warnings)
		}
	})

	t.Run("data runs map one block 923 times", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		off := int(tree.Addr[tree.NID["/d"]]) * bs
		first := tree.DirBlocks["/d"][0]
		for i := range 923 {
			put32(img, off+360+4*i, first)
		}
		put32(img, off+16, 923*bs)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		// A dentry block is read once per scan: the other 922 references are
		// skipped with a warning, so the entries are not multiplied.
		n := 0
		for _, e := range es {
			if e.Name == "x" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("x listed %d times, want once", n)
		}
		if !hasWarning(f.Info(), fmt.Sprintf("dentry block %d shared by directories %d and %d; skipped", first, tree.NID["/d"], tree.NID["/d"])) {
			t.Errorf("no sharing warning: %v", f.Info().Warnings)
		}
	})

	t.Run("damaged block map keeps the trusted prefix", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		off := int(tree.Addr[tree.NID["/d"]]) * bs
		put32(img, off+360+4*1, 1) // block 1 maps below the main area
		put32(img, off+16, 3*bs)
		f := mustOpen(t, img)
		es, err := listD(t, f)
		if err != nil || !slices.Equal(entryNames(es), []string{"x"}) {
			t.Fatalf("entries %q, %v", entryNames(es), err)
		}
		if !hasWarning(f.Info(), "block map") {
			t.Errorf("no warning: %v", f.Info().Warnings)
		}
	})

	t.Run("inline dentry area full of garbage", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), []f2fstest.File{{Path: "/i", Dir: true, Inline: true}, {Path: "/i/a"}})
		off := int(tree.Addr[tree.NID["/i"]])*bs + 360 + 4
		for i := range 3400 {
			img[off+i] = byte(i*131 + 7)
		}
		for i := range 23 {
			img[off+i] = 0xff
		}
		f := mustOpen(t, img)
		es, err := f.ReadDir(entryByName(t, readDirPath(t, f, "/"), "i"))
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(es) > 182 {
			t.Errorf("%d entries", len(es))
		}
	})

	t.Run("inline dentry flag without room", func(t *testing.T) {
		_, img, tree := treeFS(t, f2fstest.Options{Segments: 2, ExtraAttr: true, InlineXattr: true},
			[]f2fstest.File{{Path: "/i", Dir: true, Inline: true}})
		off := int(tree.Addr[tree.NID["/i"]])*bs + 360
		// i_inline_xattr_size of 920 words leaves 3 address slots: no inline area.
		binary.LittleEndian.PutUint16(img[off+2:], 920)
		f := mustOpen(t, img)
		_, err := f.ReadDir(entryByName(t, readDirPath(t, f, "/"), "i"))
		if err != nil && !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir: %v", err)
		}
	})

	t.Run("directory containing itself", func(t *testing.T) {
		_, img, tree := treeFS(t, treeOpts(), files)
		blk := make([]byte, bs)
		f2fstest.BlockArea(blk).Put(0, f2fstest.Dentry{Name: []byte("loop"), Ino: tree.NID["/d"], Type: f2fstest.FTDir})
		setDentryBlock(t, img, tree, "/d", 0, blk)
		f := mustOpen(t, img)
		n, cycles := 0, 0
		err := filesys.Walk(f, f.Root(), "/", func(_ string, _ filesys.Entry, err error) error {
			if n++; n > 100 {
				t.Fatal("walk does not terminate")
			}
			if errors.Is(err, filesys.ErrCorrupt) {
				cycles++
			}
			return nil
		})
		if err != nil || cycles != 1 {
			t.Errorf("walk: %v, %d cycle reports", err, cycles)
		}
	})
}

func TestWalkWholeTree(t *testing.T) {
	cipher := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	files := []f2fstest.File{
		{Path: "/a", Dir: true},
		{Path: "/a/b", Dir: true, Inline: true},
		{Path: "/a/b/c.txt", Data: []byte("c")},
		{Path: "/a/b/gone.txt", Data: []byte("g"), Deleted: true},
		{Path: "/a/link", Symlink: "b/c.txt", Inline: true},
		{Path: "/z", Dir: true},
		{Path: "/z/zz", Dir: true},
		{Path: "/z/zz/leaf", Data: bytes.Repeat([]byte("L"), 3*bs)},
		{Path: "/enc", Dir: true, Encrypted: true},
		{Path: "/enc/x", RawName: cipher, Data: []byte("e"), Encrypted: true},
		{Path: "/top.txt", Data: []byte("top")},
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	type rec struct {
		typ     filesys.EntryType
		deleted bool
	}
	got := map[string]rec{}
	err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		got[p] = rec{e.Type, e.Deleted}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := "/enc/~enc~" + base64.RawURLEncoding.EncodeToString(cipher)
	want := map[string]rec{
		"/a": {filesys.TypeDir, false}, "/a/b": {filesys.TypeDir, false}, "/a/b/c.txt": {filesys.TypeFile, false},
		"/a/b/gone.txt": {filesys.TypeFile, true}, "/a/link": {filesys.TypeSymlink, false},
		"/z": {filesys.TypeDir, false}, "/z/zz": {filesys.TypeDir, false}, "/z/zz/leaf": {filesys.TypeFile, false},
		"/enc": {filesys.TypeDir, false}, enc: {filesys.TypeFile, false}, "/top.txt": {filesys.TypeFile, false},
	}
	if len(got) != len(want) {
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("walk visited %q", keys)
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s = %+v, want %+v", p, got[p], w)
		}
	}
	// Every live file opens and has the size of its content.
	for _, p := range []string{"/a/b/c.txt", "/z/zz/leaf", "/top.txt"} {
		e, err := f.Lookup(p)
		if err != nil || e.ID != nidID(tree.NID[p]) {
			t.Errorf("Lookup(%q) = %+v, %v", p, e, err)
			continue
		}
		fl, err := f.Open(e)
		if err != nil || fl.Size() != e.Size {
			t.Errorf("Open(%q) = %v, %v", p, fl, err)
		}
	}
}

func TestDirectoryReadBudget(t *testing.T) {
	files := []f2fstest.File{{Path: "/big", Dir: true}, {Path: "/small", Dir: true}, {Path: "/small/x"}, {Path: "/inl", Dir: true, Inline: true}, {Path: "/inl/y"}}
	for i := range 450 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/big/f%03d", i)})
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	if n := len(tree.DirBlocks["/big"]); n != 3 {
		t.Fatalf("/big has %d blocks", n)
	}
	big, _ := f.Lookup("/big")
	small, _ := f.Lookup("/small")

	// Forged IDs and Open spend nothing.
	f.SetDirBudget(0)
	for _, id := range []string{"nid:00003", "nid:+3", "nid: 3", "nid:0x3", "nid:99999999999", "inode:3", "NID:3", "nid:3 ", ""} {
		if _, err := f.ReadDir(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ReadDir(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if fl, err := f.Open(filesys.Entry{ID: nidID(tree.NID["/small/x"])}); err != nil || fl == nil {
		t.Errorf("Open spent the directory budget: %v", err)
	}
	// Exhausted before the first entry: a CorruptError, never an empty listing.
	if _, err := f.ReadDir(small); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("ReadDir with no budget = %v, want CorruptError", err)
	}
	// An inline directory is stored in its inode: nothing to charge.
	if es, err := f.ReadDir(filesys.Entry{ID: nidID(tree.NID["/inl"])}); err != nil || len(es) != 1 {
		t.Errorf("inline ReadDir with no budget = %v, %v", es, err)
	}

	// Budget for two blocks of three: a partial listing and a warning.
	f.SetDirBudget(2 * bs)
	es, err := f.ReadDir(big)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(es) != 2*f2fstest.NrDentryInBlock-2 { // the first block holds "." and ".."
		t.Errorf("partial listing has %d entries", len(es))
	}
	if !hasWarning(f.Info(), "budget") {
		t.Errorf("no budget warning: %v", f.Info().Warnings)
	}
	// The budget is shared by every listing (and Lookup): it is now spent.
	if _, err := f.ReadDir(small); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("second listing = %v, want CorruptError", err)
	}
	if _, err := f.Lookup("/small/x"); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Lookup through an unreadable directory = %v", err)
	}
}

func TestReadDirIgnoresForgedEntryFields(t *testing.T) {
	files := []f2fstest.File{{Path: "/d", Dir: true}, {Path: "/d/x", Data: []byte("x")}, {Path: "/f", Data: []byte("f")}}
	f, _, tree := treeFS(t, treeOpts(), files)
	d := nidID(tree.NID["/d"])
	for _, e := range []filesys.Entry{
		{ID: d},
		{ID: d, Type: filesys.TypeFile, Size: 1 << 40, Deleted: false},
		{ID: d, Type: filesys.TypeDir, Deleted: true, Attrs: []filesys.KV{{Key: "inline", Value: "dentry"}}},
		{ID: d, Encrypted: true, RawName: []byte("zz"), Name: "forged"},
	} {
		es, err := f.ReadDir(e)
		if err != nil || len(es) != 1 || es[0].Name != "x" || es[0].Encrypted {
			t.Errorf("ReadDir(%+v) = %q, %v", e, entryNames(es), err)
		}
	}
	// A regular file is not a directory, whatever the entry says.
	if _, err := f.ReadDir(filesys.Entry{ID: nidID(tree.NID["/f"]), Type: filesys.TypeDir}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("ReadDir(file) = %v, want ErrUnsupported", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: nidID(4000)}); err == nil {
		t.Error("ReadDir of a free nid succeeded")
	}
}

func TestLongNameEntry(t *testing.T) {
	name := strings.Repeat("n", 255)
	f, _, tree := treeFS(t, treeOpts(), []f2fstest.File{{Path: "/" + name, Data: []byte("long")}, {Path: "/after", Data: []byte("a")}})
	es, _ := f.ReadDir(f.Root())
	if !slices.Equal(entryNames(es), []string{name, "after"}) || es[0].ID != nidID(tree.NID["/"+name]) {
		t.Fatalf("entries %q", entryNames(es))
	}
	if _, err := f.Lookup("/" + name); err != nil {
		t.Errorf("Lookup: %v", err)
	}
}

func TestInfoWarningsAccumulateAcrossReads(t *testing.T) {
	files := []f2fstest.File{{Path: "/d", Dir: true}, {Path: "/d/x", Data: []byte("x")}, {Path: "/ok", Data: []byte("o")}}
	_, img, tree := treeFS(t, treeOpts(), files)
	blk, area := dotBlock()
	area.Put(212, f2fstest.Dentry{Name: []byte("bad"), Ino: tree.NID["/ok"], Type: f2fstest.FTReg, NameLen: 200})
	setDentryBlock(t, img, tree, "/d", 0, blk)
	f := mustOpen(t, img)
	if n := len(f.Info().Warnings); n != 0 {
		t.Fatalf("warnings at Open: %v", f.Info().Warnings)
	}
	before := f.Info()
	d, _ := f.Lookup("/d")
	if _, err := f.ReadDir(d); err != nil {
		t.Fatal(err)
	}
	w1 := f.Info().Warnings
	if len(w1) == 0 || len(before.Warnings) != 0 {
		t.Fatalf("ReadDir added no warning: %v (snapshot %v)", w1, before.Warnings)
	}
	snap := f.Info()
	snap.Warnings[0] = "tampered"
	if f.Info().Warnings[0] == "tampered" {
		t.Error("Info().Warnings aliases the live list")
	}
	_, _ = f.ReadDir(d)
	_, _ = f.ReadDir(d)
	if w2 := f.Info().Warnings; len(w2) != len(w1) {
		t.Errorf("duplicates recorded: %v -> %v", w1, w2)
	}
}

// The real mkfs/sload images: the live tree a Walk finds must be exactly the
// oracle's file list (computed from the source tree, never by Minutiae), with
// the same types, sizes, permission bits and mtimes, and no warnings.
func TestFixtureTreeMatchesOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		t.Run(path, func(t *testing.T) {
			img := gunzipFixture(t, path)
			want := loadOracle(t, path)
			f := mustOpen(t, img)
			type rec struct {
				typ   string
				size  int64
				mode  uint32
				mtime int64
			}
			wantSet := map[string]rec{}
			for _, o := range want.Files {
				wantSet[o.Path] = rec{o.Type, o.Size, o.Mode, o.Mtime}
			}
			got := map[string]rec{}
			err := filesys.Walk(f, f.Root(), "/", func(p string, e filesys.Entry, err error) error {
				if err != nil {
					t.Errorf("%s: %v", p, err)
					return nil
				}
				if e.Deleted {
					t.Errorf("%s: deleted entry in a clean image", p)
				}
				if _, dup := got[p]; dup {
					t.Errorf("%s listed twice", p)
				}
				r := rec{typ: e.Type.String(), size: e.Size, mode: e.Mode & 0o7777, mtime: e.Times.Modified.T.Unix()}
				if e.Type == filesys.TypeDir {
					r.size = 0
				}
				got[p] = r
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(wantSet) {
				t.Errorf("walk found %d paths, oracle has %d", len(got), len(wantSet))
			}
			bad := 0
			for p, w := range wantSet {
				g, ok := got[p]
				if !ok || g != w {
					if bad++; bad <= 10 {
						t.Errorf("%s = %+v (found %v), want %+v", p, g, ok, w)
					}
				}
			}
			for p := range got {
				if _, ok := wantSet[p]; !ok {
					if bad++; bad <= 10 {
						t.Errorf("%s: not in the oracle", p)
					}
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %v", w)
			}
		})
	}
}
