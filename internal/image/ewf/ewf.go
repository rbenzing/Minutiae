// Package ewf reads EWF version 1 (E01) forensic images, multi-segment, as a
// read-only io.ReaderAt over the original media.
//
// The package is pure: it imports no Minutiae package and works on
// []Segment values (an io.ReaderAt plus its size) so that tests and fuzzers
// can use in-memory bytes. Every number read from a segment file is
// validated before it drives a loop or an allocation; no input may panic.
//
// The on-disk layout (segment header, section descriptors, volume, table,
// hash, digest, error2, header/header2) is implemented from the public EWF-E01
// format notes and validated against real acquisition output by the fixture
// tests; where a field offset is from memory the code says so.
package ewf

import (
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Hard caps on hostile input.
const (
	maxSegments     = 65535
	maxSections     = 1 << 20 // per segment; also bounded by fileSize/descLen
	maxChunks       = 1 << 26
	maxChunkSize    = 16 << 20
	maxErrorRanges  = 1 << 16
	maxHeaderText   = 1 << 20 // decompressed header/header2 text
	maxHeaderZ      = 2 << 20 // compressed header/header2 bytes read
	maxMetaValue    = 256     // bytes per metadata value
	maxHeaderFields = 256
	maxWarnings     = 1000
	maxUnknownKinds = 16
)

// On-disk sizes.
const (
	segHeaderLen = 13
	descLen      = 76
)

// KV is one ordered metadata key/value pair.
type KV struct{ Key, Value string }

// Segment is one segment file of an image set.
type Segment struct {
	Name string // file base name, used in Metadata only
	R    io.ReaderAt
	Size int64
}

// Reader is an opened E01 image set. It is safe for concurrent use.
type Reader struct {
	segs []Segment
	secs [][]section // every section of every segment, in file order
	geo  geometry
	meta []KV
	md5  string // stored MD5 (lower-case hex), "" when absent
	sha1 string // stored SHA-1, "" when absent
	warn warnings
	err2 *error2Info // acquisition error ranges, nil when none

	mu     sync.Mutex
	closed bool
}

// Size returns the logical media size in bytes (from the volume section).
func (r *Reader) Size() int64 { return r.geo.size }

// SectorSize returns the bytes per sector the volume section declares.
func (r *Reader) SectorSize() int { return int(r.geo.bps) }

// ChunkSize returns the bytes per chunk (sectors_per_chunk * bytes_per_sector).
func (r *Reader) ChunkSize() int { return int(r.geo.chunkSize) }

// Chunks returns the number of chunks of the media.
func (r *Reader) Chunks() int64 { return int64(r.geo.chunks) }

// Metadata returns the ordered container fields: header text, geometry,
// stored hashes and segment names.
func (r *Reader) Metadata() []KV { return slices.Clone(r.meta) }

// Warnings returns a snapshot of the non-fatal problems met so far (a live
// view: reading can add more). It is nil when there are none.
func (r *Reader) Warnings() []string { return r.warn.snapshot() }

// Close releases the reader's internal state. It is idempotent and does not
// close the segments (the caller owns them).
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// warnings is a de-duplicated, capped, concurrency-safe warning list.
type warnings struct {
	mu   sync.Mutex
	list []string
	seen map[string]struct{}
	full bool
}

const suppressedWarning = "further warnings suppressed"

func (w *warnings) add(format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.seen[msg]; dup {
		return
	}
	if len(w.list) >= maxWarnings {
		if !w.full {
			w.full = true
			w.list = append(w.list, suppressedWarning)
		}
		return
	}
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	w.seen[msg] = struct{}{}
	w.list = append(w.list, msg)
}

func (w *warnings) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}

// readFull reads exactly len(buf) bytes at off. A segment shorter than its
// declared size yields io.ErrUnexpectedEOF; other errors are returned as-is.
func readFull(r io.ReaderAt, buf []byte, off int64) error {
	if len(buf) == 0 {
		return nil
	}
	n, err := r.ReadAt(buf, off)
	if n == len(buf) && (err == nil || err == io.EOF) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func addOK(a, b uint64) (uint64, bool) {
	s := a + b
	return s, s >= a
}

func mulOK(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	return p, p/a == b
}

// capValue limits an on-disk string to maxMetaValue bytes at a rune boundary.
func capValue(s string) string {
	if len(s) <= maxMetaValue {
		return s
	}
	cut := maxMetaValue
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// safeName reduces an on-disk section type to printable ASCII for metadata.
func safeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && i < 16; i++ {
		c := s[i]
		if c > 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// Open validates a set of segment files (in order, segment 1 first) and
// returns a reader over the media. It parses the segment headers, the section
// chains, the volume, header, hash, digest and error2 sections.
//
// An image whose last segment has no done section opens with a warning (a
// truncated acquisition is evidence and stays examinable).
func Open(segs []Segment) (*Reader, error) {
	if len(segs) == 0 {
		return nil, corrupt(0, "", "no segment files")
	}
	if len(segs) > maxSegments {
		return nil, corrupt(0, "", "%d segment files; the format allows at most %d", len(segs), maxSegments)
	}
	o := &opener{r: &Reader{segs: slices.Clone(segs), secs: make([][]section, len(segs))}}
	for i := range segs {
		if err := o.segment(i); err != nil {
			return nil, err
		}
	}
	if err := o.finish(); err != nil {
		return nil, err
	}
	return o.r, nil
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }
