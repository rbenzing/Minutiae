package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// jbd2 constants. Every layout fact here is from memory of include/linux/jbd2.h (3G-R2), as confirmed by the Task 1
// probe for what the kernels write (tools/fixtures/README.md, "ext4 journal probe (3G)").
const (
	jbd2Magic                                                        = 0xC03B3998 // from memory
	jbd2Descriptor, jbd2Commit, jbd2SuperV1, jbd2SuperV2, jbd2Revoke = 1, 2, 3, 4, 5
	jCompatChecksum                                                  = 0x1
	jIncompatRevoke, jIncompat64Bit, jIncompatAsync                  = 0x1, 0x2, 0x4
	jIncompatCsumV2, jIncompatCsumV3, jIncompatFastCommit            = 0x8, 0x10, 0x20
	jIncompatKnown                                                   = jIncompatRevoke | jIncompat64Bit | jIncompatAsync | jIncompatCsumV2 | jIncompatCsumV3 | jIncompatFastCommit

	// maxJournalBytes bounds the journal this reader will index (spec 14); the scan caps below are used by the
	// transaction index (Task 5).
	maxJournalBytes   = 4 << 30
	maxJournalTxns    = 1_000_000
	maxJournalTags    = 1 << 21
	maxJournalRevokes = 1 << 21

	jsuperSize        = 1024 // the jbd2 superblock struct is 1 KiB; s_checksum is its last word
	jsuperCsumOff     = 0xFC
	jCsumTypeCRC32C   = 4 // s_checksum_type
	defaultFCBlocks   = 256
	jsuperTypeOff     = 4
	jsuperBlockSize   = 0xC
	jsuperMaxLen      = 0x10
	jsuperFirst       = 0x14
	jsuperSequence    = 0x18
	jsuperStart       = 0x1C
	jsuperCompat      = 0x24
	jsuperIncompat    = 0x28
	jsuperRoCompat    = 0x2C
	jsuperUUID        = 0x30
	jsuperCsumType    = 0x50
	jsuperNumFCBlocks = 0x54
)

// jsuper is the decoded journal superblock.
type jsuper struct {
	version                                   int
	blockSize, maxLen, first, sequence, start uint32
	compat, incompat, roCompat                uint32
	uuid                                      [16]byte
	numFC                                     uint32 // s_num_fc_blks as stored (0 means the default when fast_commit is on)
	seed                                      uint32 // crc32c of the journal UUID: the checksum seed (v2/v3 only)
	warnings                                  []string
}

func (s *jsuper) csum() bool { return s.incompat&(jIncompatCsumV2|jIncompatCsumV3) != 0 }

// fcBlocks is the size of the fast-commit area at the end of the journal: 0 without the feature, else s_num_fc_blks
// or its default of 256.
func (s *jsuper) fcBlocks() uint32 {
	if s.incompat&jIncompatFastCommit == 0 {
		return 0
	}
	if s.numFC == 0 {
		return defaultFCBlocks
	}
	return s.numFC
}

var jCompatNames = []featureName{{jCompatChecksum, "checksum"}}

var jIncompatNames = []featureName{
	{jIncompatRevoke, "revoke"},
	{jIncompat64Bit, "64bit"},
	{jIncompatAsync, "async_commit"},
	{jIncompatCsumV2, "csum_v2"},
	{jIncompatCsumV3, "csum_v3"},
	{jIncompatFastCommit, "fast_commit"},
}

// featureNames lists the journal features in bit order: compat, then incompat, then ro_compat (no ro_compat bit is
// defined). A bit with no name is listed as "<word>:<hex>". A v1 superblock has no feature words.
func (s *jsuper) featureNames() []string {
	var out []string
	add := func(word uint32, names []featureName, label string) {
		for _, n := range names {
			if word&n.bit != 0 {
				out = append(out, n.name)
				word &^= n.bit
			}
		}
		if word != 0 {
			out = append(out, fmt.Sprintf("%s:%#x", label, word))
		}
	}
	if s.version >= 2 {
		add(s.compat, jCompatNames, "compat")
		add(s.incompat, jIncompatNames, "incompat")
		add(s.roCompat, nil, "ro_compat")
	}
	return out
}

// journalMap maps journal block numbers to filesystem blocks through the runs of the journal inode.
type journalMap struct {
	runs   []filesys.Run // filesystem byte runs in journal order, all data (no hole), each a whole number of blocks
	starts []uint32      // journal block at which each run begins
	blocks uint32        // journal blocks the inode maps
}

// fsBlock returns the filesystem block that holds journal block j, or false when j is outside the mapped journal.
func (m *journalMap) fsBlock(j uint32, bs int64) (uint64, bool) {
	if j >= m.blocks || bs <= 0 {
		return 0, false
	}
	i := sort.Search(len(m.starts), func(i int) bool { return m.starts[i] > j }) - 1
	if i < 0 {
		return 0, false
	}
	return uint64((m.runs[i].Offset + int64(j-m.starts[i])*bs) / bs), true
}

// parseJSuper decodes the journal superblock b (journal block 0) of a filesystem with fsBlockSize-byte blocks (3G-R2).
// Structural damage is a *filesys.CorruptError, a feature the reader cannot honour is filesys.ErrUnsupported, and a
// wrong superblock checksum is a warning in jsuper.warnings. Every count is validated before anything is sized by it.
func parseJSuper(b []byte, fsBlockSize int) (*jsuper, error) {
	const st = "ext4 journal superblock"
	be := binary.BigEndian
	if len(b) < jsuperSize {
		return nil, corrupt(st, 0, "short journal superblock (%d bytes)", len(b))
	}
	if m := be.Uint32(b); m != jbd2Magic {
		return nil, corrupt(st, 0, "bad magic %#08x, want %#08x", m, uint32(jbd2Magic))
	}
	s := &jsuper{
		blockSize: be.Uint32(b[jsuperBlockSize:]),
		maxLen:    be.Uint32(b[jsuperMaxLen:]),
		first:     be.Uint32(b[jsuperFirst:]),
		sequence:  be.Uint32(b[jsuperSequence:]),
		start:     be.Uint32(b[jsuperStart:]),
	}
	switch t := be.Uint32(b[jsuperTypeOff:]); t {
	case jbd2SuperV1:
		s.version = 1
	case jbd2SuperV2:
		s.version = 2
	default:
		return nil, corrupt(st, jsuperTypeOff, "block type %d is not a journal superblock (%d or %d)", t, jbd2SuperV1, jbd2SuperV2)
	}
	if int64(s.blockSize) != int64(fsBlockSize) {
		return nil, corrupt(st, jsuperBlockSize, "s_blocksize %d differs from the filesystem block size %d", s.blockSize, fsBlockSize)
	}
	if s.maxLen == 0 || s.first < 1 || s.first >= s.maxLen {
		return nil, corrupt(st, jsuperFirst, "s_first %d is not in 1..s_maxlen-1 (s_maxlen %d)", s.first, s.maxLen)
	}
	if s.start != 0 && (s.start < s.first || s.start >= s.maxLen) {
		return nil, corrupt(st, jsuperStart, "s_start %d is neither 0 nor in %d..%d", s.start, s.first, s.maxLen-1)
	}
	if s.version == 1 {
		return s, nil
	}
	s.compat = be.Uint32(b[jsuperCompat:])
	s.incompat = be.Uint32(b[jsuperIncompat:])
	s.roCompat = be.Uint32(b[jsuperRoCompat:])
	copy(s.uuid[:], b[jsuperUUID:jsuperUUID+16])
	s.numFC = be.Uint32(b[jsuperNumFCBlocks:])
	if unknown := s.incompat &^ jIncompatKnown; unknown != 0 {
		return nil, fmt.Errorf("ext4 journal: unknown incompatible feature bits %#x: %w", unknown, filesys.ErrUnsupported)
	}
	if s.incompat&jIncompatCsumV2 != 0 && s.incompat&jIncompatCsumV3 != 0 {
		return nil, corrupt(st, jsuperIncompat, "the journal has both checksum v2 and v3")
	}
	if s.csum() {
		if t := b[jsuperCsumType]; t != jCsumTypeCRC32C {
			return nil, fmt.Errorf("ext4 journal: checksum type %d is not crc32c (%d): %w", t, jCsumTypeCRC32C, filesys.ErrUnsupported)
		}
		s.seed = rawCRC32C(0xFFFFFFFF, s.uuid[:])
		var z [jsuperSize]byte
		copy(z[:], b[:jsuperSize])
		clear(z[jsuperCsumOff : jsuperCsumOff+4])
		if stored, want := be.Uint32(b[jsuperCsumOff:]), rawCRC32C(0xFFFFFFFF, z[:]); stored != want {
			s.warnings = append(s.warnings, fmt.Sprintf("journal superblock checksum mismatch: stored %#08x, computed %#08x", stored, want))
		}
	}
	if fc := s.fcBlocks(); fc != 0 && uint64(fc)+uint64(s.first) >= uint64(s.maxLen) {
		return nil, corrupt(st, jsuperNumFCBlocks, "the fast-commit area of %d blocks leaves no room in a journal of %d blocks starting at %d", fc, s.maxLen, s.first)
	}
	return s, nil
}

// journalView returns a private view of f whose warnings only this journal read sees, so a damaged journal inode adds
// no filesystem warning (3G-R8, 3C-R5). newQuiet does not copy the journal caps or the stale-map caps, so they are
// passed here explicitly (Task 4 review), and a failed read in the journal inode's map walk stays an I/O error.
func (f *FS) journalView() *FS {
	q := f.newQuiet()
	q.jrnBytesCap, q.jrnTxnCap, q.jrnTagCap, q.jrnRevokeCap = f.jrnBytesCap, f.jrnTxnCap, f.jrnTagCap, f.jrnRevokeCap
	q.mapRunsCap = f.mapRunsCap
	q.ioRaw = true
	return q
}

// openJournal locates the journal through s_journal_inum and the journal inode's map, reads and decodes its
// superblock and checks that it fits the inode (3G-R2). The caller holds jrnMu. It reads through a private quiet view:
// no filesystem warning is added; findings are in jsuper.warnings. Errors: ErrNoJournal, ErrUnsupported (an external
// journal, an unknown feature), *filesys.CorruptError, an error wrapping ErrLimit, or an I/O error.
func (f *FS) openJournal() (*jsuper, *journalMap, error) {
	const st = "ext4 journal inode"
	f.jrnBuilds++
	sb := f.sb
	if !sb.hasCompat(compatHasJournal) {
		return nil, nil, filesys.ErrNoJournal
	}
	inum := sb.journalInum
	if inum == 0 {
		return nil, nil, fmt.Errorf("ext4: external journal device; the journal is not in this image (s_journal_inum 0): %w", filesys.ErrUnsupported)
	}
	q := f.journalView()
	bs := int64(sb.blockSize)

	off, err := q.inodeOffset(inum)
	if err != nil {
		return nil, nil, err
	}
	raw := make([]byte, sb.inodeSize)
	if err := readFull(q.r, raw, off); err != nil {
		return nil, nil, ioOrCorrupt(st, off, err)
	}
	in, notes, err := q.decodeInodeQuiet(inum, raw)
	if err != nil {
		return nil, nil, err
	}
	warns := notes
	if !in.csumOK {
		warns = append(warns, fmt.Sprintf("the checksum of journal inode %d does not match", inum))
	}
	state, reason, err := q.inodeState(inum)
	if err != nil {
		return nil, nil, err
	}
	switch state {
	case allocFree:
		return nil, nil, corrupt(st, off, "journal inode %d is not allocated in the inode bitmap", inum)
	case allocUnknown:
		warns = append(warns, fmt.Sprintf("whether journal inode %d is allocated cannot be confirmed: %s", inum, reason))
	}
	switch {
	case in.mode&modeTypeMask != modeReg:
		return nil, nil, corrupt(st, off, "journal inode %d is not a regular file (mode %#o)", inum, in.mode)
	case in.links < 1:
		return nil, nil, corrupt(st, off, "journal inode %d has no links", inum)
	case in.flags&inodeFlagInlineData != 0:
		return nil, nil, corrupt(st, off, "journal inode %d stores its data inline", inum)
	case in.size < bs:
		return nil, nil, corrupt(st, off, "journal inode %d is %d bytes, less than one block", inum, in.size)
	case in.size > f.jrnBytesCap:
		return nil, nil, fmt.Errorf("ext4: journal inode %d is %d bytes, more than the %d-byte cap: %w", inum, in.size, f.jrnBytesCap, filesys.ErrLimit)
	}
	runs, err := q.runs(in)
	if err != nil {
		return nil, nil, fmt.Errorf("ext4: journal inode %d: %w", inum, err)
	}
	jm := &journalMap{}
	for _, r := range runs {
		if r.Offset < 0 {
			return nil, nil, corrupt(st, off, "journal inode %d has a hole (or uninitialized extent) at journal block %d", inum, jm.blocks)
		}
		n := r.Length / bs // whole blocks; only the last run can end in a partial block
		if n == 0 {
			continue
		}
		jm.runs = append(jm.runs, r)
		jm.starts = append(jm.starts, jm.blocks)
		jm.blocks += uint32(n) // size <= jrnBytesCap, so the block count fits
	}
	if jm.blocks == 0 {
		return nil, nil, corrupt(st, off, "journal inode %d maps no block", inum)
	}

	blk0, _ := jm.fsBlock(0, bs)
	buf := make([]byte, bs) // one block
	f.jrnBlocks.Add(1)
	if err := readFull(q.r, buf, int64(blk0)*bs); err != nil {
		return nil, nil, ioOrCorrupt("ext4 journal superblock", int64(blk0)*bs, err)
	}
	js, err := parseJSuper(buf, sb.blockSize)
	if err != nil {
		return nil, nil, err
	}
	if js.maxLen > jm.blocks {
		return nil, nil, corrupt("ext4 journal superblock", jsuperMaxLen, "s_maxlen %d exceeds the %d blocks the journal inode maps", js.maxLen, jm.blocks)
	}
	if int64(js.maxLen)*bs > f.jrnBytesCap {
		return nil, nil, fmt.Errorf("ext4: journal of %d blocks is more than the %d-byte cap: %w", js.maxLen, f.jrnBytesCap, filesys.ErrLimit)
	}
	js.warnings = append(append(warns, q.warnings.Snapshot()...), js.warnings...)
	return js, jm, nil
}

// ioOrCorrupt classifies a failed read: a read past the end of the image is a corrupt (cut) structure, any other
// failure is an I/O error and is returned as such (it says nothing about the image and is never cached).
func ioOrCorrupt(structure string, off int64, err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return corrupt(structure, off, "the image ends inside it")
	}
	return err
}
