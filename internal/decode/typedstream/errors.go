package typedstream

import "errors"

// Sentinels. Every error this package returns wraps exactly one of them (a budget refusal
// wraps ErrNoBudget and the budget's own error).
var (
	// ErrNotTypedstream: the bytes do not start with a typedstream header, or the structure
	// where the text length belongs is not an integer.
	ErrNotTypedstream = errors.New("typedstream: not a typedstream")
	// ErrTruncated: the stream ends before the string object it started is complete.
	ErrTruncated = errors.New("typedstream: truncated")
	// ErrLimit: the input, the text or a declared length is above a cap.
	ErrLimit = errors.New("typedstream: limit exceeded")
	// ErrInternal: a panic was recovered; never expected for any input.
	ErrInternal = errors.New("typedstream: internal error")
	// ErrNoBudget: no budget was given, or the caller's budget refused the allocation.
	ErrNoBudget = errors.New("typedstream: memory budget exhausted")
)
