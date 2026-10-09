package plist

import "errors"

// Sentinels. Every error this package returns wraps exactly one of them.
var (
	// ErrMalformed: the bytes are not a well-formed plist of an accepted format.
	ErrMalformed = errors.New("plist: malformed")
	// ErrLimit: the document is well formed but exceeds a cap of Limits.
	ErrLimit = errors.New("plist: limit exceeded")
	// ErrUnsupported: a format or construct this package refuses on purpose.
	ErrUnsupported = errors.New("plist: unsupported")
	// ErrInternal: a panic was recovered; never expected for any input.
	ErrInternal = errors.New("plist: internal error")
	// ErrNoBudget: the caller's memory budget refused the allocation.
	ErrNoBudget = errors.New("plist: memory budget exhausted")
)
