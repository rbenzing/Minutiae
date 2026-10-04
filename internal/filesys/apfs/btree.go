package apfs

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// B-tree layout (Apple File System Reference; the Format reference of the
// project plan). A node is one block: the 32-byte object header, a 24-byte
// btree_node_phys_t header (btn_data starts at 56), the table of contents, the
// key area, free space and the value area, which ends at the end of the block
// (or before the 40-byte btree_info_t of a root node).
const (
	typeBTree     = 0x2 // BTREE: a root node
	typeBTreeNode = 0x3 // BTREE_NODE: any other node

	// Storage bits of an object type or tree type.
	storageMask      = 0xc0000000
	storagePhysical  = 0x40000000
	storageEphemeral = 0x80000000

	btnRoot    = 0x1
	btnLeaf    = 0x2
	btnFixedKV = 0x4
	btnNoHdr   = 0x10 // BTNODE_NOHEADER

	btnDataOff    = 56 // btn_data
	btreeInfoSize = 40 // btree_info_t, last bytes of a root node

	// btOffInvalid is v.off of a ghost entry (BTOFF_INVALID).
	btOffInvalid = 0xFFFF

	btHashed = 0x80 // bt_flags HASHED: index values carry a hash after the oid

	hashedChildHashLen = 32

	// maxBTreeDepth bounds the levels of a tree (and so the recursion).
	maxBTreeDepth = 16
	// maxNodeBudget bounds the nodes one scan reads; the effective budget is
	// also capped by the container's block count.
	maxNodeBudget = 1 << 20

	// minNodeReads is the least number of node reads all scans of one FS may
	// make together; the budget is 4 reads per container block above it.
	minNodeReads = 1 << 20

	// minKeyLen: every key of every APFS tree starts with an 8-byte id.
	minKeyLen = 8
)

// btreeInfo is btree_info_t, the last 40 bytes of a root node.
type btreeInfo struct {
	flags      uint32
	nodeSize   uint32
	keySize    uint32 // 0 for variable-size keys
	valSize    uint32 // 0 for variable-size values
	longestKey uint32
	longestVal uint32
	keyCount   uint64
	nodeCount  uint64
}

// rootKind says how a tree's node oids are resolved.
type rootKind int

const (
	kindPhysical rootKind = iota // oids are block addresses
	kindVirtual                  // oids go through an object map
)

// tree is an opened B-tree.
type tree struct {
	f        *FS
	root     uint64 // root oid as given
	rootAddr uint64 // block of the root node
	info     btreeInfo
	kind     rootKind
	omap     *omapView // resolves virtual oids; nil for a physical tree
}

// btRec is one live record of a node; key and val alias the node's own buffer.
type btRec struct {
	key, val []byte
}

// btNode is a validated node.
type btNode struct {
	addr   uint64
	level  uint16
	recs   []btRec // live records (ghosts removed), in node order
	ghosts int     // ghost entries that were removed
}

// openTree opens the tree rooted at rootOid; typ is the tree-type field that
// names it (om_tree_type, apfs_root_tree_type, ...): its storage bits say
// whether rootOid is a block address (physical) or a virtual oid resolved
// through o. It reads and validates the root node. A tree of ephemeral objects
// is unsupported.
func (f *FS) openTree(rootOid uint64, typ uint32, o *omapView) (*tree, error) {
	if typ&typeMask != typeBTree {
		return nil, corrupt("B-tree", -1, "tree type %#x is not a B-tree", typ)
	}
	t := &tree{f: f, root: rootOid, omap: o}
	switch typ & storageMask {
	case storagePhysical:
		t.kind = kindPhysical
	case 0:
		if o == nil {
			return nil, corrupt("B-tree", -1, "virtual tree %d has no object map to resolve it", rootOid)
		}
		t.kind = kindVirtual
	case storageEphemeral:
		return nil, unsupported("B-tree of ephemeral objects")
	default:
		return nil, corrupt("B-tree", -1, "tree type %#x has conflicting storage bits", typ)
	}
	addr, err := t.addrOf(rootOid)
	if err != nil {
		return nil, err
	}
	t.rootAddr = addr
	buf, err := t.readNode(rootOid, addr, true)
	if err != nil {
		return nil, err
	}
	if buf[32]&btnRoot == 0 { // btn_flags low byte
		return nil, corrupt("B-tree", int64(addr)*int64(f.bs), "block %d: the root of a tree is not flagged ROOT", addr)
	}
	info := btreeInfo{
		flags:      le.Uint32(buf[len(buf)-btreeInfoSize:]),
		nodeSize:   le.Uint32(buf[len(buf)-btreeInfoSize+4:]),
		keySize:    le.Uint32(buf[len(buf)-btreeInfoSize+8:]),
		valSize:    le.Uint32(buf[len(buf)-btreeInfoSize+12:]),
		longestKey: le.Uint32(buf[len(buf)-btreeInfoSize+16:]),
		longestVal: le.Uint32(buf[len(buf)-btreeInfoSize+20:]),
		keyCount:   le.Uint64(buf[len(buf)-btreeInfoSize+24:]),
		nodeCount:  le.Uint64(buf[len(buf)-btreeInfoSize+32:]),
	}
	if int64(info.nodeSize) != int64(f.bs) {
		// Larger nodes exist in theory [unverified]; only block-sized ones are read.
		return nil, unsupported("B-tree node size %d differs from the block size %d", info.nodeSize, f.bs)
	}
	if int64(info.keySize) > int64(f.bs) || int64(info.valSize) > int64(f.bs) {
		return nil, corrupt("B-tree", int64(addr)*int64(f.bs), "block %d: fixed key/value sizes %d/%d exceed a node", addr, info.keySize, info.valSize)
	}
	t.info = info
	if _, err := t.parseNode(buf, addr, true); err != nil { // validate the root now
		return nil, err
	}
	return t, nil
}

// addrOf maps a node oid to its block address: itself for a physical tree, the
// object map's answer for a virtual one.
func (t *tree) addrOf(oid uint64) (uint64, error) {
	if t.kind == kindPhysical {
		return oid, nil
	}
	paddr, size, flags, err := t.omap.resolve(oid)
	if err != nil {
		if errors.Is(err, filesys.ErrNotFound) {
			return 0, corrupt("B-tree", -1, "virtual node oid %d is not in the object map", oid)
		}
		return 0, err
	}
	if flags&omapValEncrypted != 0 {
		return 0, fmt.Errorf("apfs: %w: B-tree node oid %d is encrypted", filesys.ErrEncrypted, oid)
	}
	if flags&omapValNoHeader != 0 {
		return 0, unsupported("B-tree node oid %d is stored without an object header", oid)
	}
	if int64(size) != int64(t.f.bs) {
		return 0, corrupt("B-tree", -1, "virtual node oid %d maps %d bytes, want one %d-byte node", oid, size, t.f.bs)
	}
	return paddr, nil
}

// chargeNode takes one node read from the filesystem-wide budget. Scans are
// nested (a virtual tree resolves every node through an object map, itself a
// tree), so the per-scan budget alone does not bound the total work; this one
// does. Exhausting it is one warning, then every further tree read fails.
func (f *FS) chargeNode() error {
	if f.nodeReads.Add(-1) < 0 {
		f.warn("the B-tree node read budget of %d reads for this filesystem is exhausted; further tree reads fail", f.nodeReadLimit)
		return &budgetError{corrupt("B-tree", -1, "the node read budget of %d reads for this filesystem is exhausted", f.nodeReadLimit).(*filesys.CorruptError)}
	}
	return nil
}

// readNode reads one node block of the tree: oid is the node's oid (its block
// address for a physical tree), root says whether it is the root. The object
// header type must be BTREE for a root and BTREE_NODE otherwise. A checksum
// mismatch is a warning (the node is still parsed, with every bound checked),
// and so are a header that does not belong to this node: another oid, an xid
// newer than the tree's view, storage bits that disagree with the tree.
func (t *tree) readNode(oid, addr uint64, root bool) ([]byte, error) {
	f := t.f
	if err := f.chargeNode(); err != nil {
		return nil, err
	}
	buf, h, err := f.readObjectRaw(addr, f.bs)
	if err != nil {
		return nil, err
	}
	if !checksumOK(buf) {
		f.warn("B-tree node at block %d has a bad checksum; it is read with every bound checked", addr)
	}
	want := uint32(typeBTreeNode)
	if root {
		want = typeBTree
	}
	if h.kind() != want {
		return nil, corrupt("B-tree node", int64(addr)*int64(f.bs), "block %d has object type %#x, want %#x", addr, h.kind(), want)
	}
	if h.oid != oid {
		f.warn("B-tree node at block %d carries oid %d, want %d", addr, h.oid, oid)
	}
	view := f.nx.xid
	if t.omap != nil {
		view = t.omap.xid
	}
	if h.xid > view {
		f.warn("B-tree node at block %d has xid %d, newer than the view it is read in (xid %d)", addr, h.xid, view)
	}
	wantStorage := uint32(storagePhysical)
	if t.kind == kindVirtual {
		wantStorage = 0
	}
	if h.typ&storageMask != wantStorage {
		f.warn("B-tree node at block %d has storage bits %#x, want %#x for its tree", addr, h.typ&storageMask, wantStorage)
	}
	return buf, nil
}

// parseNode validates a node and decodes its records. Every table-of-contents
// entry is checked against the node before any key or value is sliced out.
func (t *tree) parseNode(buf []byte, addr uint64, root bool) (*btNode, error) {
	bs := len(buf)
	off := int64(addr) * int64(t.f.bs)
	bad := func(format string, a ...any) error {
		return corrupt("B-tree node", off, "block %d: %s", addr, fmt.Sprintf(format, a...))
	}
	flags := le.Uint16(buf[32:])
	level := le.Uint16(buf[34:])
	nkeys := uint64(le.Uint32(buf[36:]))
	tsOff := int(le.Uint16(buf[40:]))
	tsLen := int(le.Uint16(buf[42:]))

	if flags&btnNoHdr != 0 {
		return nil, unsupported("B-tree node at block %d has no object header", addr)
	}
	if (flags&btnRoot != 0) != root {
		return nil, bad("ROOT flag %v on a %s node", flags&btnRoot != 0, map[bool]string{true: "root", false: "non-root"}[root])
	}
	if (flags&btnLeaf != 0) != (level == 0) {
		return nil, bad("leaf flag %v with level %d", flags&btnLeaf != 0, level)
	}
	if level >= maxBTreeDepth {
		return nil, bad("level %d exceeds the depth cap of %d", level, maxBTreeDepth)
	}
	fixed := flags&btnFixedKV != 0
	entry := 8
	if fixed {
		entry = 4
	}
	valEnd := bs
	if root {
		valEnd -= btreeInfoSize
	}
	tocStart := btnDataOff + tsOff
	keyStart := tocStart + tsLen // the key area starts after the table
	if keyStart > valEnd {
		return nil, bad("table of contents [%d, +%d) does not fit the node", tocStart, tsLen)
	}
	if tsLen%4 != 0 {
		return nil, bad("table space length %d is not a multiple of 4", tsLen)
	}
	if nkeys*uint64(entry) > uint64(tsLen) {
		return nil, bad("%d entries of %d bytes do not fit a table space of %d", nkeys, entry, tsLen)
	}
	leaf := level == 0
	areaLen := valEnd - keyStart // keys grow up from keyStart, values down from valEnd

	n := &btNode{addr: addr, level: level, recs: make([]btRec, 0, nkeys)}
	for i := range int(nkeys) {
		e := buf[tocStart+i*entry:]
		var kOff, kLen, vOff, vLen int
		if fixed {
			kOff, vOff = int(le.Uint16(e)), int(le.Uint16(e[2:]))
			switch {
			case t.info.keySize > 0:
				kLen = int(t.info.keySize)
			case !leaf:
				kLen = minKeyLen // an index key is only read for its 8-byte id
			default:
				return nil, bad("fixed-size leaf entry in a tree without a fixed key size")
			}
			switch {
			case !leaf && t.info.flags&btHashed != 0:
				vLen = 8 + hashedChildHashLen
			case !leaf:
				vLen = 8
			case t.info.valSize > 0:
				vLen = int(t.info.valSize)
			default:
				return nil, bad("fixed-size leaf entry in a tree without a fixed value size")
			}
		} else {
			kOff, kLen = int(le.Uint16(e)), int(le.Uint16(e[2:]))
			vOff, vLen = int(le.Uint16(e[4:])), int(le.Uint16(e[6:]))
		}
		if vOff == btOffInvalid { // a ghost: no value
			if !leaf {
				t.f.warn("B-tree index node at block %d has a ghost entry: skipped", addr)
			}
			n.ghosts++
			continue
		}
		if kLen < minKeyLen {
			return nil, bad("entry %d: key of %d bytes is shorter than an object id", i, kLen)
		}
		if kOff > areaLen || kLen > areaLen-kOff {
			return nil, bad("entry %d: key [%d, +%d) is outside the key area of %d bytes", i, kOff, kLen, areaLen)
		}
		if vOff > areaLen {
			return nil, bad("entry %d: value offset %d is beyond the value area of %d bytes", i, vOff, areaLen)
		}
		if vLen > vOff {
			return nil, bad("entry %d: value of %d bytes at offset %d runs past the end of the node", i, vLen, vOff)
		}
		if !leaf && vLen < 8 {
			return nil, bad("entry %d: index value of %d bytes cannot hold a child oid", i, vLen)
		}
		kStart := keyStart + kOff
		vStart := valEnd - vOff
		if kStart+kLen > vStart {
			return nil, bad("entry %d: key [%d, +%d) overlaps its value at %d", i, kStart, kLen, vStart)
		}
		n.recs = append(n.recs, btRec{key: buf[kStart : kStart+kLen : kStart+kLen], val: buf[vStart : vStart+vLen : vStart+vLen]})
	}
	return n, nil
}

// scanner is the state of one scan.
type scanner struct {
	t       *tree
	prefix  func(key []byte) int
	visit   func(key, val []byte) (stop bool, err error)
	visited map[uint64]struct{}
	budget  int    // child nodes this scan may still read
	limit   int    // nodes this scan may read, the root included
	nodes   int    // nodes read
	keys    uint64 // leaf entries seen, ghosts included
	inRange bool   // a record of the range was seen
	ooo     bool   // the records are not in strictly increasing key order
	stop    bool

	// order, when set, makes the scan verify that every record it meets (also
	// the ones after the range, up to the end of the leaf that holds the first
	// of them) sorts strictly after the previous one.
	order    func(prev, cur []byte) bool
	prev     []byte
	havePrev bool
	beyond   bool // a record after the range was seen; the rest of the leaf is only order-checked
}

// scan visits, in key order, the leaf records for which prefix returns 0.
// prefix classifies a key against the wanted range: negative when the key sorts
// before it, zero inside it, positive after it; it must be monotonic in key
// order and must tolerate keys of any length of at least 8 bytes. A nil prefix
// visits every record. The seek is the one of the Format reference: in an index
// node descend into the last child whose key sorts before the range (else the
// first), then continue forward and stop at the first key after the range.
// Records that sort before the range after one inside it end the scan with a
// warning (the tree is out of order).
//
// key and val alias the node buffer and must not be modified. visit returns
// stop to end the scan early. Structural problems are *filesys.CorruptError:
// depth over 16, a node reached twice (a cycle or a shared child), more nodes
// than the budget (the container's, tightened to the tree's own bt_node_count),
// a child level that is not the parent's minus one. A scan of the whole tree
// (nil prefix) that runs to the end also checks bt_node_count and bt_key_count
// and warns when they disagree with what was read.
func (t *tree) scan(prefix func(key []byte) int, visit func(key, val []byte) (stop bool, err error)) error {
	_, err := t.scanOrdered(prefix, nil, visit)
	return err
}

// scanOrdered is scan that also reports whether the scan ended early because
// the tree is out of key order (records of the range may then have been missed:
// a caller that must not mistake that for the end of the range checks it).
// Without order, only a record sorting before the range after one inside it is
// noticed. With order (a strict "prev sorts before cur" test), every record read
// is checked against its predecessor, and after the first record beyond the
// range the rest of that leaf is read too, so a record of the range that
// follows a larger key is found: a record that is not strictly greater than the
// previous one, or a range record after a larger key, ends the scan with
// outOfOrder true. Records in later leaves are not read (the index keys say
// they sort after the range).
func (t *tree) scanOrdered(prefix func(key []byte) int, order func(prev, cur []byte) bool, visit func(key, val []byte) (stop bool, err error)) (outOfOrder bool, err error) {
	limit := t.f.nodeBudget
	if n := t.info.nodeCount; n > 0 && n < uint64(limit) {
		limit = int(n) // the tree says how many nodes it has
	}
	s := &scanner{
		t: t, prefix: prefix, visit: visit, order: order,
		visited: map[uint64]struct{}{t.rootAddr: {}},
		limit:   limit, budget: limit - 1, nodes: 1, // the root
	}
	buf, err := t.readNode(t.root, t.rootAddr, true)
	if err != nil {
		return false, err
	}
	root, err := t.parseNode(buf, t.rootAddr, true)
	if err != nil {
		return false, err
	}
	if err := s.walk(root, 1, true); err != nil {
		return false, err
	}
	if prefix == nil && !s.stop {
		s.crossCheck()
	}
	return s.ooo, nil
}

// crossCheck compares what a scan of the whole tree read with the tree's own
// bt_node_count and bt_key_count.
func (s *scanner) crossCheck() {
	t := s.t
	if uint64(s.nodes) != t.info.nodeCount {
		t.f.warn("B-tree at block %d reports %d nodes (bt_node_count) but %d were read", t.rootAddr, t.info.nodeCount, s.nodes)
	}
	if s.keys != t.info.keyCount {
		t.f.warn("B-tree at block %d reports %d keys (bt_key_count) but %d were read", t.rootAddr, t.info.keyCount, s.keys)
	}
}

func (s *scanner) cmp(key []byte) int {
	if s.prefix == nil {
		return 0
	}
	return s.prefix(key)
}

func (s *scanner) walk(n *btNode, depth int, seeking bool) error {
	if depth > maxBTreeDepth {
		return corrupt("B-tree", int64(n.addr)*int64(s.t.f.bs), "tree deeper than %d levels", maxBTreeDepth)
	}
	if n.level == 0 {
		return s.leaf(n)
	}
	if len(n.recs) == 0 {
		return corrupt("B-tree node", int64(n.addr)*int64(s.t.f.bs), "block %d: index node without children", n.addr)
	}
	start := 0
	if seeking {
		for i, r := range n.recs {
			if s.cmp(r.key) >= 0 {
				break
			}
			start = i
		}
	}
	for i := start; i < len(n.recs); i++ {
		if (!seeking || i != start) && s.cmp(n.recs[i].key) > 0 {
			s.stop = true // this child and all after it are beyond the range
			return nil
		}
		child, err := s.load(n, n.recs[i].val)
		if err != nil {
			return err
		}
		if err := s.walk(child, depth+1, seeking && i == start); err != nil {
			return err
		}
		if s.stop {
			return nil
		}
	}
	return nil
}

// load reads the child an index value points to.
func (s *scanner) load(parent *btNode, val []byte) (*btNode, error) {
	t := s.t
	oid := le.Uint64(val)
	addr, err := t.addrOf(oid)
	if err != nil {
		return nil, err
	}
	if _, dup := s.visited[addr]; dup {
		return nil, corrupt("B-tree", int64(addr)*int64(t.f.bs), "node at block %d is reached twice (a cycle or a shared child)", addr)
	}
	if s.budget <= 0 {
		return nil, corrupt("B-tree", -1, "tree has more than %d nodes (the container allows %d, the tree reports %d)", s.limit, t.f.nodeBudget, t.info.nodeCount)
	}
	s.budget--
	s.nodes++
	s.visited[addr] = struct{}{}
	buf, err := t.readNode(oid, addr, false)
	if err != nil {
		return nil, err
	}
	child, err := t.parseNode(buf, addr, false)
	if err != nil {
		return nil, err
	}
	if child.level != parent.level-1 {
		return nil, corrupt("B-tree node", int64(addr)*int64(t.f.bs), "block %d: level %d under a level %d node", addr, child.level, parent.level)
	}
	return child, nil
}

func (s *scanner) leaf(n *btNode) error {
	s.keys += uint64(len(n.recs) + n.ghosts)
	for _, r := range n.recs {
		if s.order != nil {
			if s.havePrev && !s.order(s.prev, r.key) {
				s.t.f.warn("B-tree node at block %d is out of key order: scan stopped", n.addr)
				s.ooo, s.stop = true, true
				return nil
			}
			s.prev, s.havePrev = r.key, true
			if s.beyond {
				continue // only the order is checked from here to the end of the leaf
			}
		}
		switch c := s.cmp(r.key); {
		case c > 0:
			if s.order != nil {
				s.beyond = true // keep reading this leaf: a record of the range after this one means the tree is out of order
				continue
			}
			s.stop = true
			return nil
		case c < 0:
			if s.inRange {
				s.t.f.warn("B-tree node at block %d is out of key order: scan stopped", n.addr)
				s.ooo = true
				s.stop = true
				return nil
			}
		default:
			s.inRange = true
			stop, err := s.visit(r.key, r.val)
			if err != nil {
				return err
			}
			if stop {
				s.stop = true
				return nil
			}
		}
	}
	if s.beyond {
		s.stop = true
	}
	return nil
}

// errNodeBudget marks the exhaustion of the filesystem-wide node read budget:
// a *filesys.CorruptError that callers can tell from damage to one structure.
var errNodeBudget = errors.New("node read budget exhausted")

type budgetError struct{ *filesys.CorruptError }

func (e *budgetError) Is(target error) bool {
	return target == errNodeBudget || target == filesys.ErrCorrupt
}

func (e *budgetError) Unwrap() error { return e.CorruptError }
