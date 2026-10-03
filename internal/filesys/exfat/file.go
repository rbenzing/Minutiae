package exfat

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// parseCanonical parses a canonical decimal (no sign, no leading zeros).
func parseCanonical(s string, bits int) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, bits)
	if err != nil || strconv.FormatUint(n, 10) != s {
		return 0, false
	}
	return n, true
}

// idKind says which form an Entry.ID has.
type idKind int

const (
	idDir    idKind = iota // "dir:<first cluster>": a directory (identifies the directory itself)
	idDirent               // "dirent:<dir first cluster>:<entry index>": a file, or a deleted or unallocated entry
)

// parseID parses an Entry.ID strictly: canonical decimals only.
func parseID(id string) (kind idKind, a, b uint32, err error) {
	bad := fmt.Errorf("%w: %q is not an exFAT entry id", filesys.ErrNotFound, id)
	if rest, ok := strings.CutPrefix(id, "dir:"); ok {
		n, ok := parseCanonical(rest, 32)
		if !ok {
			return 0, 0, 0, bad
		}
		return idDir, uint32(n), 0, nil
	}
	rest, ok := strings.CutPrefix(id, "dirent:")
	if !ok {
		return 0, 0, 0, bad
	}
	cl, idx, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, 0, 0, bad
	}
	c, ok1 := parseCanonical(cl, 32)
	i, ok2 := parseCanonical(idx, 32)
	if !ok1 || !ok2 {
		return 0, 0, 0, bad
	}
	return idDirent, uint32(c), uint32(i), nil
}

// attrOf returns the value of the entry attribute key.
func attrOf(e filesys.Entry, key string) (string, bool) {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

// Open opens a regular file. Only the ID is taken from the entry: the entry set
// it names ("dirent:<dir first cluster>:<entry index>") is located and
// re-parsed from the disk, and whether the entry is deleted or a directory, its
// first cluster, DataLength, ValidDataLength and NoFatChain flag come from that
// set. The entry's other fields and its attributes are informational and are
// ignored, so a stale or forged Entry cannot change what is read. DataLength
// must fit the chain and ValidDataLength must not exceed it (*CorruptError);
// the bytes from ValidDataLength to DataLength read as zeros and are a hole run.
// A deleted entry yields filesys.ErrDeleted, a directory filesys.ErrUnsupported,
// an ID that names no File entry filesys.ErrNotFound, and a set that is damaged
// (SecondaryCount above 18, no Stream Extension) a *filesys.CorruptError.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	kind, p, i, err := parseID(e.ID)
	if err != nil {
		return nil, err
	}
	if kind == idDir {
		return nil, fmt.Errorf("%w: %q is a directory", filesys.ErrUnsupported, e.ID)
	}
	r, err := f.locate(p, i)
	if err != nil {
		return nil, err
	}
	switch {
	case r.deleted:
		return nil, filesys.ErrDeleted
	case r.attrs&attrDirectory != 0:
		return nil, fmt.Errorf("%w: %q is a directory", filesys.ErrUnsupported, e.ID)
	case r.damaged:
		return nil, corrupt("exFAT file", -1, "the entry set of %q is damaged (SecondaryCount or Stream Extension)", e.ID)
	case r.dataLen > math.MaxInt64 || r.valid > math.MaxInt64:
		return nil, corrupt("exFAT file", -1, "DataLength %d / ValidDataLength %d is larger than any file", r.dataLen, r.valid)
	}
	return f.openExtent(r.first, int64(r.dataLen), int64(r.valid), r.noFatChain())
}

// openExtent builds the file of size bytes (valid of them initialised) whose
// chain starts at first.
func (f *FS) openExtent(first uint32, size, valid int64, noFat bool) (filesys.File, error) {
	const st = "exFAT file"
	switch {
	case size < 0 || valid < 0:
		return nil, corrupt(st, -1, "negative length (DataLength %d, ValidDataLength %d)", size, valid)
	case valid > size:
		return nil, corrupt(st, -1, "ValidDataLength %d exceeds DataLength %d", valid, size)
	}
	runs := []filesys.Run{}
	if size > 0 {
		capacity, ok := filesys.MulOK(int64(f.clusterCount), f.cs)
		if !ok || size > capacity {
			return nil, corrupt(st, -1, "DataLength %d is larger than the %d bytes of the cluster heap", size, capacity)
		}
		need := uint64(size+f.cs-1) / uint64(f.cs) // size <= capacity: cannot overflow
		exts, err := f.chain(first, need, true, noFat)
		if err != nil {
			return nil, fmt.Errorf("DataLength %d needs %d clusters: %w", size, need, err)
		}
		remaining := valid
		for _, x := range exts {
			if remaining == 0 {
				break
			}
			n := min(int64(x.n)*f.cs, remaining) // an extent lies inside the heap: cannot overflow
			runs = append(runs, filesys.Run{Offset: f.clusterOff(x.first), Length: n})
			remaining -= n
		}
		if size > valid {
			runs = append(runs, filesys.Run{Offset: -1, Length: size - valid})
		}
	}
	fl := &file{size: size, runs: runs, r: f.data}
	var pos int64
	for _, r := range runs {
		fl.starts = append(fl.starts, pos)
		pos += r.Length
	}
	return fl, nil
}

// file is the content of one entry. It is immutable, so concurrent ReadAt
// calls are safe.
type file struct {
	size   int64
	runs   []filesys.Run // covers [0, size); the last may be a hole
	starts []int64       // starts[i] is the file offset where runs[i] begins
	r      io.ReaderAt   // the volume, for non-hole runs
}

// Size is the content length (DataLength).
func (fl *file) Size() int64 { return fl.size }

// Runs returns a copy of the volume-relative runs: the clusters up to
// ValidDataLength, then one hole run for the rest.
func (fl *file) Runs() []filesys.Run { return slices.Clone(fl.runs) }

// ReadAt reads file content; the part past ValidDataLength reads as zeros.
func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("exfat: negative read offset")
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
		i := sort.Search(len(fl.starts), func(i int) bool { return fl.starts[i] > pos }) - 1
		if i < 0 {
			return int(n), io.ErrUnexpectedEOF // unreachable: starts[0] is 0
		}
		r, within := fl.runs[i], pos-fl.starts[i]
		chunk := min(want-n, r.Length-within)
		if r.Offset < 0 {
			clear(p[n : n+chunk])
		} else if err := readFull(fl.r, p[n:n+chunk], r.Offset+within); err != nil {
			return int(n), err
		}
		n += chunk
	}
	if eof {
		return int(n), io.EOF
	}
	return int(n), nil
}
