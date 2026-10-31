package f2fs

import "unsafe"

// Test-only access to the stale-node index (3D Task 4).

// StaleNode is a read-only copy of an index entry.
type StaleNode struct {
	NID, Addr, Ino, Ver, Ofs uint32
	SSA                      bool
}

// NodeMeta is a read-only copy of the per-nid facts of the index.
type NodeMeta struct {
	NID            uint32
	PostCheckpoint uint16
	Evicted        bool
}

// NodeIndexStats counts what the index pass did.
type NodeIndexStats = indexStats

// Sizes of the index records, pinned by a test (3D-R22).
const (
	StaleNodeSize = unsafe.Sizeof(staleNode{})
	NodeMetaSize  = unsafe.Sizeof(nidMeta{})
)

// SetNodeIndexCap lowers the number of entries the index may hold.
func (f *FS) SetNodeIndexCap(n int) { f.indexCap = n }

// NodeIndexStats returns the counters of the index (zero before it is built).
func (f *FS) NodeIndexStats() NodeIndexStats { return f.idxStats() }

// BuildNodeIndex builds the index (once) and returns its error.
func (f *FS) BuildNodeIndex() error {
	_, err := f.nodeIndex()
	return err
}

// NodeIndexLookup returns the indexed copies of nid, highest version first.
func (f *FS) NodeIndexLookup(nid uint32) ([]StaleNode, NodeMeta, error) {
	x, err := f.nodeIndex()
	if err != nil {
		return nil, NodeMeta{}, err
	}
	nodes, m, err := x.lookup(nid)
	out := make([]StaleNode, len(nodes))
	for i, n := range nodes {
		out[i] = StaleNode{n.nid, n.addr, n.ino, n.ver, n.ofs, n.ssa}
	}
	return out, NodeMeta{m.nid, m.postCheckpoint, m.evicted}, err
}
