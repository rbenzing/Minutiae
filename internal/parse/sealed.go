package parse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// SealedReaderAt wraps a read-only io.ReaderAt (which it never exposes) with a
// total read limit for Probe and a seal the host sets when the job ends. It
// has one method for reading, ReadAt, so a parser holding it can neither
// write, seek, close nor reach a file descriptor. The host and the parsertest
// harness use this one implementation, so a parser cannot pass the harness and
// then behave differently under the host.
type SealedReaderAt struct {
	r      io.ReaderAt
	b      *ReadBudget
	sealed atomic.Bool
}

// NewSealedReaderAt wraps r. probeLimit is the total number of bytes ReadAt
// may return across all calls; 0 means unlimited.
func NewSealedReaderAt(r io.ReaderAt, probeLimit int64) *SealedReaderAt {
	return &SealedReaderAt{r: r, b: NewReadBudget(probeLimit)}
}

// ReadAt reads from the wrapped reader. After Seal it returns 0, ErrSealed (a
// read in flight when Seal is called returns 0, ErrSealed too, so no byte COUNT is
// delivered after the seal; the caller's buffer may still have been filled by that read, so
// the host never reads a parser's buffer). Once the probe limit is used up it returns
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
	if s.b.limit > 0 {
		// Reserve before reading so concurrent readers cannot exceed the limit.
		for {
			cur := s.b.read.Load()
			remaining := s.b.limit - cur
			if remaining <= 0 {
				return 0, ErrProbeLimit
			}
			want = min(int64(len(p)), remaining)
			if s.b.read.CompareAndSwap(cur, cur+want) {
				break
			}
		}
	}
	n, err := s.r.ReadAt(p[:want], off)
	if s.b.limit > 0 && int64(n) < want {
		s.b.read.Add(-(want - int64(n))) // give back what was not returned
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

// ReadBudget is a read limit shared by several SealedReaderAts: the host gives one Probe call ONE
// budget, whatever number of inputs and Lookuper opens it reads.
type ReadBudget struct {
	limit int64
	read  atomic.Int64
}

// NewReadBudget returns a budget of limit bytes; 0 means unlimited.
func NewReadBudget(limit int64) *ReadBudget { return &ReadBudget{limit: max(limit, 0)} }

// NewSealedReaderAtShared wraps r so that its reads count against b together with every other
// reader made from b. A nil b means unlimited.
func NewSealedReaderAtShared(r io.ReaderAt, b *ReadBudget) *SealedReaderAt {
	if b == nil {
		return NewSealedReaderAt(r, 0)
	}
	return &SealedReaderAt{r: r, b: b}
}

// Reader returns the view of s a parser gets: an io.ReaderAt with no other method, so a parser
// cannot reach Seal (or anything else) by a type assertion. The host and the harness keep s itself to
// seal it.
func (s *SealedReaderAt) Reader() io.ReaderAt { return readerView{s} }

// readerView has ReadAt and nothing else; it is not comparable to or convertible back to the
// sealed reader by a type assertion.
type readerView struct{ s *SealedReaderAt }

func (v readerView) ReadAt(p []byte, off int64) (int, error) { return v.s.ReadAt(p, off) }

// WithoutDeadline returns ctx with its deadline hidden: cancellation and values flow on, Deadline
// reports none. The host owns every time limit; a parser that read the deadline would put the start
// time of the job into its output.
func WithoutDeadline(ctx context.Context) context.Context { return noDeadline{ctx} }

type noDeadline struct{ context.Context }

func (noDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
