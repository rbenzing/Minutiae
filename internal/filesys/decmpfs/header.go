package decmpfs

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Header and type numbers. See the package comment for their evidence.
const (
	HeaderSize          = 16
	TypeZlibAttr uint32 = 3
	TypeZlibRsrc uint32 = 4
	magic               = "fpmc"
)

// Header is the decoded 16-byte start of a com.apple.decmpfs attribute: the
// magic "fpmc", the compression type (u32) and the uncompressed size (u64),
// both little-endian.
type Header struct {
	Type uint32
	Size uint64
}

// HeaderError is a decmpfs attribute whose header cannot be read. It wraps
// filesys.ErrUnsupported: the file is compressed (its flag says so) but nothing
// can be decoded.
type HeaderError struct{ Reason string }

func (e *HeaderError) Error() string        { return "decmpfs: unusable header: " + e.Reason }
func (e *HeaderError) Is(target error) bool { return target == filesys.ErrUnsupported }

// UnsupportedError is a well-formed header of a type (or layout) that is not
// decoded. It wraps filesys.ErrUnsupported and carries what was found.
type UnsupportedError struct {
	Type   uint32
	Size   uint64
	Reason string
}

func (e *UnsupportedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("decmpfs: compression type %d is not decoded: %s", e.Type, e.Reason)
	}
	return fmt.Sprintf("decmpfs: compression type %d is not decoded", e.Type)
}

func (e *UnsupportedError) Is(target error) bool { return target == filesys.ErrUnsupported }

// ParseHeader decodes the header at the start of attr (the whole attribute
// value). A short value, another magic or a size above MaxInt64 is a
// *HeaderError.
func ParseHeader(attr []byte) (Header, error) {
	if len(attr) < HeaderSize {
		return Header{}, &HeaderError{Reason: fmt.Sprintf("%d bytes are shorter than the %d-byte header", len(attr), HeaderSize)}
	}
	if string(attr[:4]) != magic {
		return Header{}, &HeaderError{Reason: "the magic is not fpmc"}
	}
	size := binary.LittleEndian.Uint64(attr[8:])
	if size > math.MaxInt64 {
		return Header{}, &HeaderError{Reason: fmt.Sprintf("an uncompressed size of %d does not fit a signed 64-bit length", size)}
	}
	return Header{Type: binary.LittleEndian.Uint32(attr[4:]), Size: size}, nil
}

// Decodable reports whether this package can decode the type.
func (h Header) Decodable() bool { return h.Type == TypeZlibAttr || h.Type == TypeZlibRsrc }
