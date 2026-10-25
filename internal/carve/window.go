package carve

import (
	"context"
	"errors"
	"io"
	"math"
)

// windowCacheSize is the fixed size of the read cache of Window.Byte; every
// refill checks the context (3E-R29), so a validator walking a 4 GiB window
// stops at the next refill of a cancelled run.
const windowCacheSize = 64 << 10

// The refusals of a Window.
var (
	// ErrOutsideWindow: a read reached past the hit's window (counted).
	ErrOutsideWindow = errors.New("carve: read outside the window")
	// ErrValidateBudget: the validator read budget of the scan is spent.
	ErrValidateBudget = errors.New("carve: validate budget exhausted")
)

// budget caps the bytes validators may read in one scan. There is no unlimited
// budget: a nil *budget is a zero-limit one and refuses every read (fail closed).
type budget struct {
	limit, spent int64
	exhausted    bool
}

// newBudget returns a budget of limit bytes, clamped to 0..MaxValidateBudget.
func newBudget(limit int64) *budget {
	return &budget{limit: max(0, min(limit, MaxValidateBudget))}
}

// charge books n bytes before they are read; false (and exhausted) when they
// do not fit. Nothing is booked for a refused read.
func (b *budget) charge(n int64) bool {
	if b == nil {
		return false
	}
	if n > b.limit-b.spent {
		b.exhausted = true
		return false
	}
	b.spent += n
	return true
}

// Window is a validator's bounded view of one hit: offsets are relative to the
// hit's first byte and no read can leave [0, Len()). A declared size read from
// the media only ever moves a cursor inside this window.
type Window struct {
	ctx         context.Context
	r           io.ReaderAt
	base        int64
	length      int64
	bud         *budget
	cache       []byte
	cacheOff    int64
	cacheLen    int
	outside     int64
	cacheLoaded bool
	readErr     error // the first I/O error of the media (not a refusal of the window)
}

func newWindow(ctx context.Context, r io.ReaderAt, base, length int64, b *budget) *Window {
	if base < 0 || length < 0 {
		length = 0
	}
	if length > math.MaxInt64-base {
		length = math.MaxInt64 - base
	}
	return &Window{ctx: ctx, r: r, base: base, length: length, bud: b}
}

// Len is the number of bytes of the window.
func (w *Window) Len() int64 { return w.length }

func (w *Window) outsideReads() int64 { return w.outside }

// readFull reads exactly len(p) bytes at the reader coordinate off.
func (w *Window) readFull(p []byte, off int64) error {
	n, err := w.r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if w.readErr == nil {
		w.readErr = err
	}
	return err
}

// ReadAt reads from off, relative to the hit. The part of the request that
// lies past the window is not read: ReadAt returns the bytes inside and
// ErrOutsideWindow, and counts the refusal.
func (w *Window) ReadAt(p []byte, off int64) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 || off >= w.length {
		w.outside++
		return 0, ErrOutsideWindow
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := int64(len(p))
	truncated := n > w.length-off
	if truncated {
		n = w.length - off
		w.outside++
	}
	if !w.bud.charge(n) {
		return 0, ErrValidateBudget
	}
	if err := w.readFull(p[:n], w.base+off); err != nil {
		return 0, err
	}
	if truncated {
		return int(n), ErrOutsideWindow
	}
	return int(n), nil
}

// Byte returns the byte at off, relative to the hit, through a fixed 64 KiB
// cache. Each refill checks the context and is charged to the budget.
func (w *Window) Byte(off int64) (byte, error) {
	if off < 0 || off >= w.length {
		w.outside++
		return 0, ErrOutsideWindow
	}
	if !w.cacheLoaded || off < w.cacheOff || off >= w.cacheOff+int64(w.cacheLen) {
		if err := w.refill(off); err != nil {
			return 0, err
		}
	}
	return w.cache[off-w.cacheOff], nil
}

func (w *Window) refill(off int64) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	start := off - off%windowCacheSize
	n := min(int64(windowCacheSize), w.length-start)
	if !w.bud.charge(n) {
		return ErrValidateBudget
	}
	if w.cache == nil {
		w.cache = make([]byte, windowCacheSize)
	}
	w.cacheLoaded = false
	if err := w.readFull(w.cache[:n], w.base+start); err != nil {
		return err
	}
	w.cacheOff, w.cacheLen, w.cacheLoaded = start, int(n), true
	return nil
}
