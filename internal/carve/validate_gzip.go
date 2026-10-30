package carve

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// gzipSearchMax is how far a NUL-terminated header string (FNAME, FCOMMENT)
// is searched for its terminator.
const gzipSearchMax = 64 << 10

// gzipNameMax is the longest FNAME recorded in the basis.
const gzipNameMax = 64

// validateGzip checks a gzip member header (RFC 1952, from memory: see
// docs/limitations.md): the magic, method 8, the reserved flag bits 5-7 zero,
// and the optional FEXTRA, FNAME, FCOMMENT and FHCRC fields, which must lie
// inside the window. The deflate stream is never inflated, so no end can be
// proved: the object is bounded to the window end and the scanner names the
// assumption (length-capped or cut-at-run-end). At confidence 25 the hit is
// below the default floor (3E-R2).
func validateGzip(w *Window) Verdict {
	var h [10]byte
	if _, err := w.ReadAt(h[:], 0); err != nil {
		return Verdict{Outcome: Rejected, Reason: readFailReason(err)}
	}
	flags := h[3]
	if h[0] != 0x1F || h[1] != 0x8B || h[2] != 8 || flags&0xE0 != 0 {
		return Verdict{Outcome: Rejected, Reason: "bad-header"}
	}
	pos := int64(10)
	if flags&0x04 != 0 { // FEXTRA: a 16-bit length, then that many bytes, skipped
		var l [2]byte
		if _, err := w.ReadAt(l[:], pos); err != nil {
			return Verdict{Outcome: Rejected, Reason: readFailReason(err)}
		}
		pos += 2 + (int64(l[0]) | int64(l[1])<<8)
		if pos > w.Len() {
			return Verdict{Outcome: Rejected, Reason: "truncated"}
		}
	}
	var name []byte
	for _, bit := range []byte{0x08, 0x10} { // FNAME, then FCOMMENT
		if flags&bit == 0 {
			continue
		}
		s, end, reason := gzipString(w, pos, bit == 0x08)
		if reason != "" {
			return Verdict{Outcome: Rejected, Reason: reason}
		}
		if bit == 0x08 {
			name = s
		}
		pos = end
	}
	if flags&0x02 != 0 { // FHCRC: two bytes
		pos += 2
		if pos > w.Len() {
			return Verdict{Outcome: Rejected, Reason: "truncated"}
		}
	}
	v := Verdict{Outcome: Bounded, Length: w.Len()}
	if flags&0x08 != 0 {
		v.Basis = []string{"name=" + escapeBasis(name)}
	}
	return v
}

// gzipString reads a NUL-terminated string at pos in 4 KiB pieces, at most
// gzipSearchMax bytes. It returns the first gzipNameMax bytes when keep is
// set, and the offset past the NUL. No NUL within the limit is a bad header; a
// window that ends first is truncated.
func gzipString(w *Window, pos int64, keep bool) (s []byte, end int64, reason string) {
	var buf [4096]byte
	for off := int64(0); off < gzipSearchMax; {
		n := min(int64(len(buf)), gzipSearchMax-off, w.Len()-pos-off)
		if n <= 0 {
			return nil, 0, "truncated"
		}
		if _, err := w.ReadAt(buf[:n], pos+off); err != nil {
			return nil, 0, readFailReason(err)
		}
		for i, c := range buf[:n] {
			if c == 0 {
				if keep && len(s) < gzipNameMax {
					s = append(s, buf[:min(int64(i), int64(gzipNameMax-len(s)))]...)
				}
				return s, pos + off + int64(i) + 1, ""
			}
		}
		if keep && len(s) < gzipNameMax {
			s = append(s, buf[:min(n, int64(gzipNameMax-len(s)))]...)
		}
		off += n
	}
	return nil, 0, "bad-header"
}

// gzipEscapedMax bounds the escaped name text of a basis entry: the raw name is
// at most gzipNameMax bytes but one byte can escape to four.
const gzipEscapedMax = 128

// escapeBasis renders bytes printably for a basis entry: ASCII text as is,
// everything else as an escape. The escaped text is bounded (not the raw
// bytes): past gzipEscapedMax it ends in "..." at a character boundary.
func escapeBasis(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); {
		_, size := utf8.DecodeRune(b[i:])
		q := strconv.QuoteToASCII(string(b[i : i+size]))
		q = q[1 : len(q)-1]
		if sb.Len()+len(q) > gzipEscapedMax {
			sb.WriteString("...")
			break
		}
		sb.WriteString(q)
		i += size
	}
	return sb.String()
}
