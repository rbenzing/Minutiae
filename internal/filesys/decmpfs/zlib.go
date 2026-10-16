package decmpfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	MaxInlinePayload = 1 << 16 // longest attribute payload decoded (type 3)
	zlibBlockSize    = 1 << 16
	rsrcHeaderSize   = 0x100
	maxBlocks        = 1 << 20 // 64 GiB of content: the table is read in full
	maxStoredBlock   = 1 << 17 // longest compressed block accepted
	maxInflate       = 1 << 24 // most bytes one block or inline payload may inflate to
	// maxZlibRatio bounds uncompressed/compressed: deflate cannot exceed about
	// 1032:1, so a header that claims more is lying, whatever the stream says (a
	// decompression bomb is refused before anything is inflated).
	maxZlibRatio = 1100
)

// Content is decoded file content: not a byte range of the image, so it has no
// runs.
type Content interface {
	io.ReaderAt
	Size() int64
}

// Fork is the resource fork of the file: a byte range of known length. A read
// that cannot be completed must fail (the fork is all-or-error); an error
// wrapping filesys.ErrCorrupt is passed through unchanged.
type Fork interface {
	io.ReaderAt
	Size() int64
}

func corrupt(format string, a ...any) error {
	return &filesys.CorruptError{Structure: "decmpfs", Offset: -1, Reason: fmt.Sprintf(format, a...)}
}

// Open decodes a compressed file from the whole value of its
// com.apple.decmpfs attribute and, for type 4, its resource fork (nil for
// type 3). See the package comment for what is decoded.
func Open(attr []byte, rsrc Fork) (Content, error) {
	h, err := ParseHeader(attr)
	if err != nil {
		return nil, err
	}
	switch h.Type {
	case TypeZlibAttr:
		return openInline(h, attr[HeaderSize:])
	case TypeZlibRsrc:
		return openResource(h, rsrc)
	}
	return nil, &UnsupportedError{Type: h.Type, Size: h.Size, Reason: "only types 3 and 4 are implemented"}
}

// inflateBlock decodes one zlib block (or stored block) to exactly want bytes.
// It never allocates from want: the output buffer grows with the bytes the
// stream really produces, the read is capped at want+1, and want itself must be
// plausible for the input (below maxInflate and within maxZlibRatio of the
// compressed length) before anything is inflated.
func inflateBlock(src []byte, want int) ([]byte, error) {
	if len(src) == 0 {
		return nil, corrupt("an empty block")
	}
	if want < 0 {
		return nil, corrupt("a negative expected size")
	}
	if src[0]&0x0F == 0x0F { // stored
		if len(src)-1 != want {
			return nil, corrupt("a stored block of %d bytes where %d are expected", len(src)-1, want)
		}
		return src[1:], nil
	}
	if int64(want) > maxInflate || int64(want) > int64(len(src))*maxZlibRatio {
		return nil, corrupt("a block of %d compressed bytes cannot inflate to the %d bytes expected (bounds: %d bytes, ratio %d)", len(src), want, int64(maxInflate), maxZlibRatio)
	}
	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, corrupt("not a zlib stream: %v", err)
	}
	defer func() { _ = zr.Close() }()
	var out bytes.Buffer
	n, err := io.Copy(&out, io.LimitReader(zr, int64(want)+1)) // a clean EOF also verifies the checksum
	switch {
	case err != nil:
		return nil, corrupt("the stream is damaged after %d bytes: %v", n, err)
	case n < int64(want):
		return nil, corrupt("the stream holds %d bytes, fewer than the %d expected", n, want)
	case n > int64(want):
		return nil, corrupt("the stream holds more than the %d bytes expected", want)
	}
	return out.Bytes(), nil
}

// mem is decoded content held in memory.
type mem struct{ data []byte }

func (m *mem) Size() int64 { return int64(len(m.data)) }
func (m *mem) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("decmpfs: negative offset %d", off)
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func openInline(h Header, payload []byte) (Content, error) {
	if h.Size == 0 {
		return &mem{}, nil
	}
	if len(payload) > MaxInlinePayload {
		return nil, corrupt("an attribute payload of %d bytes is over the %d-byte bound", len(payload), MaxInlinePayload)
	}
	if h.Size > maxZlibRatio*uint64(len(payload)) {
		return nil, corrupt("a header claiming %d bytes from a %d-byte payload is implausible", h.Size, len(payload))
	}
	// h.Size <= 1100 x 64 KiB here, so it fits an int; inflateBlock refuses
	// anything over maxInflate before inflating.
	data, err := inflateBlock(payload, int(h.Size))
	if err != nil {
		return nil, err
	}
	return &mem{data: data}, nil
}

// readFull reads exactly len(p) bytes at off or fails; a short fork is corrupt.
func readFull(r Fork, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return corrupt("the resource fork ends at %d, inside a structure that needs %d bytes at %d", off+int64(n), len(p), off)
	}
	return err
}

// rsrcFile reads a type-4 file block by block from the resource fork.
type rsrcFile struct {
	fork   Fork
	size   int64
	blocks []struct{ off, n int64 } // fork offset and length of each compressed block

	mu       sync.Mutex
	cachedAt int64 // index of the cached block, -1 when none
	cached   []byte
}

func (z *rsrcFile) Size() int64 { return z.size }

func (z *rsrcFile) block(i int64) ([]byte, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.cachedAt == i {
		return z.cached, nil
	}
	b := z.blocks[i]
	src := make([]byte, b.n) // <= maxStoredBlock
	if err := readFull(z.fork, src, b.off); err != nil {
		return nil, err
	}
	want := int(min(zlibBlockSize, z.size-i*zlibBlockSize))
	out, err := inflateBlock(src, want)
	if err != nil {
		return nil, err
	}
	z.cachedAt, z.cached = i, out
	return out, nil
}

func (z *rsrcFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("decmpfs: negative offset %d", off)
	}
	if off >= z.size {
		return 0, io.EOF
	}
	var eof error
	if left := z.size - off; left < int64(len(p)) {
		p, eof = p[:left], io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		blk, err := z.block(cur / zlibBlockSize)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], blk[cur%zlibBlockSize:])
	}
	return n, eof
}

// openResource validates a type-4 file's block table against the resource fork
// and returns a lazily decoding file. Nothing is inflated before every table
// entry has been checked.
func openResource(h Header, fork Fork) (Content, error) {
	if h.Size == 0 {
		return &mem{}, nil
	}
	if fork == nil {
		return nil, corrupt("a type-4 file has no resource fork")
	}
	rl := fork.Size()
	if rl < 0 || rl > math.MaxInt64-rsrcHeaderSize {
		return nil, corrupt("a resource fork of %d bytes", rl)
	}
	if rl < rsrcHeaderSize+8 {
		return nil, corrupt("a resource fork of %d bytes is too small for the block table", rl)
	}
	var hdr [rsrcHeaderSize + 8]byte
	if err := readFull(fork, hdr[:], 0); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(hdr[:]) != rsrcHeaderSize {
		return nil, &UnsupportedError{Type: h.Type, Size: h.Size, Reason: "unrecognised resource fork layout"}
	}
	nb := binary.LittleEndian.Uint32(hdr[rsrcHeaderSize+4:])
	want := (h.Size + zlibBlockSize - 1) / zlibBlockSize
	if uint64(nb) != want {
		return nil, corrupt("the block table has %d entries but %d bytes need %d blocks", nb, h.Size, want)
	}
	if nb > maxBlocks {
		return nil, &UnsupportedError{Type: h.Type, Size: h.Size, Reason: fmt.Sprintf("%d blocks is over the %d-block bound", nb, maxBlocks)}
	}
	tableLen := 8 * int64(nb) // <= 8 MiB
	if rsrcHeaderSize+8+tableLen > rl {
		return nil, corrupt("the block table of %d entries runs past the %d-byte resource fork", nb, rl)
	}
	table := make([]byte, tableLen)
	if err := readFull(fork, table, rsrcHeaderSize+8); err != nil {
		return nil, err
	}
	z := &rsrcFile{fork: fork, size: int64(h.Size), cachedAt: -1}
	z.blocks = make([]struct{ off, n int64 }, nb)
	first := 4 + tableLen // the first byte after the table, relative to the block count
	var total uint64
	for i := range z.blocks {
		off := int64(binary.LittleEndian.Uint32(table[8*i:]))
		n := int64(binary.LittleEndian.Uint32(table[8*i+4:]))
		abs := rsrcHeaderSize + 4 + off
		if off < first || n == 0 || n > maxStoredBlock || abs+n > rl {
			return nil, corrupt("block %d (offset %d, %d bytes) does not lie inside the %d-byte resource fork after the table", i, off, n, rl)
		}
		z.blocks[i].off, z.blocks[i].n = abs, n
		total += uint64(n)
	}
	if h.Size > maxZlibRatio*total {
		return nil, corrupt("a header claiming %d bytes from %d compressed bytes is implausible", h.Size, total)
	}
	return z, nil
}
