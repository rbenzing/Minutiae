package carve

import (
	"errors"
	"strconv"
)

// validateJPEG accepts a JPEG from its marker structure and the walk of its entropy-coded data: SOI, a marker
// loop over length-prefixed segments, every scan walked with the frame's Huffman tables (validate_jpeg_scan.go)
// and an EOI after at least one scan. A segment length only moves a cursor inside the window; nothing is
// allocated from it (the one allocation, the refinement state of a progressive frame, is checked against
// jpegRefineCap first), every loop iteration advances at least one byte, and nothing is decoded to pixels. A
// frame the walk cannot cover (a type other than baseline, extended sequential or progressive Huffman, a second
// frame, a refinement state over the cap) is not validated: its length is still found from the markers, and it
// is a footer hit saying why. The layout is from memory (docs/limitations.md).
func validateJPEG(w *Window) Verdict {
	reject := func(reason string) Verdict { return Verdict{Outcome: Rejected, Reason: reason} }
	// fail maps an error: past the window is a truncated object, a walk rejection keeps its reason; any other
	// error (media read, spent budget, cancel) is rejected unreadable and the scanner re-labels it from the
	// window's own state.
	fail := func(err error) Verdict {
		if errors.Is(err, ErrOutsideWindow) {
			return reject("truncated")
		}
		if reason, ok := isJPEGReject(err); ok {
			return reject(reason)
		}
		return reject("unreadable")
	}
	if b, err := w.byteChecked(0); err != nil {
		return fail(err)
	} else if b != 0xFF {
		return reject("bad-marker")
	}
	if b, err := w.byteChecked(1); err != nil {
		return fail(err)
	} else if b != 0xD8 {
		return reject("bad-marker")
	}
	var (
		pos      int64 = 2
		segments int
		scans    int
		dims     string
		haveFrm  bool
		unwalked string // why the entropy-coded data is not walked, "" while it is
		frame    jpegFrame
		tabs     [2][4]jpegHuff
		ri       int64
		hdr      [12]byte
		winLen   = w.Len()
	)
	for {
		b, err := w.byteChecked(pos)
		if err != nil {
			return fail(err)
		}
		if b != 0xFF {
			return reject("bad-marker")
		}
		pos++
		var code byte
		for { // fill: further FF before the code
			if code, err = w.byteChecked(pos); err != nil {
				return fail(err)
			}
			pos++
			if code != 0xFF {
				break
			}
		}
		switch {
		case code == 0xD9:
			if scans == 0 {
				return reject("no-scan")
			}
			basis := []string{"dims=" + dims, "segments=" + strconv.Itoa(segments), "scans=" + strconv.Itoa(scans)}
			if frame.progressive {
				basis = append(basis, "progressive")
			}
			if unwalked != "" {
				return Verdict{
					Outcome: Footer, Length: pos, Basis: append(basis, "entropy-not-walked"),
					Assumptions: []string{"entropy-not-walked=" + unwalked},
				}
			}
			v := Verdict{Outcome: Validated, Length: pos, Basis: append(basis, "entropy-walked")}
			if frame.nonstdPadding {
				v.Assumptions = []string{"jpeg-padding=nonstandard"}
			}
			return v
		case code == 0x01:
			continue
		case code == 0x00 || code == 0xD8 || (code >= 0xD0 && code <= 0xD7):
			return reject("bad-marker")
		}
		var lb [2]byte
		if err := w.readChecked(lb[:], pos); err != nil {
			return fail(err)
		}
		l := int64(lb[0])<<8 | int64(lb[1])
		if l < 2 {
			return reject("bad-length")
		}
		// Early exit only: a segment running past the window would also be
		// found truncated by the next read, so this is not a safety bound.
		if l > winLen-pos {
			return reject("truncated")
		}
		segments++
		segAt := pos
		pos += l
		switch {
		case code >= 0xC0 && code <= 0xCF && code != 0xC4 && code != 0xC8 && code != 0xCC: // a frame header
			// L = 8 + 3 x components.
			if l < 8 {
				return reject("bad-frame")
			}
			if err := w.readChecked(hdr[:6], segAt+2); err != nil {
				return fail(err)
			}
			nf := int64(hdr[5])
			if nf < 1 || nf > 4 || l != 8+3*nf {
				return reject("bad-frame")
			}
			h := int64(hdr[1])<<8 | int64(hdr[2])
			wd := int64(hdr[3])<<8 | int64(hdr[4])
			if h == 0 || wd == 0 {
				return reject("zero-dimensions")
			}
			if haveFrm {
				unwalked = "multiple-frames"
				continue
			}
			haveFrm = true
			dims = strconv.Itoa(int(wd)) + "x" + strconv.Itoa(int(h))
			if code != 0xC0 && code != 0xC1 && code != 0xC2 {
				unwalked = "frame-type"
				continue
			}
			if err := w.readChecked(hdr[:3*nf], segAt+8); err != nil {
				return fail(err)
			}
			frame.progressive, frame.nf = code == 0xC2, int(nf)
			for i := range frame.nf {
				c := &frame.comps[i]
				c.id, c.h, c.v = int(hdr[3*i]), int(hdr[3*i+1]>>4), int(hdr[3*i+1]&15)
				if c.h < 1 || c.h > 4 || c.v < 1 || c.v > 4 {
					return reject("bad-frame")
				}
				for j := range i {
					if frame.comps[j].id == c.id {
						return reject("bad-frame")
					}
				}
			}
			frame.setGeometry(wd, h)
			if frame.progressive && frame.stateBlocks > jpegRefineCap/8 {
				unwalked = "refinement-state-cap"
			}
		case code == 0xC4: // DHT
			if unwalked != "" {
				continue
			}
			if err := readJPEGTables(w, segAt+2, pos, &tabs); err != nil {
				return fail(err)
			}
		case code == 0xDD: // DRI
			if l != 4 {
				return reject("bad-length")
			}
			if err := w.readChecked(lb[:], segAt+2); err != nil {
				return fail(err)
			}
			ri = int64(lb[0])<<8 | int64(lb[1])
		case code == 0xDA: // SOS
			if !haveFrm {
				return reject("no-frame")
			}
			// L = 6 + 2 x Ns, Ns 1..4.
			if l < 6 {
				return reject("bad-scan")
			}
			var sh [12]byte
			if err := w.readChecked(sh[:1], segAt+2); err != nil {
				return fail(err)
			}
			ns := int64(sh[0])
			if ns < 1 || ns > 4 || l != 6+2*ns {
				return reject("bad-scan")
			}
			scans++
			if unwalked != "" {
				var err error
				if pos, err = skipJPEGEntropy(w, pos); err != nil {
					return fail(err)
				}
				continue
			}
			if err := w.readChecked(sh[:4+2*ns], segAt+2); err != nil {
				return fail(err)
			}
			sc, why := frame.parseScan(sh[:4+2*ns], &tabs)
			if why != "" {
				return reject(why)
			}
			var err error
			if pos, err = frame.walkScan(w, &tabs, sc, ri, pos); err != nil {
				return fail(err)
			}
		}
	}
}

// readJPEGTables loads the Huffman tables of a DHT segment payload [at, end).
func readJPEGTables(w *Window, at, end int64, tabs *[2][4]jpegHuff) error {
	for at < end {
		var hd [17]byte
		if end-at < 17 {
			return jpegReject("bad-huffman")
		}
		if err := w.readChecked(hd[:], at); err != nil {
			return err
		}
		at += 17
		class, id := int(hd[0]>>4), int(hd[0]&15)
		if class > 1 || id > 3 {
			return jpegReject("bad-huffman")
		}
		var counts [16]byte
		copy(counts[:], hd[1:])
		n := 0
		for _, c := range counts {
			n += int(c)
		}
		if n > 256 || int64(n) > end-at {
			return jpegReject("bad-huffman")
		}
		var syms [256]byte
		if n > 0 {
			if err := w.readChecked(syms[:n], at); err != nil {
				return err
			}
		}
		at += int64(n)
		var t jpegHuff
		if !t.load(&counts, syms[:n]) {
			return jpegReject("bad-huffman")
		}
		tabs[class][id] = t
	}
	return nil
}

// parseScan checks a scan header (the payload after the length: Ns, Ns component selectors, Ss, Se, Ah/Al) against
// the frame and the tables defined so far; the reason is "" when the scan can be walked.
func (f *jpegFrame) parseScan(p []byte, tabs *[2][4]jpegHuff) (*jpegScan, string) {
	ns := int(p[0])
	sc := &jpegScan{}
	for i := range ns {
		fi := -1
		for j := range f.nf {
			if f.comps[j].id == int(p[1+2*i]) {
				fi = j
			}
		}
		if fi < 0 || slicesContainsInt(sc.comps, fi) {
			return nil, "bad-scan"
		}
		sc.comps = append(sc.comps, fi)
		sc.dc[i], sc.ac[i] = int(p[2+2*i]>>4), int(p[2+2*i]&15)
		if sc.dc[i] > 3 || sc.ac[i] > 3 {
			return nil, "bad-scan"
		}
	}
	q := p[1+2*ns:]
	sc.ss, sc.se, sc.ah, sc.al = int(q[0]), int(q[1]), int(q[2]>>4), int(q[2]&15)
	switch {
	case !f.progressive:
		if sc.ss != 0 || sc.se != 63 || sc.ah != 0 || sc.al != 0 {
			return nil, "bad-scan"
		}
		for i := range ns {
			if !tabs[0][sc.dc[i]].set || !tabs[1][sc.ac[i]].set {
				return nil, "missing-table"
			}
		}
	default:
		if sc.se < sc.ss || sc.se > 63 || sc.ah > 13 || sc.al > 13 {
			return nil, "bad-scan"
		}
		if sc.ah != 0 && sc.ah != sc.al+1 {
			return nil, "bad-scan"
		}
		if sc.ss == 0 && sc.se != 0 || sc.ss > 0 && ns != 1 {
			return nil, "bad-scan"
		}
		for i := range ns {
			switch {
			case sc.ss > 0 && !tabs[1][sc.ac[i]].set, sc.ss == 0 && sc.ah == 0 && !tabs[0][sc.dc[i]].set:
				return nil, "missing-table"
			}
		}
	}
	return sc, ""
}

func slicesContainsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// skipJPEGEntropy finds the end of a scan's entropy-coded data without walking it: FF 00 stuffing, FF D0-D7
// restart and FF FF fill stay inside; FF followed by any other code ends it at that FF. An empty scan is
// rejected. It returns the position of that FF.
func skipJPEGEntropy(w *Window, pos int64) (int64, error) {
	data := false
	for {
		b, err := w.byteChecked(pos)
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			data = true
			pos++
			continue
		}
		n, err := w.byteChecked(pos + 1)
		if err != nil {
			return 0, err
		}
		switch {
		case n == 0x00:
			data = true
			pos += 2
		case n >= 0xD0 && n <= 0xD7:
			pos += 2
		case n == 0xFF:
			pos++
		default:
			if !data {
				return 0, jpegReject("empty-scan")
			}
			return pos, nil
		}
	}
}
