package tl

// Limits (plan 4I4 spec section 7.1, writer's call). They are constants, never parameters a
// caller can widen.
const (
	// MaxBlob is the largest blob read (16 MiB); a larger one is ErrTooLarge before any read.
	MaxBlob = 16 << 20
	// MaxDepth is the deepest object nesting a walker follows.
	MaxDepth = 32
	// MaxVector is the largest vector count accepted.
	MaxVector = 10000
	// MaxString is the largest string or bytes length (the 3-byte maximum).
	MaxString = 16777215
	// MaxSteps is the work budget of one blob: every reading call is one step.
	MaxSteps = 1000000
)
