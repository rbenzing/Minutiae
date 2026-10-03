package ext4

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Inode layout (offsets inside the inode).
const (
	iMode        = 0x0
	iUID         = 0x2
	iSizeLo      = 0x4
	iAtime       = 0x8
	iCtime       = 0xC
	iMtime       = 0x10
	iDtime       = 0x14
	iGID         = 0x18
	iLinks       = 0x1A
	iFlags       = 0x20
	iBlock       = 0x28
	iGeneration  = 0x64
	iFileACLLo   = 0x68
	iSizeHigh    = 0x6C
	iUIDHigh     = 0x78
	iGIDHigh     = 0x7A
	iChecksumLo  = 0x7C
	iFileACLHigh = 0x76
	iExtraIsize  = 0x80
	iChecksumHi  = 0x82
	iCtimeExtra  = 0x84
	iMtimeExtra  = 0x88
	iAtimeExtra  = 0x8C
	iCrtime      = 0x90
	iCrtimeExtra = 0x94

	inodeBlockLen = 60
)

// Inode flags used so far.
const (
	inodeFlagEncrypt = 0x800
)

// Mode type bits.
const (
	modeTypeMask = 0xF000
	modeDir      = 0x4000
	modeReg      = 0x8000
	modeSymlink  = 0xA000
)

// inode is a decoded on-disk inode. extra is the part after the 128-byte
// header (the extra fields, then the in-inode xattr area).
type inode struct {
	num     uint32
	mode    uint16
	uid     uint32
	gid     uint32
	size    int64
	links   uint16
	flags   uint32
	block   [inodeBlockLen]byte
	fileACL uint64
	times   filesys.Times
	csumOK  bool // true when the inode checksum matches or is not applicable
	extra   []byte

	generation      uint32
	extraIsize      int    // effective i_extra_isize: 0 when absent or invalid
	extraBad        bool   // i_extra_isize is odd-sized or larger than the inode
	orphanNext      uint32 // i_dtime of a linked inode: the next inode on the orphan list
	sizeHighIgnored uint32 // non-zero i_size_high that does not count towards the size
	nsInvalid       bool   // a raw nanosecond field was >= 1e9 and was clamped
	offset          int64  // byte offset of the inode in the image

	xattrNames []string
	xattrErr   error
}

// inode reads and decodes inode n. Out-of-range numbers, a group whose
// descriptor is unusable, and a size beyond int64 are *filesys.CorruptError; a
// checksum mismatch is not an error (csumOK=false).
func (f *FS) inode(n uint32) (*inode, error) {
	const st = "ext4 inode"
	sb := f.sb
	if n < 1 || n > sb.inodesCount {
		return nil, corrupt(st, -1, "inode %d is outside 1..%d", n, sb.inodesCount)
	}
	g := uint64(n-1) / uint64(sb.inodesPerGroup)
	idx := uint64(n-1) % uint64(sb.inodesPerGroup)
	// s_inodes_count is only warned about at Open, so bound the group by the
	// descriptor table too.
	if g >= uint64(len(f.groups)) {
		return nil, corrupt(st, -1, "inode %d is in group %d, but only %d groups have descriptors", n, g, len(f.groups))
	}
	gd := &f.groups[g]
	if gd.bad {
		return nil, corrupt(st, -1, "inode %d is in group %d, whose descriptor is unreadable or whose inode table lies outside the filesystem", n, g)
	}
	// gd.bad is false, so the whole table (itableBlocks blocks) is inside the
	// filesystem; idx < inodes_per_group keeps the inode inside the table.
	off, ok := filesys.MulOK(int64(gd.inodeTable), int64(sb.blockSize))
	if ok {
		var rel int64
		if rel, ok = filesys.MulOK(int64(idx), int64(sb.inodeSize)); ok {
			off, ok = filesys.AddOK(off, rel)
		}
	}
	if !ok || off+int64(sb.inodeSize) > f.size {
		return nil, corrupt(st, off, "inode %d lies outside the filesystem", n)
	}
	raw := make([]byte, sb.inodeSize)
	if err := readFull(f.r, raw, off); err != nil {
		return nil, corrupt(st, off, "inode %d: read failed: %v", n, err)
	}
	in, err := f.decodeInode(n, raw)
	if err != nil {
		if ce, ok := err.(*filesys.CorruptError); ok {
			ce.Offset = off + iSizeLo
		}
		return nil, err
	}
	in.offset = off
	if xs, err := f.xattrs(in); err != nil {
		in.xattrErr = err
		in.xattrNames = xattrNames(xs)
	} else {
		in.xattrNames = xattrNames(xs)
	}
	return in, nil
}

func xattrNames(xs []xattr) []string {
	var names []string
	for _, x := range xs {
		names = append(names, x.Name)
	}
	return names
}

// decodeInode decodes raw (one whole inode).
func (f *FS) decodeInode(n uint32, raw []byte) (*inode, error) {
	le := binary.LittleEndian
	sb := f.sb
	in := &inode{
		num:        n,
		mode:       le.Uint16(raw[iMode:]),
		uid:        uint32(le.Uint16(raw[iUID:])) | uint32(le.Uint16(raw[iUIDHigh:]))<<16,
		gid:        uint32(le.Uint16(raw[iGID:])) | uint32(le.Uint16(raw[iGIDHigh:]))<<16,
		links:      le.Uint16(raw[iLinks:]),
		flags:      le.Uint32(raw[iFlags:]),
		generation: le.Uint32(raw[iGeneration:]),
		fileACL:    uint64(le.Uint32(raw[iFileACLLo:])),
	}
	// The flags are honoured as they are (they decide how i_block is read), but
	// a flag the filesystem has no feature bit for is not what a kernel would
	// have written.
	if in.flags&inodeFlagExtents != 0 && !sb.hasIncompat(incompatExtents) {
		f.warn("inode %d has the extents flag but the filesystem has no extent feature; the flag is honoured", n)
	}
	if in.flags&inodeFlagInlineData != 0 && !sb.hasIncompat(incompatInlineData) {
		f.warn("inode %d has the inline data flag but the filesystem has no inline_data feature; the flag is honoured", n)
	}
	copy(in.block[:], raw[iBlock:iBlock+inodeBlockLen])
	if sb.hasIncompat(incompat64Bit) {
		in.fileACL |= uint64(le.Uint16(raw[iFileACLHigh:])) << 32
	}

	// i_size: the high half counts for regular files (and for directories with
	// large_dir), as the kernel's ext4_isize. Anything above MaxInt64 is
	// corrupt, never silently wrapped.
	size := uint64(le.Uint32(raw[iSizeLo:]))
	if in.mode&modeTypeMask == modeReg || sb.hasIncompat(incompatLargedir) {
		size |= uint64(le.Uint32(raw[iSizeHigh:])) << 32
	} else {
		in.sizeHighIgnored = le.Uint32(raw[iSizeHigh:])
	}
	if size > math.MaxInt64 {
		return nil, corrupt("ext4 inode", -1, "inode %d size %d exceeds the addressable range", n, size)
	}
	in.size = int64(size)

	if len(raw) > goodOldInodeSz {
		in.extra = raw[goodOldInodeSz:]
		e := int(le.Uint16(raw[iExtraIsize:]))
		if e%4 != 0 || goodOldInodeSz+e > len(raw) {
			in.extraBad = true
		} else {
			in.extraIsize = e
		}
	}

	// Timestamps. A field beyond i_extra_isize does not exist.
	fits := func(end int) bool { return goodOldInodeSz+in.extraIsize >= end }
	extraOf := func(off, end int) (uint32, bool) {
		if fits(end) {
			return le.Uint32(raw[off:]), true
		}
		return 0, false
	}
	ex, ok := extraOf(iAtimeExtra, iAtimeExtra+4)
	in.times.Accessed = in.decodeTime(le.Uint32(raw[iAtime:]), ex, ok)
	ex, ok = extraOf(iCtimeExtra, iCtimeExtra+4)
	in.times.Changed = in.decodeTime(le.Uint32(raw[iCtime:]), ex, ok)
	ex, ok = extraOf(iMtimeExtra, iMtimeExtra+4)
	in.times.Modified = in.decodeTime(le.Uint32(raw[iMtime:]), ex, ok)
	if fits(iCrtime + 4) {
		ex, ok = extraOf(iCrtimeExtra, iCrtimeExtra+4)
		in.times.Created = in.decodeTime(le.Uint32(raw[iCrtime:]), ex, ok)
	}
	// i_dtime is the deletion time only while the inode is unlinked. On an inode
	// that still has links it is the next-inode link of the orphan list.
	if dt := le.Uint32(raw[iDtime:]); dt != 0 {
		if in.links == 0 {
			in.times.Deleted = filesys.Timestamp{T: time.Unix(int64(dt), 0).UTC(), ZoneKnown: true}
		} else {
			in.orphanNext = dt
		}
	}

	in.csumOK = f.inodeChecksumOK(n, raw, in.extraIsize)
	return in, nil
}

// decodeTime combines a 32-bit seconds field with its optional extra field,
// following the kernel (ext4_decode_extra_time): the seconds are a signed
// 32-bit value, the low two bits of extra extend them by bits<<32, and the
// upper 30 bits are nanoseconds (clamped, and flagged, when >= 1e9). Zero
// seconds is an unset time.
func (in *inode) decodeTime(secs uint32, extra uint32, haveExtra bool) filesys.Timestamp {
	sec := int64(int32(secs))
	var ns int64
	if haveExtra {
		sec += int64(extra&3) << 32
		ns = int64(extra >> 2)
		if ns > 999_999_999 {
			// Not a valid nanosecond count: clamp rather than let time.Unix carry
			// it into the seconds, and say so on the entry.
			ns, in.nsInvalid = 999_999_999, true
		}
	}
	if sec == 0 {
		return filesys.Timestamp{}
	}
	return filesys.Timestamp{T: time.Unix(sec, ns).UTC(), ZoneKnown: true}
}

// inodeChecksumOK verifies the metadata_csum inode checksum. It follows the
// kernel's ext4_inode_csum (fs/ext4/inode.c): crc32c, seeded with the
// filesystem checksum seed (s_checksum_seed with metadata_csum_seed, else
// crc32c(~0, uuid)), over the little-endian inode number, the little-endian
// i_generation (0x64), then the whole inode with i_checksum_lo (0x7C) and, when
// i_extra_isize reaches it (extra >= 4), i_checksum_hi (0x82) taken as zero.
// i_checksum_lo holds the low 16 bits and i_checksum_hi the high 16 bits; an
// inode without the hi field is compared on 16 bits. An all-zero inode (never
// written) is accepted, as e2fsprogs does. Without metadata_csum the result is
// true.
func (f *FS) inodeChecksumOK(n uint32, raw []byte, extraIsize int) bool {
	sb := f.sb
	if !sb.metadataCsum() {
		return true
	}
	le := binary.LittleEndian
	var hdr [8]byte
	le.PutUint32(hdr[0:], n)
	copy(hdr[4:], raw[iGeneration:iGeneration+4])
	c := rawCRC32C(sb.csumSeed, hdr[:])

	zero2 := []byte{0, 0}
	hasHi := len(raw) > goodOldInodeSz && extraIsize >= 4
	c = rawCRC32C(c, raw[:iChecksumLo])
	c = rawCRC32C(c, zero2)
	end := iChecksumLo + 2
	if hasHi {
		c = rawCRC32C(c, raw[end:iChecksumHi])
		c = rawCRC32C(c, zero2)
		end = iChecksumHi + 2
	}
	c = rawCRC32C(c, raw[end:])

	stored := uint32(le.Uint16(raw[iChecksumLo:]))
	if hasHi {
		stored |= uint32(le.Uint16(raw[iChecksumHi:])) << 16
	} else {
		c &= 0xFFFF
	}
	if stored == c {
		return true
	}
	for _, b := range raw {
		if b != 0 {
			return false
		}
	}
	return true
}

// inodeSeed is the checksum seed of the blocks that belong to inode in (extent
// blocks, directory blocks): the filesystem seed folded with the little-endian
// inode number and i_generation (the kernel's i_csum_seed).
func (f *FS) inodeSeed(in *inode) uint32 {
	var w [4]byte
	binary.LittleEndian.PutUint32(w[:], in.num)
	c := rawCRC32C(f.sb.csumSeed, w[:])
	binary.LittleEndian.PutUint32(w[:], in.generation)
	return rawCRC32C(c, w[:])
}

// toEntry converts the inode to a directory entry named name. raw is the
// on-disk name when it differs from name (else nil). Size, mode and times come
// from the inode; the caller sets Deleted for an unlinked directory entry.
func toEntry(name string, raw []byte, in *inode) filesys.Entry {
	e := filesys.Entry{
		Name:      name,
		RawName:   raw,
		ID:        "inode:" + strconv.FormatUint(uint64(in.num), 10),
		Size:      in.size,
		Mode:      uint32(in.mode),
		UID:       in.uid,
		GID:       in.gid,
		Times:     in.times,
		Encrypted: in.flags&inodeFlagEncrypt != 0,
	}
	switch in.mode & modeTypeMask {
	case modeReg:
		e.Type = filesys.TypeFile
	case modeDir:
		e.Type = filesys.TypeDir
	case modeSymlink:
		e.Type = filesys.TypeSymlink
	default:
		e.Type = filesys.TypeOther
	}
	e.Attrs = append(e.Attrs,
		filesys.KV{Key: "flags", Value: "0x" + strconv.FormatUint(uint64(in.flags), 16)},
		filesys.KV{Key: "links", Value: strconv.Itoa(int(in.links))},
	)
	if len(in.xattrNames) > 0 {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "xattrs", Value: strings.Join(in.xattrNames, ",")})
	}
	if in.xattrErr != nil {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "xattrs_error", Value: in.xattrErr.Error()})
	}
	if in.orphanNext != 0 {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "orphan_next", Value: strconv.FormatUint(uint64(in.orphanNext), 10)})
	}
	if in.nsInvalid {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "time_ns", Value: "invalid"})
	}
	if in.sizeHighIgnored != 0 {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "size_high_ignored", Value: strconv.FormatUint(uint64(in.sizeHighIgnored), 10)})
	}
	if in.extraBad {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "extra_isize", Value: "bad"})
	}
	if !in.csumOK {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "checksum", Value: "bad"})
	}
	return e
}
