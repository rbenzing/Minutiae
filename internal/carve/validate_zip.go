package carve

import (
	"bytes"
	"errors"
	"strconv"
)

// validateZIP accepts a ZIP archive by its end of central directory record
// (EOCD). From offset 30 forward (bounded by the window and budgeted) every
// "PK\x05\x06" is a candidate: a candidate with a non-zero disk field is
// skipped and counted ("multi-disk" when nothing else wins), a ZIP64 marker is
// rejected ("zip64"), and one whose central directory (offset C, size S with
// C + S == P, C >= 30) does not walk exactly (N entries of 46 bytes plus name,
// extra and comment, landing on P, each local-header offset below C and
// pointing at "PK\x03\x04") is skipped. The first consistent candidate wins:
// the length is P + 22 + the comment length. Entry data is never read or
// inflated; only headers, 32-byte name prefixes and the EOCD are. The layouts
// and the identity name rules are from memory (docs/limitations.md).
func validateZIP(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	var sig [4]byte
	if n, err := w.ReadAt(sig[:], 0); err != nil {
		if n >= 2 && string(sig[:2]) != "PK" {
			return reject("bad-signature")
		}
		return fail(err)
	}
	if string(sig[:]) != "PK\x03\x04" {
		return reject("bad-signature")
	}
	multiDisk := false
	for p := int64(30); p+4 <= w.Len(); p++ {
		b, err := w.Byte(p)
		if err != nil {
			return fail(err)
		}
		if b != 'P' {
			continue
		}
		var s [3]byte
		for i := range s {
			if s[i], err = w.Byte(p + 1 + int64(i)); err != nil {
				return fail(err)
			}
		}
		if s != [3]byte{'K', 5, 6} {
			continue
		}
		v, state, err := zipCandidate(w, p)
		if err != nil {
			return fail(err)
		}
		switch state {
		case zipWins, zipRejects:
			return v
		case zipMultiDisk:
			multiDisk = true
		}
	}
	if multiDisk {
		return reject("multi-disk")
	}
	return reject("no-eocd")
}

type zipState int

const (
	zipSkipped zipState = iota
	zipMultiDisk
	zipWins
	zipRejects
)

// zipCandidate judges the EOCD candidate at window offset p. A returned error
// is a read failure (or a refusal of the window or budget).
func zipCandidate(w *Window, p int64) (Verdict, zipState, error) {
	if p+22 > w.Len() {
		return Verdict{}, zipSkipped, nil
	}
	var e [22]byte
	if _, err := w.ReadAt(e[:], p); err != nil {
		return Verdict{}, zipSkipped, err
	}
	le16 := func(b []byte, o int) int64 { return int64(b[o]) | int64(b[o+1])<<8 }
	le32 := func(b []byte, o int) int64 { return le16(b, o) | le16(b, o+2)<<16 }
	n, total, size, off := le16(e[:], 8), le16(e[:], 10), le32(e[:], 12), le32(e[:], 16)
	if le16(e[:], 4) != 0 || le16(e[:], 6) != 0 {
		// Counted as a split archive only when the other fields are plausible;
		// a stray signature in random data stays "no-eocd".
		if n == total && total >= 1 && off >= 30 && off+size == p {
			return Verdict{}, zipMultiDisk, nil
		}
		return Verdict{}, zipSkipped, nil
	}
	if n == 0xFFFF || total == 0xFFFF || size == 0xFFFFFFFF || off == 0xFFFFFFFF {
		return Verdict{Outcome: Rejected, Reason: "zip64"}, zipRejects, nil
	}
	if p >= 20 {
		var loc [4]byte
		if _, err := w.ReadAt(loc[:], p-20); err != nil {
			return Verdict{}, zipSkipped, err
		}
		if string(loc[:]) == "PK\x06\x07" {
			return Verdict{Outcome: Rejected, Reason: "zip64"}, zipRejects, nil
		}
	}
	if n != total || total < 1 || off < 30 || off+size != p {
		return Verdict{}, zipSkipped, nil
	}
	kind, ok, err := zipWalk(w, off, p, total)
	if err != nil || !ok {
		return Verdict{}, zipSkipped, err
	}
	comment := le16(e[:], 20)
	length := p + 22 + comment
	if length > w.Len() {
		return Verdict{Outcome: Rejected, Reason: "truncated"}, zipRejects, nil
	}
	return Verdict{
		Outcome: Validated, Length: length, Type: kind, Ext: kind,
		Basis: []string{"entries=" + strconv.FormatInt(total, 10)},
	}, zipWins, nil
}

// zipWalk walks the central directory [c, p) of n entries and returns the
// identity from the entry names. ok is false when the directory does not tile
// exactly or a local-header offset is not a local header.
func zipWalk(w *Window, c, p, n int64) (kind string, ok bool, err error) {
	var apk, dex, jar, ctypes, word, xl, ppt bool
	pos := c
	for range n {
		if p-pos < 46 {
			return "", false, nil
		}
		var h [46]byte
		if _, err := w.ReadAt(h[:], pos); err != nil {
			return "", false, err
		}
		if string(h[:4]) != "PK\x01\x02" {
			return "", false, nil
		}
		nl := int64(h[28]) | int64(h[29])<<8
		el := int64(h[30]) | int64(h[31])<<8
		cl := int64(h[32]) | int64(h[33])<<8
		lo := int64(h[42]) | int64(h[43])<<8 | int64(h[44])<<16 | int64(h[45])<<24
		next := pos + 46 + nl + el + cl
		if next > p || lo >= c {
			return "", false, nil
		}
		var l [4]byte
		if _, err := w.ReadAt(l[:], lo); err != nil {
			return "", false, err
		}
		if string(l[:]) != "PK\x03\x04" {
			return "", false, nil
		}
		var name [32]byte
		if k := min(nl, int64(len(name))); k > 0 {
			if _, err := w.ReadAt(name[:k], pos+46); err != nil {
				return "", false, err
			}
		}
		pre := name[:min(nl, int64(len(name)))]
		switch string(pre) {
		case "AndroidManifest.xml":
			apk = true
		case "classes.dex":
			dex = true
		case "META-INF/MANIFEST.MF":
			jar = true
		case "[Content_Types].xml":
			ctypes = true
		}
		word = word || bytes.HasPrefix(pre, []byte("word/"))
		xl = xl || bytes.HasPrefix(pre, []byte("xl/"))
		ppt = ppt || bytes.HasPrefix(pre, []byte("ppt/"))
		pos = next
	}
	if pos != p {
		return "", false, nil
	}
	switch {
	case apk && dex:
		return "apk", true, nil
	case jar:
		return "jar", true, nil
	case ctypes && word:
		return "docx", true, nil
	case ctypes && xl:
		return "xlsx", true, nil
	case ctypes && ppt:
		return "pptx", true, nil
	}
	return "zip", true, nil
}
