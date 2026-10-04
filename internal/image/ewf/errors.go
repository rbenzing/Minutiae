package ewf

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrCorrupt marks an image whose structure cannot be trusted (bad
	// checksum, inconsistent section chain, impossible geometry ...).
	ErrCorrupt = errors.New("ewf: corrupt image")
	// ErrChunkCorrupt marks a read that hit a chunk that cannot be decoded.
	ErrChunkCorrupt = errors.New("ewf: corrupt chunk")
	// ErrUnsupported marks an EWF variant this reader does not handle.
	ErrUnsupported = errors.New("ewf: unsupported")
)

// CorruptError describes where an image is structurally broken. Segment is
// the 1-based segment file number, or 0 for an image-level problem. It
// matches ErrCorrupt with errors.Is.
type CorruptError struct {
	Segment int
	Section string
	Reason  string
}

func (e *CorruptError) Error() string {
	var b strings.Builder
	b.WriteString("ewf: corrupt image")
	if e.Segment > 0 {
		fmt.Fprintf(&b, ": segment %d", e.Segment)
	}
	if e.Section != "" {
		fmt.Fprintf(&b, ", section %q", e.Section)
	}
	b.WriteString(": ")
	b.WriteString(e.Reason)
	return b.String()
}

// Unwrap makes errors.Is(err, ErrCorrupt) true.
func (e *CorruptError) Unwrap() error { return ErrCorrupt }

// ChunkError is the error of a read that could not decode one chunk. It
// matches ErrChunkCorrupt, and the underlying cause (a checksum or
// zlib failure, a segment that ends early) with errors.Is. An I/O error of the
// segment source is not a ChunkError.
type ChunkError struct {
	Chunk   int64
	Segment int
	Offset  int64
	Err     error
}

func (e *ChunkError) Error() string {
	return fmt.Sprintf("ewf: chunk %d (segment %d, offset %d): %v", e.Chunk, e.Segment, e.Offset, e.Err)
}

// Unwrap returns ErrChunkCorrupt and the cause.
func (e *ChunkError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrChunkCorrupt}
	}
	return []error{ErrChunkCorrupt, e.Err}
}

func corrupt(seg int, section, format string, args ...any) error {
	return &CorruptError{Segment: seg, Section: section, Reason: fmt.Sprintf(format, args...)}
}

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}
