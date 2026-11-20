package tl

import (
	"errors"
	"fmt"
)

// Sentinels. Every error this package returns is an *Error wrapping exactly one of them.
var (
	// ErrTruncated: fewer bytes remain than the primitive, the length prefix or the padding needs.
	ErrTruncated = errors.New("tl: truncated")
	// ErrBadBool: a bool constructor other than IDBoolTrue and IDBoolFalse.
	ErrBadBool = errors.New("tl: bad bool constructor")
	// ErrBadVector: a boxed vector whose constructor is not IDVector.
	ErrBadVector = errors.New("tl: bad vector constructor")
	// ErrVectorCount: a vector count below 0, above MaxVector, or whose elements cannot fit
	// the bytes that remain (checked before the first element).
	ErrVectorCount = errors.New("tl: vector count out of bounds")
	// ErrStringLength: a string or bytes length past the end of the blob or above MaxString.
	ErrStringLength = errors.New("tl: string length out of bounds")
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
}

// Error names the sentinel, the offset and the path.
func (e *Error) Error() string {
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
