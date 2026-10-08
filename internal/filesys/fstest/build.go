package fstest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// BuildSpec describes an MTFS image for Build.
//
// Nodes are laid out in sorted path order. Missing parent directories are
// created implicitly as live directories; an explicit Node for a directory
// overrides that. Entry ids are assigned in sorted path order starting at "2"
// (the root is "1").
type BuildSpec struct {
	// Label is the volume label (Info.Label).
	Label string
	// BlockSize is the allocation block size; 0 means 512.
	BlockSize int
	// FreeBlocks is the number of extra unallocated blocks after the data.
	FreeBlocks int
	// Nodes are the files, directories and symlinks to create.
	Nodes []Node
}

// Node is one file, directory or symlink of a BuildSpec. A Node with Dir set
// is a directory, one with Link set is a symlink, anything else is a regular
// file. Build panics on an inconsistent spec (it is a test helper).
type Node struct {
	// Path is the absolute slash path ("/a/b.txt"); a missing leading slash
	// is added.
	Path string
	// Dir makes the node a directory (no Data, Link, Fragments or Hole).
	Dir bool
	// Link makes the node a symlink to this target. Opening it yields the
	// target bytes and it has no runs.
	Link string
	// Data is the file content. Its blocks are stored block-aligned in the
	// data area; Entry.Size is len(Data).
	Data []byte
	// Deleted marks the directory entry deleted. Its data and runs stay in
	// the image, and its blocks still count as allocated for Unallocated.
	Deleted bool
	// Encrypted marks the entry encrypted (and Info.Encrypted); the stored
	// bytes are returned as they are.
	Encrypted bool
	// Fragments: 0 or 1 stores the content contiguously. N >= 2 splits it
	// into N block-aligned runs of near-equal size separated by one free
	// block each; the content must have at least N blocks.
	Fragments int
	// Inline stores the content (Data) inline in the table, as the small-file
	// optimisation of real filesystems does: no data blocks, no runs, and
	// File.Runs returns nil. Only for regular files without Fragments or Hole.
	Inline bool
	// Hole makes the file's second block a sparse hole: a run with Offset -1
	// that takes no storage and reads as zeros whatever Data holds there.
	// The content must have at least two blocks.
	Hole bool
	// Mode holds the permission bits (the file type bits are added). Zero
	// means 0644 for files, 0755 for directories and 0777 for symlinks.
	Mode     uint32
	UID, GID uint32
	// MTime is the modification time in Unix seconds; 0 means absent.
	MTime int64
	// Freed (only with Deleted) makes the node's data blocks count as free for
	// Unallocated, as a real delete leaves them; a Deleted node without it keeps
	// them allocated (the existing behaviour).
	Freed bool
	// Recover plants the stale maps the filesystem's Recoverer returns for this
	// deleted node (only with Deleted).
	Recover []RecoverMap
}

// RecoverMap is one stale recovery map planted on a deleted Node. Build panics
// when Method is empty or when RunsOf names no node.
type RecoverMap struct {
	// Method is the candidate method token (required).
	Method string
	// RunsOf is the path of the node whose laid-out runs the map uses (trimmed
	// to the map's size, holes skipped); "" means this node.
	RunsOf string
	// Runs are explicit runs, used verbatim and never validated (hostile maps);
	// they win over RunsOf.
	Runs []filesys.Run
	// Size is the claimed size; 0 means the size of the node the runs come from.
	Size                         int64
	Basis, Assumptions, Warnings []string
	Mode                         uint32
	Encrypted                    bool
}

// blockRun is a run in block units within the data area; start -1 is a hole.
type blockRun struct{ start, n int64 }

type built struct {
	rec  record
	node Node
	runs []blockRun
}

// Build returns the bytes of an MTFS image for spec. The image ends with
// spec.FreeBlocks unallocated blocks; the data area starts at the first block
// boundary after the table.
func Build(spec BuildSpec) []byte {
	bs := int64(spec.BlockSize)
	if bs == 0 {
		bs = 512
	}
	if bs < 1 || bs > maxBlockSize {
		panic(fmt.Sprintf("fstest: block size %d outside 1..%d", bs, maxBlockSize))
	}
	if spec.FreeBlocks < 0 {
		panic("fstest: negative FreeBlocks")
	}
	nodes := normalize(spec.Nodes)
	paths := make([]string, 0, len(nodes))
	for p := range nodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	ids := map[string]string{"/": rootID}
	items := make([]*built, 0, len(paths))
	for i, p := range paths {
		ids[p] = fmt.Sprint(i + 2)
	}
	var next int64 // next free block of the data area
	for _, p := range paths {
		n := nodes[p]
		b := &built{node: n, rec: baseRecord(p, n, ids)}
		next = layout(b, bs, next)
		items = append(items, b)
	}
	total := next + int64(spec.FreeBlocks)

	root := record{ID: rootID, Name: "", Type: "dir", Mode: 0o040755}
	dataStart := alignUp(headerLen, bs)
	var tableJSON []byte
	for {
		tbl := table{Label: spec.Label, BlockSize: bs, Entries: []record{root}}
		abs := map[string][]run{} // laid-out runs by node path, for Recover maps that borrow them
		for _, b := range items {
			var runs []run
			for _, r := range b.runs {
				off := int64(-1)
				if r.start >= 0 {
					off = dataStart + r.start*bs
				}
				runs = append(runs, run{Offset: off, Length: r.n * bs})
			}
			abs[b.node.Path] = runs
		}
		for _, b := range items {
			rec := b.rec
			rec.Runs = abs[b.node.Path]
			for _, rm := range b.node.Recover {
				rec.Recover = append(rec.Recover, resolveRecover(b.node, rm, nodes, abs))
			}
			tbl.Entries = append(tbl.Entries, rec)
		}
		var err error
		if tableJSON, err = json.Marshal(tbl); err != nil {
			panic(err) // cannot happen: plain data
		}
		need := alignUp(headerLen+int64(len(tableJSON)), bs)
		if need <= dataStart {
			break
		}
		dataStart = need // offsets grow with dataStart, so iterate until the table fits
	}
	// Pad the table with JSON whitespace so the data area starts exactly at
	// the end of the table.
	tableJSON = append(tableJSON, strings.Repeat(" ", int(dataStart-headerLen)-len(tableJSON))...)

	img := make([]byte, dataStart+total*bs)
	copy(img, magic)
	binary.LittleEndian.PutUint64(img[8:], uint64(len(tableJSON)))
	copy(img[headerLen:], tableJSON)
	for _, b := range items {
		var cursor int64 // logical byte position in Data
		for _, r := range b.runs {
			length := r.n * bs
			if r.start >= 0 && cursor < int64(len(b.node.Data)) {
				copy(img[dataStart+r.start*bs:dataStart+r.start*bs+length], b.node.Data[cursor:])
			}
			cursor += length
		}
	}
	return img
}

func alignUp(v, bs int64) int64 { return (v + bs - 1) / bs * bs }

// normalize cleans paths, adds implicit parent directories and checks the
// spec for consistency.
func normalize(in []Node) map[string]Node {
	nodes := map[string]Node{}
	for _, n := range in {
		p := path.Clean("/" + n.Path)
		if p == "/" {
			panic(fmt.Sprintf("fstest: node path %q is the root", n.Path))
		}
		if _, dup := nodes[p]; dup {
			panic(fmt.Sprintf("fstest: duplicate node %q", p))
		}
		switch {
		case n.Inline && (n.Dir || n.Link != "" || n.Fragments != 0 || n.Hole):
			panic(fmt.Sprintf("fstest: inline node %q must be a regular file without Fragments or Hole", p))
		case n.Dir && (n.Data != nil || n.Link != "" || n.Fragments != 0 || n.Hole):
			panic(fmt.Sprintf("fstest: directory %q cannot have Data, Link, Fragments or Hole", p))
		case n.Link != "" && (n.Data != nil || n.Fragments != 0 || n.Hole):
			panic(fmt.Sprintf("fstest: symlink %q cannot have Data, Fragments or Hole", p))
		case n.Dir && n.Link != "":
			panic(fmt.Sprintf("fstest: %q is both Dir and Link", p))
		}
		if (n.Freed || len(n.Recover) > 0) && !n.Deleted {
			panic(fmt.Sprintf("fstest: node %q has Freed or Recover but is not Deleted", p))
		}
		for i, rm := range n.Recover {
			if rm.Method == "" {
				panic(fmt.Sprintf("fstest: node %q recover map %d has no Method", p, i))
			}
		}
		n.Path = p
		nodes[p] = n
	}
	for p := range nodes {
		for d := path.Dir(p); d != "/"; d = path.Dir(d) {
			if parent, ok := nodes[d]; ok {
				if !parent.Dir {
					panic(fmt.Sprintf("fstest: %q is under %q, which is not a directory", p, d))
				}
				continue
			}
			nodes[d] = Node{Path: d, Dir: true}
		}
	}
	return nodes
}

func baseRecord(p string, n Node, ids map[string]string) record {
	rec := record{
		ID:        ids[p],
		ParentID:  ids[path.Dir(p)],
		Name:      path.Base(p),
		Deleted:   n.Deleted,
		Freed:     n.Freed,
		Encrypted: n.Encrypted,
		UID:       n.UID,
		GID:       n.GID,
		MTime:     n.MTime,
	}
	perm, typeBits := n.Mode&0o7777, uint32(0o100000)
	switch {
	case n.Dir:
		rec.Type, typeBits = "dir", 0o040000
		if n.Mode == 0 {
			perm = 0o755
		}
	case n.Link != "":
		rec.Type, typeBits, rec.Link, rec.Size = "symlink", 0o120000, n.Link, int64(len(n.Link))
		if n.Mode == 0 {
			perm = 0o777
		}
	default:
		rec.Type, rec.Size = "file", int64(len(n.Data))
		if n.Inline {
			rec.Inline = n.Data
		}
		if n.Mode == 0 {
			perm = 0o644
		}
	}
	rec.Mode = typeBits | perm
	return rec
}

// layout allocates the blocks of a regular file starting at block next and
// returns the first block after it.
func layout(b *built, bs, next int64) int64 {
	n := b.node
	if n.Dir || n.Link != "" || n.Inline {
		return next
	}
	nb := (int64(len(n.Data)) + bs - 1) / bs
	frags := int64(max(n.Fragments, 1))
	if frags > max(nb, 1) {
		panic(fmt.Sprintf("fstest: %q has %d blocks, too few for %d fragments", n.Path, nb, frags))
	}
	if n.Hole && nb < 2 {
		panic(fmt.Sprintf("fstest: %q has %d block(s), a hole needs at least 2", n.Path, nb))
	}
	base, rem := nb/frags, nb%frags
	var lb int64 // logical block number
	for g := range frags {
		if g > 0 {
			next++ // one free block between fragments
		}
		count := base
		if g < rem {
			count++
		}
		for range count {
			if n.Hole && lb == 1 {
				b.runs = append(b.runs, blockRun{start: -1, n: 1})
			} else if last := len(b.runs) - 1; last >= 0 && b.runs[last].start >= 0 && b.runs[last].start+b.runs[last].n == next {
				b.runs[last].n++
				next++
			} else {
				b.runs = append(b.runs, blockRun{start: next, n: 1})
				next++
			}
			lb++
		}
	}
	return next
}

// resolveRecover turns a planted RecoverMap into its table form: explicit runs verbatim, otherwise the
// laid-out runs of the node it borrows from (holes skipped), cut to the claimed size.
func resolveRecover(self Node, rm RecoverMap, nodes map[string]Node, abs map[string][]run) recoverRec {
	src := self
	if rm.RunsOf != "" {
		p := path.Clean("/" + rm.RunsOf)
		n, ok := nodes[p]
		if !ok {
			panic(fmt.Sprintf("fstest: %q: recover map RunsOf %q names no node", self.Path, rm.RunsOf))
		}
		src = n
	}
	size := rm.Size
	if size == 0 {
		size = int64(len(src.Data))
	}
	out := recoverRec{
		Method: rm.Method, Size: size, Basis: rm.Basis, Assumptions: rm.Assumptions,
		Warnings: rm.Warnings, Mode: rm.Mode, Encrypted: rm.Encrypted,
	}
	if len(rm.Runs) > 0 {
		for _, r := range rm.Runs {
			out.Runs = append(out.Runs, run{Offset: r.Offset, Length: r.Length})
		}
		return out
	}
	left := size
	for _, r := range abs[src.Path] {
		if left <= 0 {
			break
		}
		if r.Offset < 0 {
			continue
		}
		take := min(r.Length, left)
		out.Runs = append(out.Runs, run{Offset: r.Offset, Length: take})
		left -= take
	}
	return out
}
