package tl

import (
	"errors"
	"fmt"
)

// Sentinels. Every error this package returns is an *Error wrapping exactly one of them.
var (
	// ErrTruncated: fewer bytes remain than the primitive, the length prefix, the padding, a
	// string's declared length or a vector's declared count (at its minimum element size)
	// needs. A blob that ends is never a limit hit (spec ruling T7): the 3-byte framing cannot
	// declare a string above MaxString, so a string past the end is always ErrTruncated.
	ErrTruncated = errors.New("tl: truncated")
	// ErrBadBool: a bool constructor other than IDBoolTrue and IDBoolFalse.
	ErrBadBool = errors.New("tl: bad bool constructor")
	// ErrBadVector: a boxed vector whose constructor is not IDVector.
	ErrBadVector = errors.New("tl: bad vector constructor")
	// ErrVectorCount: a vector count below 0 or above MaxVector (checked before the first
	// element, whatever the bytes that remain).
	ErrVectorCount = errors.New("tl: vector count out of bounds")
	// ErrDepth: object nesting above MaxDepth.
	ErrDepth = errors.New("tl: nesting too deep")
	// ErrSteps: more reads than MaxSteps (the work budget of one blob).
	ErrSteps = errors.New("tl: step budget exhausted")
	// ErrTooLarge: a blob above MaxBlob, refused before any read.
	ErrTooLarge = errors.New("tl: blob too large")
	// ErrNoBudget: no budget was given, or the caller's budget refused an allocation.
	ErrNoBudget = errors.New("tl: memory budget exhausted")
	// ErrInternal: a caller's misuse or a recovered panic; never expected for any input.
	ErrInternal = errors.New("tl: internal error")
)

// Error is the one error type of this package: the sentinel, the offset in the blob where the
// failed read began and the field path (empty for the Reader; the walker fills it).
type Error struct {
	Err    error
	Offset int
	Path   string
	// Declared and Remaining are set on a length or count that runs past the end (ErrTruncated,
	// spec ruling T7): the declared string length or vector count, and the bytes that remained
	// after its prefix or header. Both are 0 on every other error.
	Declared  int
	Remaining int
}

// Error names the sentinel, the offset and the path, and the declared value and the bytes
// that remained when they are set.
func (e *Error) Error() string {
	if e.Declared != 0 {
		return fmt.Sprintf("%v at offset %d (path %q; declared %d, %d bytes remain)", e.Err, e.Offset, e.Path, e.Declared, e.Remaining)
	}
	return fmt.Sprintf("%v at offset %d (path %q)", e.Err, e.Offset, e.Path)
}

// Unwrap returns the sentinel.
func (e *Error) Unwrap() error { return e.Err }

// Budget is charged by a caller that copies out of a blob (the walker); the Reader itself
// never allocates from a stored length and takes no budget.
type Budget interface {
	Alloc(n int64) error
	Free(n int64)
}
