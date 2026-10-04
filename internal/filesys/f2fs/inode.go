package f2fs

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// struct f2fs_inode field offsets (a 4 KiB node block, see node.go for the
// footer). Derived from the kernel's include/linux/f2fs_fs.h, in declaration
// order:
//
//	i_mode u16 @0, i_advise u8 @2, i_inline u8 @3, i_uid u32 @4, i_gid @8,
//	i_links @12, i_size u64 @16, i_blocks u64 @24, i_atime u64 @32, i_ctime
//	@40, i_mtime @48, i_atime_nsec u32 @56, i_ctime_nsec @60, i_mtime_nsec
//	@64, i_generation @68, i_current_depth / i_gc_failures (a union) @72,
//	i_xattr_nid @76, i_flags @80, i_pino @84, i_namelen @88, i_name[F2FS_NAME_LEN
//	= 255] @92 (to 347), i_dir_level u8 @347, i_ext (struct f2fs_extent: fofs,
//	blk, len, 12 bytes) @348, then i_addr[DEF_ADDRS_PER_INODE = 923] @360 (to
//	360 + 923*4 = 4052) and i_nid[DEF_NIDS_PER_INODE = 5] @4052 (to 4072), where
//	the node footer starts.
const (
	iMode        = 0
	iAdvise      = 2
	iInline      = 3
	iUID         = 4
	iGID         = 8
	iLinks       = 12
	iSize        = 16
	iBlocks      = 24
	iAtime       = 32
	iCtime       = 40
	iMtime       = 48
	iAtimeNsec   = 56
	iCtimeNsec   = 60
	iMtimeNsec   = 64
	iGeneration  = 68
	iCurDepth    = 72
	iXattrNID    = 76
	iFlags       = 80
	iPino        = 84
	iNameLen     = 88
	iName        = 92
	fNameLen     = 255
	iDirLevel    = 347
	iExt         = 348
	iAddr        = 360
	addrsPerIno  = 923 // DEF_ADDRS_PER_INODE
	iNID         = iAddr + addrsPerIno*4
	nidsPerInode = 5
)

// Extra inode attribute header. With F2FS_EXTRA_ATTR in i_inline, i_addr[]
// starts with this packed struct (offsets relative to i_addr):
//
//	i_extra_isize u16 @0, i_inline_xattr_size u16 @2 (in 4-byte words),
//	i_projid u32 @4, i_inode_checksum u32 @8, i_crtime u64 @12,
//	i_crtime_nsec u32 @20, i_compr_blocks u64 @24, i_compress_algorithm u8
//	@32, i_log_cluster_size u8 @33, i_compress_flag u16 @34, end @36.
//
// A field exists only when it lies wholly inside i_extra_isize (the kernel's
// F2FS_FITS_IN_INODE: offsetof(field) + sizeof(field) <= i_extra_isize).
// Data addresses start at i_addr[i_extra_isize / 4].
const (
	xaExtraIsize  = 0
	xaInlineXattr = 2
	xaChecksum    = 8
	xaCrtime      = 12
	xaCrtimeNsec  = 20

	// minExtraIsize is the smallest legal i_extra_isize: the header's own
	// first two fields.
	minExtraIsize = 4
	// maxExtraIsize bounds i_extra_isize by the whole i_addr area.
	maxExtraIsize = addrsPerIno * 4

	// defaultInlineXattrAddrs is DEFAULT_INLINE_XATTR_ADDRS: the words reserved
	// at the end of i_addr for inline xattrs when the size is not recorded
	// per inode (no FLEXIBLE_INLINE_XATTR).
	defaultInlineXattrAddrs = 50

	// minInlineXattrSize and maxInlineXattrSize bound a recorded inline xattr
	// size in words (kernel xattr.h): the minimum is sizeof(struct
	// f2fs_xattr_header)/4 = 24/4; the maximum leaves room for the full
	// 36-byte extra header, the reserved inline-data word and the 40-byte
	// minimum inline dentry: 923 - 9 - 1 - 10.
	minInlineXattrSize = 6
	maxInlineXattrSize = addrsPerIno - 36/4 - 1 - 40/4

	// iInodeChecksum is offsetof(struct f2fs_inode, i_inode_checksum): the
	// checksum word is i_addr + xaChecksum.
	iInodeChecksum = iAddr + xaChecksum
)

// i_inline flags.
const (
	inlineXattr   = 0x01 // F2FS_INLINE_XATTR
	inlineData    = 0x02 // F2FS_INLINE_DATA
	inlineDentry  = 0x04 // F2FS_INLINE_DENTRY
	inlineDataExt = 0x08 // F2FS_DATA_EXIST
	inlineDots    = 0x10 // F2FS_INLINE_DOTS
	inlineExtra   = 0x20 // F2FS_EXTRA_ATTR
	inlinePinFile = 0x40 // F2FS_PIN_FILE
)

// i_flags bits.
const (
	flagCompr   = 0x4   // F2FS_COMPR_FL (FS_COMPR_FL)
	flagEncrypt = 0x800 // F2FS_ENCRYPT_FL (FS_ENCRYPT_FL)
)

// POSIX file type bits of i_mode.
const (
	modeTypeMask = 0o170000
	modeReg      = 0o100000
	modeDir      = 0o040000
	modeSymlink  = 0o120000
)

// maxUnixSeconds is 9999-12-31T23:59:59Z, the last second a time.Time can
// serialize; larger on-disk values are reported invalid.
const maxUnixSeconds = 253402300799

// inode is a parsed f2fs_inode.
type inode struct {
	nid uint32 // node id == inode number

	mode      uint16
	advise    uint8
	inline    uint8
	uid, gid  uint32
	links     uint32
	size      int64
	blocks    uint64
	times     filesys.Times
	nsInvalid bool // some nsec field was >= 1e9 (clamped)
	timeBad   bool // some seconds field is outside the representable range (omitted)

	generation uint32
	curDepth   uint32 // i_current_depth (directories) / i_gc_failures (files)
	xattrNID   uint32
	flags      uint32
	pino       uint32
	name       []byte // i_name[:i_namelen], nil when i_namelen is out of range
	nameBad    bool
	dirLevel   uint8
	ext        [3]uint32 // i_ext: fofs, blk, len (a cache hint; never trusted)
	nids       [nidsPerInode]uint32

	extraIsize   int    // bytes of the extra attribute header (0 without EXTRA_ATTR)
	xattrWords   int    // words reserved at the end of i_addr for inline xattrs
	projID       uint32 // valid when extraIsize >= 8
	addrStart    int    // byte offset in raw of the first data address slot
	addrSlots    int    // data address slots: 923 - extraIsize/4 - xattrWords
	csumChecked  bool
	csumOK       bool
	compressed   bool // F2FS_COMPR_FL on a regular file
	encrypted    bool
	raw          []byte // the whole node block (footer included)
	inlineXattrB bool   // INLINE_XATTR flag
}

func (in *inode) typ() uint16 { return in.mode & modeTypeMask }

// inode reads and parses inode ino.
func (f *FS) inode(ino uint32) (*inode, error) {
	const st = "f2fs inode"
	raw, err := f.node(ino)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if fi := le.Uint32(raw[footIno:]); fi != ino {
		// An inode's footer ino equals its nid (the kernel's RAW_IS_INODE).
		return nil, corrupt(st, -1, "node %d is not an inode (footer ino is %d)", ino, fi)
	}
	in := &inode{
		nid:        ino,
		raw:        raw,
		mode:       le.Uint16(raw[iMode:]),
		advise:     raw[iAdvise],
		inline:     raw[iInline],
		uid:        le.Uint32(raw[iUID:]),
		gid:        le.Uint32(raw[iGID:]),
		links:      le.Uint32(raw[iLinks:]),
		blocks:     le.Uint64(raw[iBlocks:]),
		generation: le.Uint32(raw[iGeneration:]),
		curDepth:   le.Uint32(raw[iCurDepth:]),
		xattrNID:   le.Uint32(raw[iXattrNID:]),
		flags:      le.Uint32(raw[iFlags:]),
		pino:       le.Uint32(raw[iPino:]),
		dirLevel:   raw[iDirLevel],
	}
	size := le.Uint64(raw[iSize:])
	if size > math.MaxInt64 {
		return nil, corrupt(st, -1, "inode %d size %d exceeds the addressable range", ino, size)
	}
	in.size = int64(size)
	for i := range in.ext {
		in.ext[i] = le.Uint32(raw[iExt+4*i:])
	}
	for i := range in.nids {
		in.nids[i] = le.Uint32(raw[iNID+4*i:])
	}
	if nl := le.Uint32(raw[iNameLen:]); nl <= fNameLen {
		in.name = raw[iName : iName+int(nl)]
	} else {
		in.nameBad = true
	}
	in.encrypted = in.flags&flagEncrypt != 0
	in.compressed = in.flags&flagCompr != 0 && in.typ() == modeReg
	in.inlineXattrB = in.inline&inlineXattr != 0

	if err := f.parseExtra(in); err != nil {
		return nil, err
	}

	// Times. i_crtime exists only with INODE_CRTIME and a header that reaches
	// it (kernel f2fs_inode_crtime / F2FS_FITS_IN_INODE).
	in.times.Accessed = in.decodeTime(le.Uint64(raw[iAtime:]), le.Uint32(raw[iAtimeNsec:]))
	in.times.Changed = in.decodeTime(le.Uint64(raw[iCtime:]), le.Uint32(raw[iCtimeNsec:]))
	in.times.Modified = in.decodeTime(le.Uint64(raw[iMtime:]), le.Uint32(raw[iMtimeNsec:]))
	if f.sb.has(featInodeCrtime) && in.extraIsize >= xaCrtimeNsec+4 {
		x := raw[iAddr:]
		in.times.Created = in.decodeTime(le.Uint64(x[xaCrtime:]), le.Uint32(x[xaCrtimeNsec:]))
	}
	// F2FS has no deletion time: Times.Deleted stays absent.

	f.checkInodeChecksum(in)
	return in, nil
}

// parseExtra validates the extra attribute header and the inline xattr
// reservation and derives where the data address slots lie.
//
// The kernel (sanity_check_inode / do_read_inode) requires EXTRA_ATTR only
// with the extra_attr feature, i_extra_isize a multiple of 4 inside the
// header limits, and the reservations to fit in i_addr[]. Reservation rule
// (do_read_inode, get_inline_xattr_addrs): with FLEXIBLE_INLINE_XATTR and an
// extra header, the inode records the size itself (i_inline_xattr_size, in
// words) and it applies whether or not INLINE_XATTR is set; the recorded size
// must lie in [minInlineXattrSize, maxInlineXattrSize] when INLINE_XATTR is
// set. Otherwise it is the fixed DEFAULT_INLINE_XATTR_ADDRS (50) for an inode
// with INLINE_XATTR or INLINE_DENTRY, else nothing.
// Slots = 923 - i_extra_isize/4 - reserved words.
func (f *FS) parseExtra(in *inode) error {
	const st = "f2fs inode"
	le := binary.LittleEndian
	x := in.raw[iAddr:]
	recorded := false
	if in.inline&inlineExtra != 0 {
		if !f.sb.has(featExtraAttr) {
			return corrupt(st, -1, "inode %d has an extra attribute header but the volume lacks the extra_attr feature", in.nid)
		}
		e := int(le.Uint16(x[xaExtraIsize:]))
		if e < minExtraIsize || e > maxExtraIsize || e%4 != 0 {
			return corrupt(st, -1, "inode %d has i_extra_isize %d (want a multiple of 4 in %d..%d)", in.nid, e, minExtraIsize, maxExtraIsize)
		}
		in.extraIsize = e
		if f.sb.has(featFlexInlineXat) {
			recorded = true
			in.xattrWords = int(le.Uint16(x[xaInlineXattr:]))
			if in.inlineXattrB && (in.xattrWords < minInlineXattrSize || in.xattrWords > maxInlineXattrSize) {
				return corrupt(st, -1, "inode %d records an inline xattr size of %d words (want %d..%d)", in.nid, in.xattrWords, minInlineXattrSize, maxInlineXattrSize)
			}
		}
		if e >= 8 {
			in.projID = le.Uint32(x[4:])
		}
	}
	if !recorded && (in.inlineXattrB || in.inline&inlineDentry != 0) {
		in.xattrWords = defaultInlineXattrAddrs
	}
	in.addrSlots = addrsPerIno - in.extraIsize/4 - in.xattrWords
	if in.addrSlots < 0 {
		return corrupt(st, -1, "inode %d reserves %d bytes of extra attributes and %d words of inline xattr, more than the %d-word address area", in.nid, in.extraIsize, in.xattrWords, addrsPerIno)
	}
	in.addrStart = iAddr + in.extraIsize
	return nil
}

// decodeTime converts an on-disk seconds/nanoseconds pair. Zero seconds is an
// unset time; seconds beyond year 9999 are omitted and flagged; nanoseconds
// >= 1e9 are clamped and flagged (time.Unix would otherwise carry them into
// the seconds).
func (in *inode) decodeTime(sec uint64, nsec uint32) filesys.Timestamp {
	if sec == 0 {
		return filesys.Timestamp{}
	}
	if sec > maxUnixSeconds {
		in.timeBad = true
		return filesys.Timestamp{}
	}
	ns := int64(nsec)
	if nsec > 999_999_999 {
		ns, in.nsInvalid = 999_999_999, true
	}
	return filesys.Timestamp{T: time.Unix(int64(sec), ns).UTC(), ZoneKnown: true}
}

// inodeChecksum computes the inode checksum of the node block raw.
//
// Derivation (kernel f2fs_inode_chksum): the seed is
// crc32_le(~0, sb.uuid); the checksum chains crc32_le over the little-endian
// footer ino, the little-endian i_generation, the inode bytes before the
// checksum word (offsetof(i_inode_checksum) = 368), four zero bytes in place
// of the word, then everything after it to the end of the 4 KiB block
// (footer included).
func (f *FS) inodeChecksum(raw []byte) uint32 {
	le := binary.LittleEndian
	var w [4]byte
	seed := rawCRC32(^uint32(0), f.sb.uuid[:])
	le.PutUint32(w[:], le.Uint32(raw[footIno:]))
	c := rawCRC32(seed, w[:])
	c = rawCRC32(c, raw[iGeneration:iGeneration+4])
	c = rawCRC32(c, raw[:iInodeChecksum])
	c = rawCRC32(c, make([]byte, 4))
	return rawCRC32(c, raw[iInodeChecksum+4:])
}

// checkInodeChecksum verifies the checksum when the volume has INODE_CHKSUM
// and the inode's extra header reaches i_inode_checksum (kernel
// f2fs_enable_inode_chksum); otherwise there is nothing to verify.
func (f *FS) checkInodeChecksum(in *inode) {
	if !f.sb.has(featInodeChksum) || in.inline&inlineExtra == 0 || in.extraIsize < xaChecksum+4 {
		return
	}
	in.csumChecked = true
	stored := binary.LittleEndian.Uint32(in.raw[iInodeChecksum:])
	in.csumOK = stored == f.inodeChecksum(in.raw)
}

// inlineNames lists the inline features in use, for the "inline" attribute.
func (in *inode) inlineNames() string {
	var s []string
	for _, x := range []struct {
		bit  uint8
		name string
	}{{inlineData, "data"}, {inlineDentry, "dentry"}, {inlineXattr, "xattr"}} {
		if in.inline&x.bit != 0 {
			s = append(s, x.name)
		}
	}
	return strings.Join(s, ",")
}

// toEntry converts the inode to a directory entry named name. raw is the
// on-disk name when it differs from name (else nil). Size, mode and times come
// from the inode; the caller sets Deleted for an unlinked directory entry.
func toEntry(name string, raw []byte, in *inode) filesys.Entry {
	e := filesys.Entry{
		Name:      name,
		RawName:   raw,
		ID:        "nid:" + strconv.FormatUint(uint64(in.nid), 10),
		Size:      in.size,
		Mode:      uint32(in.mode),
		UID:       in.uid,
		GID:       in.gid,
		Times:     in.times,
		Encrypted: in.encrypted,
	}
	switch in.typ() {
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
		filesys.KV{Key: "links", Value: strconv.FormatUint(uint64(in.links), 10)},
	)
	if s := in.inlineNames(); s != "" {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "inline", Value: s})
	}
	if in.compressed {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "compressed", Value: "true"})
	}
	if in.nsInvalid {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "time_ns", Value: "invalid"})
	}
	if in.timeBad {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "time", Value: "invalid"})
	}
	if in.csumChecked && !in.csumOK {
		e.Attrs = append(e.Attrs, filesys.KV{Key: "checksum", Value: "bad"})
	}
	return e
}

// parseNodeID parses an Entry.ID of the form "nid:<ino>" strictly: canonical
// decimal only (no sign, space, leading zero, 0x prefix or trailing junk),
// within uint32 and non-zero. Anything else wraps filesys.ErrNotFound so a
// forged ID is rejected without touching the volume.
func parseNodeID(id string) (uint32, error) {
	bad := fmt.Errorf("%w: %q is not an F2FS node id", filesys.ErrNotFound, id)
	rest, ok := strings.CutPrefix(id, "nid:")
	if !ok {
		return 0, bad
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != rest {
		return 0, bad
	}
	return uint32(n), nil
}
