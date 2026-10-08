package plist

// Limits caps what one document may cost.
type Limits struct {
	// MaxNodes bounds the fully expanded node count (and the object table).
	MaxNodes uint64
	// MaxDepth bounds container nesting.
	MaxDepth int
	// MaxPayload bounds the expanded string and data bytes.
	MaxPayload uint64
}

// DefaultLimits returns the limits used when the caller has no better ones.
func DefaultLimits() Limits {
	return Limits{MaxNodes: 1 << 20, MaxDepth: 64, MaxPayload: 64 << 20}
}

// Budget is the caller's memory budget (satisfied by the parse package's budget view);
// it is declared here so this package imports no Minutiae package.
type Budget interface {
	Alloc(n int64) error
	Free(n int64)
}
