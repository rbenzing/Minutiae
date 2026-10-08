package plist

import (
	"fmt"

	howett "howett.net/plist"
)

// MaxInput is the largest document Decode accepts; a larger one is ErrLimit before any work.
const MaxInput = 64 << 20

// Memory-charge model (see Decode). Per input byte the library keeps its own copy of the
// document and scratch buffers; per expanded node a value plus a map or slice slot; per
// expanded payload byte the library's string or data plus the normalized copy.
const (
	chargePerInputByte = 8
	chargePerNode      = 64
	chargePerPayload   = 2
)

// Decode decodes one binary or XML plist into plain values: map[string]any, []any, string
// (always valid UTF-8; a string that is not is a RawString, see below),
// int64 (uint64 above math.MaxInt64), float64, bool, []byte, time.Time (UTC; plist dates
// carry no zone; years 1 to 9999) and UID, plus two raw forms for what a Go string or time.Time
// would alter: RawString (invalid UTF-8, or a lone UTF-16 surrogate, bytes kept exactly) and
// RawDate (a NaN, infinite or out-of-range date, the stored Cocoa seconds kept exactly). A
// dictionary key of that kind is ErrUnsupported, and an XML CF$UID with a negative integer is
// ErrMalformed. OpenStep and GNUstep text plists are refused (ErrUnsupported), and
// so is anything else that is not one of the two formats.
//
// Order: a nil budget is ErrNoBudget; input over MaxInput is ErrLimit; the document is
// validated and measured (Check: format gate, size, depth, expanded node count and expanded
// payload bytes, all capped by DefaultLimits); the budget is charged
//
//	estimate = len(b)*8 + nodes*64 + payload*2
//
// from the validator's own counts BEFORE the decoding library runs, so a refused charge
// allocates nothing and returns an error wrapping both ErrNoBudget and the budget's error;
// then the library decodes under a recover guard and the result is normalized. On any error
// the charge is freed. On success it stays: the caller's per-invocation budget view is
// released by the host at the end of the job.
//
// A duplicate key in an XML dict keeps the last value (the library's behaviour, a known
// property; detecting duplicates is a non-goal). NaN reals pass through.
func Decode(b []byte, budget Budget) (v any, err error) {
	err = guard(func() error {
		var e error
		v, e = decodeCore(b, budget)
		return e
	})
	if err != nil {
		v = nil
	}
	return v, err
}

// decodeCore is Decode without the recover guard: the fuzz targets and tests call it so a
// panic in the library is a failure rather than a recovered error.
func decodeCore(b []byte, budget Budget) (any, error) {
	if budget == nil {
		return nil, fmt.Errorf("%w: no budget", ErrNoBudget)
	}
	if len(b) > MaxInput {
		return nil, limited("input of %d bytes above %d", len(b), MaxInput)
	}
	nodes, payload, err := measureCore(b, DefaultLimits())
	if err != nil {
		return nil, err
	}
	// Every term is bounded (len <= 64 MiB, nodes <= 1<<20, payload <= 64 MiB), so the sum
	// fits an int64 with room to spare.
	estimate := int64(len(b))*chargePerInputByte + int64(nodes)*chargePerNode + int64(payload)*chargePerPayload
	if err := budget.Alloc(estimate); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoBudget, err)
	}
	ok := false
	defer func() {
		if !ok {
			budget.Free(estimate)
		}
	}()
	in, norm := b, normalizer{}
	if LooksLikeXML(b) {
		if hasNegativeUID(b) {
			return nil, malformed("negative CF$UID")
		}
		rewritten, base, found, ok := rewriteSurrogates(b)
		if !ok {
			return nil, fmt.Errorf("%w: no free private code points to stand in for surrogate references", ErrUnsupported)
		}
		if found {
			in, norm = rewritten, normalizer{base: base}
		}
	}
	var raw any
	format, err := howett.Unmarshal(in, &raw)
	if err != nil {
		return nil, malformed("%v", err)
	}
	var out any
	switch format {
	case howett.BinaryFormat:
		w, top, werr := newBplistWalker(b, DefaultLimits(), false)
		if werr != nil {
			return nil, werr
		}
		out, err = w.normalizeBinary(top, raw)
	case howett.XMLFormat:
		out, err = norm.value(raw)
	default:
		return nil, fmt.Errorf("%w: library read format %d", ErrUnsupported, format)
	}
	if err != nil {
		return nil, err
	}
	ok = true
	return out, nil
}
