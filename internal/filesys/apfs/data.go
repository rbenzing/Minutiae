package apfs

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// File extents (Apple File System Reference; the Format reference of the
// project plan). The extent records of a file are keyed by its private id
// (shared by clones; from the plan, marked there as not verified against the
// PDF), not by its inode number.
const (
	fsTypeFileExtent = 8

	extentLenMask    = 0x00ffffffffffffff // j_file_extent_val_t.len_and_flags: the length
	extentKeySize    = 16                 // j_key_t header + logical_addr
	extentValSize    = 24                 // len_and_flags, phys_block_num, crypto_id
	cryptoSWID       = 4                  // CRYPTO_SW_ID, the id of software-encrypted extents
	maxFileRuns      = 1 << 20            // runs one file may map
	maxExtentRecords = 1 << 22            // extent records read for one file
)

// cryptoAnomaly reports a crypto id that is neither 0 nor CRYPTO_SW_ID on an
// unencrypted volume: the file has its own key (per-file encryption).
func cryptoAnomaly(id uint64) bool { return id != 0 && id != cryptoSWID }

// Open opens a regular file or a symlink. Everything is derived from e.ID:
// no other field of e is looked at. A directory, a special file and a
// decmpfs-compressed file are filesys.ErrUnsupported, anything below an
// encrypted volume is filesys.ErrEncrypted. A symlink returns its target.
// Content of a file with its own key (a crypto id other than 0 and
// CRYPTO_SW_ID) is returned as stored on disk, never decrypted (decryption is
// roadmap sub-project 10), with a warning.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	kind, slot, _, _, ok := parseEntryID(e.ID)
	if !ok {
		return nil, notFound("entry ID %q", e.ID)
	}
	if kind == idSnaps {
		if err := f.snapsTarget(slot, e.ID); err != nil {
			return nil, err
		}
	}
	if kind != idNode {
		return nil, fmt.Errorf("apfs: %w: %q is a directory", filesys.ErrUnsupported, e.ID)
	}
	v, view, ino, err := f.nodeTarget(e.ID)
	if err != nil {
		return nil, err
	}
	in, err := f.inode(v, view, ino)
	if err != nil {
		return nil, err
	}
	switch in.mode & modeTypeMask {
	case modeDir:
		return nil, fmt.Errorf("apfs: %w: inode %d of volume %d is a directory", filesys.ErrUnsupported, ino, v.slot)
	case modeSymlink:
		return f.openSymlink(v, in)
	case modeReg:
	default:
		return nil, fmt.Errorf("apfs: %w: inode %d of volume %d (mode %#o) is neither a regular file nor a symlink", filesys.ErrUnsupported, ino, v.slot, in.mode)
	}
	if in.compressed() {
		return nil, fmt.Errorf("apfs: %w: decmpfs-compressed file", filesys.ErrUnsupported)
	}
	var size int64
	if in.hasDstream {
		size = in.size
	}
	if size == 0 {
		return &file{r: f.data}, nil
	}
	m, err := f.fileMap(v, view, in, size)
	if err != nil {
		return nil, err
	}
	if cryptoAnomaly(in.cryptoID) || m.cryptoSeen {
		id := in.cryptoID
		if !cryptoAnomaly(id) {
			id = m.crypto
		}
		f.warn("volume %d inode %d: per-file encryption (crypto id %#x): the content is returned as stored on disk (ciphertext), not decrypted", v.slot, ino, id)
	}
	fl := &file{r: f.data, size: size, avail: m.covered, runs: m.runs, tail: m.tail}
	fl.starts = make([]int64, len(m.runs))
	var at int64
	for i, r := range m.runs {
		fl.starts[i] = at
		at += r.Length
	}
	return fl, nil
}

// openSymlink returns the target held in the symlink xattr.
func (f *FS) openSymlink(v *volume, in *inode) (filesys.File, error) {
	switch {
	case in.symlinkOK:
		n := int64(len(in.symlink))
		return &file{size: n, avail: n, mem: slices.Clone(in.symlink), inline: true}, nil
	case in.symlinkSet:
		return nil, unsupported("the target of symlink inode %d of volume %d is stored as a data stream", in.ino, v.slot)
	}
	return nil, corrupt("symlink", -1, "volume %d inode %d is a symlink without a target", v.slot, in.ino)
}

// fileMap is the validated extent list of one file.
type fileMap struct {
	runs       []filesys.Run // file order; the trusted prefix when tail is set
	covered    int64         // bytes the runs account for
	tail       error         // what reading [covered, size) returns; nil when covered == size
	crypto     uint64        // the first extent crypto id that marks per-file encryption
	cryptoSeen bool
}

// runBuilder collects runs in file order, merging adjacent ones by hand (never
// filesys.MergeRuns, which sorts and drops holes) and enforcing the run cap.
type runBuilder struct {
	runs []filesys.Run
	max  int
}

// add appends r; false means the cap is reached and nothing changed.
func (b *runBuilder) add(r filesys.Run) bool {
	if n := len(b.runs); n > 0 {
		last := &b.runs[n-1]
		if (r.Offset < 0 && last.Offset < 0) || (r.Offset >= 0 && last.Offset >= 0 && last.Offset+last.Length == r.Offset) {
			last.Length += r.Length
			return true
		}
	}
	if len(b.runs) >= b.max {
		return false
	}
	b.runs = append(b.runs, r)
	return true
}

// addAll appends the runs or none of them.
func (b *runBuilder) addAll(rs ...filesys.Run) bool {
	n := len(b.runs)
	var last filesys.Run
	if n > 0 {
		last = b.runs[n-1]
	}
	for _, r := range rs {
		if !b.add(r) {
			b.runs = b.runs[:n]
			if n > 0 {
				b.runs[n-1] = last
			}
			return false
		}
	}
	return true
}

// fileMap reads and validates the extents of in (file size bytes) and builds
// the runs. Per extent: the logical address is block-aligned and not before
// the end of the previous extent, the length is a positive multiple of the
// block size, and a non-hole extent lies inside the container. The first
// extent that fails ends the trusted prefix (the runs so far; reads beyond
// fail with filesys.ErrCorrupt, never zeros). Gaps between extents and
// phys_block_num 0 are holes; extents entirely beyond size are ignored (the
// scan stops there). A read failure that is not corruption is returned.
func (f *FS) fileMap(v *volume, view uint64, in *inode, size int64) (*fileMap, error) {
	m := &fileMap{}
	rb := &runBuilder{max: f.maxFileRuns}
	var (
		pos     int64  // bytes of the file the runs cover
		prevEnd uint64 // end of the previous extent as the records state it
		reason  string // why the prefix ends, when it does
		records int
	)
	capReason := fmt.Sprintf("more than %d runs", f.maxFileRuns)

	if in.privateID == 0 || in.privateID > maxIno {
		reason = fmt.Sprintf("the private id %d is not a valid object id", in.privateID)
	} else {
		t, err := f.fsTree(v, view)
		if err != nil {
			return nil, err
		}
		f.scans.Add(1)
		id := in.privateID
		prefix := func(key []byte) int {
			k := le.Uint64(key)
			kid, kt := k&jobjIDMask, uint8(k>>60)
			switch {
			case kid < id:
				return -1
			case kid > id:
				return 1
			case kt < fsTypeFileExtent:
				return -1
			case kt > fsTypeFileExtent:
				return 1
			}
			return 0
		}
		bsz := uint64(f.bs)
		ooo, err := t.scanOrdered(prefix, func(key, val []byte) (bool, error) {
			if records++; records > maxExtentRecords {
				reason = fmt.Sprintf("more than %d extent records", maxExtentRecords)
				return true, nil
			}
			if len(key) != extentKeySize || len(val) != extentValSize {
				reason = fmt.Sprintf("an extent record has a %d-byte key and a %d-byte value, not %d and %d", len(key), len(val), extentKeySize, extentValSize)
				return true, nil
			}
			logical := le.Uint64(key[8:])
			if logical >= uint64(size) {
				return true, nil // this extent and every later one lies beyond the end of the file
			}
			lenFlags := le.Uint64(val)
			length, flags := lenFlags&extentLenMask, uint8(lenFlags>>56)
			phys, crypto := le.Uint64(val[8:]), le.Uint64(val[16:])
			switch {
			case logical%bsz != 0:
				reason = fmt.Sprintf("extent at logical address %d is not block-aligned", logical)
			case logical < prevEnd:
				reason = fmt.Sprintf("extent at logical address %d overlaps or precedes the previous extent (which ends at %d)", logical, prevEnd)
			case length == 0 || length%bsz != 0:
				reason = fmt.Sprintf("extent at logical address %d has length %d, not a positive multiple of the %d-byte block size", logical, length, bsz)
			}
			var physOff int64
			if reason == "" && phys != 0 {
				ok := phys <= math.MaxInt64/bsz
				if ok {
					physOff = int64(phys * bsz)
					end, eok := filesys.AddOK(physOff, int64(length))
					ok = eok && end <= f.size
				}
				if !ok {
					reason = fmt.Sprintf("extent at logical address %d (block %d, %d bytes) lies outside the %d-byte container", logical, phys, length, f.size)
				}
			}
			if reason != "" {
				return true, nil
			}
			if flags != 0 {
				f.warn("volume %d inode %d: extent at logical address %d has flags %#x, none are defined", v.slot, in.ino, logical, flags)
			}
			if !m.cryptoSeen && cryptoAnomaly(crypto) {
				m.crypto, m.cryptoSeen = crypto, true
			}
			prevEnd = logical + length // cannot overflow: logical < 2^63, length < 2^56
			end := int64(min(prevEnd, uint64(size)))
			run := filesys.Run{Offset: -1, Length: end - int64(logical)}
			if phys != 0 {
				run.Offset = physOff
			}
			var ok bool
			if int64(logical) > pos {
				ok = rb.addAll(filesys.Run{Offset: -1, Length: int64(logical) - pos}, run)
			} else {
				ok = rb.addAll(run)
			}
			if !ok {
				reason = capReason
				return true, nil
			}
			pos = end
			return false, nil
		})
		switch {
		case err != nil && !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrNotFound) && !isShort(err):
			return nil, fmt.Errorf("apfs: volume %d inode %d: extents: %w", v.slot, in.ino, err)
		case err != nil && reason == "":
			reason = fmt.Sprintf("the extent tree cannot be read: %v", err)
		case ooo && reason == "":
			reason = "the extent records are not in key order, so later extents may not have been reached"
		}
		if reason == "" && pos < size { // a trailing gap is a hole
			if rb.addAll(filesys.Run{Offset: -1, Length: size - pos}) {
				pos = size
			} else {
				reason = capReason
			}
		}
	}
	m.runs, m.covered = rb.runs, pos
	if reason != "" {
		m.tail = corrupt("file extents", -1, "volume %d inode %d: %s", v.slot, in.ino, reason)
		f.warn("volume %d inode %d: %s; the first %d of %d bytes are readable and the rest reads as an error", v.slot, in.ino, reason, pos, size)
	}
	return m, nil
}

// file is the content of one inode. It is immutable, so concurrent ReadAt
// calls are safe.
type file struct {
	r      io.ReaderAt   // the container, uncached, for non-hole runs
	size   int64         // content length
	avail  int64         // bytes the runs (or mem) cover; size unless the extents are damaged
	inline bool          // the content is in mem (a symlink target)
	mem    []byte        // inline content (runs is nil)
	runs   []filesys.Run // file order, trimmed to avail
	starts []int64       // starts[i] is the file offset where runs[i] begins
	tail   error         // what reading [avail, size) returns; nil when avail == size
}

// Size is the content length.
func (fl *file) Size() int64 { return fl.size }

// Runs returns a copy of the container-relative runs in file order (nil for a
// symlink or an empty file). They cover [0, Size()) exactly unless the extents
// are damaged; then they cover only the trusted prefix and reads at or beyond
// its end fail with filesys.ErrCorrupt.
func (fl *file) Runs() []filesys.Run { return slices.Clone(fl.runs) }

// ReadAt reads file content. Holes read as zeros; bytes the extent list does
// not vouch for are an error (a *filesys.CorruptError), never zeros. A read
// failure of the underlying reader is returned as it is.
func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("apfs: negative read offset")
	}
	if off >= fl.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	want, eof := int64(len(p)), false
	if rem := fl.size - off; want > rem {
		want, eof = rem, true
	}
	var n int64
	for n < want {
		pos := off + n
		if pos >= fl.avail {
			if fl.tail != nil {
				return int(n), fl.tail
			}
			return int(n), io.ErrUnexpectedEOF // unreachable: avail == size without a tail error
		}
		if fl.inline {
			n += int64(copy(p[n:want], fl.mem[pos:]))
			continue
		}
		i := sort.Search(len(fl.starts), func(i int) bool { return fl.starts[i] > pos }) - 1
		if i < 0 {
			return int(n), io.ErrUnexpectedEOF // unreachable: starts[0] is 0
		}
		r, within := fl.runs[i], pos-fl.starts[i]
		chunk := min(want-n, r.Length-within)
		if r.Offset < 0 {
			clear(p[n : n+chunk])
		} else if err := readFull(fl.r, p[n:n+chunk], r.Offset+within); err != nil {
			if isShort(err) {
				return int(n), corrupt("file data", r.Offset+within, "the container ends inside the file's data: %v", err)
			}
			return int(n), fmt.Errorf("apfs: file data: read at offset %d failed: %w", r.Offset+within, err)
		}
		n += chunk
	}
	if eof {
		return int(n), io.EOF
	}
	return int(n), nil
}
