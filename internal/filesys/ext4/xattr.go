package ext4

import (
	"encoding/binary"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	xattrMagic      = 0xEA020000
	xattrHeaderSize = 32   // header of an xattr block
	xattrEntrySize  = 16   // fixed part of an entry, before the name
	xattrCsumOff    = 0x10 // h_checksum in the header of an xattr block
)

// xattr is one extended attribute. Inum is non-zero when the value lives in a
// separate EA inode (ea_inode feature); Value is then nil and the value is read through eaInodeValue.
type xattr struct {
	Name  string
	Value []byte
	Inum  uint32
	// ValueSize is e_value_size, kept only for a value held in an EA inode (Inum != 0): it is what the EA inode's
	// i_size must equal, and nothing is ever sized from it.
	ValueSize uint32
}

// xattrPrefix maps e_name_index to the name prefix.
func xattrPrefix(index byte) string {
	switch index {
	case 1:
		return "user."
	case 2:
		return "system.posix_acl_access"
	case 3:
		return "system.posix_acl_default"
	case 4:
		return "trusted."
	case 6:
		return "security."
	case 7:
		return "system."
	case 8:
		return "system.richacl"
	case 9:
		return "encryption."
	}
	return "index" + strconv.Itoa(int(index)) + "."
}

// xattrs returns the extended attributes of in: the in-inode area first, then
// the block named by i_file_acl. On a corrupt table it returns the attributes
// read before the damage together with a *filesys.CorruptError.
func (f *FS) xattrs(in *inode) ([]xattr, error) {
	xs, _, err := f.xattrsChecked(in)
	return xs, err
}

// xattrsChecked is xattrs that also reports whether the xattr block failed its checksum. The attributes of
// such a block are still returned (a checksum that fails is shown, never hidden), and a warning names the
// block. The in-inode area has no checksum of its own: the inode checksum covers it.
func (f *FS) xattrsChecked(in *inode) ([]xattr, bool, error) {
	var out []xattr
	var firstErr error
	csumBad := false
	if !in.extraBad && in.extraIsize+4 <= len(in.extra) {
		area := in.extra[in.extraIsize:]
		if binary.LittleEndian.Uint32(area) == xattrMagic {
			// Value offsets are relative to the first entry, right after the magic.
			got, err := parseXattrs(area[4:], 0)
			if ce, ok := err.(*filesys.CorruptError); ok {
				ce.Offset += in.offset + int64(goodOldInodeSz+in.extraIsize+4) // absolute image offset
			}
			out = append(out, got...)
			firstErr = err
		}
	}
	if in.fileACL != 0 {
		got, bad, err := f.xattrBlock(in.fileACL)
		csumBad = bad
		out = append(out, got...)
		if firstErr == nil {
			firstErr = err
		}
	}
	return out, csumBad, firstErr
}

// xattrBlock reads and parses the extended attribute block blk. Under metadata_csum with the crc32c type the
// block checksum is verified: a mismatch is a warning (deduplicated by text, so a block shared by many inodes
// warns once) and csumBad, and the attributes are still returned. Without metadata_csum, or with another
// checksum type, h_checksum is not a checksum this reader can judge, so no verdict is made.
func (f *FS) xattrBlock(blk uint64) ([]xattr, bool, error) {
	const st = "ext4 xattr block"
	if blk < uint64(f.sb.firstDataBlock) || blk >= uint64(f.sb.blocksCount) {
		return nil, false, corrupt(st, -1, "i_file_acl block %d is outside the %d-block filesystem", blk, f.sb.blocksCount)
	}
	off, ok := filesys.MulOK(int64(blk), int64(f.sb.blockSize))
	if !ok {
		return nil, false, corrupt(st, -1, "block %d offset overflows", blk)
	}
	buf := make([]byte, f.sb.blockSize) // one block: bounded by the block size
	if err := readFull(f.r, buf, off); err != nil {
		return nil, false, corrupt(st, off, "read failed: %v", err)
	}
	if m := binary.LittleEndian.Uint32(buf); m != xattrMagic {
		return nil, false, corrupt(st, off, "bad magic %#08x, want %#08x", m, uint32(xattrMagic))
	}
	csumBad := false
	if f.sb.metadataCsum() && f.sb.checksumType == csumTypeCRC32C {
		stored := binary.LittleEndian.Uint32(buf[xattrCsumOff:])
		if want := f.xattrBlockCsum(blk, buf); stored != want {
			csumBad = true
			f.warn("xattr block %d: checksum mismatch (stored %#08x, computed %#08x)", blk, stored, want)
		}
	}
	got, err := parseXattrs(buf, xattrHeaderSize)
	if ce, ok := err.(*filesys.CorruptError); ok {
		ce.Offset += off
	}
	return got, csumBad, err
}

// xattrBlockCsum is the kernel's ext4_xattr_block_csum: crc32c with the filesystem seed over the little-endian
// 64-bit block number, then the block with its h_checksum field read as zero.
func (f *FS) xattrBlockCsum(blk uint64, buf []byte) uint32 {
	var nr [8]byte
	binary.LittleEndian.PutUint64(nr[:], blk)
	var zero [4]byte
	c := rawCRC32C(f.sb.csumSeed, nr[:])
	c = rawCRC32C(c, buf[:xattrCsumOff])
	c = rawCRC32C(c, zero[:])
	return rawCRC32C(c, buf[xattrCsumOff+4:])
}

// parseXattrs parses the entry table of an xattr container: region holds the
// entries starting at entriesOff, and value offsets are relative to the start
// of region. The table ends with a zero 32-bit word (or the end of region).
// Every entry consumes at least xattrEntrySize bytes, so the loop terminates;
// entries read before a bad one are returned with the error.
func parseXattrs(region []byte, entriesOff int) ([]xattr, error) {
	const st = "ext4 xattr table"
	le := binary.LittleEndian
	if entriesOff < 0 || entriesOff > len(region) {
		return nil, corrupt(st, int64(entriesOff), "entry table starts outside the %d-byte area", len(region))
	}
	var out []xattr
	pos := entriesOff
	var total uint64 // value bytes copied so far
	for pos+4 <= len(region) && le.Uint32(region[pos:]) != 0 {
		if pos+xattrEntrySize > len(region) {
			return out, corrupt(st, int64(pos), "entry header runs past the %d-byte area", len(region))
		}
		nameLen := int(region[pos])
		index := region[pos+1]
		valueOff := uint64(le.Uint16(region[pos+2:]))
		inum := le.Uint32(region[pos+4:])
		valueSize := uint64(le.Uint32(region[pos+8:]))
		nameEnd := pos + xattrEntrySize + nameLen
		if nameEnd > len(region) {
			return out, corrupt(st, int64(pos), "name of %d bytes runs past the %d-byte area", nameLen, len(region))
		}
		x := xattr{Name: xattrPrefix(index) + string(region[pos+xattrEntrySize:nameEnd]), Inum: inum}
		switch {
		case inum != 0:
			// Value stored in an EA inode: read through eaInodeValue, not here.
			x.ValueSize = uint32(valueSize)
		case valueSize > 0:
			if valueOff+valueSize > uint64(len(region)) {
				return out, corrupt(st, int64(pos), "value of %d bytes at offset %d lies outside the %d-byte area", valueSize, valueOff, len(region))
			}
			if total += valueSize; total > uint64(len(region)) {
				return out, corrupt(st, int64(pos), "values of overlapping entries total more than the %d-byte area", len(region))
			}
			x.Value = append([]byte(nil), region[valueOff:valueOff+valueSize]...)
		}
		out = append(out, x)
		pos += (xattrEntrySize + nameLen + 3) &^ 3
	}
	return out, nil
}
