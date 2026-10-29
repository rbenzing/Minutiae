package carve

import (
	"errors"
	"strconv"
)

// pdfAlign is the header unit the window assumes for "run-aligned": a "%PDF-"
// at a multiple of it ends the search for the end of the file (the window
// does not carry the scan's unit; 512 is the default one).
const pdfAlign = 512

// validatePDF accepts a PDF by its end-of-file markers. After the header
// ("%PDF-1." or "%PDF-2." and a digit) it searches forward, bounded by the
// window and the budget, for "startxref", an end of line, a decimal offset X,
// an end of line and "%%EOF", where X lies before that "startxref" and points
// at "xref" or at "<digits> <digits> obj". Each such group is one counting
// revision; the object ends after the LAST one and its end-of-line bytes. The
// search stops at the window end or at the next "%PDF-" at an aligned
// offset. Nothing in the body is parsed. The layout is from memory
// (docs/limitations.md).
func validatePDF(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	var h [8]byte
	if n, err := w.ReadAt(h[:], 0); err != nil {
		if n >= 5 && string(h[:5]) != "%PDF-" {
			return reject("bad-signature")
		}
		return fail(err)
	}
	if string(h[:5]) != "%PDF-" {
		return reject("bad-signature")
	}
	if (h[5] != '1' && h[5] != '2') || h[6] != '.' || h[7] < '0' || h[7] > '9' {
		return reject("bad-version")
	}
	revisions, end := 0, int64(0)
	for i := int64(8); i < w.Len(); i++ {
		c, err := w.Byte(i)
		if err != nil {
			return fail(err)
		}
		switch c {
		case '%':
			if i%pdfAlign == 0 {
				if ok, err := pdfMatch(w, i, "%PDF-"); err != nil {
					return fail(err)
				} else if ok {
					return pdfVerdictOf(revisions, end)
				}
			}
		case 's':
			ok, err := pdfMatch(w, i, "startxref")
			if err != nil {
				return fail(err)
			}
			if !ok {
				continue
			}
			e, err := pdfRevision(w, i)
			if err != nil {
				return fail(err)
			}
			if e > 0 {
				revisions, end = revisions+1, e
				i = e - 1
			}
		}
	}
	return pdfVerdictOf(revisions, end)
}

func pdfVerdictOf(revisions int, end int64) Verdict {
	if revisions == 0 {
		return Verdict{Outcome: Rejected, Reason: "no-eof"}
	}
	return Verdict{
		Outcome: Footer, Length: end,
		Basis:       []string{"revisions=" + strconv.Itoa(revisions)},
		Assumptions: []string{"last-eof-in-window"},
	}
}

// pdfMatch reports whether the window holds s at off (false near the window
// end; a read failure is an error).
func pdfMatch(w *Window, off int64, s string) (bool, error) {
	if w.Len()-off < int64(len(s)) {
		return false, nil
	}
	for k := range len(s) {
		b, err := w.Byte(off + int64(k))
		if err != nil {
			return false, err
		}
		if b != s[k] {
			return false, nil
		}
	}
	return true, nil
}

// pdfEOL returns the length of the end of line at off (0 when there is none).
func pdfEOL(w *Window, off int64) (int64, error) {
	if off >= w.Len() {
		return 0, nil
	}
	b, err := w.Byte(off)
	if err != nil || (b != '\r' && b != '\n') {
		return 0, err
	}
	if b == '\r' && off+1 < w.Len() {
		if n, err := w.Byte(off + 1); err != nil || n == '\n' {
			return 2, err
		}
	}
	return 1, nil
}

// pdfRevision judges the "startxref" at off: it returns the window offset
// after the "%%EOF" and its end of line when "startxref" starts a counting
// revision, else 0.
func pdfRevision(w *Window, off int64) (int64, error) {
	pos := off + 9
	n, err := pdfEOL(w, pos)
	if err != nil || n == 0 {
		return 0, err
	}
	pos += n
	var x int64
	digits := 0
	for ; pos < w.Len(); pos++ {
		b, err := w.Byte(pos)
		if err != nil {
			return 0, err
		}
		if b < '0' || b > '9' {
			break
		}
		if digits++; digits > 15 {
			return 0, nil
		}
		x = x*10 + int64(b-'0')
	}
	if digits == 0 {
		return 0, nil
	}
	if n, err = pdfEOL(w, pos); err != nil || n == 0 {
		return 0, err
	}
	pos += n
	if ok, err := pdfMatch(w, pos, "%%EOF"); err != nil || !ok {
		return 0, err
	}
	pos += 5
	if x >= off {
		return 0, nil
	}
	if ok, err := pdfSectionAt(w, x); err != nil || !ok {
		return 0, err
	}
	n, err = pdfEOL(w, pos)
	return pos + n, err
}

// pdfSectionAt reports whether "xref" or "<digits> <digits> obj" starts at x.
func pdfSectionAt(w *Window, x int64) (bool, error) {
	var b [32]byte
	n := min(int64(len(b)), w.Len()-x)
	if n <= 0 {
		return false, nil
	}
	if _, err := w.ReadAt(b[:n], x); err != nil {
		return false, err
	}
	s := b[:n]
	if len(s) >= 4 && string(s[:4]) == "xref" {
		return true, nil
	}
	i := 0
	for part := 0; part < 2; part++ {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start || i >= len(s) || s[i] != ' ' {
			return false, nil
		}
		i++
	}
	return len(s)-i >= 3 && string(s[i:i+3]) == "obj", nil
}
