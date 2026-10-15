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

// maxDepthCap bounds MaxDepth whatever the caller passes: the binary walk recurses once per
// level, so an unbounded MaxDepth would let a hostile document overflow the goroutine stack,
// which is fatal and cannot be recovered.
const maxDepthCap = 4096

// effective returns the limits to enforce: DefaultLimits for the zero value (all fields zero;
// a partly filled Limits is used as given, so a zero field there refuses everything), and
// MaxDepth capped at maxDepthCap.
func (l Limits) effective() Limits {
	if l == (Limits{}) {
		l = DefaultLimits()
	}
	l.MaxDepth = min(l.MaxDepth, maxDepthCap)
	return l
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
