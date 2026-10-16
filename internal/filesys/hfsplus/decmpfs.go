package hfsplus

import (
	"fmt"
	"math"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/decmpfs"
)

// decmpfs (transparent compression): the layouts and the decoders live in the
// shared package internal/filesys/decmpfs (see its package comment for what is
// decoded and on what evidence). This file only adapts HFS+ forks to it.

// decmpfsHeader is a decoded decmpfs attribute header; ok is false when the
// attribute is missing or its header unusable.
type decmpfsHeader struct {
	ok   bool
	typ  uint32
	size uint64
}

func parseDecmpfs(v []byte) decmpfsHeader {
	h, err := decmpfs.ParseHeader(v)
	if err != nil {
		return decmpfsHeader{}
	}
	return decmpfsHeader{ok: true, typ: h.Type, size: h.Size}
}

// label names the compression for the "compressed" attribute.
func (h decmpfsHeader) label() string {
	switch {
	case !h.ok:
		return "unknown"
	case h.typ == decmpfs.TypeZlibAttr || h.typ == decmpfs.TypeZlibRsrc:
		return "zlib"
	}
	return strconv.FormatUint(uint64(h.typ), 10)
}

// contentFile presents decoded content as a File: the bytes are decoded, not
// copied from the image, so there are no runs.
type contentFile struct{ decmpfs.Content }

func (contentFile) Runs() []filesys.Run { return nil }

// rsrcFork reads a mapped resource fork all-or-error for the decoder.
type rsrcFork struct {
	fm   *forkMap
	size int64
}

func (r rsrcFork) Size() int64 { return r.size }
func (r rsrcFork) ReadAt(p []byte, off int64) (int, error) {
	if err := readForkFull(r.fm, p, off); err != nil {
		return 0, err
	}
	return len(p), nil
}

// openDecmpfs decodes a compressed file. A type-4 file's resource fork must be
// mapped completely before anything is inflated.
func (f *FS) openDecmpfs(o *object) (filesys.File, error) {
	var fork decmpfs.Fork
	if o.cmp.typ == decmpfs.TypeZlibRsrc {
		rd := o.node.rsrc
		if rd.logicalSize > math.MaxInt64 {
			return nil, corrupt("decmpfs", -1, "a resource fork of %d bytes", rd.logicalSize)
		}
		fm, err := f.forkPrefix(o.node.id, true, rd)
		if err != nil {
			return nil, fmt.Errorf("the resource fork of a compressed file cannot be mapped: %w", err)
		}
		if !fm.complete {
			return nil, corrupt("decmpfs", -1, "the resource fork of a compressed file is incomplete")
		}
		fork = rsrcFork{fm: fm, size: int64(rd.logicalSize)}
	}
	c, err := decmpfs.Open(o.attrs.value, fork)
	if err != nil {
		return nil, err
	}
	return contentFile{c}, nil
}
