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
	chargePerNode      = 478
	chargePerPayload   = 2
	// chargePerNode and chargePerXMLNode are held to a measurement, not a guess: for every shape of
	// TestChargeCoversMeasuredAllocation (flat and dictionary-heavy documents, deep nesting, empty
	// containers, long strings, data, in binary and XML) the estimate is at least the bytes the call
	// allocates, also under -race. The worst measured total cost per node (allocation minus the
	// input and payload terms, under -race) was 382.4 B for a binary document (a 60-deep nest of
	// one-key dictionaries: a map in the library and another in the normalizer) and 1678.6 B for an
	// XML document (300000 empty dictionaries: the encoding/xml scan, the library and the key-set
	// bookkeeping). Each total carries a 25 percent margin: chargePerNode = ceil(1.25 * 382.4) =
	// 478, and an XML node is charged chargePerNode + chargePerXMLNode = ceil(1.25 * 1678.6) =
	// 2099, so chargePerXMLNode = 2099 - 478 = 1621. An over-charge is allowed, an under-charge is
	// not. chargePerXMLNode is charged per XML node on top of the terms above, up front.
	chargePerXMLNode = 1621
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
//	estimate = len(b)*8 + nodes*478 + payload*2   (+ nodes*1621 for an XML document)
//
// from the validator's own counts BEFORE the decoding library runs, so a refused charge
// allocates nothing and returns an error wrapping both ErrNoBudget and the budget's error;
// then the library decodes under a recover guard and the result is normalized. On any error
// the charge is freed. On success it stays: the decoder never frees a successful charge, and
// the caller's per-invocation budget view is dropped with the invocation.
//
// Decode is strict: a document (binary or XML) that repeats a dictionary key is refused with
// a *LossError (wrapping ErrUnsupported), because which value counts cannot be decided; that
// check comes last, so a malformed or over-limit document returns the same sentinel as from
// DecodeReport. DecodeReport is the lenient form: it keeps the last value and counts the
// repeats in its Report (a lower bound). Two <key> elements in a row, which would drop the
// first key without a value, are ErrMalformed, as is any child element inside <key> or
// <integer>. A binary set (tag 0xC) and the binary null and fill bytes are ErrMalformed: the
// decoding library does not read them (Check still walks a set, bounding it, before the
// library refuses it). A binary UID of 16 bytes with a non-zero high word, like a 16-byte
// integer that does not fit 64 bits, is ErrUnsupported. NaN reals pass through.
func Decode(b []byte, budget Budget) (v any, err error) {
	err = guard(func() error {
		var e error
		v, _, e = decodeCore(b, budget, true)
		return e
	})
	if err != nil {
		v = nil
	}
	return v, err
}

// decodeCore is Decode without the recover guard: the fuzz targets and tests call it so a
// panic in the library is a failure rather than a recovered error.
func decodeCore(b []byte, budget Budget, strict bool) (any, Report, error) {
	if budget == nil {
		return nil, Report{}, fmt.Errorf("%w: no budget", ErrNoBudget)
	}
	if len(b) > MaxInput {
		return nil, Report{}, limited("input of %d bytes above %d", len(b), MaxInput)
	}
	nodes, payload, err := measureCore(b, DefaultLimits())
	if err != nil {
		return nil, Report{}, err
	}
	// Every term is bounded (len <= 64 MiB, nodes <= 1<<20, payload <= 64 MiB), so the sum
	// fits an int64 with room to spare.
	estimate := int64(len(b))*chargePerInputByte + int64(nodes)*chargePerNode + int64(payload)*chargePerPayload
	if LooksLikeXML(b) {
		// The duplicate-key scan holds one set of key texts per open dictionary.
		estimate += int64(nodes) * chargePerXMLNode
	}
	if err := budget.Alloc(estimate); err != nil {
		return nil, Report{}, fmt.Errorf("%w: %w", ErrNoBudget, err)
	}
	ok := false
	defer func() {
		if !ok {
			budget.Free(estimate)
		}
	}()
	in, norm := b, normalizer{}
	var rep Report
	if LooksLikeXML(b) {
		dups, err := scanXMLStructure(b)
		if err != nil {
			return nil, Report{}, err
		}
		rep.DuplicateKeys = dups
		rewritten, base, found, ok := rewriteSurrogates(b)
		if !ok {
			return nil, Report{}, fmt.Errorf("%w: no free private code points to stand in for surrogate references", ErrUnsupported)
		}
		if found {
			in, norm = rewritten, normalizer{base: base}
		}
	}
	var raw any
	format, err := howett.Unmarshal(in, &raw)
	if err != nil {
		return nil, Report{}, malformed("%v", err)
	}
	var out any
	switch format {
	case howett.BinaryFormat:
		w, top, werr := newBplistWalker(b, DefaultLimits(), false)
		if werr != nil {
			return nil, Report{}, werr
		}
		out, err = w.normalizeBinary(top, raw)
		rep.DuplicateKeys = sat(rep.DuplicateKeys, w.dups)
	case howett.XMLFormat:
		out, err = norm.value(raw)
	default:
		return nil, Report{}, fmt.Errorf("%w: library read format %d", ErrUnsupported, format)
	}
	if err != nil {
		return nil, Report{}, err
	}
	// Do not fix: the strict refusal comes last on purpose. A malformed or over-limit document
	// above returns its own sentinel from the strict and the lenient entry point alike, and the
	// deferred Free runs here.
	if strict && !rep.Lossless() {
		return nil, Report{}, &LossError{Report: rep}
	}
	ok = true
	return out, rep, nil
}
