package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// patchDirMap makes directory dir's i_addr slots 0..n-1 all name blk and its
// i_size n blocks: the hostile construction behind a block-sharing directory.
func patchDirMap(img []byte, tree *f2fstest.Tree, dir string, blk uint32, n int) {
	off := int(tree.Addr[tree.NID[dir]]) * bs
	for i := range n {
		put32(img, off+360+4*i, blk)
	}
	put32(img, off+16, uint32(n*bs))
}

func readNIDs(t *testing.T, f *f2fs.FS, n uint32) []string {
	t.Helper()
	es, err := f.ReadDir(filesys.Entry{ID: nidID(n)})
	if err != nil {
		t.Fatalf("ReadDir(nid %d): %v", n, err)
	}
	return entryNames(es)
}

// A dentry block belongs to one directory. Directories that map another
// directory's block (or one block 923 times) neither multiply its entries nor
// spend read budget: the first reader claims the block, the others skip it with
// a warning.
func TestDirectoriesSharingDentryBlocks(t *testing.T) {
	files := []f2fstest.File{
		{Path: "/d", Dir: true},
		{Path: "/d/x", Data: []byte("x")},
		{Path: "/e", Dir: true},
		{Path: "/e/y", Data: []byte("y")},
		{Path: "/f", Dir: true},
		{Path: "/f/z", Data: []byte("z")},
	}
	build := func() (*f2fs.FS, *f2fstest.Tree) {
		_, img, tree := treeFS(t, treeOpts(), files)
		shared := tree.DirBlocks["/d"][0]
		patchDirMap(img, tree, "/e", shared, 1)   // /e maps /d's block once
		patchDirMap(img, tree, "/f", shared, 923) // /f maps it in every slot
		return mustOpen(t, img), tree
	}

	t.Run("owner first", func(t *testing.T) {
		f, tree := build()
		d, e, g := tree.NID["/d"], tree.NID["/e"], tree.NID["/f"]
		if got := readNIDs(t, f, d); !slices.Equal(got, []string{"x"}) {
			t.Errorf("/d = %q", got)
		}
		if got := readNIDs(t, f, d); !slices.Equal(got, []string{"x"}) { // the owner may read it again
			t.Errorf("/d again = %q", got)
		}
		if got := readNIDs(t, f, e); len(got) != 0 {
			t.Errorf("/e lists %q from /d's block", got)
		}
		if got := readNIDs(t, f, g); len(got) != 0 {
			t.Errorf("/f lists %q from /d's block", got)
		}
		shared := tree.DirBlocks["/d"][0]
		for _, w := range []string{
			fmt.Sprintf("dentry block %d shared by directories %d and %d; skipped", shared, d, e),
			fmt.Sprintf("dentry block %d shared by directories %d and %d; skipped", shared, d, g),
		} {
			if !hasWarning(f.Info(), w) {
				t.Errorf("no warning %q in %q", w, f.Info().Warnings)
			}
		}
		if _, err := f.Lookup("/d/x"); err != nil {
			t.Errorf("Lookup in the owner: %v", err)
		}
		wantNoEntry(t, f, "/e/x")
		wantNoEntry(t, f, "/f/x")
	})

	t.Run("sharer first", func(t *testing.T) {
		f, tree := build()
		d, e := tree.NID["/d"], tree.NID["/e"]
		if got := readNIDs(t, f, e); !slices.Equal(got, []string{"x"}) {
			t.Fatalf("/e = %q; the first reader of the block keeps it", got)
		}
		if got := readNIDs(t, f, d); len(got) != 0 {
			t.Errorf("/d = %q", got)
		}
		if !hasWarning(f.Info(), fmt.Sprintf("shared by directories %d and %d; skipped", e, d)) {
			t.Errorf("warnings %q", f.Info().Warnings)
		}
	})
}

// Many directories that all map one dentry block 923 times: a whole-tree walk
// yields that block's entries once and fits a three-block read budget (the
// root, the owner, and nothing for the 40 sharers).
func TestWalkWithSharedDentryBlocksIsBounded(t *testing.T) {
	files := []f2fstest.File{{Path: "/a", Dir: true}, {Path: "/a/x", Data: []byte("x")}}
	for i := range 40 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/n%02d", i), Dir: true}, f2fstest.File{Path: fmt.Sprintf("/n%02d/leaf", i), Data: []byte("l")})
	}
	_, img, tree := treeFS(t, treeOpts(), files)
	shared := tree.DirBlocks["/a"][0]
	for i := range 40 {
		patchDirMap(img, tree, fmt.Sprintf("/n%02d", i), shared, 923)
	}
	f := mustOpen(t, img)
	f.SetDirBudget(3 * bs)
	visited, xs := 0, 0
	err := filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("walk error: %v", err)
			return nil
		}
		if visited++; visited > 1000 {
			t.Fatal("walk does not terminate")
		}
		if e.Name == "x" {
			xs++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 + 40 + 1; xs != 1 || visited != want { // /a, the 40 n dirs and x
		t.Errorf("walk visited %d entries, x %d times; want %d and 1", visited, xs, want)
	}
	if hasWarning(f.Info(), "read budget") {
		t.Errorf("the sharers spent read budget: %q", f.Info().Warnings)
	}
}

// A canonical ID that cannot name an inode of the volume (beyond the NAT, the
// node/meta inode, a free nid, a node block that is not an inode) is a forged
// or stale ID: Open and ReadDir report ErrNotFound, never a CorruptError.
func TestForgedCanonicalIDsAreNotFound(t *testing.T) {
	o := treeOpts()
	o.Segments = 3
	files := []f2fstest.File{{Path: "/f", Data: bytes.Repeat([]byte{7}, 1000*bs)}, {Path: "/d", Dir: true}}
	f, _, tree := treeFS(t, o, files)
	g := f.Geometry()
	inodes := map[uint32]bool{}
	for _, n := range tree.NID {
		inodes[n] = true
	}
	var dataNode uint32
	for nid := range tree.Addr {
		if !inodes[nid] {
			dataNode = nid
		}
	}
	if dataNode == 0 {
		t.Fatal("no direct node in the image")
	}
	ids := []string{
		nidID(g.NodeIno), nidID(g.MetaIno), nidID(f2fstest.Geometry(o).NATCapacity()), "nid:4294967295", "nid:1000000000",
		nidID(dataNode), nidID(tree.NID["/d"] + 1000),
	}
	for _, id := range ids {
		if _, err := f.Open(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("Open(%q) = %v; want ErrNotFound and not ErrCorrupt", id, err)
		}
		if _, err := f.ReadDir(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("ReadDir(%q) = %v; want ErrNotFound and not ErrCorrupt", id, err)
		}
	}
	// Genuine IDs still work.
	if _, err := f.Open(filesys.Entry{ID: nidID(tree.NID["/f"])}); err != nil {
		t.Errorf("Open of a real file: %v", err)
	}
	if _, err := f.ReadDir(filesys.Entry{ID: nidID(tree.NID["/d"])}); err != nil {
		t.Errorf("ReadDir of a real directory: %v", err)
	}
}

// When the per-FS directory read budget runs out part-way through a directory,
// Lookup must not claim "no such entry" for names it never got to see: it
// returns a CorruptError. A name found before the cut-off is still returned.
func TestLookupBudgetExhaustedIsNotNotFound(t *testing.T) {
	files := []f2fstest.File{{Path: "/big", Dir: true}}
	for i := range 450 {
		files = append(files, f2fstest.File{Path: fmt.Sprintf("/big/f%03d", i)})
	}
	f, _, tree := treeFS(t, treeOpts(), files)
	if n := len(tree.DirBlocks["/big"]); n != 3 {
		t.Fatalf("/big has %d blocks", n)
	}
	f.SetDirBudget(2 * bs) // the root's block and the first block of /big
	if _, err := f.Lookup("/big/f000"); err != nil {
		t.Fatalf("name in the first block: %v", err)
	}
	for _, p := range []string{"/big/f449", "/big/no-such-name"} {
		f.SetDirBudget(2 * bs)
		_, err := f.Lookup(p)
		var ce *filesys.CorruptError
		if errors.Is(err, filesys.ErrNotFound) || !errors.As(err, &ce) || !strings.Contains(ce.Reason, "directory read budget exhausted") {
			t.Errorf("Lookup(%q) = %v; want a CorruptError about the read budget, not ErrNotFound", p, err)
		}
	}
}
