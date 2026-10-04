// Package apfs is a read-only APFS parser. It works on an io.ReaderAt, never
// writes, and never panics on hostile input: every on-disk number is
// validated before it drives a loop or an allocation. It implements the
// filesys interfaces and imports no other Minutiae package.
//
// The container is mounted from its newest valid checkpoint. Block 0 (a copy
// of a possibly stale superblock) supplies only the block size and the
// location of the checkpoint descriptor area; the checkpoint itself is the
// NX_SUPERBLOCK object with the largest xid in that ring whose checksum,
// checkpoint-mapping blocks and ephemeral objects (the space manager) all
// verify. A newer checkpoint that does not verify is skipped with a warning
// and the next older one is used. Older checkpoints are never analysed here
// (deleted-data recovery is roadmap sub-project 3).
//
// Layout facts tagged as unverified in the project plan are noted in the code
// where they are used; real containers made by an independent tool are the
// check on them.
package apfs

import (
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlocks = 256 // blocks held by the metadata cache

	// probeSize is what Probe and Open read first: the object header and the
	// start of the superblock all fit in the smallest block.
	probeSize = minBlockSize
)

// FS is an opened APFS container. After Open it is safe for concurrent use:
// the only mutable state is the warning list, which is mutex-protected.
type FS struct {
	r    io.ReaderAt // cached view of the container for metadata, clamped to size
	data io.ReaderAt // uncached view for file content, clamped to size
	size int64       // container size in bytes (declared size clamped to the image)

	bs     int    // container block size
	blocks uint64 // declared block count (bounds every block address)

	nx nxSuper    // the selected checkpoint's superblock
	cp checkpoint // the selected checkpoint

	nodeBudget int       // nodes one B-tree scan may read
	cmap       *omapView // the container object map (virtual oid to block)

	warns filesys.Warnings
}

// readFull reads exactly len(p) bytes at off; a read that returns all the
// bytes together with io.EOF succeeds.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

// warn records a problem that does not stop the read (see filesys.Warnings:
// identical messages once, at most filesys.MaxWarnings distinct ones). On-disk
// strings must be %q-quoted by the caller.
func (f *FS) warn(format string, a ...any) { f.warns.Add(format, a...) }

// Probe reports whether r starts with an APFS container superblock: the
// magic "NXSB" at offset 32, the NX_SUPERBLOCK object type and a block size
// that is a power of two in [4096, 65536]. It reads nothing from an image of fewer than 4096 bytes.
func Probe(r io.ReaderAt, size int64) bool {
	if size < probeSize {
		return false
	}
	var b [40]byte
	if err := readFull(r, b[:], 0); err != nil {
		return false
	}
	return string(b[32:36]) == nxMagic && validBlockSize(le.Uint32(b[36:])) && parseHeader(b[:]).kind() == typeNXSuperblock
}

// Open mounts the container in r (size bytes) from its newest valid
// checkpoint. It fails with a *filesys.CorruptError when block 0 or the
// checkpoint area cannot be laid out or no checkpoint verifies, and with an
// error wrapping filesys.ErrUnsupported for Fusion, version 1 and
// non-contiguous checkpoint areas. A bad checksum on block 0, a rejected
// newer checkpoint, a container that claims more than the image holds and
// unknown feature bits are reported through Info().Warnings.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	if size < probeSize {
		return nil, corrupt("container superblock", 0, "image of %d bytes is too small for a container superblock", size)
	}
	raw := io.NewSectionReader(r, 0, size)
	first := make([]byte, probeSize)
	if err := readFull(raw, first, 0); err != nil {
		return nil, fmt.Errorf("apfs: read block 0: %w", err)
	}
	if string(first[32:36]) != nxMagic {
		return nil, corrupt("container superblock", 32, "bad magic %q, want %q", first[32:36], nxMagic)
	}
	nx0 := parseNX(first)
	if err := nx0.validateGeometry(); err != nil {
		return nil, err
	}
	f := &FS{
		r:      filesys.NewCachedReader(raw, int(nx0.blockSize), cacheBlocks),
		data:   raw,
		bs:     int(nx0.blockSize),
		blocks: nx0.blockCount,
	}
	f.checkBlockZero()

	nx, cp, err := f.selectCheckpoint(&nx0)
	if err != nil {
		return nil, err
	}
	switch {
	case nx.incompat&incompatFusion != 0:
		return nil, unsupported("Fusion container")
	case nx.incompat&incompatVersion1 != 0:
		return nil, unsupported("version 1 (pre-release) container")
	}
	if u := nx.incompat &^ knownIncompat; u != 0 {
		f.warn("container has unknown incompatible feature bits %#x", u)
	}
	f.nx, f.cp = nx, cp
	f.blocks = nx.blockCount

	// validate checked that blockCount x blockSize fits an int64.
	declared := int64(nx.blockCount) * int64(nx.blockSize)
	f.size = declared
	if declared > size {
		f.warn("container declares %d bytes (%d blocks) but the image holds %d: truncated image, size clamped", declared, nx.blockCount, size)
		f.size = size
	}
	vol := io.NewSectionReader(r, 0, f.size)
	f.r = filesys.NewCachedReader(vol, f.bs, cacheBlocks)
	f.data = vol
	f.nodeBudget = int(min(uint64(maxNodeBudget), f.blocks))
	if nx.omapOid == 0 {
		return nil, corrupt("container superblock", 160, "the container has no object map")
	}
	om, err := f.openOmap(nx.omapOid)
	if err != nil {
		return nil, err
	}
	f.cmap = om
	return f, nil
}

// checkBlockZero reports a block 0 that does not verify. It is never trusted
// for anything but the block size and the area location, so this is only a
// warning.
func (f *FS) checkBlockZero() {
	buf := make([]byte, f.bs)
	if err := readFull(f.r, buf, 0); err != nil {
		f.warn("block 0 could not be read in full (truncated image): its checksum was not verified")
		return
	}
	if !checksumOK(buf) {
		f.warn("block 0 checksum mismatch: the checkpoint ring is used instead")
		return
	}
	if parseHeader(buf).kind() != typeNXSuperblock {
		f.warn("block 0 is not an NX_SUPERBLOCK object")
	}
}

// Info describes the container. Warnings are live: they include everything
// reported so far, also by reads made after Open. Volumes are filled in once
// the volume layer exists.
func (f *FS) Info() filesys.Info {
	return filesys.Info{
		Type:      "apfs",
		UUID:      formatUUID(f.nx.uuid),
		BlockSize: f.bs,
		Size:      f.size,
		Features:  f.nx.featureNames(),
		Warnings:  f.warns.Snapshot(),
	}
}
