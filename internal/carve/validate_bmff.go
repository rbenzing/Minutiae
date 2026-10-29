package carve

import (
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
)

// bmffMaxBasis caps the box types listed in the basis.
const bmffMaxBasis = 16

// bmffTopLevel reports whether t is a type the top-level walk continues over.
// The list is from memory (docs/limitations.md).
func bmffTopLevel(t string) bool {
	switch t {
	case "moov", "mdat", "free", "skip", "wide", "uuid", "meta", "moof", "mfra",
		"sidx", "ssix", "styp", "pdin", "prft", "emsg", "udta", "junk", "pnot":
		return true
	}
	return false // ftyp included: a second ftyp starts another file
}

// bmffTypeBytesOK: every type byte is printable ASCII or 0xA9.
func bmffTypeBytesOK(t []byte) bool {
	for _, c := range t {
		if (c < 0x20 || c > 0x7E) && c != 0xA9 {
			return false
		}
	}
	return true
}

// bmffIdentity maps a major brand to the refined type, extension and whether
// the file must hold a picture meta box.
func bmffIdentity(brand string) (typ, ext string, heif bool) {
	switch brand {
	case "heic", "heix", "heim", "heis", "hevc", "hevx":
		return "heic", "heic", true
	case "mif1", "msf1":
		return "heif", "heif", true
	case "qt  ":
		return "mov", "mov", false
	case "mp41", "mp42", "mp71", "avc1", "dash", "M4V ", "M4A ", "M4B ", "M4P ", "f4v ", "isom":
		return "mp4", "mp4", false
	}
	if strings.HasPrefix(brand, "3gp") || strings.HasPrefix(brand, "3g2") {
		return "3gp", "3gp", false
	}
	if len(brand) == 4 && brand[:3] == "iso" && brand[3] >= '2' && brand[3] <= '9' {
		return "mp4", "mp4", false
	}
	return "isobmff", "bmff", false
}

// bmffBrandText renders a brand for the basis: printable ASCII as is, any
// other byte as \xNN, so the text stays lossless.
func bmffBrandText(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		if c >= 0x20 && c <= 0x7E {
			s.WriteByte(c)
		} else {
			s.WriteString(`\x` + strconv.FormatUint(uint64(c)>>4, 16) + strconv.FormatUint(uint64(c)&15, 16))
		}
	}
	return s.String()
}

// bmffKids is what a child walk learned: a mvhd child, and the handler type
// of the first hdlr child.
type bmffKids struct {
	mvhd, hdlr bool
	handler    string
}

// bmffChildren walks the children that must tile [lo, hi) exactly. Every
// iteration advances by at least 8 bytes inside a range that already fits the
// window. reason is a rejection; err is a read failure of the window.
func bmffChildren(w *Window, lo, hi int64) (k bmffKids, reason string, err error) {
	for pos := lo; pos < hi; {
		if hi-pos < 8 {
			return k, "bad-children", nil
		}
		var hdr [8]byte
		if _, err := w.ReadAt(hdr[:], pos); err != nil {
			return k, "", err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		if size < 8 || size > hi-pos || !bmffTypeBytesOK(hdr[4:]) {
			return k, "bad-children", nil
		}
		switch string(hdr[4:]) {
		case "mvhd":
			k.mvhd = true
		case "hdlr":
			if !k.hdlr {
				k.hdlr = true
				if size >= 8+12 { // version/flags, pre_defined, handler_type
					var h [4]byte
					if _, err := w.ReadAt(h[:], pos+16); err != nil {
						return k, "", err
					}
					k.handler = string(h[:])
				}
			}
		}
		pos += size
	}
	return k, "", nil
}

// validateBMFF accepts an ISO base media file from its box structure alone:
// an ftyp first, then top-level boxes of the known set up to the first byte
// that is not one, with a moov (or, for HEIF brands, a meta whose hdlr is
// "pict") among them. The identity (mp4, mov, 3gp, heic, heif) comes from the
// major brand, checked against that structure. A box size only moves a cursor
// inside the window; every iteration advances at least 8 bytes; no media data
// is read. The brand table, the top-level list and the meta/hdlr layout are
// from memory (docs/limitations.md).
func validateBMFF(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	// fail maps a window error: past the window is a truncated object; any
	// other error (media read, spent budget, cancel) is rejected unreadable
	// and the scanner re-labels it from the window's own state.
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		return reject("unreadable")
	}
	winLen := w.Len()
	var hdr [8]byte
	if _, err := w.ReadAt(hdr[:], 0); err != nil {
		return fail(err)
	}
	ftypSize := int64(binary.BigEndian.Uint32(hdr[:4]))
	if string(hdr[4:]) != "ftyp" || ftypSize < 16 || ftypSize > 4096 || (ftypSize-16)%4 != 0 {
		return reject("bad-ftyp")
	}
	if ftypSize > winLen {
		return reject("truncated")
	}
	var brandB [4]byte
	if _, err := w.ReadAt(brandB[:], 8); err != nil {
		return fail(err)
	}
	brand := bmffBrandText(brandB[:])
	typ, ext, heif := bmffIdentity(string(brandB[:]))

	var (
		pos      = ftypSize
		boxes    []string
		haveMoov bool
		havePict bool
	)
	have := func() bool {
		if heif {
			return havePict
		}
		return haveMoov
	}
	finish := func(v Verdict) Verdict {
		v.Type, v.Ext = typ, ext
		v.Basis = []string{"brand=" + brand, "boxes=" + strings.Join(boxes, ",")}
		return v
	}
	for winLen-pos >= 8 {
		if _, err := w.ReadAt(hdr[:], pos); err != nil {
			return fail(err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		boxType := string(hdr[4:])
		if (size >= 2 && size <= 7) || !bmffTypeBytesOK(hdr[4:]) || !bmffTopLevel(boxType) {
			break
		}
		hdrLen := int64(8)
		switch size {
		case 0:
			if !have() {
				return reject("unbounded-box")
			}
			if len(boxes) < bmffMaxBasis {
				boxes = append(boxes, boxType)
			}
			return finish(Verdict{Outcome: Bounded, Length: winLen, Assumptions: []string{"box-extends-to-end"}})
		case 1:
			var big [8]byte
			if _, err := w.ReadAt(big[:], pos+8); err != nil {
				return fail(err)
			}
			large := binary.BigEndian.Uint64(big[:])
			if large < 16 {
				return reject("bad-length")
			}
			if large > uint64(winLen-pos) { // also covers values past MaxInt64
				return reject("truncated")
			}
			size, hdrLen = int64(large), 16
		}
		if size > winLen-pos {
			return reject("truncated")
		}
		switch boxType {
		case "moov":
			k, reason, err := bmffChildren(w, pos+hdrLen, pos+size)
			if err != nil {
				return fail(err)
			}
			if reason == "" && !k.mvhd {
				reason = "bad-children"
			}
			if reason != "" {
				return reject(reason)
			}
			haveMoov = true
		case "meta":
			if !heif {
				break // not needed for this identity: its layout is not judged
			}
			if size-hdrLen < 4 {
				return reject("bad-children")
			}
			k, reason, err := bmffChildren(w, pos+hdrLen+4, pos+size)
			if err != nil {
				return fail(err)
			}
			if reason != "" {
				return reject(reason)
			}
			if k.handler == "pict" {
				havePict = true
			}
		}
		if len(boxes) < bmffMaxBasis {
			boxes = append(boxes, boxType)
		}
		pos += size
	}
	if !have() {
		if heif {
			return reject("missing-meta")
		}
		return reject("missing-moov")
	}
	return finish(Verdict{Outcome: Validated, Length: pos})
}
