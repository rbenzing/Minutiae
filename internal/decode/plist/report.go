package plist

import "fmt"

// Report counts what a decode saw in the document but could not carry into the plain values.
// DecodeReport counts per OCCURRENCE in the expanded tree (a shared dictionary referenced
// twice counts twice); UnarchiveReport counts per RESOLVED archive object (a shared object
// counts once). Plus adds two reports of different measures and is for totals only.
//
// Every count is a lower bound: in a binary document a duplicate inside an entry that a later
// entry replaced is not counted; the XML scan counts every key of the document, also inside
// such an entry, so for the same tree the XML count is at least the binary one. Zero is exact;
// a nonzero count means at least that many entries were lost.
type Report struct {
	// DuplicateKeys is a lower bound on the key occurrences whose entry was replaced by a later
	// entry with the same key; a duplicate inside an entry that a later entry replaced is not
	// counted. Zero is exact, a nonzero count means at least that many entries were lost.
	DuplicateKeys uint64
}

// Lossless reports whether nothing was lost.
func (r Report) Lossless() bool { return r == Report{} }

// Plus adds two reports, saturating.
func (r Report) Plus(o Report) Report {
	return Report{DuplicateKeys: sat(r.DuplicateKeys, o.DuplicateKeys)}
}

// LossError is the strict forms' refusal: a document that decodes only by losing something the
// plain values cannot show. errors.Is(err, ErrUnsupported) holds; errors.As gives the Report,
// whose counts are lower bounds (see Report).
type LossError struct{ Report Report }

// Error says that at least that many entries would be lost: the count is a lower bound.
func (e *LossError) Error() string {
	return fmt.Sprintf("%v: at least %d duplicate dictionary key occurrence(s) would be lost, a lower bound (DecodeReport keeps the last value and counts them)",
		ErrUnsupported, e.Report.DuplicateKeys)
}

// Unwrap returns ErrUnsupported.
func (e *LossError) Unwrap() error { return ErrUnsupported }

// DecodeReport is the lenient form of Decode: the same checks, charge and normalization, but a
// document that repeats a dictionary key is decoded (the last value wins, as in the format
// libraries) and the repeats are counted in the Report instead of refused. Decode refuses
// such a document with a *LossError. A malformed or over-limit document returns the same
// error from both.
func DecodeReport(b []byte, budget Budget) (v any, rep Report, err error) {
	err = guard(func() error {
		var e error
		v, rep, e = decodeCore(b, budget, false)
		return e
	})
	if err != nil {
		v, rep = nil, Report{}
	}
	return v, rep, err
}
