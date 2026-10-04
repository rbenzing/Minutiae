package apfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

var le = binary.LittleEndian

// Object types (obj_phys_t.o_type & 0xffff). The storage flags (the high
// bits: ephemeral 0x80000000, physical 0x40000000) are not interpreted yet.
const (
	typeMask = 0xffff

	typeNXSuperblock  = 0x1
	typeSpaceman      = 0x5
	typeOmap          = 0xb
	typeCheckpointMap = 0xc

	// anyType makes readObject accept every object type.
	anyType = ^uint32(0)

	// maxObjectBytes bounds one object read (the largest ephemeral objects
	// are a few blocks; this only stops a hostile size from driving an
	// allocation).
	maxObjectBytes = 16 << 20
)

// objHeader is obj_phys_t.
type objHeader struct {
	cksum, oid, xid uint64
	typ, subtype    uint32
}

// kind is the object type without the storage flags.
func (h objHeader) kind() uint32 { return h.typ & typeMask }

func parseHeader(b []byte) objHeader {
	return objHeader{
		cksum:   le.Uint64(b[0:]),
		oid:     le.Uint64(b[8:]),
		xid:     le.Uint64(b[16:]),
		typ:     le.Uint32(b[24:]),
		subtype: le.Uint32(b[28:]),
	}
}

const fletcherModulus = 0xFFFFFFFF

// Fletcher64 returns the checksum an object must store in its first 8 bytes:
// over b[8:] as little-endian 32-bit words (a trailing partial word is
// ignored), sum1 += w, sum2 += sum1, both modulo 0xFFFFFFFF; then
// c1 = M - (sum1+sum2)%M, c2 = M - (sum1+c1)%M and the value is c2<<32 | c1.
func Fletcher64(b []byte) uint64 {
	var sum1, sum2 uint64
	if len(b) > 8 {
		body := b[8:]
		words := len(body) / 4
		// Reduce every 1024 words: sum1 stays below 2^43 and sum2 below 2^53
		// in between, far from uint64 overflow.
		for i := 0; i < words; {
			n := min(words-i, 1024)
			for j := range n {
				sum1 += uint64(le.Uint32(body[4*(i+j):]))
				sum2 += sum1
			}
			sum1 %= fletcherModulus
			sum2 %= fletcherModulus
			i += n
		}
	}
	c1 := fletcherModulus - (sum1+sum2)%fletcherModulus
	c2 := fletcherModulus - (sum1+c1)%fletcherModulus
	return c2<<32 | c1
}

// checksumOK reports whether b (a whole object: header included) carries a
// correct checksum. A block of zeros does not verify.
func checksumOK(b []byte) bool {
	if len(b) < 12 || len(b)%4 != 0 {
		return false
	}
	return le.Uint64(b) == Fletcher64(b)
}

// errChecksum is matched (errors.Is) by every checksum mismatch.
var errChecksum = errors.New("apfs: object checksum mismatch")

// checksumError reports an object whose Fletcher-64 does not verify. Callers
// decide whether that is a warning (skip the region it governs) or fatal.
type checksumError struct{ paddr uint64 }

func (e *checksumError) Error() string {
	return fmt.Sprintf("apfs: object checksum mismatch at block %d", e.paddr)
}

// Is makes a checksum mismatch match both errChecksum and filesys.ErrCorrupt.
func (e *checksumError) Is(target error) bool {
	return target == errChecksum || target == filesys.ErrCorrupt
}

// isShort reports a read that ran past the end of the image.
func isShort(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// readObject reads the object of size bytes at block paddr, verifies its
// Fletcher-64 and, unless want is anyType, its type. size must be a positive
// multiple of the block size inside the container. A checksum mismatch is a
// *checksumError; a wrong type or bad bounds a *filesys.CorruptError; read
// failures are returned wrapped as they are (a short read matches isShort).
func (f *FS) readObject(paddr uint64, size int, want uint32) ([]byte, objHeader, error) {
	if size <= 0 || size > maxObjectBytes || size%f.bs != 0 {
		return nil, objHeader{}, corrupt("object", -1, "object at block %d has unusable size %d (block size %d)", paddr, size, f.bs)
	}
	nblk := uint64(size / f.bs)
	if paddr >= f.blocks || nblk > f.blocks-paddr {
		return nil, objHeader{}, corrupt("object", -1, "object at block %d (%d blocks) lies outside the container of %d blocks", paddr, nblk, f.blocks)
	}
	off := int64(paddr) * int64(f.bs) // paddr < blocks, and blocks*bs fits int64 (validated)
	buf := make([]byte, size)
	if err := readFull(f.r, buf, off); err != nil {
		return nil, objHeader{}, fmt.Errorf("apfs: read object at block %d: %w", paddr, err)
	}
	if !checksumOK(buf) {
		return nil, objHeader{}, &checksumError{paddr: paddr}
	}
	h := parseHeader(buf)
	if want != anyType && h.kind() != want {
		return nil, objHeader{}, corrupt("object", off, "object at block %d has type %#x, want %#x", paddr, h.kind(), want)
	}
	return buf, h, nil
}
