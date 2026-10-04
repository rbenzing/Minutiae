package hfsplus

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// decmpfs (transparent compression). The com.apple.decmpfs attribute holds a
// 16-byte header (magic "fpmc", compression type u32 and uncompressed size u64,
// both little-endian) and, for type 3, the zlib data itself. Type 4 keeps the
// zlib blocks in the resource fork: a 0x100-byte header whose first big-endian
// word is 0x100, then (at 0x100) a big-endian data size, a little-endian block
// count at 0x104 and a table of (offset, size) little-endian pairs from 0x108,
// offsets relative to 0x104; each block decodes to 64 KiB (the last one less).
// A block (or an inline payload) whose first byte has its low four bits set
// (0xFF) is stored uncompressed after that byte. All of this is from memory of
// the format and covered by builder images only: no real compressed file was
// available, so anything that does not fit it is reported, not guessed at.
// Other types (LZVN, LZFSE, ...) are unsupported.
const (
	decmpfsHeaderSize = 16
	decmpfsTypeInline = 3 // zlib data in the attribute
	decmpfsTypeRsrc   = 4 // zlib blocks in the resource fork

	zlibBlockSize    = 1 << 16
	rsrcHeaderSize   = 0x100
	maxDecmpfsBlocks = 1 << 20 // 64 GiB of content: the table is read in full
	maxStoredBlock   = 1 << 17 // longest compressed block accepted
	// maxZlibRatio bounds uncompressed/compressed: deflate cannot exceed about
	// 1032:1, so a header that claims more than this is lying, whatever the
	// stream says (a decompression bomb is refused before anything is inflated).
	maxZlibRatio = 1100
)

// decmpfsHeader is a decoded decmpfs attribute header; ok is false when the
// attribute is missing or its header unusable.
type decmpfsHeader struct {
	ok   bool
	typ  uint32
	size uint64
}

func parseDecmpfs(v []byte) decmpfsHeader {
	if len(v) < decmpfsHeaderSize || string(v[:4]) != "fpmc" {
		return decmpfsHeader{}
	}
	size := binary.LittleEndian.Uint64(v[8:])
	if size > math.MaxInt64 {
		return decmpfsHeader{}
	}
	return decmpfsHeader{ok: true, typ: binary.LittleEndian.Uint32(v[4:]), size: size}
}

// label names the compression for the "compressed" attribute.
func (h decmpfsHeader) label() string {
	switch {
	case !h.ok:
		return "unknown"
	case h.typ == decmpfsTypeInline || h.typ == decmpfsTypeRsrc:
		return "zlib"
	}
	return strconv.FormatUint(uint64(h.typ), 10)
}

// inflateBlock decodes one zlib block (or stored block) to exactly want bytes.
// Anything else - a bad stream, a short or long result, a failed checksum - is
// a CorruptError. The output is never larger than want.
func inflateBlock(src []byte, want int) ([]byte, error) {
	if len(src) == 0 {
		return nil, corrupt("decmpfs block", -1, "an empty block")
	}
	if src[0]&0x0F == 0x0F { // stored
		if len(src)-1 != want {
			return nil, corrupt("decmpfs block", -1, "a stored block of %d bytes where %d are expected", len(src)-1, want)
		}
		return src[1:], nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, corrupt("decmpfs block", -1, "not a zlib stream: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out := make([]byte, want)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, corrupt("decmpfs block", -1, "the stream holds fewer than the %d bytes expected: %v", want, err)
	}
	var one [1]byte
	if n, err := zr.Read(one[:]); n != 0 || !errors.Is(err, io.EOF) { // EOF also verifies the checksum
		if err == nil || errors.Is(err, io.EOF) {
			err = errors.New("more data than the size in the header")
		}
		return nil, corrupt("decmpfs block", -1, "the stream does not end after the %d bytes expected: %v", want, err)
	}
	return out, nil
}

// memFile is the decoded content of an inline compressed file: not a byte
// range of the image, so no runs.
type memFile struct{ data []byte }

func (m *memFile) Size() int64         { return int64(len(m.data)) }
func (m *memFile) Runs() []filesys.Run { return nil }
func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative offset %d", off)
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

// openZlibInline decodes a type-3 file: zlib data in the attribute.
func (f *FS) openZlibInline(o *object) (filesys.File, error) {
	h := o.cmp
	payload := o.attrs.value[decmpfsHeaderSize:]
	if h.size == 0 {
		return &memFile{}, nil
	}
	if h.size > maxZlibRatio*uint64(len(payload)) {
		return nil, corrupt("decmpfs", -1, "a header claiming %d bytes from a %d-byte payload is implausible", h.size, len(payload))
	}
	data, err := inflateBlock(payload, int(h.size)) // <= 1100 x 64 KiB: fits
	if err != nil {
		return nil, err
	}
	return &memFile{data: data}, nil
}

// zlibRsrcFile reads a type-4 file block by block from the resource fork.
type zlibRsrcFile struct {
	fm     *forkMap
	size   int64
	blocks []struct{ off, n int64 } // fork-relative offset and length of each compressed block

	mu       sync.Mutex
	cachedAt int64 // index of the cached block, -1 when none
	cached   []byte
}

func (z *zlibRsrcFile) Size() int64         { return z.size }
func (z *zlibRsrcFile) Runs() []filesys.Run { return nil }

func (z *zlibRsrcFile) block(i int64) ([]byte, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.cachedAt == i {
		return z.cached, nil
	}
	b := z.blocks[i]
	src := make([]byte, b.n) // <= maxStoredBlock
	if err := readForkFull(z.fm, src, b.off); err != nil {
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

func (z *zlibRsrcFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative offset %d", off)
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

// openZlibRsrc validates a type-4 file's block table and returns a lazily
// decoding file. The table, offsets and sizes are all checked against the
// resource fork before anything is inflated.
func (f *FS) openZlibRsrc(o *object) (filesys.File, error) {
	h := o.cmp
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
	rl := int64(rd.logicalSize)
	if h.size == 0 {
		return &memFile{}, nil
	}
	if rl < rsrcHeaderSize+8 {
		return nil, corrupt("decmpfs", -1, "a resource fork of %d bytes is too small for the block table", rl)
	}
	var hdr [rsrcHeaderSize + 8]byte
	if err := readForkFull(fm, hdr[:], 0); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(hdr[:]) != rsrcHeaderSize {
		return nil, fmt.Errorf("%w: decmpfs-compressed file (unrecognised resource fork layout)", filesys.ErrUnsupported)
	}
	nb := binary.LittleEndian.Uint32(hdr[rsrcHeaderSize+4:])
	want := (h.size + zlibBlockSize - 1) / zlibBlockSize
	if uint64(nb) != want {
		return nil, corrupt("decmpfs", -1, "the block table has %d entries but %d bytes need %d blocks", nb, h.size, want)
	}
	if nb > maxDecmpfsBlocks {
		return nil, fmt.Errorf("%w: decmpfs-compressed file too large (%d blocks)", filesys.ErrUnsupported, nb)
	}
	tableLen := 8 * int64(nb) // <= 8 MiB
	if rsrcHeaderSize+8+tableLen > rl {
		return nil, corrupt("decmpfs", -1, "the block table of %d entries runs past the %d-byte resource fork", nb, rl)
	}
	table := make([]byte, tableLen)
	if err := readForkFull(fm, table, rsrcHeaderSize+8); err != nil {
		return nil, err
	}
	z := &zlibRsrcFile{fm: fm, size: int64(h.size), cachedAt: -1}
	z.blocks = make([]struct{ off, n int64 }, nb)
	first := 4 + tableLen // the first byte after the table, relative to the block count
	var total uint64
	for i := range z.blocks {
		off := int64(binary.LittleEndian.Uint32(table[8*i:]))
		n := int64(binary.LittleEndian.Uint32(table[8*i+4:]))
		abs := rsrcHeaderSize + 4 + off
		if off < first || n == 0 || n > maxStoredBlock || abs+n > rl {
			return nil, corrupt("decmpfs", -1, "block %d (offset %d, %d bytes) does not lie inside the %d-byte resource fork after the table", i, off, n, rl)
		}
		z.blocks[i].off, z.blocks[i].n = abs, n
		total += uint64(n)
	}
	if h.size > maxZlibRatio*total {
		return nil, corrupt("decmpfs", -1, "a header claiming %d bytes from %d compressed bytes is implausible", h.size, total)
	}
	return z, nil
}
