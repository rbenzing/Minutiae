package f2fs

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// MethodNodeScan is the recovery method of this reader: the block map of a
// deleted file, read from the stale inode and node blocks the kernel leaves in
// the free node segments (spec 6.5; 3D).
const MethodNodeScan = "f2fs-node-scan"

var _ filesys.Recoverer = (*FS)(nil)

// Recoverable returns where the content of the deleted entry e may still be.
// Only e.ID is used, and everything is re-resolved from the volume.
//
// The ID of a deleted directory slot is "dentry:<dir>:<blk>:<slot>"; a live
// "nid:<n>" is ErrNotDeleted. The steps (3D-R14, R8, R2-R4):
//
//  1. the one dentry slot is read again from disk (listing rules);
//  2. the CURRENT NAT must not map the inode number: if it does the number is
//     live (a reuse, an orphan, a rename's stale slot, a hard link) and the
//     answer is filesys.ErrReused;
//  3. the stale copies of the inode number are looked up in the footer index;
//  4. the copy whose identity is proven by structure is chosen (chooseInode);
//  5. its block map is walked through the same index, and ends at the first
//     hole, foreign or missing node, tie or bad address (a trusted prefix; the
//     stop token is passed on verbatim as "stale-chain-stop=<reason>@<n>").
//
// A free dentry slot that names inode 0 or an inode beyond the NAT, or has an
// impossible name or type, is damaged, not a deleted record: the listing
// offers no ID for it, so asking for it is an unlisted slot (ErrNotFound) and
// never a candidate (the same rule as the listing, which skips such a slot
// without a warning).
//
// No byte is read, filled or guessed here: the result is a map. A scan that
// cannot decide is ErrUndecided or ErrLimit, never ErrNotFound.
func (f *FS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	if strings.HasPrefix(e.ID, "nid:") {
		return nil, f.liveNID(e.ID)
	}
	dirNID, blk, slot, ok := parseDentryLoc(e.ID)
	sb := f.sb
	if !ok || slot >= nrDentryInBlock || dirNID == 0 || dirNID == sb.nodeIno || dirNID == sb.metaIno || uint64(dirNID) >= sb.natCapacity() {
		return nil, fmt.Errorf("%w: %q is not the ID of a deleted directory slot of this volume", filesys.ErrNotFound, e.ID)
	}
	dir, d, err := f.resolveSlot(e.ID, dirNID, blk, slot)
	if err != nil {
		return nil, err
	}
	switch {
	case d.ftype == 2:
		return nil, fmt.Errorf("%w: %q names a directory; the content of a deleted directory is not recovered", filesys.ErrUnsupported, e.ID)
	case d.ftype != 1 && d.ftype != 7:
		return nil, fmt.Errorf("%w: %q names a special file (file_type %d)", filesys.ErrUnsupported, e.ID, d.ftype)
	case d.ino == sb.nodeIno || d.ino == sb.metaIno:
		return nil, fmt.Errorf("%w: %q names the reserved inode %d", filesys.ErrNotFound, e.ID, d.ino)
	}
	ent, err := f.natGet(d.ino)
	switch {
	case err == nil:
		return nil, fmt.Errorf("f2fs: %w: inode %d is live (NAT %d)", filesys.ErrReused, d.ino, ent.addr)
	case !errors.Is(err, filesys.ErrNotFound):
		return nil, err
	}
	below, ok := f.cp.verLow()
	if !ok {
		return nil, fmt.Errorf("%w: checkpoint version beyond 32 bits (%#x), footer versions cannot be compared", filesys.ErrUnsupported, f.cp.ver)
	}
	idx, err := f.nodeIndex()
	if err != nil {
		return nil, err
	}
	copies, err := f.chooseInode(idx, dirNID, d)
	if err != nil {
		return nil, err
	}
	return []filesys.Candidate{f.candidate(e.ID, dir, d, copies, below)}, nil
}

// liveNID answers for an "nid:<n>" ID: a live inode is not deleted, anything
// else is not found. The ID is checked before any read.
func (f *FS) liveNID(id string) error {
	n, err := parseNodeID(id)
	if err != nil {
		return err
	}
	if n == f.sb.nodeIno || n == f.sb.metaIno || uint64(n) >= f.sb.natCapacity() {
		return fmt.Errorf("%w: node id %d is not an inode of this volume", filesys.ErrNotFound, n)
	}
	switch _, err := f.natGet(n); {
	case err == nil:
		return filesys.ErrNotDeleted
	case errors.Is(err, filesys.ErrNotFound):
		return fmt.Errorf("%w: node id %d is free", filesys.ErrNotFound, n)
	default:
		return err
	}
}

// parseDentryLoc splits a canonical "dentry:<dir>:<blk>:<slot>" ID (decimal,
// no sign, no leading zero, each within uint32). parseDentryID stays the
// yes/no form used by Open.
func parseDentryLoc(id string) (dir uint32, blk int64, slot int, ok bool) {
	rest, ok := strings.CutPrefix(id, "dentry:")
	if !ok {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var v [3]uint64
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != p {
			return 0, 0, 0, false
		}
		v[i] = n
	}
	return uint32(v[0]), int64(v[1]), int(v[2]), true
}

// resolveSlot re-reads the one dentry area the ID names, with the rules of
// the listing (scanArea), and returns the deleted record in slot (3D-R14). The
// block is not charged to the listing budget and does not consult the block
// claims: both are listing state that depends on order.
func (f *FS) resolveSlot(id string, dirNID uint32, blk int64, slot int) (*inode, *dentry, error) {
	dir, err := f.inode(dirNID)
	if err != nil {
		var ni *notInodeError
		if errors.As(err, &ni) {
			return nil, nil, fmt.Errorf("%w: %q: node %d is not an inode", filesys.ErrNotFound, id, dirNID)
		}
		return nil, nil, err
	}
	if dir.typ() != modeDir {
		return nil, nil, fmt.Errorf("%w: %q: inode %d is not a directory", filesys.ErrNotFound, id, dirNID)
	}
	a, first, err := f.areaAt(dir, blk)
	if err != nil {
		return nil, nil, err
	}
	var found *dentry
	w := &dirScan{f: f, dir: dir, enc: dir.encrypted, wantDeleted: true}
	w.visit = func(r *dentry) bool {
		if r.slot != slot {
			return true
		}
		c := *r
		c.name = slices.Clone(r.name) // r.name aliases the area buffer
		found = &c
		return false
	}
	w.scanArea(a, blk, first)
	switch {
	case found != nil && found.deleted:
		return dir, found, nil
	case found != nil || (slot < a.max && a.live(slot)):
		return nil, nil, fmt.Errorf("%w: slot %d of block %d of directory %d is a live record", filesys.ErrNotDeleted, slot, blk, dirNID)
	}
	return nil, nil, fmt.Errorf("%w: slot %d of block %d of directory %d holds no deleted record", filesys.ErrNotFound, slot, blk, dirNID)
}

// staleCopy is a proven stale copy of the deleted file's inode with the block
// map walked from it.
type staleCopy struct {
	node  staleNode
	in    *inode
	res   staleResult
	total int    // copies of the inode number in the index
	note  string // the warning about copies not used (empty when none)
}

// rejects counts the stale copies not used, by the first check they failed.
type rejects struct{ footer, ofs, version, layout, typ, parent, name int }

func (r rejects) sum() int {
	return r.footer + r.ofs + r.version + r.layout + r.typ + r.parent + r.name
}

func (r rejects) String() string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{r.footer, "footer"}, {r.ofs, "offset"}, {r.version, "version"}, {r.layout, "layout or checksum"}, {r.typ, "type"}, {r.parent, "parent"}, {r.name, "name"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p.what, p.n))
		}
	}
	return strings.Join(parts, ", ")
}

// treeBlocks is the most blocks the node tree of an inode can address: its
// own address slots, two direct nodes, two indirect nodes and one
// double-indirect node.
func treeBlocks(in *inode) int64 {
	const a = int64(addrsPerBlock)
	return int64(in.addrSlots) + 2*a + 2*a*a + a*a*a
}

// chooseInode picks the stale copy of inode d.ino that is the deleted
// file's inode, by structure (3D-R2): the footer names the inode number, the
// block is an inode (offset 0) that parses and whose checksum holds, i_pino is
// the dentry's directory, i_name is the dentry's name, the type matches the
// dentry, and the footer version is below the checkpoint's. Of those the
// highest version wins (3D-R4); copies tied at it are all returned when their
// maps are identical, else the answer is ErrUndecided. A copy that matches by
// name and parent but is damaged is never presented as absent (3D-R13), and
// nothing qualifying while the per-inode cap evicted copies is ErrUndecided
// (3D-R19).
func (f *FS) chooseInode(idx *nodeIndex, dirNID uint32, d *dentry) ([]staleCopy, error) {
	below, ok := f.cp.verLow()
	if !ok {
		return nil, fmt.Errorf("%w: checkpoint version beyond 32 bits (%#x)", filesys.ErrUnsupported, f.cp.ver)
	}
	copies, meta, err := idx.lookup(d.ino)
	if err != nil {
		return nil, err
	}
	var rej rejects
	var damaged []uint32 // copies that match by name and parent but fail the layout or the checksum
	type cand struct {
		node staleNode
		in   *inode
	}
	var qual []cand
	for _, c := range copies {
		switch {
		case c.nid != d.ino || c.ino != d.ino:
			rej.footer++
			continue
		case c.ofs != 0:
			rej.ofs++
			continue
		case c.ver >= below:
			rej.version++
			continue
		}
		raw, err := readBlock(f.data, c.addr)
		if err != nil {
			return nil, readError("f2fs stale inode", int64(c.addr)*blockSize, err)
		}
		in, perr := f.parseInodeBlock(raw, d.ino)
		if perr == nil && in.csumChecked && !in.csumOK {
			perr = corrupt("f2fs stale inode", int64(c.addr)*blockSize, "inode %d: checksum mismatch", d.ino)
		}
		if perr != nil {
			if rawIdentity(raw, dirNID, d.name) {
				damaged = append(damaged, c.addr)
			} else {
				rej.layout++
			}
			continue
		}
		switch {
		case in.typ() != modeReg && d.ftype == 1, in.typ() != modeSymlink && d.ftype == 7:
			rej.typ++
		case in.pino != dirNID:
			rej.parent++
		case in.nameBad || !bytes.Equal(in.name, d.name):
			rej.name++
		default:
			qual = append(qual, cand{c, in})
		}
	}
	note := ""
	if rej.sum()+len(damaged) > 0 || meta.evicted {
		note = fmt.Sprintf("inode %d: %d of %d stale copies not used (%s", d.ino, rej.sum()+len(damaged), len(copies), rej)
		if len(damaged) > 0 {
			note += fmt.Sprintf("; %d damaged but named like this entry", len(damaged))
		}
		note += ")"
		if meta.evicted {
			note += "; the per-inode cap evicted older copies"
		}
	}
	if len(qual) == 0 {
		return nil, noCopyError(d, copies, meta, rej, damaged)
	}
	// The highest version wins; copies tied at it are examined together.
	top := qual[0].node.ver
	var group []cand
	for _, q := range qual {
		if q.node.ver == top {
			group = append(group, q)
		}
	}
	slices.SortFunc(group, func(a, b cand) int { return cmp.Compare(a.node.addr, b.node.addr) })
	src := &indexSource{f: f, idx: idx, below: below}
	out := make([]staleCopy, 0, len(group))
	for _, g := range group {
		in := g.in
		switch {
		case in.inline&inlineData != 0:
			return nil, fmt.Errorf("%w: inline data: reported, not recovered (inode %d)", filesys.ErrUnsupported, d.ino)
		case in.compressed:
			return nil, fmt.Errorf("%w: compressed file, reported, not recovered (inode %d)", filesys.ErrUnsupported, d.ino)
		case in.size > treeBlocks(in)*blockSize:
			return nil, corrupt("f2fs stale inode", int64(g.node.addr)*blockSize, "inode %d: stale size %d is beyond what a node tree can address", d.ino, in.size)
		case in.size > f.size:
			return nil, corrupt("f2fs stale inode", int64(g.node.addr)*blockSize, "inode %d: stale size %d is larger than the %d-byte volume", d.ino, in.size, f.size)
		}
		res, err := f.staleMap(in, src)
		if err != nil {
			return nil, err
		}
		out = append(out, staleCopy{node: g.node, in: in, res: res, total: len(copies), note: note})
	}
	for _, o := range out[1:] {
		if o.in.size != out[0].in.size || o.res.stop != out[0].res.stop || !slices.Equal(o.res.runs, out[0].res.runs) {
			var blocks []string
			for _, c := range out {
				blocks = append(blocks, fmt.Sprintf("0x%x", c.node.addr))
			}
			return nil, fmt.Errorf("f2fs: %w: inode %d has %d copies at version %d (blocks %s) whose block maps differ", filesys.ErrUndecided, d.ino, len(out), top, strings.Join(blocks, ", "))
		}
	}
	return out, nil
}

// noCopyError is the answer when no stale copy qualifies. Unknown is never
// absent: evicted copies make it undecided, and a damaged copy that names
// this entry makes it a corruption, before "not found" is decided.
func noCopyError(d *dentry, copies []staleNode, meta nidMeta, rej rejects, damaged []uint32) error {
	switch {
	case meta.evicted:
		return fmt.Errorf("f2fs: %w: copies of inode %d were evicted by the per-inode cap of %d; a qualifying one may have been among them", filesys.ErrUndecided, d.ino, maxCopiesPerNID)
	case len(damaged) > 0:
		var blocks []string
		for _, a := range damaged {
			blocks = append(blocks, fmt.Sprintf("0x%x", a))
		}
		return corrupt("f2fs stale inode", int64(damaged[0])*blockSize, "inode %d: %d stale copies name this entry but fail the layout or checksum (blocks %s)", d.ino, len(damaged), strings.Join(blocks, ", "))
	case len(copies) == 0 && meta.postCheckpoint == 0:
		return fmt.Errorf("%w: no stale inode node of inode %d", filesys.ErrNotFound, d.ino)
	}
	text := fmt.Sprintf("no stale copy of inode %d qualifies for this entry (%d copies", d.ino, len(copies))
	if rej.sum() > 0 {
		text += ": " + rej.String()
	}
	text += ")"
	if meta.postCheckpoint > 0 {
		text += fmt.Sprintf("; %d post-checkpoint copies were not used", meta.postCheckpoint)
	}
	return fmt.Errorf("%w: %s", filesys.ErrNotFound, text)
}

// rawIdentity reports whether the inode block raw carries the parent and name
// of the dentry, read straight from the bytes (the block may not parse).
func rawIdentity(raw []byte, dirNID uint32, name []byte) bool {
	if len(raw) != blockSize || binary.LittleEndian.Uint32(raw[iPino:]) != dirNID {
		return false
	}
	nl := binary.LittleEndian.Uint32(raw[iNameLen:])
	return nl <= fNameLen && bytes.Equal(raw[iName:iName+int(nl)], name)
}

// candidate builds the claim from the chosen copies (all tied copies have the
// same map; the first one speaks). Slices are fresh on every call.
func (f *FS) candidate(id string, dir *inode, d *dentry, copies []staleCopy, below uint32) filesys.Candidate {
	c0 := copies[0]
	in := c0.in
	basis := []string{id, fmt.Sprintf("inode=%d", d.ino)}
	prevMatch := false
	for _, c := range copies {
		basis = append(basis, fmt.Sprintf("node:nid=%d:blk=0x%x", d.ino, c.node.addr))
		if prev, err := f.natPrevAddr(d.ino); err == nil && prev == c.node.addr {
			prevMatch = true
		}
	}
	basis = append(basis, fmt.Sprintf("cp_ver=%d", c0.node.ver), fmt.Sprintf("checkpoint=%d", below), "inode-parent=match", "inode-name=match")
	if in.csumChecked {
		basis = append(basis, "inode-checksum=ok")
	} else {
		basis = append(basis, "inode-checksum=not-recorded")
	}
	switch {
	case c0.node.ssa:
		basis = append(basis, "ssa=match")
	case c0.node.why == whySSACurrent:
		basis = append(basis, "ssa=current-segment")
	default:
		basis = append(basis, "ssa=unreadable")
	}
	if prevMatch {
		basis = append(basis, "nat-prev-copy=match")
	}
	basis = append(basis, fmt.Sprintf("node-copies=%d", c0.total), fmt.Sprintf("chain-nodes=%d", len(c0.res.nodes)))
	assumptions := []string{"newest-stale-copy-before-checkpoint"}
	if c0.res.stop != "" {
		assumptions = append(assumptions, "stale-chain-stop="+c0.res.stop)
	}
	var warnings []string
	if c0.note != "" {
		warnings = []string{c0.note}
	}
	return filesys.Candidate{
		Method:      MethodNodeScan,
		Size:        in.size,
		Runs:        slices.Clone(c0.res.runs),
		Basis:       basis,
		Assumptions: assumptions,
		Times:       in.times,
		Mode:        uint32(in.mode),
		Encrypted:   in.encrypted || dir.encrypted,
		Warnings:    warnings,
	}
}
