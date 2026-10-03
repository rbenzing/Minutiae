package ext4

import (
	"encoding/binary"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	xattrMagic      = 0xEA020000
	xattrHeaderSize = 32 // header of an xattr block
	xattrEntrySize  = 16 // fixed part of an entry, before the name
)

// xattr is one extended attribute. Inum is non-zero when the value lives in a
// separate EA inode (ea_inode feature); Value is then nil and not read.
type xattr struct {
	Name  string
	Value []byte
	Inum  uint32
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
	var out []xattr
	var firstErr error
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
		got, err := f.xattrBlock(in.fileACL)
		out = append(out, got...)
		if firstErr == nil {
			firstErr = err
		}
	}
	return out, firstErr
}

// xattrBlock reads and parses the extended attribute block blk. The block
// checksum is not verified.
func (f *FS) xattrBlock(blk uint64) ([]xattr, error) {
	const st = "ext4 xattr block"
	if blk < uint64(f.sb.firstDataBlock) || blk >= uint64(f.sb.blocksCount) {
		return nil, corrupt(st, -1, "i_file_acl block %d is outside the %d-block filesystem", blk, f.sb.blocksCount)
	}
	off, ok := filesys.MulOK(int64(blk), int64(f.sb.blockSize))
	if !ok {
		return nil, corrupt(st, -1, "block %d offset overflows", blk)
	}
	buf := make([]byte, f.sb.blockSize) // one block: bounded by the block size
	if err := readFull(f.r, buf, off); err != nil {
		return nil, corrupt(st, off, "read failed: %v", err)
	}
	if m := binary.LittleEndian.Uint32(buf); m != xattrMagic {
		return nil, corrupt(st, off, "bad magic %#08x, want %#08x", m, uint32(xattrMagic))
	}
	got, err := parseXattrs(buf, xattrHeaderSize)
	if ce, ok := err.(*filesys.CorruptError); ok {
		ce.Offset += off
	}
	return got, err
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
			// Value stored in an EA inode: not read here.
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
