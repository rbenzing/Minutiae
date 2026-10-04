package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// B-tree geometry limits (see the plan's Global Constraints).
const (
	minNodeSize  = 512
	maxNodeSize  = 32768
	maxTreeDepth = 16

	btHeaderRecSize = 106 // the B-tree header record in node 0

	nodeKindLeaf  = -1
	nodeKindIndex = 0
	nodeKindMap   = 2

	btBadClose = 1 // kBTBadCloseMask: the tree was not closed cleanly

	// maxKeyLength limits per tree kind (TN1150): catalog keys are at most 516
	// bytes, extents-overflow keys are exactly 10, attribute keys at most 266.
	maxCatalogKeyLength    = 516
	maxAttributesKeyLength = 266
)

type treeKind int

const (
	treeCatalog treeKind = iota
	treeExtents
	treeAttributes
	numTreeKinds
)

func (k treeKind) String() string {
	switch k {
	case treeCatalog:
		return "catalog B-tree"
	case treeExtents:
		return "extents-overflow B-tree"
	}
	return "attributes B-tree"
}

// keyCmp compares a record's key (the bytes after its keyLength field) with
// the key being searched for: negative when the record key sorts first.
type keyCmp func(key []byte) (int, error)

// btree is an opened, header-validated B-tree.
type btree struct {
	f    *FS
	kind treeKind
	fork *forkMap

	nodeSize    int
	depth       int
	root        uint32
	firstLeaf   uint32
	lastLeaf    uint32
	leafRecords uint32
	totalNodes  uint32
	maxKey      uint16
	keyCompare  byte
	attrs       uint32
}

// btNode is one validated node: its descriptor and the record offset table.
type btNode struct {
	num          uint32
	fLink, bLink uint32
	kind         int8
	height       uint8
	buf          []byte
	offs         []uint16 // numRecords+1 offsets; the last is the free-space offset
}

func (n *btNode) numRecords() int { return len(n.offs) - 1 }

// record returns record i (0 <= i < numRecords), key and data together.
func (n *btNode) record(i int) []byte { return n.buf[n.offs[i]:n.offs[i+1]] }

// parseNode validates a node image: the descriptor and the record offset
// table (offsets even, at least 14, strictly increasing, not overlapping the
// table; the free-space offset at or after the last record and before the
// table). Bytes after the free-space offset (stale slack) are never records.
func parseNode(num uint32, buf []byte) (*btNode, error) {
	size := len(buf)
	if size < minNodeSize || size > maxNodeSize || size&(size-1) != 0 {
		return nil, corrupt("B-tree node", -1, "node %d has impossible size %d", num, size)
	}
	be := binary.BigEndian
	n := &btNode{
		num:    num,
		fLink:  be.Uint32(buf[0:]),
		bLink:  be.Uint32(buf[4:]),
		kind:   int8(buf[offNodeKind]),
		height: buf[9],
		buf:    buf,
	}
	switch n.kind {
	case nodeKindLeaf, nodeKindIndex, nodeKindHeader, nodeKindMap:
	default:
		return nil, corrupt("B-tree node", -1, "node %d has unknown kind %d", num, n.kind)
	}
	nrec := int(be.Uint16(buf[10:]))
	if nrec > (size-nodeDescSize)/4 {
		return nil, corrupt("B-tree node", -1, "node %d claims %d records, more than %d bytes can hold", num, nrec, size)
	}
	table := 2 * (nrec + 1)
	limit := size - table // records and the free-space offset must end here
	n.offs = make([]uint16, nrec+1)
	prev := nodeDescSize
	for i := 0; i <= nrec; i++ {
		off := int(be.Uint16(buf[size-2*(i+1):]))
		switch {
		case off%2 != 0:
			return nil, corrupt("B-tree node", -1, "node %d record offset %d is odd", num, off)
		case off < nodeDescSize:
			return nil, corrupt("B-tree node", -1, "node %d record offset %d lies inside the node descriptor", num, off)
		case off > limit:
			return nil, corrupt("B-tree node", -1, "node %d record offset %d lies beyond the free space (offset table starts at %d)", num, off, limit)
		}
		if i < nrec {
			if i > 0 && off <= prev { // strictly increasing between records
				return nil, corrupt("B-tree node", -1, "node %d record offsets are not increasing (%d after %d)", num, off, prev)
			}
		} else if nrec > 0 && off < prev { // the free-space offset is at or after the last record
			return nil, corrupt("B-tree node", -1, "node %d free-space offset %d is before its last record at %d", num, off, prev)
		}
		n.offs[i] = uint16(off)
		prev = off
	}
	return n, nil
}

// splitRecord separates a record into its key bytes (after keyLength) and its
// data (after the key, padded to an even offset).
func splitRecord(rec []byte) (key, data []byte, err error) {
	if len(rec) < 2 {
		return nil, nil, corrupt("B-tree record", -1, "record of %d bytes has no key length", len(rec))
	}
	kl := int(binary.BigEndian.Uint16(rec))
	if 2+kl > len(rec) {
		return nil, nil, corrupt("B-tree record", -1, "key length %d runs past the %d-byte record", kl, len(rec))
	}
	dataOff := min((2+kl+1)&^1, len(rec))
	return rec[2 : 2+kl], rec[dataOff:], nil
}

// openBTree reads and validates the header node of a B-tree held in fk. Every
// header field that later drives a loop, an allocation or an offset is checked
// here; any inconsistency is a *filesys.CorruptError.
func (f *FS) openBTree(kind treeKind, fk *forkMap) (*btree, error) {
	name := kind.String()
	if fk.logical < nodeDescSize+btHeaderRecSize {
		return nil, corrupt(name, -1, "the fork holds only %d bytes, too small for a header node", fk.logical)
	}
	var hb [nodeDescSize + btHeaderRecSize]byte
	if err := readForkFull(fk, hb[:], 0); err != nil {
		return nil, err
	}
	be := binary.BigEndian
	h := hb[nodeDescSize:]
	t := &btree{
		f:           f,
		kind:        kind,
		fork:        fk,
		depth:       int(be.Uint16(h[0:])),
		root:        be.Uint32(h[2:]),
		leafRecords: be.Uint32(h[6:]),
		firstLeaf:   be.Uint32(h[10:]),
		lastLeaf:    be.Uint32(h[14:]),
		nodeSize:    int(be.Uint16(h[18:])),
		maxKey:      be.Uint16(h[20:]),
		totalNodes:  be.Uint32(h[22:]),
		keyCompare:  h[37],
		attrs:       be.Uint32(h[38:]),
	}
	ns := t.nodeSize
	if ns < minNodeSize || ns > maxNodeSize || ns&(ns-1) != 0 {
		return nil, corrupt(name, -1, "node size %d is not a power of two in [%d, %d]", ns, minNodeSize, maxNodeSize)
	}
	if t.totalNodes == 0 {
		return nil, corrupt(name, -1, "the tree has no nodes")
	}
	if bytes, ok := filesys.MulOK(int64(t.totalNodes), int64(ns)); !ok || uint64(bytes) > fk.logical {
		return nil, corrupt(name, -1, "%d nodes of %d bytes do not fit the %d-byte fork", t.totalNodes, ns, fk.logical)
	}
	if t.depth > maxTreeDepth {
		return nil, corrupt(name, -1, "tree depth %d exceeds %d", t.depth, maxTreeDepth)
	}
	if t.root >= t.totalNodes || t.firstLeaf >= t.totalNodes || t.lastLeaf >= t.totalNodes {
		return nil, corrupt(name, -1, "root %d, first leaf %d or last leaf %d is outside the %d nodes", t.root, t.firstLeaf, t.lastLeaf, t.totalNodes)
	}
	if (t.depth == 0) != (t.root == 0) {
		return nil, corrupt(name, -1, "depth %d with root node %d (an empty tree has both 0; node 0 is the header)", t.depth, t.root)
	}
	if t.depth > 0 && (t.firstLeaf == 0 || t.lastLeaf == 0) {
		return nil, corrupt(name, -1, "first leaf %d or last leaf %d is the header node", t.firstLeaf, t.lastLeaf)
	}
	minKey, maxKey := 6, maxCatalogKeyLength
	switch kind {
	case treeExtents:
		minKey, maxKey = extentsKeyLen, extentsKeyLen
	case treeAttributes:
		minKey, maxKey = 12, maxAttributesKeyLength
	}
	if int(t.maxKey) < minKey || int(t.maxKey) > maxKey {
		return nil, corrupt(name, -1, "maximum key length %d is outside [%d, %d]", t.maxKey, minKey, maxKey)
	}
	// Node 0: a header node with its three records (header, user data, map).
	n0, err := t.readNode(0)
	if err != nil {
		return nil, err
	}
	if n0.kind != nodeKindHeader || n0.numRecords() != 3 || n0.offs[0] != nodeDescSize {
		return nil, corrupt(name, -1, "node 0 is not a header node with 3 records (kind %d, %d records)", n0.kind, n0.numRecords())
	}
	if t.depth > 0 { // the root must be a node of the height the header claims
		root, err := t.node(t.root)
		if err != nil {
			return nil, err
		}
		if int(root.height) != t.depth {
			return nil, corrupt(name, -1, "the root node %d has height %d, not the tree depth %d", t.root, root.height, t.depth)
		}
	}
	if t.attrs&btBadClose != 0 {
		f.warn("%s was not closed cleanly (kBTBadCloseMask is set)", name)
	}
	return t, nil
}

// readForkFull reads exactly len(p) bytes of a fork; a short read is an error.
func readForkFull(fk *forkMap, p []byte, off int64) error {
	n, err := fk.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("hfsplus: short fork read")
	}
	return err
}

// readNode reads and parses node n without kind checks beyond the descriptor
// and offset-table rules.
func (t *btree) readNode(n uint32) (*btNode, error) {
	if n >= t.totalNodes {
		return nil, corrupt(t.kind.String(), -1, "node %d is outside the %d nodes", n, t.totalNodes)
	}
	off, ok := filesys.MulOK(int64(n), int64(t.nodeSize)) // fits: totalNodes*nodeSize <= logical size, checked at open
	if !ok {
		return nil, corrupt(t.kind.String(), -1, "node %d offset overflows", n)
	}
	buf := make([]byte, t.nodeSize)
	if err := readForkFull(t.fork, buf, off); err != nil {
		return nil, err
	}
	nd, err := parseNode(n, buf)
	if err != nil {
		var ce *filesys.CorruptError
		if errors.As(err, &ce) {
			ce.Structure = t.kind.String() + " node"
			ce.Offset = off
		}
		return nil, err
	}
	if nd.kind == nodeKindHeader && n != 0 {
		return nil, corrupt(t.kind.String(), off, "node %d is a header node", n)
	}
	return nd, nil
}

// node reads a leaf or index node for traversal: the descriptor must say leaf
// or index, and a leaf's height must be 1.
func (t *btree) node(n uint32) (*btNode, error) {
	nd, err := t.readNode(n)
	if err != nil {
		return nil, err
	}
	switch nd.kind {
	case nodeKindLeaf:
		if nd.height != 1 {
			return nil, corrupt(t.kind.String(), -1, "leaf node %d has height %d, not 1", n, nd.height)
		}
	case nodeKindIndex:
		if nd.height < 2 {
			return nil, corrupt(t.kind.String(), -1, "index node %d has height %d", n, nd.height)
		}
	default:
		return nil, corrupt(t.kind.String(), -1, "node %d has kind %d, not leaf or index", n, nd.kind)
	}
	return nd, nil
}

// search descends from the root to the leaf where key would be and returns
// that leaf and the position of the first record whose key is not less than
// the searched key (possibly the leaf's record count: the record, if any, is
// then in a following leaf). The descent visits at most treeDepth nodes, each
// one level lower than its parent; a loop or a height mismatch is a
// CorruptError. An empty tree gives a nil leaf.
func (t *btree) search(cmp keyCmp) (*btNode, int, error) {
	if t.depth == 0 {
		return nil, 0, nil
	}
	var path [maxTreeDepth]uint32
	cur := t.root
	for level := t.depth; level >= 1; level-- {
		nd, err := t.node(cur)
		if err != nil {
			return nil, 0, err
		}
		if int(nd.height) != level {
			return nil, 0, corrupt(t.kind.String(), -1, "node %d has height %d where the descent expects %d", cur, nd.height, level)
		}
		if (level == 1) != (nd.kind == nodeKindLeaf) {
			return nil, 0, corrupt(t.kind.String(), -1, "node %d of kind %d at height %d", cur, nd.kind, level)
		}
		if level == 1 {
			pos, err := firstAtOrAfter(nd, cmp, 0)
			return nd, pos, err
		}
		if nd.numRecords() == 0 {
			return nil, 0, corrupt(t.kind.String(), -1, "index node %d is empty", cur)
		}
		pos, err := firstAtOrAfter(nd, cmp, 1) // first record whose key is greater than the target
		if err != nil {
			return nil, 0, err
		}
		if pos > 0 {
			pos-- // the last record whose key is not greater than the target
		}
		_, data, err := splitRecord(nd.record(pos))
		if err != nil {
			return nil, 0, err
		}
		if len(data) < 4 {
			return nil, 0, corrupt(t.kind.String(), -1, "index record %d of node %d has no child pointer", pos, cur)
		}
		child := binary.BigEndian.Uint32(data)
		path[t.depth-level] = cur
		for _, a := range path[:t.depth-level+1] {
			if a == child {
				return nil, 0, corrupt(t.kind.String(), -1, "index node %d points at itself or an ancestor (node %d)", cur, child)
			}
		}
		cur = child
	}
	return nil, 0, corrupt(t.kind.String(), -1, "descent ended without a leaf") // unreachable: level 1 returns
}

// firstAtOrAfter binary-searches node nd for the first record whose key
// compares at least minCmp against the target (minCmp 0: not less than the
// target; 1: greater than it).
func firstAtOrAfter(nd *btNode, cmp keyCmp, minCmp int) (int, error) {
	lo, hi := 0, nd.numRecords()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		key, _, err := splitRecord(nd.record(mid))
		if err != nil {
			return 0, err
		}
		c, err := cmp(key)
		if err != nil {
			return 0, err
		}
		if c >= minCmp {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}

// scan walks the leaf chain from record pos of leaf nd through the fLink
// pointers, calling fn for every record (key and data together) until fn
// returns false or the chain ends. Every link must be inside the tree, every
// node reached must be a leaf and none may be visited twice: a loop is a
// CorruptError, as is a chain longer than totalNodes.
func (t *btree) scan(nd *btNode, pos int, fn func(rec []byte) (bool, error)) error {
	return t.scanHook(nd, pos, nil, fn)
}

// scanHook is scan with a hook that runs for the first leaf and before each
// further leaf is read; an error from it ends the scan and is returned (it is
// how a caller charges the leaves it reads to a budget).
func (t *btree) scanHook(nd *btNode, pos int, onNode func(num uint32) error, fn func(rec []byte) (bool, error)) error {
	if onNode != nil {
		if err := onNode(nd.num); err != nil {
			return err
		}
	}
	visited := map[uint32]struct{}{}
	for {
		visited[nd.num] = struct{}{}
		if nd.kind != nodeKindLeaf || nd.height != 1 {
			return corrupt(t.kind.String(), -1, "node %d in the leaf chain is not a leaf", nd.num)
		}
		for i := pos; i < nd.numRecords(); i++ {
			more, err := fn(nd.record(i))
			if err != nil || !more {
				return err
			}
		}
		pos = 0
		next := nd.fLink
		if next == 0 {
			return nil
		}
		if next >= t.totalNodes {
			return corrupt(t.kind.String(), -1, "leaf %d links to node %d outside the %d nodes", nd.num, next, t.totalNodes)
		}
		if _, seen := visited[next]; seen || uint32(len(visited)) >= t.totalNodes {
			return corrupt(t.kind.String(), -1, "the leaf chain loops at node %d", next)
		}
		if onNode != nil {
			if err := onNode(next); err != nil {
				return err
			}
		}
		var err error
		if nd, err = t.node(next); err != nil {
			return err
		}
	}
}

// scanFrom scans the leaf records from the first one not less than the key.
func (t *btree) scanFrom(cmp keyCmp, fn func(rec []byte) (bool, error)) error {
	leaf, pos, err := t.search(cmp)
	if err != nil || leaf == nil {
		return err
	}
	return t.scan(leaf, pos, fn)
}

// scanAll scans every leaf record from the first leaf.
func (t *btree) scanAll(fn func(rec []byte) (bool, error)) error {
	if t.depth == 0 {
		return nil
	}
	leaf, err := t.node(t.firstLeaf)
	if err != nil {
		return err
	}
	return t.scan(leaf, 0, fn)
}

// seek returns up to n records starting at the first one not less than the key.
func (t *btree) seek(cmp keyCmp, n int) ([][]byte, error) {
	var out [][]byte
	err := t.scanFrom(cmp, func(rec []byte) (bool, error) {
		out = append(out, rec)
		return len(out) < n, nil
	})
	return out, err
}

// tree returns the (lazily opened, cached) B-tree of the given kind. The
// attributes tree is nil, with no error, when the volume has none. Failures
// are not cached.
func (f *FS) tree(kind treeKind) (*btree, error) {
	f.treeMu.Lock()
	t := f.trees[kind]
	f.treeMu.Unlock()
	if t != nil {
		return t, nil
	}
	var fileID uint32
	var fd forkData
	switch kind {
	case treeCatalog:
		fileID, fd = 4, f.vh.catalog
	case treeExtents:
		fileID, fd = cnidExtents, f.vh.extents
	default:
		fileID, fd = 8, f.vh.attributesFile
		if fd.totalBlocks == 0 {
			return nil, nil
		}
	}
	fk, err := f.fork(fileID, false, fd)
	if err != nil {
		return nil, err
	}
	t, err = f.openBTree(kind, fk)
	if err != nil {
		return nil, err
	}
	f.treeMu.Lock()
	if prev := f.trees[kind]; prev != nil {
		t = prev
	} else {
		f.trees[kind] = t
	}
	f.treeMu.Unlock()
	return t, nil
}
