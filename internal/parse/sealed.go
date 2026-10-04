package parse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

// SealedReaderAt wraps a read-only io.ReaderAt (which it never exposes) with a
// total read limit for Probe and a seal the host sets when the job ends. It
// has one method for reading, ReadAt, so a parser holding it can neither
// write, seek, close nor reach a file descriptor. The host and the parsertest
// harness use this one implementation, so a parser cannot pass the harness and
// then behave differently under the host.
type SealedReaderAt struct {
	r      io.ReaderAt
	limit  int64
	read   atomic.Int64
	sealed atomic.Bool
}

// NewSealedReaderAt wraps r. probeLimit is the total number of bytes ReadAt
// may return across all calls; 0 means unlimited.
func NewSealedReaderAt(r io.ReaderAt, probeLimit int64) *SealedReaderAt {
	return &SealedReaderAt{r: r, limit: max(probeLimit, 0)}
}

// ReadAt reads from the wrapped reader. After Seal it returns 0, ErrSealed (a
// read in flight when Seal is called returns 0, ErrSealed too, so no bytes are
// delivered after the seal). Once the probe limit is used up it returns
// ErrProbeLimit, and a read crossing the limit is cut at it.
func (s *SealedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if s == nil || s.r == nil {
		return 0, errors.New("parse: no input")
	}
	if s.sealed.Load() {
		return 0, ErrSealed
	}
	if off < 0 {
		return 0, fmt.Errorf("parse: negative read offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	want := int64(len(p))
	if s.limit > 0 {
		// Reserve before reading so concurrent readers cannot exceed the limit.
		for {
			cur := s.read.Load()
			remaining := s.limit - cur
			if remaining <= 0 {
				return 0, ErrProbeLimit
			}
			want = min(int64(len(p)), remaining)
			if s.read.CompareAndSwap(cur, cur+want) {
				break
			}
		}
	}
	n, err := s.r.ReadAt(p[:want], off)
	if s.limit > 0 && int64(n) < want {
		s.read.Add(-(want - int64(n))) // give back what was not returned
	}
	if s.sealed.Load() {
		return 0, ErrSealed
	}
	if want < int64(len(p)) && int64(n) == want && (err == nil || errors.Is(err, io.EOF)) {
		return n, ErrProbeLimit
	}
	return n, err
}

// Seal makes every later (and every in-flight) ReadAt fail with ErrSealed. It
// is idempotent, atomic, and safe from any goroutine.
func (s *SealedReaderAt) Seal() {
	if s != nil {
		s.sealed.Store(true)
	}
}

// TickEvery is how many loop iterations pass between context checks in Tick.
const TickEvery = 4096

// Tick is the cheap cancellation poll for long loops in parsers and decoders:
// it returns ctx.Err() when i is a multiple of TickEvery and nil otherwise.
// A nil context is never cancelled.
func Tick(ctx context.Context, i int) error {
	if i%TickEvery != 0 || ctx == nil {
		return nil
	}
	return ctx.Err()
}
