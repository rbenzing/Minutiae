package plist

import "fmt"

// Report counts what a decode saw in the document but could not carry into the plain values.
// DecodeReport counts per OCCURRENCE in the expanded tree (a shared dictionary referenced
// twice counts twice); UnarchiveReport counts per RESOLVED archive object (a shared object
// counts once). Plus adds two reports of different measures and is for totals only.
type Report struct {
	DuplicateKeys uint64 // key occurrences whose entry was replaced by a later entry with the same key
}

// Lossless reports whether nothing was lost.
func (r Report) Lossless() bool { return r == Report{} }

// Plus adds two reports, saturating.
func (r Report) Plus(o Report) Report {
	return Report{DuplicateKeys: sat(r.DuplicateKeys, o.DuplicateKeys)}
}

// LossError is the strict forms' refusal: a document that decodes only by losing something the
// plain values cannot show. errors.Is(err, ErrUnsupported) holds; errors.As gives the Report.
type LossError struct{ Report Report }

func (e *LossError) Error() string {
	return fmt.Sprintf("%v: %d duplicate dictionary key occurrence(s) would be lost (DecodeReport keeps the last value and counts them)",
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
