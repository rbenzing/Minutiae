package hfsplus_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

func buildFiles(t testing.TB, o hfsplustest.Options, files []hfsplustest.File) ([]byte, *hfsplustest.Layout) {
	t.Helper()
	return hfsplustest.BuildLayout(o, files)
}

// manyFiles returns n empty files f000, f001, ... in the root folder.
func manyFiles(n int) []hfsplustest.File {
	files := make([]hfsplustest.File, n)
	for i := range files {
		files[i] = hfsplustest.File{Path: fmt.Sprintf("/f%03d", i)}
	}
	return files
}

// small is a 512-byte-node, 512-byte-block geometry that gives a deep tree
// with a handful of files.
var small = hfsplustest.Options{BlockSize: 512, Blocks: 2048, NodeSize: 512, ExtentsNodeSize: 512}

// catOff is the image offset of byte rel of catalog node n.
func catOff(lay *hfsplustest.Layout, n uint32, rel int) int {
	return int(lay.CatalogOffset(int64(n)*int64(lay.NodeSize) + int64(rel)))
}

func putCat16(img []byte, lay *hfsplustest.Layout, n uint32, rel int, v uint16) {
	be.PutUint16(img[catOff(lay, n, rel):], v)
}

func putCat32(img []byte, lay *hfsplustest.Layout, n uint32, rel int, v uint32) {
	for i := range 4 {
		img[catOff(lay, n, rel+i)] = byte(v >> (24 - 8*i))
	}
}

// recOffset is the node-relative offset of record i, read from the node's
// offset table.
func recOffset(img []byte, lay *hfsplustest.Layout, n uint32, i int) int {
	return int(be.Uint16(img[catOff(lay, n, int(lay.NodeSize)-2*(i+1)):]))
}

// childPtrRel is the node-relative offset of the child pointer of index record i.
func childPtrRel(img []byte, lay *hfsplustest.Layout, n uint32, i int) int {
	off := recOffset(img, lay, n, i)
	kl := int(be.Uint16(img[catOff(lay, n, off):]))
	return off + (2+kl+1)&^1
}

func catalogTree(t testing.TB, img []byte) (*hfsplus.FS, *hfsplus.BTree) {
	t.Helper()
	f := open(t, img)
	tr, err := f.Tree(hfsplus.TreeCatalog)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	return f, tr
}

func countRecords(tr *hfsplus.BTree) (int, error) {
	n := 0
	err := tr.ScanAll(func([]byte) (bool, error) { n++; return true, nil })
	return n, err
}

func TestBTreeHeaderFields(t *testing.T) {
	t.Run("catalog, extents", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{}, manyFiles(3))
		f, tr := catalogTree(t, img)
		h := tr.Header()
		want := hfsplus.BTreeHeader{NodeSize: 4096, Depth: 1, Root: 1, FirstLeaf: 1, LastLeaf: 1, TotalNodes: lay.CatalogNodes, LeafRecords: 8, MaxKeyLength: 516, KeyCompare: 0xCF}
		if h != want {
			t.Errorf("catalog header = %+v, want %+v", h, want)
		}
		et, err := f.Tree(hfsplus.TreeExtents)
		if err != nil {
			t.Fatal(err)
		}
		eh := et.Header()
		if eh.NodeSize != 1024 || eh.Depth != 0 || eh.Root != 0 || eh.MaxKeyLength != 10 || eh.TotalNodes != lay.ExtentsNodes || eh.LeafRecords != 0 {
			t.Errorf("extents header = %+v", eh)
		}
		if at, err := f.Tree(hfsplus.TreeAttributes); at != nil || err != nil {
			t.Errorf("attributes tree = %v, %v; want none", at, err)
		}
		if got := len(f.Info().Warnings); got != 0 {
			t.Errorf("Warnings = %q", f.Info().Warnings)
		}
	})
	t.Run("hfsx binary", func(t *testing.T) {
		img, _ := buildFiles(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, nil)
		_, tr := catalogTree(t, img)
		if kc := tr.Header().KeyCompare; kc != 0xBC {
			t.Errorf("keyCompareType = %#x", kc)
		}
	})
	t.Run("bad close warns", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{}, nil)
		img[catOff(lay, 0, 14+41)] |= 1 // kBTBadCloseMask in the attributes word
		f := open(t, img)
		if _, err := f.Tree(hfsplus.TreeCatalog); err != nil {
			t.Fatal(err)
		}
		if !hasWarning(f.Info(), "not closed cleanly") {
			t.Errorf("Warnings = %q", f.Info().Warnings)
		}
	})
}

func TestBTreeMultiLevelLookup(t *testing.T) {
	files := manyFiles(60)
	img, lay := buildFiles(t, small, files)
	if lay.CatalogDepth != 3 {
		t.Fatalf("catalog depth = %d, want 3 (the test needs a three-level tree)", lay.CatalogDepth)
	}
	f, tr := catalogTree(t, img)
	if h := tr.Header(); h.Depth != 3 || h.Root != lay.CatalogRoot || h.NodeSize != 512 {
		t.Fatalf("header = %+v", h)
	}
	for i, fl := range files {
		name := strings.TrimPrefix(fl.Path, "/")
		r, stored, err := f.FindRecord(2, name)
		if err != nil || r.Type != 2 || r.ID != uint32(16+i) || stored != name || lay.CNIDs[fl.Path] != r.ID {
			t.Fatalf("FindRecord(2, %q) = %+v, %q, %v", name, r, stored, err)
		}
	}
	if _, _, err := f.FindRecord(2, "f060"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("miss: %v, want ErrNotFound", err)
	}
	if _, _, err := f.FindRecord(2, "a"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("miss before the first key: %v", err)
	}
	if _, _, err := f.FindRecord(2, "zzz"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("miss after the last key: %v", err)
	}
	// Threads: ids 16.. are file ids; each has a file thread back to the root.
	for i := range files {
		th, err := f.CatalogThread(uint32(16 + i))
		if err != nil || th.Type != 4 || th.Parent != 2 || string(utf16.Decode(th.Name)) != fmt.Sprintf("f%03d", i) {
			t.Fatalf("thread %d = %+v, %v", 16+i, th, err)
		}
	}
	if _, err := f.CatalogThread(9999); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("thread of a missing id: %v", err)
	}
	// A full scan sees every record once, in key order.
	n, err := countRecords(tr)
	if err != nil || n != 2+2*len(files) {
		t.Errorf("scan = %d, %v; want %d records", n, err, 2+2*len(files))
	}
	lastParent, lastName := uint32(0), ""
	if err := f.CatalogScan(func(p uint32, name string, _ hfsplus.CatRec) bool {
		if p < lastParent || p == lastParent && name < lastName {
			t.Errorf("keys out of order: (%d,%q) after (%d,%q)", p, name, lastParent, lastName)
		}
		lastParent, lastName = p, name
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}

func TestBTreeCaseModes(t *testing.T) {
	files := []hfsplustest.File{{Path: "/Readme"}, {Path: "/a"}, {Path: "/A"}}
	t.Run("hfs+ folds", func(t *testing.T) {
		img, _ := buildFiles(t, hfsplustest.Options{}, files[:1])
		f := open(t, img)
		r, stored, err := f.FindRecord(2, "README")
		if err != nil || r.Type != 2 || stored != "Readme" {
			t.Fatalf("FindRecord = %+v, %q, %v", r, stored, err)
		}
	})
	t.Run("hfs+ with a binary keyCompareType still folds", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{}, files[:1])
		img[catOff(lay, 0, 14+37)] = 0xBC
		f := open(t, img)
		if _, stored, err := f.FindRecord(2, "readme"); err != nil || stored != "Readme" {
			t.Fatalf("FindRecord = %q, %v", stored, err)
		}
	})
	t.Run("hfsx binary", func(t *testing.T) {
		img, lay := buildFiles(t, hfsplustest.Options{HFSX: true, CaseSensitive: true}, files)
		f := open(t, img)
		ra, _, erra := f.FindRecord(2, "a")
		rA, _, errA := f.FindRecord(2, "A")
		if erra != nil || errA != nil || ra.ID == rA.ID || ra.ID != lay.CNIDs["/a"] || rA.ID != lay.CNIDs["/A"] {
			t.Errorf("a = %+v %v, A = %+v %v", ra, erra, rA, errA)
		}
		if _, _, err := f.FindRecord(2, "README"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("binary lookup of README: %v", err)
		}
	})
	t.Run("hfsx folding", func(t *testing.T) {
		img, _ := buildFiles(t, hfsplustest.Options{HFSX: true}, files[:1])
		f := open(t, img)
		if _, stored, err := f.FindRecord(2, "README"); err != nil || stored != "Readme" {
			t.Fatalf("FindRecord = %q, %v", stored, err)
		}
	})
}

func TestBTreeNodeStraddlesExtents(t *testing.T) {
	// 1024-byte blocks, 4096-byte nodes, one block per extent: every node
	// spans four extents, separated by free blocks, and the catalog needs
	// extents-overflow records.
	o := hfsplustest.Options{BlockSize: 1024, Blocks: 1024, NodeSize: 4096, CatalogFragment: 1}
	files := manyFiles(40)
	img, lay := buildFiles(t, o, files)
	if len(lay.CatalogExtents) <= 8 || lay.CatalogNodes < 4 {
		t.Fatalf("extents = %d, nodes = %d: the catalog is not fragmented enough", len(lay.CatalogExtents), lay.CatalogNodes)
	}
	for i, e := range lay.CatalogExtents[1:] {
		if prev := lay.CatalogExtents[i]; e.Start != prev.Start+prev.Count+1 {
			t.Fatalf("extent %d starts at %d, want a one-block gap after %d+%d", i+1, e.Start, prev.Start, prev.Count)
		}
	}
	f, tr := catalogTree(t, img)
	if n, err := countRecords(tr); err != nil || n != 2+2*len(files) {
		t.Fatalf("scan = %d, %v", n, err)
	}
	for _, fl := range files {
		if _, _, err := f.FindRecord(2, strings.TrimPrefix(fl.Path, "/")); err != nil {
			t.Fatalf("FindRecord(%s): %v", fl.Path, err)
		}
	}
	if w := f.Info().Warnings; len(w) != 0 {
		t.Errorf("Warnings = %q", w)
	}
}

func TestRecordOffsetsOutsideNodeAreCorrupt(t *testing.T) {
	// Patch the offset table or descriptor of the only leaf (node 1) of a
	// 3-file catalog; the offsets are read back from the table.
	files := manyFiles(3)
	cases := []struct {
		name  string
		patch func(img []byte, lay *hfsplustest.Layout)
	}{
		{"offset beyond the node", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*2, 0xFFFE)
		}},
		{"offset just past the node size", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*2, uint16(lay.NodeSize))
		}},
		{"odd offset", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*2, uint16(recOffset(img, lay, 1, 1)+1))
		}},
		{"offset inside the descriptor", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*2, 8)
		}},
		{"offsets not increasing", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*3, uint16(recOffset(img, lay, 1, 0)))
		}},
		{"offsets decreasing", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, int(lay.NodeSize)-2*3, uint16(recOffset(img, lay, 1, 3)+2))
		}},
		{"free offset overlaps the offset table", func(img []byte, lay *hfsplustest.Layout) {
			nrec := int(be.Uint16(img[catOff(lay, 1, 10):]))
			putCat16(img, lay, 1, int(lay.NodeSize)-2*(nrec+1), uint16(int(lay.NodeSize)-2*nrec))
		}},
		{"free offset before the last record", func(img []byte, lay *hfsplustest.Layout) {
			nrec := int(be.Uint16(img[catOff(lay, 1, 10):]))
			putCat16(img, lay, 1, int(lay.NodeSize)-2*(nrec+1), 16)
		}},
		{"numRecords too large for the node", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, 10, 5000)
		}},
		{"numRecords one past what fits", func(img []byte, lay *hfsplustest.Layout) {
			putCat16(img, lay, 1, 10, uint16((int(lay.NodeSize)-14)/4+1))
		}},
		{"unknown node kind", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, 1, 8)] = 7 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := buildFiles(t, hfsplustest.Options{}, files)
			c.patch(img, lay)
			f := open(t, img) // Open itself survives: the damage shows when the node is read
			// The tree has a single node, the root, so opening it reads the damage.
			_, err := f.Tree(hfsplus.TreeCatalog)
			wantCorrupt(t, err)
			_, _, err = f.FindRecord(2, "f001")
			wantCorrupt(t, err)
		})
	}
}

func TestBTreeLeafChainCycleIsCorrupt(t *testing.T) {
	img0, lay0 := buildFiles(t, small, manyFiles(8))
	_ = img0
	leaves := lay0.CatalogLevels[0]
	if len(leaves) < 4 {
		t.Fatalf("%d leaves, need at least 4", len(leaves))
	}
	cases := []struct {
		name         string
		node, target uint32
	}{
		{"link to an earlier leaf", leaves[2], leaves[0]},
		{"link to itself", leaves[1], leaves[1]},
		{"link to the previous leaf", leaves[3], leaves[2]},
		{"last leaf links back to the first", leaves[len(leaves)-1], leaves[0]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := buildFiles(t, small, manyFiles(8))
			putCat32(img, lay, c.node, 0, c.target)
			_, tr := catalogTree(t, img)
			_, err := countRecords(tr)
			wantCorrupt(t, err)
			if !strings.Contains(err.Error(), "loops") {
				t.Errorf("error = %v, want a loop report", err)
			}
		})
	}
	t.Run("link outside the tree", func(t *testing.T) {
		img, lay := buildFiles(t, small, manyFiles(8))
		putCat32(img, lay, leaves[1], 0, lay.CatalogNodes+5)
		_, tr := catalogTree(t, img)
		_, err := countRecords(tr)
		wantCorrupt(t, err)
	})
	t.Run("link to a non-leaf", func(t *testing.T) {
		img, lay := buildFiles(t, small, manyFiles(8))
		putCat32(img, lay, leaves[1], 0, lay.CatalogRoot)
		_, tr := catalogTree(t, img)
		_, err := countRecords(tr)
		wantCorrupt(t, err)
	})
}

func TestBTreeIndexLoopIsCorrupt(t *testing.T) {
	files := manyFiles(60)
	_, lay0 := buildFiles(t, small, files)
	if lay0.CatalogDepth != 3 {
		t.Fatalf("depth %d, need 3", lay0.CatalogDepth)
	}
	root := lay0.CatalogRoot
	mid := lay0.CatalogLevels[1][0]
	lookup := func(f *hfsplus.FS) error {
		// Every name must be tried: only some descend through the damaged record.
		var first error
		for _, fl := range files {
			if _, _, err := f.FindRecord(2, strings.TrimPrefix(fl.Path, "/")); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	cases := []struct {
		name  string
		patch func(img []byte, lay *hfsplustest.Layout)
	}{
		{"root record points at the root", func(img []byte, lay *hfsplustest.Layout) {
			putCat32(img, lay, root, childPtrRel(img, lay, root, 0), root)
		}},
		{"middle record points at itself", func(img []byte, lay *hfsplustest.Layout) {
			putCat32(img, lay, mid, childPtrRel(img, lay, mid, 0), mid)
		}},
		{"middle record points at its ancestor", func(img []byte, lay *hfsplustest.Layout) {
			putCat32(img, lay, mid, childPtrRel(img, lay, mid, 0), root)
		}},
		{"index record points at the header node", func(img []byte, lay *hfsplustest.Layout) {
			putCat32(img, lay, mid, childPtrRel(img, lay, mid, 0), 0)
		}},
		{"index record points outside the tree", func(img []byte, lay *hfsplustest.Layout) {
			putCat32(img, lay, mid, childPtrRel(img, lay, mid, 0), lay.CatalogNodes)
		}},
		{"depth smaller than the tree", func(img []byte, lay *hfsplustest.Layout) { putCat16(img, lay, 0, 14, 2) }},
		{"depth larger than the tree", func(img []byte, lay *hfsplustest.Layout) { putCat16(img, lay, 0, 14, 4) }},
		{"leaf height wrong", func(img []byte, lay *hfsplustest.Layout) {
			for _, l := range lay.CatalogLevels[0] {
				img[catOff(lay, l, 9)] = 2
			}
		}},
		{"index height wrong", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, mid, 9)] = 5 }},
		{"index node of kind leaf", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, mid, 8)] = 0xFF }},
		{"empty index node", func(img []byte, lay *hfsplustest.Layout) { putCat16(img, lay, mid, 10, 0) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := buildFiles(t, small, files)
			c.patch(img, lay)
			f := open(t, img)
			wantCorrupt(t, lookup(f))
		})
	}
}

func TestBTreeHeaderHostile(t *testing.T) {
	_, lay0 := buildFiles(t, small, manyFiles(8))
	leaf := lay0.CatalogLevels[0][0]
	set16 := func(off int, v uint16) func([]byte, *hfsplustest.Layout) {
		return func(img []byte, lay *hfsplustest.Layout) { putCat16(img, lay, 0, 14+off, v) }
	}
	set32 := func(off int, v uint32) func([]byte, *hfsplustest.Layout) {
		return func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+off, v) }
	}
	cases := []struct {
		name  string
		patch func(img []byte, lay *hfsplustest.Layout)
	}{
		{"node size 0", set16(18, 0)},
		{"node size 3", set16(18, 3)},
		{"node size 256", set16(18, 256)},
		{"node size not a power of two", set16(18, 1000)},
		{"node size 65535", set16(18, 0xFFFF)},
		{"node size 65536 truncated to 0", set16(18, 0x0000)},
		{"node size above the maximum", set16(18, 0x8001)},
		{"total nodes huge", set32(22, 0xFFFFFFFF)},
		{"total nodes just too many", func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+22, lay.CatalogNodes+1) }},
		{"total nodes zero", set32(22, 0)},
		{"root beyond the tree", func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+2, lay.CatalogNodes) }},
		{"first leaf beyond the tree", set32(10, 0x7FFFFFFF)},
		{"last leaf beyond the tree", func(img []byte, lay *hfsplustest.Layout) { putCat32(img, lay, 0, 14+14, lay.CatalogNodes+9) }},
		{"depth 17", set16(0, 17)},
		{"depth 0 with a root", set16(0, 0)},
		{"root 0 with a depth", set32(2, 0)},
		{"first leaf is the header", set32(10, 0)},
		{"node 0 is not a header node", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, 0, 8)] = 0xFF }},
		{"node 0 is a map node", func(img []byte, lay *hfsplustest.Layout) { img[catOff(lay, 0, 8)] = 2 }},
		{"node 0 with 2 records", func(img []byte, lay *hfsplustest.Layout) { putCat16(img, lay, 0, 10, 2) }},
		{"maxKeyLength absurd", set16(20, 0xFFFF)},
		{"maxKeyLength 0", set16(20, 0)},
		{"maxKeyLength above 516", set16(20, 517)},
		{"map node as root", func(img []byte, lay *hfsplustest.Layout) {
			img[catOff(lay, leaf, 8)] = 2
			putCat32(img, lay, 0, 14+2, leaf)
			putCat16(img, lay, 0, 14, 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, lay := buildFiles(t, small, manyFiles(8))
			c.patch(img, lay)
			cr := &countingReader{r: bytes.NewReader(img)}
			f, err := hfsplus.Open(cr, int64(len(img)))
			if strings.HasPrefix(c.name, "node 0 ") {
				// A first node that is not a header node (kind, height, 3 records, no
				// backward link) is caught by Open itself: the header points at something
				// that is not a catalog, so detection can fall through to another driver.
				wantCorrupt(t, err)
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			before := cr.n
			_, err = f.Tree(hfsplus.TreeCatalog)
			wantCorrupt(t, err)
			if read := cr.n - before; read > 64<<10 {
				t.Errorf("opening the tree read %d bytes before failing", read)
			}
		})
	}
	t.Run("extents tree header", func(t *testing.T) {
		img, lay := buildFiles(t, small, nil)
		be.PutUint16(img[lay.ExtentsOffset(14+20):], 11) // maxKeyLength must be exactly 10
		f := open(t, img)
		_, err := f.Tree(hfsplus.TreeExtents)
		wantCorrupt(t, err)
	})
}

func TestStaleNodeSlackIsNotListed(t *testing.T) {
	for _, o := range []hfsplustest.Options{
		{StaleSlack: true},
		{StaleSlack: true, HFSX: true, CaseSensitive: true},
		{StaleSlack: true, BlockSize: 1024, Blocks: 512, NodeSize: 1024},
	} {
		files := manyFiles(5)
		img, lay := buildFiles(t, o, files)
		stale := make([]byte, 0, 18)
		for _, u := range utf16.Encode([]rune("stale.txt")) {
			stale = append(stale, byte(u>>8), byte(u))
		}
		if bytes.Count(img, stale) == 0 {
			t.Fatalf("%+v: the builder left no stale record in the image", o)
		}
		f, tr := catalogTree(t, img)
		n := 0
		err := tr.ScanAll(func(rec []byte) (bool, error) {
			n++
			if bytes.Contains(rec, stale) {
				t.Errorf("a scan returned the stale record")
			}
			return true, nil
		})
		if err != nil || n != 2+2*len(files) {
			t.Errorf("%+v: scan = %d records, %v; want %d", o, n, err, 2+2*len(files))
		}
		if _, _, err := f.FindRecord(2, "stale.txt"); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%+v: FindRecord(stale.txt) = %v, want ErrNotFound", o, err)
		}
		if err := f.CatalogScan(func(p uint32, name string, r hfsplus.CatRec) bool {
			if name == "stale.txt" || r.ID == 4242 {
				t.Errorf("CatalogScan returned the stale record (%d, %q)", p, name)
			}
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if len(f.Info().Warnings) != 0 {
			t.Errorf("Warnings = %q", f.Info().Warnings)
		}
		_ = lay
	}
}

func TestTreeOpenFailureIsRepeatable(t *testing.T) {
	// A header that passes Open (the first node is a header node) but whose
	// tree header is bad: the failure is reported again, not cached as success.
	img, lay := buildFiles(t, hfsplustest.Options{}, nil)
	putCat16(img, lay, 0, 14+18, 1000) // node size not a power of two
	f := open(t, img)
	for range 2 {
		if _, err := f.Tree(hfsplus.TreeCatalog); !errors.Is(err, filesys.ErrCorrupt) {
			t.Fatalf("Tree = %v", err)
		}
	}
}

// Open fails with a corrupt-structure error when the first node of the catalog
// file is not a B-tree header node, so detect.OpenWith falls through to the next
// matching driver; a catalog the image does not hold at all (a truncated image)
// still opens, with a warning, so the allocation bitmap stays reachable.
func TestOpenChecksTheCatalogHeaderNode(t *testing.T) {
	img, lay := buildFiles(t, hfsplustest.Options{}, nil)
	bad := bytes.Clone(img)
	bad[catOff(lay, 0, 8)] = 0xFF
	if _, err := hfsplus.Open(bytes.NewReader(bad), int64(len(bad))); err == nil || !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Open = %v, want a corrupt-structure error", err)
	}
	zero := bytes.Clone(img)
	clear(zero[catOff(lay, 0, 0):catOff(lay, 0, 14)])
	if _, err := hfsplus.Open(bytes.NewReader(zero), int64(len(zero))); !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Open with a zeroed descriptor = %v", err)
	}
	cut := img[:catOff(lay, 0, 0)+4]
	f, err := hfsplus.Open(bytes.NewReader(cut), int64(len(cut)))
	if err != nil {
		t.Fatalf("Open of an image cut inside the catalog: %v", err)
	}
	if !hasWarning(f.Info(), "catalog B-tree header") {
		t.Errorf("warnings = %q", f.Info().Warnings)
	}
}
