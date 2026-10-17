package decmpfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

type bytesFork []byte

func (b bytesFork) Size() int64 { return int64(len(b)) }
func (b bytesFork) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func zlibBytes(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	if _, err := w.Write(b); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func compressible(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = "hello, world "[i%13]
	}
	return b
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+i>>8*7) + seed
	}
	return b
}

// rsrcZlib builds the resource fork of a type-4 file the way the HFS+ reader's
// tests always did: a 0x100-byte header (big-endian 0x100 first), then the
// data size (big-endian), the block count (little-endian) and (offset, size)
// pairs (little-endian, relative to the block count), then the blocks.
func rsrcZlib(content []byte, raw ...int) []byte {
	const blk = 65536
	nb := (len(content) + blk - 1) / blk
	blocks := make([][]byte, nb)
	for i := range blocks {
		chunk := content[i*blk : min(len(content), (i+1)*blk)]
		if slices.Contains(raw, i) {
			blocks[i] = append([]byte{0xFF}, chunk...)
		} else {
			blocks[i] = zlibBytes(chunk)
		}
	}
	table := 4 + 8*nb
	dataSize := table
	for _, b := range blocks {
		dataSize += len(b)
	}
	out := make([]byte, 0x100+4+dataSize+50)
	binary.BigEndian.PutUint32(out[0:], 0x100)
	binary.BigEndian.PutUint32(out[4:], uint32(0x100+4+dataSize))
	binary.BigEndian.PutUint32(out[8:], uint32(dataSize))
	binary.BigEndian.PutUint32(out[12:], 0x32)
	binary.BigEndian.PutUint32(out[0x100:], uint32(dataSize))
	binary.LittleEndian.PutUint32(out[0x104:], uint32(nb))
	off := table
	for i, b := range blocks {
		binary.LittleEndian.PutUint32(out[0x108+8*i:], uint32(off))
		binary.LittleEndian.PutUint32(out[0x108+8*i+4:], uint32(len(b)))
		copy(out[0x104+off:], b)
		off += len(b)
	}
	return out
}

func readAll(t *testing.T, c Content) []byte {
	t.Helper()
	buf := make([]byte, c.Size())
	n, err := c.ReadAt(buf, 0)
	if n != len(buf) || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt(all %d) = %d, %v", len(buf), n, err)
	}
	return buf
}

func TestInlineZlib(t *testing.T) {
	content := compressible(5000)
	raw := []byte("stored as is")
	for name, tc := range map[string]struct {
		attr []byte
		want []byte
	}{
		"zlib":   {value(3, 5000, zlibBytes(content)), content},
		"stored": {value(3, uint64(len(raw)), append([]byte{0xFF}, raw...)), raw},
		"empty":  {value(3, 0, zlibBytes(nil)), nil},
	} {
		c, err := Open(tc.attr, nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if c.Size() != int64(len(tc.want)) || !bytes.Equal(readAll(t, c), tc.want) {
			t.Errorf("%s: wrong content", name)
		}
	}
	c, _ := Open(value(3, 5000, zlibBytes(content)), nil)
	buf := make([]byte, 100)
	if n, err := c.ReadAt(buf, 4950); n != 50 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:50], content[4950:]) {
		t.Errorf("ReadAt near the end = %d, %v", n, err)
	}
	if n, err := c.ReadAt(buf, 5000); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := c.ReadAt(buf, -1); err == nil {
		t.Error("a negative offset must fail")
	}
}

func TestInlineZlibLiesAreCorrupt(t *testing.T) {
	content := compressible(5000)
	raw := []byte("stored as is")
	for name, attr := range map[string][]byte{
		"claims more than the stream holds": value(3, 9000, zlibBytes(content)),
		"stream holds more than claimed":    value(3, 100, zlibBytes(content)),
		"implausible size":                  value(3, 1<<45, zlibBytes(content)),
		"stored length differs":             value(3, 3, append([]byte{0xFF}, raw...)),
		"not a zlib stream":                 value(3, 10, []byte("not zlib at all")),
		"empty payload":                     value(3, 10, nil),
	} {
		c, err := Open(attr, nil)
		if !errors.Is(err, filesys.ErrCorrupt) || c != nil {
			t.Errorf("%s: Open = %v, %v; want ErrCorrupt", name, c, err)
		}
	}
}

func TestInlinePayloadOverTheBoundIsCorrupt(t *testing.T) {
	big := value(3, 1<<20, make([]byte, MaxInlinePayload+1))
	if c, err := Open(big, nil); !errors.Is(err, filesys.ErrCorrupt) || c != nil {
		t.Errorf("a payload over %d bytes: %v, %v", MaxInlinePayload, c, err)
	}
}

// totalAlloc runs fn and returns the bytes the program allocated meanwhile.
func totalAlloc(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A header may declare any size; the decoder must not allocate from it (ruling
// Y20b: the shipped code did make([]byte, want)).
func TestInflateBlockDoesNotAllocateFromTheDeclaredSize(t *testing.T) {
	const declared = 10 << 20                // plausible for the input (ratio and ceiling), far more than the stream holds
	payload := zlibBytes(pattern(10<<10, 5)) // ~10 KiB of incompressible bytes: ratio bound is 11 MB
	var err error
	if n := totalAlloc(func() { _, err = inflateBlock(payload, declared) }); n > 1<<20 {
		t.Errorf("inflateBlock allocated %d bytes for a stream of 10 KiB declared as %d", n, declared)
	}
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a stream shorter than declared: %v, want ErrCorrupt", err)
	}
	// An absurd declared size is refused before anything is inflated or allocated.
	if n := totalAlloc(func() { _, err = inflateBlock(zlibBytes(compressible(100)), 1<<40) }); n > 1<<20 {
		t.Errorf("a declared size of 1<<40 allocated %d bytes", n)
	}
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("an implausible declared size: %v, want ErrCorrupt", err)
	}
	// Through Open: a tiny attribute with a huge declared size. (Assertion 3,
	// this one, is refused by the ratio check in openInline, not by the
	// allocation bound; assertions 1 and 2 exercise inflateBlock's own bounds.)
	if n := totalAlloc(func() { _, err = Open(value(3, 1<<40, zlibBytes(compressible(100))), nil) }); n > 1<<20 {
		t.Errorf("Open allocated %d bytes for a declared size of 1<<40", n)
	}
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("Open with an implausible declared size: %v, want ErrCorrupt", err)
	}
}

// A stream with more bytes than declared is cut at declared+1 and refused; a
// stream that inflates to the ceiling is not read past it.
func TestInflateBlockOverrunAndCeiling(t *testing.T) {
	content := compressible(5000)
	if _, err := inflateBlock(zlibBytes(content), 4000); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a stream longer than declared: %v, want ErrCorrupt", err)
	}
	if got, err := inflateBlock(zlibBytes(content), 5000); err != nil || !bytes.Equal(got, content) {
		t.Errorf("an exact stream: %d bytes, %v", len(got), err)
	}
	// The ceiling is pinned exactly (ruling Y62): a stream that really holds
	// maxInflate+1 bytes, declared as that, is refused before it is inflated;
	// one of exactly maxInflate bytes is accepted. Both compress far below the
	// ratio bound's input need, so only the ceiling can refuse the first.
	over := zlibBytes(make([]byte, maxInflate+1))
	if _, err := inflateBlock(over, maxInflate+1); !errors.Is(err, filesys.ErrCorrupt) {
		t.Errorf("a block of maxInflate+1 bytes: %v, want ErrCorrupt", err)
	}
	if got, err := inflateBlock(zlibBytes(make([]byte, maxInflate)), maxInflate); err != nil || len(got) != maxInflate {
		t.Errorf("a block of exactly maxInflate bytes: %d bytes, %v; want accepted", len(got), err)
	}
	if c, err := Open(value(3, maxInflate+1, over), nil); !errors.Is(err, filesys.ErrCorrupt) || c != nil {
		t.Errorf("an inline payload of maxInflate+1 bytes: content %t, %v; want ErrCorrupt", c != nil, err)
	}
}

func TestResourceZlib(t *testing.T) {
	big := make([]byte, 3*65536+777)
	copy(big, compressible(65536))
	copy(big[65536:], pattern(65536, 3))
	copy(big[2*65536:], compressible(65536))
	copy(big[3*65536:], pattern(777, 9))
	c, err := Open(value(4, uint64(len(big)), nil), bytesFork(rsrcZlib(big, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Size() != int64(len(big)) || !bytes.Equal(readAll(t, c), big) {
		t.Fatal("big content differs")
	}
	buf := make([]byte, 70000)
	if n, err := c.ReadAt(buf, 60000); n != len(buf) || err != nil || !bytes.Equal(buf, big[60000:130000]) {
		t.Errorf("read across blocks = %d, %v", n, err)
	}
	if n, err := c.ReadAt(buf[:500], int64(len(big))-100); n != 100 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:100], big[len(big)-100:]) {
		t.Errorf("read across the end = %d, %v", n, err)
	}
	if c, err := Open(value(4, 0, nil), bytesFork(nil)); err != nil || c.Size() != 0 {
		t.Errorf("an empty type-4 file: %v, %v", c, err)
	}
}

func TestResourceZlibDamageIsCorrupt(t *testing.T) {
	big := make([]byte, 3*65536+777)
	copy(big, compressible(65536))
	readErr := func(attr []byte, fork Fork) error {
		c, err := Open(attr, fork)
		if err == nil {
			_, err = c.ReadAt(make([]byte, c.Size()), 0) // the damage may only show on read
		}
		return err
	}
	for name, tc := range map[string]struct {
		size uint64
		fork Fork
	}{
		"size needs another block":    {uint64(len(big)) + 65536, bytesFork(rsrcZlib(big))},
		"last block shorter":          {uint64(len(big)) + 100, bytesFork(rsrcZlib(big))},
		"no resource fork":            {uint64(len(big)), bytesFork(nil)},
		"nil fork":                    {uint64(len(big)), nil},
		"short last block":            {70000, bytesFork(rsrcZlib(big[:70000-100]))},
		"fork smaller than the table": {uint64(len(big)), bytesFork(make([]byte, 0x105))},
	} {
		if err := readErr(value(4, tc.size, nil), tc.fork); !errors.Is(err, filesys.ErrCorrupt) {
			t.Errorf("%s: err = %v; want ErrCorrupt", name, err)
		}
	}
	var ue *UnsupportedError
	err := readErr(value(4, 400, nil), bytesFork(append([]byte{0, 0, 0, 1}, make([]byte, 400)...)))
	if !errors.As(err, &ue) || ue.Type != 4 || !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("an unrecognised resource fork layout: %v; want *UnsupportedError{Type: 4}", err)
	}
}

func TestResourceBlockTableHostile(t *testing.T) {
	content := compressible(70000)
	good := rsrcZlib(content)
	cases := map[string]func(b []byte){
		"block count huge":     func(b []byte) { binary.LittleEndian.PutUint32(b[0x104:], 0xFFFFFFF0) },
		"block count wrong":    func(b []byte) { binary.LittleEndian.PutUint32(b[0x104:], 5) },
		"offset past the fork": func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 0x7FFFFFF0) },
		"size past the fork":   func(b []byte) { binary.LittleEndian.PutUint32(b[0x10C:], 0x7FFFFFF0) },
		"offset overflows":     func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 0xFFFFFFFF) },
		"offset inside table":  func(b []byte) { binary.LittleEndian.PutUint32(b[0x108:], 2) },
		"zero-size block":      func(b []byte) { binary.LittleEndian.PutUint32(b[0x10C:], 0) },
		"garbage block": func(b []byte) {
			off := int(binary.LittleEndian.Uint32(b[0x108:])) + 0x104
			for i := range 8 {
				b[off+i] ^= 0x5A
			}
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			rs := slices.Clone(good)
			patch(rs)
			c, err := Open(value(4, uint64(len(content)), nil), bytesFork(rs))
			if err == nil {
				_, err = c.ReadAt(make([]byte, c.Size()), 0)
			}
			if !errors.Is(err, filesys.ErrCorrupt) {
				t.Errorf("err = %v; want ErrCorrupt", err)
			}
		})
	}
}

func TestOtherTypesAreUnsupportedWithTheirType(t *testing.T) {
	for _, typ := range []uint32{1, 2, 5, 6, 7, 8, 9, 10, 11, 12, 99} {
		c, err := Open(value(typ, 1234, []byte("xxxx")), bytesFork(make([]byte, 400)))
		var ue *UnsupportedError
		if !errors.As(err, &ue) || ue.Type != typ || ue.Size != 1234 || c != nil || !errors.Is(err, filesys.ErrUnsupported) {
			t.Errorf("type %d: Open = %v, %v; want *UnsupportedError{Type: %d}", typ, c, err, typ)
		}
	}
	if c, err := Open([]byte("not a header"), nil); !errors.Is(err, filesys.ErrUnsupported) || c != nil {
		t.Errorf("a bad header: %v, %v", c, err)
	}
}

func FuzzOpen(f *testing.F) {
	f.Add(value(3, 5, zlibBytes([]byte("hello"))), []byte(nil))
	f.Add(value(4, 70000, nil), rsrcZlib(compressible(70000)))
	f.Add(value(7, 9, []byte("lzvn")), []byte(nil))
	f.Fuzz(func(t *testing.T, attr, fork []byte) {
		c, err := Open(attr, bytesFork(fork))
		if err != nil {
			return
		}
		if c.Size() < 0 || c.Size() > 1100*int64(len(attr)+len(fork)+1) {
			t.Fatalf("size %d out of proportion to %d+%d input bytes", c.Size(), len(attr), len(fork))
		}
		buf := make([]byte, min(c.Size(), 1<<20))
		_, _ = c.ReadAt(buf, 0)
	})
}
