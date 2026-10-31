package carve

import (
	"encoding/binary"
	"errors"
	"strings"
)

// jxlSignatureHead is the first 8 bytes of the 12-byte JPEG XL signature box:
// size 12 and type "JXL "; the payload is 0D 0A 87 0A.
const jxlSignatureHead = "\x00\x00\x00\x0CJXL "

// jxlTopLevel reports whether t is a box the JPEG XL container walk continues
// over. The set is from memory (docs/limitations.md).
func jxlTopLevel(t string) bool {
	switch t {
	case "jxll", "jxli", "jxlc", "jxlp", "jbrd", "Exif", "xml ", "jumb", "brob", "free", "skip":
		return true
	}
	return false
}

// validateJXLContainer accepts a JPEG XL container from its boxes alone: the
// signature box, an ftyp with major brand "jxl ", then top-level boxes of the
// JPEG XL set up to the first byte that is not one. The codestream must be
// exactly one jxlc, or jxlp parts whose indices count up from 0 with the high
// bit on the last one only, never both. Size 0 on the jxlc (or on the last
// jxlp) extends to the window end and is bounded; size 0 on any other box is
// unbounded-box. A brob box is walked and never decompressed. The only
// codestream bytes read are its first two, which must be FF 0A (in the jxlc
// payload, or in the first jxlp after its index word), else bad-codestream.
func validateJXLContainer(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	var sig [12]byte
	if _, err := w.ReadAt(sig[:], 0); err != nil {
		return fail(err)
	}
	if string(sig[:]) != jxlSignatureHead+"\x0D\x0A\x87\x0A" {
		return reject("bad-signature")
	}
	ftypSize, brand, reason, err := bmffFtyp(w, 12)
	if err != nil {
		return fail(err)
	}
	if reason != "" {
		return reject(reason)
	}
	if string(brand[:]) != "jxl " {
		return reject("bad-ftyp")
	}

	var (
		sawC  bool   // a jxlc was seen
		parts uint32 // jxlp boxes seen
		final bool   // the last-flagged jxlp was seen
	)
	// jxlpIndex reads the index word of the jxlp box b: its counter and flag.
	jxlpIndex := func(b bmffBox) (index uint32, last bool, err error) {
		var word [4]byte
		if _, err := w.ReadAt(word[:], b.Pos+b.HdrLen); err != nil {
			return 0, false, err
		}
		v := binary.BigEndian.Uint32(word[:])
		return v &^ (1 << 31), v>>31 == 1, nil
	}
	// hasMagic reports whether the codestream starting at at, with avail bytes
	// available to its box, begins FF 0A. Two bytes are the only codestream
	// bytes read.
	hasMagic := func(at, avail int64) (bool, error) {
		if avail < 2 {
			return false, nil
		}
		var m [2]byte
		if _, err := w.ReadAt(m[:], at); err != nil {
			return false, err
		}
		return m[0] == 0xFF && m[1] == 0x0A, nil
	}
	visit := func(b bmffBox) (string, error) {
		switch b.Type {
		case "jxlc":
			if sawC || parts > 0 {
				return "bad-codestream", nil
			}
			sawC = true
			ok, err := hasMagic(b.Pos+b.HdrLen, b.Size-b.HdrLen)
			if err != nil {
				return "", err
			}
			if !ok {
				return "bad-codestream", nil
			}
		case "jxlp":
			if sawC || final || b.Size-b.HdrLen < 4 {
				return "bad-codestream", nil
			}
			index, last, err := jxlpIndex(b)
			if err != nil {
				return "", err
			}
			if index != parts {
				return "bad-codestream", nil
			}
			if parts == 0 {
				ok, err := hasMagic(b.Pos+b.HdrLen+4, b.Size-b.HdrLen-4)
				if err != nil {
					return "", err
				}
				if !ok {
					return "bad-codestream", nil
				}
			}
			parts++
			final = last
		}
		return "", nil
	}
	end, zero, boxes, reason, err := bmffWalk(w, 12+ftypSize, jxlTopLevel, visit)
	if err != nil {
		return fail(err)
	}
	if reason != "" {
		return reject(reason)
	}
	finish := func(v Verdict) Verdict {
		v.Type, v.Ext = "jxl", "jxl"
		v.Basis = []string{"brand=jxl ", "boxes=" + strings.Join(boxes, ",")}
		return v
	}
	if zero != nil {
		switch zero.Type {
		case "jxlc":
			if sawC || parts > 0 {
				return reject("bad-codestream")
			}
			ok, err := hasMagic(zero.Pos+8, w.Len()-zero.Pos-8)
			if err != nil {
				return fail(err)
			}
			if !ok {
				return reject("bad-codestream")
			}
		case "jxlp":
			if sawC || final {
				return reject("bad-codestream")
			}
			if w.Len()-zero.Pos < 12 {
				return reject("truncated")
			}
			index, last, err := jxlpIndex(*zero)
			if err != nil {
				return fail(err)
			}
			if !last {
				return reject("unbounded-box")
			}
			if index != parts {
				return reject("bad-codestream")
			}
			if parts == 0 {
				ok, err := hasMagic(zero.Pos+12, w.Len()-zero.Pos-12)
				if err != nil {
					return fail(err)
				}
				if !ok {
					return reject("bad-codestream")
				}
			}
		default:
			return reject("unbounded-box")
		}
		return finish(Verdict{Outcome: Bounded, Length: w.Len(), Assumptions: []string{"box-extends-to-end"}})
	}
	switch {
	case !sawC && parts == 0:
		return reject("missing-codestream")
	case parts > 0 && !final:
		return reject("bad-codestream")
	}
	return finish(Verdict{Outcome: Validated, Length: end})
}
