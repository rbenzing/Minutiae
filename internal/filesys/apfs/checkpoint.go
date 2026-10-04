package apfs

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// checkpoint_map_phys_t / checkpoint_mapping_t layout.
const (
	cpmFlagsOff   = 32
	cpmCountOff   = 36
	cpmEntriesOff = 40
	cpmEntrySize  = 40
	cpmLast       = 0x1 // CHECKPOINT_MAP_LAST

	// maxMappings bounds the checkpoint-map entries one checkpoint may hold
	// (a real container maps a handful of ephemeral objects).
	maxMappings = 1 << 16
	// maxCandidates bounds how many checkpoint superblocks are tried.
	maxCandidates = 256
	// maxEphemeralBytes bounds the ephemeral object bytes verified in total
	// while selecting a checkpoint, and maxMapBlocks the checkpoint-map blocks
	// read (a real container has a handful of each; the budgets only stop a
	// forged descriptor ring from making Open read the whole image over and
	// over).
	maxEphemeralBytes = 256 << 20
	maxMapBlocks      = 1 << 14
)

// mapping is one checkpoint_mapping_t: where an ephemeral object lives.
type mapping struct {
	typ, subtype, size uint32
	fsOid, oid, paddr  uint64
}

// checkpoint is the selected checkpoint: its position in the descriptor ring
// and its ephemeral-object map (oid to location in the data area).
type checkpoint struct {
	index     int // ring index of the superblock
	ephemeral map[uint64]mapping
}

// selBudget is what is left of the work Open may spend on checkpoint
// candidates in total.
type selBudget struct {
	maps int   // checkpoint-map blocks
	eph  int64 // ephemeral object bytes
}

// candidate is a superblock-looking block of the descriptor area.
type candidate struct {
	idx int
	xid uint64
}

// selectCheckpoint finds the newest valid checkpoint. It scans the descriptor
// area header by header, takes every NX_SUPERBLOCK object as a candidate and
// tries them by descending xid: a candidate is accepted when its checksum,
// geometry, mapping blocks and ephemeral objects (spaceman) all verify.
// Rejected candidates newer than the accepted one are reported as warnings.
// No candidate is a *filesys.CorruptError. nx0 is block 0: it supplies the
// block size and the area location and nothing else.
func (f *FS) selectCheckpoint(nx0 *nxSuper) (nxSuper, checkpoint, error) {
	bud := &selBudget{maps: maxMapBlocks, eph: maxEphemeralBytes}
	var cands []candidate
	var hdr [40]byte
	for i := range int(nx0.descBlocks) {
		off := int64(nx0.descBase+uint64(i)) * int64(f.bs)
		if err := readFull(f.data, hdr[:], off); err != nil {
			if isShort(err) {
				break // the image ends here, and so does everything after it
			}
			return nxSuper{}, checkpoint{}, fmt.Errorf("apfs: scan descriptor area: %w", err)
		}
		if h := parseHeader(hdr[:]); h.kind() == typeNXSuperblock && string(hdr[32:36]) == nxMagic {
			cands = append(cands, candidate{idx: i, xid: h.xid})
		}
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		switch {
		case a.xid > b.xid:
			return -1
		case a.xid < b.xid:
			return 1
		}
		return a.idx - b.idx
	})
	if len(cands) > maxCandidates {
		f.warn("descriptor area holds %d checkpoint superblocks; only the %d newest are considered", len(cands), maxCandidates)
		cands = cands[:maxCandidates]
	}
	for _, c := range cands {
		nx, cp, reason, err := f.tryCandidate(nx0, c, bud)
		if err != nil {
			return nxSuper{}, checkpoint{}, err
		}
		if reason == "" {
			return nx, cp, nil
		}
		f.warn("checkpoint xid %d (descriptor ring block %d) rejected: %s", c.xid, c.idx, reason)
	}
	return nxSuper{}, checkpoint{}, corrupt("checkpoint descriptor area", int64(nx0.descBase)*int64(f.bs),
		"no valid checkpoint among %d superblock candidates in %d blocks", len(cands), nx0.descBlocks)
}

// tryCandidate validates one checkpoint. A non-empty reason rejects it; a
// non-nil error is a read failure (not corruption) and aborts the open.
func (f *FS) tryCandidate(nx0 *nxSuper, c candidate, bud *selBudget) (nxSuper, checkpoint, string, error) {
	fail := func(format string, a ...any) (nxSuper, checkpoint, string, error) {
		return nxSuper{}, checkpoint{}, fmt.Sprintf(format, a...), nil
	}
	// readOne reads one verified object; ok is false (with a reason) for
	// corruption and a short read, err non-nil for a real I/O failure.
	readOne := func(label string, paddr uint64, size int, want uint32) ([]byte, objHeader, string, error) {
		b, h, err := f.readObject(paddr, size, want)
		switch {
		case err == nil:
			return b, h, "", nil
		case isShort(err):
			return nil, h, label + " is unreadable (truncated image)", nil
		case errors.Is(err, errChecksum):
			return nil, h, label + ": checksum mismatch", nil
		case errors.Is(err, filesys.ErrCorrupt):
			return nil, h, label + ": " + err.Error(), nil
		}
		return nil, h, "", err
	}

	sbAddr := nx0.descBase + uint64(c.idx)
	buf, h, reason, err := readOne("superblock", sbAddr, f.bs, typeNXSuperblock)
	if err != nil || reason != "" {
		return nxSuper{}, checkpoint{}, reason, err
	}
	nx := parseNX(buf)
	nx.xid = h.xid
	if err := nx.validate(); err != nil {
		return fail("%v", err)
	}
	if nx.blockSize != nx0.blockSize || nx.descBase != nx0.descBase || nx.descBlocks != nx0.descBlocks {
		return fail("its descriptor area (block %d, %d blocks, block size %d) differs from block 0's (block %d, %d blocks, block size %d)",
			nx.descBase, nx.descBlocks, nx.blockSize, nx0.descBase, nx0.descBlocks, nx0.blockSize)
	}

	// The superblock's own cursor claim should agree with where it sits: the
	// checkpoint occupies descLen ring blocks starting at descIndex and its
	// superblock is the last of them. That reading is verified only against
	// single-checkpoint containers, so a disagreement is a warning, not a reason
	// to reject a checkpoint whose checksums, xids and objects all verify.
	if pos := (int64(nx.descIndex) + int64(nx.descLen) - 1) % int64(nx.descBlocks); pos != int64(c.idx) {
		f.warn("checkpoint xid %d: its descriptor cursor (index %d, length %d) puts the superblock at ring index %d, not %d; accepted (the cursor reading is unverified)", nx.xid, nx.descIndex, nx.descLen, pos, c.idx)
	}

	// The descLen-1 blocks immediately before the superblock in the ring are
	// its checkpoint-mapping blocks (the ring may wrap).
	nmaps := int(nx.descLen) - 1
	if nmaps < 1 {
		return fail("descriptor length %d leaves no checkpoint-map block", nx.descLen)
	}
	if bud.maps -= nmaps; bud.maps < 0 {
		return fail("checkpoint-map read budget of %d blocks exhausted (descriptor length %d)", maxMapBlocks, nx.descLen)
	}
	ring := int(nx.descBlocks)
	eph := map[uint64]mapping{}
	total := 0
	for j := range nmaps {
		pos := ((c.idx-nmaps+j)%ring + ring) % ring
		addr := nx.descBase + uint64(pos)
		label := fmt.Sprintf("checkpoint-map block at ring index %d", pos)
		mb, mh, reason, err := readOne(label, addr, f.bs, typeCheckpointMap)
		if err != nil || reason != "" {
			return nxSuper{}, checkpoint{}, reason, err
		}
		if mh.xid != nx.xid {
			return fail("%s has xid %d, want %d", label, mh.xid, nx.xid)
		}
		flags := le.Uint32(mb[cpmFlagsOff:])
		isLast := j == nmaps-1
		switch {
		case isLast && flags&cpmLast == 0:
			return fail("the last %s lacks CHECKPOINT_MAP_LAST", label)
		case !isLast && flags&cpmLast != 0:
			f.warn("checkpoint xid %d: %s is flagged CHECKPOINT_MAP_LAST but is not the last of its checkpoint", nx.xid, label)
		}
		count := le.Uint32(mb[cpmCountOff:])
		if need, ok := filesys.MulOK(int64(count), cpmEntrySize); !ok || need > int64(f.bs-cpmEntriesOff) {
			return fail("%s claims %d mappings, more than fit the block", label, count)
		}
		if total += int(count); total > maxMappings {
			return fail("the checkpoint holds more than %d mappings", maxMappings)
		}
		for i := range int(count) {
			e := mb[cpmEntriesOff+i*cpmEntrySize:]
			m := mapping{
				typ: le.Uint32(e[0:]), subtype: le.Uint32(e[4:]), size: le.Uint32(e[8:]),
				fsOid: le.Uint64(e[16:]), oid: le.Uint64(e[24:]), paddr: le.Uint64(e[32:]),
			}
			reject, note := nx.checkMapping(m, f.bs)
			if reject != "" {
				return fail("%s", reject)
			}
			if note != "" {
				f.warn("checkpoint xid %d: %s; accepted (the data-range reading is unverified)", nx.xid, note)
			}
			if _, dup := eph[m.oid]; dup {
				return fail("ephemeral object oid %d is mapped twice", m.oid)
			}
			eph[m.oid] = m
		}
	}

	// Every ephemeral object must be readable with a valid checksum; the
	// spaceman must be among them.
	sm, ok := eph[nx.spacemanOid]
	if !ok || sm.typ&typeMask != typeSpaceman {
		return fail("no spaceman mapping for oid %d", nx.spacemanOid)
	}
	for _, oid := range sortedKeys(eph) {
		m := eph[oid]
		if bud.eph -= int64(m.size); bud.eph < 0 {
			return fail("ephemeral objects exceed %d bytes", int64(maxEphemeralBytes))
		}
		label := fmt.Sprintf("ephemeral object oid %d", m.oid)
		if m.oid == nx.spacemanOid {
			label = "spaceman"
		}
		_, oh, reason, err := readOne(label, m.paddr, int(m.size), m.typ&typeMask)
		if err != nil || reason != "" {
			return nxSuper{}, checkpoint{}, reason, err
		}
		if oh.oid != m.oid {
			return fail("%s at block %d carries oid %d, want %d", label, m.paddr, oh.oid, m.oid)
		}
	}
	return nx, checkpoint{index: c.idx, ephemeral: eph}, "", nil
}

// checkMapping validates one checkpoint_mapping_t against the data area: reject
// is a reason to reject the checkpoint, note a finding that is only reported.
func (n *nxSuper) checkMapping(m mapping, bs int) (reject, note string) {
	if m.size == 0 || m.size > maxObjectBytes || int(m.size)%bs != 0 {
		return fmt.Sprintf("mapping of oid %d has unusable size %d", m.oid, m.size), ""
	}
	nblk := uint64(m.size) / uint64(bs)
	end, ok := addU64(m.paddr, nblk)
	if !ok || m.paddr < n.dataBase || end > n.dataBase+uint64(n.dataBlocks) {
		return fmt.Sprintf("mapping of oid %d (block %d, %d blocks) lies outside the data area [%d, +%d)", m.oid, m.paddr, nblk, n.dataBase, n.dataBlocks), ""
	}
	// The object should lie inside the live part of the data ring, [dataIndex,
	// dataIndex+dataLen) modulo the area. Treating the data area as a ring
	// follows the descriptor area; it is unverified against a multi-checkpoint
	// container (mkapfs writes index 0 and never wraps), so only a note.
	start := (m.paddr - n.dataBase + uint64(n.dataBlocks) - uint64(n.dataIndex)) % uint64(n.dataBlocks)
	if start+nblk > uint64(n.dataLen) {
		return "", fmt.Sprintf("mapping of oid %d (block %d, %d blocks) lies outside the checkpoint's data range (index %d, length %d)", m.oid, m.paddr, nblk, n.dataIndex, n.dataLen)
	}
	return "", ""
}

func sortedKeys(m map[uint64]mapping) []uint64 {
	ks := make([]uint64, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
