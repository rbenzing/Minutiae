package fat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Open opens the file named by e.ID ("dirent:<directory cluster>:<index>"). The
// directory entry is re-read from the volume: its first cluster, size and
// state decide everything, and the Entry fields and Attrs the caller passes
// (Size, Type, Deleted, first_cluster ...) are informational only and ignored.
// An entry that is deleted on disk is filesys.ErrDeleted; a directory, a
// volume label or a "dir:" ID is filesys.ErrUnsupported.
//
// The runs come from the FAT chain, cut at the directory entry's size. A chain
// that is shorter than the size (it ends, runs into a free or bad cluster,
// loops, or leaves the volume) is not an error here: the file is opened with the
// clusters that can be trusted, Runs lists only those (a strict prefix of the
// content, as the filesys.File contract allows: filesys.CheckRunsPrefix accepts
// it, filesys.CheckRuns does not), every read at or beyond the end of that
// prefix returns a *filesys.CorruptError "chain shorter than file size" and
// never zeros, and an Info warning is added. A chain longer than the size is
// cut at the size.
func (f *FS) Open(e filesys.Entry) (filesys.File, error) {
	if _, ok := parseDirID(e.ID); ok {
		return nil, fmt.Errorf("%w: %s is a directory", filesys.ErrUnsupported, e.ID)
	}
	dir, idx, ok := parseDirentID(e.ID)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a FAT file id", filesys.ErrNotFound, e.ID)
	}
	raw, err := f.liveEntryAt(dir, idx)
	if err != nil {
		return nil, err
	}
	if raw[11]&0x18 != 0 {
		return nil, fmt.Errorf("%w: entry %d of the directory at cluster %d is not a regular file", filesys.ErrUnsupported, idx, dir)
	}
	return f.openData(f.firstCluster(raw), int64(binary.LittleEndian.Uint32(raw[28:])))
}

// openData builds the file of size bytes whose chain starts at first.
func (f *FS) openData(first uint32, size int64) (*file, error) {
	fl := &file{size: size, r: f.data}
	if size == 0 {
		return fl, nil
	}
	cs := int64(f.clusterSize())
	maxRuns := int(f.count)           // a file cannot have more runs than the volume has clusters
	need := int((size + cs - 1) / cs) // size < 2^32: fits
	clusters, _, chainErr := f.chainN(first, need)
	var reason string
	if len(clusters) >= need {
		if chainErr != nil { // all the data is there, but the chain does not end properly
			f.warn("file at cluster %d: the chain does not end cleanly after its %d data clusters: %v", first, need, chainErr)
		}
	} else {
		reason = fmt.Sprintf("chain shorter than file size: %d of %d clusters (%d of %d bytes)", len(clusters), need, int64(len(clusters))*cs, size)
		if chainErr != nil {
			reason += ": " + chainErr.Error()
		}
		f.warn("file at cluster %d: %s; the missing tail reads as an error", first, reason)
	}
	remaining := size
	for _, c := range clusters[:min(len(clusters), need)] {
		off, ok := f.clusterOffset(c)
		if !ok {
			return nil, corrupt("file data", -1, "cluster %d has no valid offset", c)
		}
		n := min(cs, remaining)
		if k := len(fl.runs); k > 0 && fl.runs[k-1].Offset+fl.runs[k-1].Length == off {
			fl.runs[k-1].Length += n
		} else {
			if k >= maxRuns {
				return nil, corrupt("file data", -1, "file at cluster %d has more than %d runs", first, maxRuns)
			}
			fl.starts = append(fl.starts, fl.avail)
			fl.runs = append(fl.runs, filesys.Run{Offset: off, Length: n})
		}
		fl.avail += n
		remaining -= n
	}
	if fl.avail < size {
		fl.tail = corrupt("file data", -1, "%s", reason)
	}
	return fl, nil
}

// file is the content of one directory entry. It is immutable, so concurrent
// ReadAt calls are safe.
type file struct {
	size   int64
	avail  int64         // bytes the available runs cover (== size unless the chain is short)
	runs   []filesys.Run // file order; trimmed to avail
	starts []int64       // starts[i] is the file offset where runs[i] begins
	tail   error         // what reading [avail, size) returns; nil when avail == size
	r      io.ReaderAt   // the volume, uncached
}

// Size is the file size from the directory entry.
func (fl *file) Size() int64 { return fl.size }

// Runs returns a copy of the volume-relative runs in file order (not sorted),
// covering [0, Size()) exactly unless the chain is shorter than the size (then
// only the available prefix; reads past it fail with filesys.ErrCorrupt). Clusters that are consecutive in the file and
// adjacent on disk are one run; the list is never reordered.
func (fl *file) Runs() []filesys.Run { return slices.Clone(fl.runs) }

// ReadAt reads file content. Bytes past the end of the cluster chain are an
// error (a *filesys.CorruptError), never zeros.
func (fl *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("fat: negative read offset")
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
		i := sort.Search(len(fl.starts), func(i int) bool { return fl.starts[i] > pos }) - 1
		if i < 0 {
			return int(n), io.ErrUnexpectedEOF // unreachable: starts[0] is 0
		}
		r, within := fl.runs[i], pos-fl.starts[i]
		chunk := min(want-n, r.Length-within)
		if err := readFull(fl.r, p[n:n+chunk], r.Offset+within); err != nil {
			return int(n), err
		}
		n += chunk
	}
	if eof {
		return int(n), io.EOF
	}
	return int(n), nil
}
