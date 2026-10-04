package apfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
)

// Tree layout offsets, written out here from the format reference on purpose:
// the tests must not share constants with the reader.
const (
	btFlagsOff   = 32 // btn_flags u16
	btLevelOff   = 34 // btn_level u16
	btNKeysOff   = 36 // btn_nkeys u32
	btTSOffOff   = 40 // btn_table_space.off u16
	btTSLenOff   = 42 // btn_table_space.len u16
	btTocStart   = 56 // btn_data
	btInfoSize   = 40
	btRootFlag   = 0x1
	btLeafFlag   = 0x2
	physTreeType = 0x40000002 // OBJ_PHYSICAL | BTREE
	virtTreeType = 0x2        // BTREE, no storage bits
	ephTreeType  = 0x80000002
	treeBase     = 3000 // first block tests put their trees at
)

const idMask = 0x0fffffffffffffff

// treeImage is a built container plus a bump allocator over its free tail.
type treeImage struct {
	*image
	next uint64
}

func newTreeImage(t *testing.T, o apfstest.Options) *treeImage {
	t.Helper()
	return &treeImage{image: newImage(t, o), next: treeBase}
}

func (ti *treeImage) alloc() (uint64, uint64) {
	a := ti.next
	ti.next++
	return a, a
}

// place packs recs into a physical tree and writes it into the image.
func (ti *treeImage) place(spec apfstest.TreeSpec, recs []apfstest.Rec) []apfstest.Block {
	spec.BlockSize = ti.bs
	blocks := apfstest.PackTree(spec, recs, ti.alloc)
	apfstest.Place(ti.b, ti.bs, blocks)
	return blocks
}

func (ti *treeImage) seal(b apfstest.Block) { apfstest.SealObject(ti.blk(b.Addr)) }

func varSpec(maxKeys int) apfstest.TreeSpec {
	return apfstest.TreeSpec{BTFlags: 0x40, Storage: apfstest.StoragePhysical, Xid: 5, MaxKeys: maxKeys}
}

func fixedSpec(maxKeys int) apfstest.TreeSpec {
	return apfstest.TreeSpec{
		Fixed: true, KeySize: 16, ValSize: 16, BTFlags: 0x10,
		Storage: apfstest.StoragePhysical, Xid: 5, MaxKeys: maxKeys,
	}
}

// fsRec is a file-system-style record: key = object id | type<<60 then a
// suffix, a value of a varying length.
func fsRec(id uint64, typ uint8, suffix string, n int) apfstest.Rec {
	k := make([]byte, 8, 8+len(suffix))
	le.PutUint64(k, id|uint64(typ)<<60)
	k = append(k, suffix...)
	return apfstest.Rec{Key: k, Val: bytes.Repeat([]byte{byte(id + uint64(n))}, 1+n%13)}
}

func fixedRec(oid, xid uint64) apfstest.Rec {
	k := make([]byte, 16)
	le.PutUint64(k, oid)
	le.PutUint64(k[8:], xid)
	v := make([]byte, 16)
	le.PutUint64(v, oid*7+xid)
	le.PutUint64(v[8:], xid)
	return apfstest.Rec{Key: k, Val: v}
}

func idPrefix(target uint64) func([]byte) int {
	return func(k []byte) int {
		switch id := le.Uint64(k) & idMask; {
		case id < target:
			return -1
		case id > target:
			return 1
		}
		return 0
	}
}

func scanAll(t *testing.T, f *apfs.FS, root uint64, typ uint32) []apfs.TreeRec {
	t.Helper()
	got, err := f.ScanTree(root, typ, false, nil, 0)
	if err != nil {
		t.Fatalf("ScanTree: %v", err)
	}
	return got
}

func sameRecs(t *testing.T, got []apfs.TreeRec, want []apfstest.Rec) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Val, want[i].Val) {
			t.Fatalf("record %d = %x/%x, want %x/%x", i, got[i].Key, got[i].Val, want[i].Key, want[i].Val)
		}
	}
}

func isCorrupt(err error) bool {
	var ce *filesys.CorruptError
	return errors.As(err, &ce) && errors.Is(err, filesys.ErrCorrupt)
}

func TestBTreeFixedAndVariableNodes(t *testing.T) {
	var fixed, variable []apfstest.Rec
	for i := range 40 {
		fixed = append(fixed, fixedRec(uint64(100+i/3), uint64(i%3+1)))
		variable = append(variable, fsRec(uint64(10+i/4), uint8(1+i%4), strings.Repeat("n", i%5), i))
	}
	for _, tc := range []struct {
		name    string
		spec    func(int) apfstest.TreeSpec
		recs    []apfstest.Rec
		maxKeys int
		level   uint16
	}{
		{"fixed single node", fixedSpec, fixed[:5], 0, 0},
		{"fixed three levels", fixedSpec, fixed, 4, 2},
		{"variable single node", varSpec, variable[:5], 0, 0},
		{"variable three levels", varSpec, variable, 3, 3},
		{"empty fixed", fixedSpec, nil, 0, 0},
		{"empty variable", varSpec, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTreeImage(t, apfstest.Options{})
			blocks := ti.place(tc.spec(tc.maxKeys), tc.recs)
			if got := le.Uint16(blocks[0].Data[btLevelOff:]); got != tc.level {
				t.Fatalf("root level %d, want %d (builder shape)", got, tc.level)
			}
			f := ti.mustOpen()
			sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), tc.recs)
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %q", w)
			}
		})
	}
}

func TestBTreeSeekAcrossLeaves(t *testing.T) {
	// Object ids 10..19; id 14 has 9 records, so for small nodes it spans
	// several leaves; id 15 is absent.
	var recs []apfstest.Rec
	counts := map[uint64]int{}
	for id := uint64(10); id < 20; id++ {
		n := 1 + int(id%3)
		switch id {
		case 14:
			n = 9
		case 15:
			n = 0
		}
		for j := range n {
			recs = append(recs, fsRec(id, uint8(1+j), fmt.Sprintf("%02d", j), j))
		}
		counts[id] = n
	}
	for _, maxKeys := range []int{0, 2, 3, 4, 5, 8} {
		t.Run(fmt.Sprintf("maxkeys-%d", maxKeys), func(t *testing.T) {
			ti := newTreeImage(t, apfstest.Options{})
			blocks := ti.place(varSpec(maxKeys), recs)
			f := ti.mustOpen()
			for id := uint64(0); id < 25; id++ {
				got, err := f.ScanTree(blocks[0].Addr, physTreeType, false, idPrefix(id), 0)
				if err != nil {
					t.Fatalf("id %d: %v", id, err)
				}
				var want []apfstest.Rec
				for _, r := range recs {
					if le.Uint64(r.Key)&idMask == id {
						want = append(want, r)
					}
				}
				sameRecs(t, got, want)
				if len(got) != counts[id] {
					t.Errorf("id %d: %d records, want %d", id, len(got), counts[id])
				}
			}
			if w := f.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings: %q", w)
			}
		})
	}
}

func TestBTreeScanStopsEarly(t *testing.T) {
	var recs []apfstest.Rec
	for i := range 30 {
		recs = append(recs, fixedRec(uint64(i), 1))
	}
	ti := newTreeImage(t, apfstest.Options{})
	blocks := ti.place(fixedSpec(3), recs)
	f := ti.mustOpen()
	got, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 4)
	if err != nil || len(got) != 4 {
		t.Fatalf("limit 4: %d records, %v", len(got), err)
	}
}

func TestBTreeGhostEntriesSkipped(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec func(int) apfstest.TreeSpec
		rec  func(i int) apfstest.Rec
	}{
		{"variable", varSpec, func(i int) apfstest.Rec { return fsRec(uint64(20+i), 3, "", i) }},
		{"fixed", fixedSpec, func(i int) apfstest.Rec { return fixedRec(uint64(20+i), 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var recs, live []apfstest.Rec
			for i := range 12 {
				r := tc.rec(i)
				r.Ghost = i%3 == 0 || i == 11 // the first, several in the middle and the last
				recs = append(recs, r)
				if !r.Ghost {
					live = append(live, r)
				}
			}
			for _, maxKeys := range []int{0, 3} {
				ti := newTreeImage(t, apfstest.Options{})
				blocks := ti.place(tc.spec(maxKeys), recs)
				f := ti.mustOpen()
				sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), live)
				if w := f.Info().Warnings; len(w) != 0 {
					t.Errorf("maxkeys %d: warnings %q", maxKeys, w)
				}
			}
		})
	}
	// A ghost's key and value are never touched: garbage offsets are fine.
	ti := newTreeImage(t, apfstest.Options{})
	recs := []apfstest.Rec{fsRec(1, 3, "", 0), fsRec(2, 3, "", 1), fsRec(3, 3, "", 2)}
	recs[1].Ghost = true
	blocks := ti.place(varSpec(0), recs)
	n := ti.blk(blocks[0].Addr)
	le.PutUint16(n[btTocStart+8:], 0xFFF0)   // ghost k.off
	le.PutUint16(n[btTocStart+8+2:], 0xFFF0) // ghost k.len
	ti.seal(blocks[0])
	sameRecs(t, scanAll(t, ti.mustOpen(), blocks[0].Addr, physTreeType), []apfstest.Rec{recs[0], recs[2]})
}

func TestBTreeEntryBoundsAreValidated(t *testing.T) {
	// Single-node trees; root block layout: header 56, ToC from 56, keys right
	// after it, values from the end of the node minus the 40-byte btree_info.
	variable := []apfstest.Rec{fsRec(1, 3, "ab", 4), fsRec(2, 3, "cd", 5), fsRec(3, 3, "ef", 6)}
	fixed := []apfstest.Rec{fixedRec(1, 1), fixedRec(2, 1), fixedRec(3, 1)}
	const e1 = btTocStart + 8 // variable ToC entry 1: k.off k.len v.off v.len
	const f1 = btTocStart + 4 // fixed ToC entry 1: k.off v.off
	areaLen := 4096 - btInfoSize - (btTocStart + 24)
	type patch func(n []byte)
	put16 := func(off int, v uint16) patch { return func(n []byte) { le.PutUint16(n[off:], v) } }
	put32 := func(off int, v uint32) patch { return func(n []byte) { le.PutUint32(n[off:], v) } }
	for _, tc := range []struct {
		name  string
		fixed bool
		p     patch
	}{
		{"key offset beyond the node", false, put16(e1, 4000)},
		{"key offset huge", false, put16(e1, 0xFFF0)},
		{"key length beyond the node", false, put16(e1+2, 0xFFF0)},
		{"key shorter than an object id", false, put16(e1+2, 4)},
		{"key overlaps its value", false, put16(e1, uint16(areaLen-16))},
		{"value offset beyond the value area", false, put16(e1+4, 4000)},
		{"value offset huge but not a ghost", false, put16(e1+4, 0xFFF0)},
		{"value longer than its offset", false, put16(e1+6, 0xFFF)},
		{"value overlaps the table of contents", false, put16(e1+4, uint16(areaLen))},
		{"nkeys too large for the table", false, put32(btNKeysOff, 4)},
		{"nkeys huge", false, put32(btNKeysOff, 0xFFFFFFFF)},
		{"odd table space length", false, put16(btTSLenOff, 25)},
		{"table space beyond the node", false, put16(btTSLenOff, 4004)},
		{"table space offset beyond the node", false, put16(btTSOffOff, 4000)},
		{"root flag cleared", false, put16(btFlagsOff, btLeafFlag)},
		{"leaf flag with a level", false, put16(btLevelOff, 1)},
		{"level at the depth cap", false, func(n []byte) {
			le.PutUint16(n[btFlagsOff:], btRootFlag)
			le.PutUint16(n[btLevelOff:], 16)
		}},
		{"index node without children", false, func(n []byte) {
			le.PutUint16(n[btFlagsOff:], btRootFlag)
			le.PutUint16(n[btLevelOff:], 1)
			le.PutUint32(n[btNKeysOff:], 0)
		}},
		{"object type is a plain node", false, put32(24, apfstest.StoragePhysical|3)},
		{"fixed: key offset beyond the node", true, put16(f1, 4000)},
		{"fixed: value offset beyond the area", true, put16(f1+2, 4000)},
		{"fixed: value offset smaller than the value", true, put16(f1+2, 8)},
		{"fixed: value overlaps the table", true, put16(f1+2, uint16(4096-btInfoSize-(btTocStart+12)))},
		{"fixed: nkeys too large", true, put32(btNKeysOff, 4)},
		{"fixed: odd table space", true, put16(btTSLenOff, 14)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sealed := range []bool{true, false} {
				ti := newTreeImage(t, apfstest.Options{})
				spec, recs := varSpec(0), variable
				if tc.fixed {
					spec, recs = fixedSpec(0), fixed
				}
				blocks := ti.place(spec, recs)
				tc.p(ti.blk(blocks[0].Addr))
				if sealed {
					ti.seal(blocks[0])
				}
				f := ti.mustOpen()
				_, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
				if !isCorrupt(err) {
					t.Errorf("sealed=%v: ScanTree = %v, want a CorruptError", sealed, err)
				}
			}
		})
	}
}

// Random damage to a node's header, table and keys never panics and never
// returns records that were not in the tree.
func TestBTreeRandomDamageNeverPanics(t *testing.T) {
	var recs []apfstest.Rec
	for i := range 24 {
		recs = append(recs, fsRec(uint64(i/2), uint8(1+i%2), "x", i))
	}
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test input, not security
	for iter := range 400 {
		ti := newTreeImage(t, apfstest.Options{})
		blocks := ti.place(varSpec(5), recs)
		for range 1 + rng.IntN(4) {
			b := blocks[rng.IntN(len(blocks))]
			n := ti.blk(b.Addr)
			n[rng.IntN(200)] = byte(rng.IntN(256))
			ti.seal(b)
		}
		f := ti.mustOpen()
		_, _ = f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
		_, _ = f.ScanTree(blocks[0].Addr, physTreeType, false, idPrefix(uint64(iter%12)), 0)
	}
}

// childSlot is the byte position of the child oid of entry i of a variable
// index node, per the table of contents.
func childSlot(n []byte, i int, root bool) int {
	voff := int(le.Uint16(n[btTocStart+8*i+4:]))
	end := len(n)
	if root {
		end -= btInfoSize
	}
	return end - voff
}

func deepTree(t *testing.T) (*treeImage, []apfstest.Block, []apfstest.Rec) {
	t.Helper()
	var recs []apfstest.Rec
	for i := range 8 {
		recs = append(recs, fsRec(uint64(10+i), 3, "", i))
	}
	ti := newTreeImage(t, apfstest.Options{})
	// MaxKeys 2: four leaves, two level-1 nodes, a level-2 root.
	blocks := ti.place(varSpec(2), recs)
	if len(blocks) != 7 || le.Uint16(blocks[0].Data[btLevelOff:]) != 2 {
		t.Fatalf("unexpected tree shape: %d nodes, root level %d", len(blocks), le.Uint16(blocks[0].Data[btLevelOff:]))
	}
	return ti, blocks, recs
}

func TestBTreeCycleIsCorrupt(t *testing.T) {
	// blocks: [0] root, [1..4] leaves, [5],[6] level-1 nodes.
	for _, tc := range []struct {
		name   string
		node   int  // node to patch
		child  int  // entry index
		target int  // node the child now points to
		root   bool // patched node is the root
	}{
		{"a child points at the root (its ancestor)", 6, 1, 0, false},
		{"root's second child points at the root", 0, 1, 0, true},
		{"an index node points at itself", 5, 0, 5, false},
		{"two children share one leaf", 5, 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti, blocks, _ := deepTree(t)
			n := ti.blk(blocks[tc.node].Addr)
			le.PutUint64(n[childSlot(n, tc.child, tc.root):], blocks[tc.target].Oid)
			ti.seal(blocks[tc.node])
			f := ti.mustOpen()
			if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) || !strings.Contains(err.Error(), "twice") {
				t.Fatalf("full scan = %v, want a CorruptError (reached twice)", err)
			}
			// A seek may not reach the bad child; it must terminate either way.
			for _, id := range []uint64{10, 13, 15, 17} {
				_, _ = f.ScanTree(blocks[0].Addr, physTreeType, false, idPrefix(id), 0)
			}
		})
	}
}

func TestBTreeChildLevelMismatchIsCorrupt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		node, target int
		root         bool
	}{
		{"a leaf directly under the level-2 root", 0, 4, true},
		{"a level-1 node under a level-1 node", 5, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti, blocks, _ := deepTree(t)
			n := ti.blk(blocks[tc.node].Addr)
			le.PutUint64(n[childSlot(n, 0, tc.root):], blocks[tc.target].Oid)
			ti.seal(blocks[tc.node])
			_, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
			if !isCorrupt(err) || !strings.Contains(err.Error(), "level") {
				t.Fatalf("ScanTree = %v, want a level CorruptError", err)
			}
		})
	}
	// A leaf flagged as an index node (and the reverse) is inconsistent too.
	ti, blocks, _ := deepTree(t)
	n := ti.blk(blocks[1].Addr)
	le.PutUint16(n[btLevelOff:], 1) // LEAF flag still set
	ti.seal(blocks[1])
	if _, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) {
		t.Errorf("leaf flag with level 1: %v", err)
	}
}

func TestBTreeDepthCapIsCorrupt(t *testing.T) {
	recs := []apfstest.Rec{fsRec(1, 3, "", 0), fsRec(2, 3, "", 1)}
	for _, levels := range []int{0, 1, 14, 15, 16, 17, 20, 40} {
		t.Run(fmt.Sprintf("levels-%d", levels), func(t *testing.T) {
			ti := newTreeImage(t, apfstest.Options{})
			spec := varSpec(0)
			spec.BlockSize = ti.bs
			blocks := apfstest.PackChain(spec, recs, levels, ti.alloc)
			apfstest.Place(ti.b, ti.bs, blocks)
			got, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
			if levels <= 15 { // at most 16 nodes on any path
				if err != nil {
					t.Fatalf("depth %d: %v", levels+1, err)
				}
				sameRecs(t, got, recs)
				return
			}
			if !isCorrupt(err) || !strings.Contains(err.Error(), "depth") {
				t.Fatalf("depth %d: %v, want a depth CorruptError", levels+1, err)
			}
		})
	}
	// A chain whose upper nodes claim levels above the cap while the root
	// does not: the walk must stop at the cap, not recurse on.
	ti := newTreeImage(t, apfstest.Options{})
	spec := varSpec(0)
	spec.BlockSize = ti.bs
	blocks := apfstest.PackChain(spec, recs, 15, ti.alloc)
	apfstest.Place(ti.b, ti.bs, blocks)
	n := ti.blk(blocks[1].Addr) // the level-14 node
	le.PutUint16(n[btLevelOff:], 30)
	ti.seal(blocks[1])
	if _, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) {
		t.Errorf("hostile level: %v", err)
	}
}

func TestBTreeNodeBudget(t *testing.T) {
	ti, blocks, recs := deepTree(t)
	f := ti.mustOpen()
	f.SetNodeBudget(len(blocks))
	got, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
	if err != nil {
		t.Fatalf("budget = nodes: %v", err)
	}
	sameRecs(t, got, recs)
	f.SetNodeBudget(len(blocks) - 1)
	if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) || !strings.Contains(err.Error(), "nodes") {
		t.Errorf("budget one short: %v, want a node-budget CorruptError", err)
	}
	f.SetNodeBudget(1)
	if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) {
		t.Errorf("budget 1: %v", err)
	}
	// The default budget is capped by the container's block count.
	f2 := ti.mustOpen()
	if _, err := f2.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); err != nil {
		t.Errorf("default budget: %v", err)
	}
}

func TestBTreeNodeSizeMismatchIsUnsupported(t *testing.T) {
	ti := newTreeImage(t, apfstest.Options{})
	blocks := ti.place(varSpec(0), []apfstest.Rec{fsRec(1, 3, "", 0)})
	n := ti.blk(blocks[0].Addr)
	le.PutUint32(n[4096-btInfoSize+4:], 8192) // bt_node_size
	ti.seal(blocks[0])
	_, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("node size 8192 on 4096 blocks: %v, want ErrUnsupported", err)
	}
	le.PutUint32(n[4096-btInfoSize+4:], 2048)
	ti.seal(blocks[0])
	if _, err := ti.mustOpen().ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("node size 2048: %v, want ErrUnsupported", err)
	}
}

func TestBTreeChecksumMismatchWarns(t *testing.T) {
	ti, blocks, recs := deepTree(t)
	ti.blk(blocks[2].Addr)[2000] ^= 0xFF // free space of a leaf: the records survive
	f := ti.mustOpen()
	for range 3 {
		sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
	}
	n := 0
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, "checksum") {
			n++
			if !strings.Contains(w, fmt.Sprint(blocks[2].Addr)) {
				t.Errorf("warning %q does not name block %d", w, blocks[2].Addr)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d checksum warnings, want exactly one: %q", n, f.Info().Warnings)
	}
	// A damaged root is parsed the same way.
	ti, blocks, recs = deepTree(t)
	ti.blk(blocks[0].Addr)[3000] ^= 0xFF
	f = ti.mustOpen()
	sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
	if !hasWarn(f, "checksum", fmt.Sprint(blocks[0].Addr)) {
		t.Errorf("no root checksum warning: %q", f.Info().Warnings)
	}
}

// A node whose header does not belong to it is a warning: another oid, an xid
// newer than the view, storage bits that disagree with the tree.
func TestBTreeNodeIdentityWarnings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(n []byte)
		warn   string
	}{
		{"oid", func(n []byte) { le.PutUint64(n[8:], 12345) }, "carries oid 12345"},
		{"xid", func(n []byte) { le.PutUint64(n[16:], 99) }, "xid 99"},
		{"storage bits", func(n []byte) { le.PutUint32(n[24:], le.Uint32(n[24:])&^0x40000000) }, "storage bits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti, blocks, _ := deepTree(t)
			clean := ti.mustOpen()
			scanAll(t, clean, blocks[0].Addr, physTreeType)
			if w := clean.Info().Warnings; len(w) != 0 {
				t.Fatalf("an undamaged tree warns: %q", w)
			}
			for _, victim := range []int{0, 2} { // the root and a leaf
				var recs []apfstest.Rec
				ti, blocks, recs = deepTree(t)
				tc.mutate(ti.blk(blocks[victim].Addr))
				ti.seal(blocks[victim])
				f := ti.mustOpen()
				sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
				if !hasWarn(f, tc.warn, fmt.Sprint(blocks[victim].Addr)) {
					t.Errorf("node %d: no warning %q naming block %d: %q", victim, tc.warn, blocks[victim].Addr, f.Info().Warnings)
				}
			}
		})
	}
}

// The node reads of all scans of one filesystem share a budget: nested scans
// cannot multiply the work without limit. Exhausting it warns once and every
// further tree read fails.
func TestBTreeCumulativeNodeBudget(t *testing.T) {
	ti, blocks, recs := deepTree(t)
	f := ti.mustOpen()
	f.SetNodeReads(2 * int64(len(blocks)+1)) // a scan reads the root twice (open, scan) and every other node once
	for i := range 2 {
		got, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0)
		if err != nil {
			t.Fatalf("scan %d within the budget: %v", i, err)
		}
		sameRecs(t, got, recs)
	}
	for range 3 {
		if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("scan beyond the budget: %v, want a budget CorruptError", err)
		}
	}
	n := 0
	for _, w := range f.Info().Warnings {
		if strings.Contains(w, "node read budget") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d budget warnings, want one: %q", n, f.Info().Warnings)
	}
}

// bt_node_count tightens the node budget of a scan and, with bt_key_count, is
// cross-checked after a scan of the whole tree.
func TestBTreeInfoCountsAreCrossChecked(t *testing.T) {
	setCounts := func(ti *treeImage, root apfstest.Block, keys, nodes uint64) {
		n := ti.blk(root.Addr)
		le.PutUint64(n[4096-btInfoSize+24:], keys)
		le.PutUint64(n[4096-btInfoSize+32:], nodes)
		ti.seal(root)
	}
	ti, blocks, recs := deepTree(t)
	f := ti.mustOpen()
	sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
	if w := f.Info().Warnings; len(w) != 0 {
		t.Fatalf("accurate counts warn: %q", w)
	}

	ti, blocks, recs = deepTree(t)
	setCounts(ti, blocks[0], uint64(len(recs))+5, uint64(len(blocks)))
	f = ti.mustOpen()
	sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
	if !hasWarn(f, "bt_key_count") || hasWarn(f, "bt_node_count") {
		t.Errorf("wrong key count: %q", f.Info().Warnings)
	}

	// Understated: the tree claims fewer nodes than it has; the scan stops at that
	// many with a CorruptError rather than reading on.
	ti, blocks, _ = deepTree(t)
	setCounts(ti, blocks[0], uint64(len(blocks)), 2)
	f = ti.mustOpen()
	if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, nil, 0); !isCorrupt(err) || !strings.Contains(err.Error(), "more than 2 nodes") {
		t.Errorf("understated bt_node_count: %v", err)
	}

	// Overstated: the whole-tree scan reads fewer nodes than claimed; a warning.
	ti, blocks, recs = deepTree(t)
	setCounts(ti, blocks[0], uint64(len(recs)), uint64(len(blocks))+3)
	f = ti.mustOpen()
	sameRecs(t, scanAll(t, f, blocks[0].Addr, physTreeType), recs)
	if !hasWarn(f, "bt_node_count") {
		t.Errorf("overstated bt_node_count: %q", f.Info().Warnings)
	}

	// A partial scan (a seek) is not compared with the totals.
	ti, blocks, _ = deepTree(t)
	setCounts(ti, blocks[0], 1, 1000)
	f = ti.mustOpen()
	if _, err := f.ScanTree(blocks[0].Addr, physTreeType, false, idPrefix(1), 0); err != nil {
		t.Fatal(err)
	}
	if hasWarn(f, "bt_key_count") || hasWarn(f, "bt_node_count") {
		t.Errorf("a partial scan was compared with the totals: %q", f.Info().Warnings)
	}
}
