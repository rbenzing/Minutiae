package carve

import "strconv"

// Ogg page limits: the header is 27 bytes, a page has at most 255 segments of
// at most 255 bytes each.
const (
	oggHeaderLen  = 27
	oggMaxPageLen = oggHeaderLen + 255 + 255*255
	// oggMaxStreams bounds the logical streams one object may begin (a typed
	// rejection beyond it, never unbounded state).
	oggMaxStreams = 256
)

// oggCRCTable builds the 256-entry table of the Ogg CRC-32 (polynomial
// 0x04C11DB7, not reflected). It is built per call: the package keeps no state.
func oggCRCTable() *[256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return &t
}

// validateOgg accepts an Ogg object from its page structure (from memory: see
// docs/limitations.md). Each page is a 27-byte header, a lacing table and the
// body; its checksum is verified. The first page must begin a stream (BOS), a
// page may only belong to a stream that began and has not ended, and the
// object ends at the page after which every begun stream has seen its EOS.
// A walk that stops before that (other bytes, or the window end) is no-eos.
// Pages are read whole into one fixed buffer; no size read from the media
// allocates or moves the cursor outside the window.
func validateOgg(w *Window) Verdict {
	tab := oggCRCTable()
	page := make([]byte, oggMaxPageLen)
	type state struct{ ended bool }
	streams := make(map[uint32]*state)
	open, pages := 0, 0
	pos := int64(0)
	for {
		left := w.Len() - pos
		if left < 4 {
			return Verdict{Outcome: Rejected, Reason: "no-eos"}
		}
		n := min(left, oggHeaderLen)
		if _, err := w.ReadAt(page[:n], pos); err != nil {
			return Verdict{Outcome: Rejected, Reason: readFailReason(err)}
		}
		if string(page[:4]) != "OggS" {
			return Verdict{Outcome: Rejected, Reason: "no-eos"}
		}
		if n < oggHeaderLen {
			return Verdict{Outcome: Rejected, Reason: "truncated"}
		}
		if page[4] != 0 || page[5]&^7 != 0 {
			return Verdict{Outcome: Rejected, Reason: "bad-page"}
		}
		segs := int64(page[26])
		if left < oggHeaderLen+segs {
			return Verdict{Outcome: Rejected, Reason: "truncated"}
		}
		if segs > 0 {
			if _, err := w.ReadAt(page[oggHeaderLen:oggHeaderLen+segs], pos+oggHeaderLen); err != nil {
				return Verdict{Outcome: Rejected, Reason: readFailReason(err)}
			}
		}
		total := oggHeaderLen + segs
		for _, l := range page[oggHeaderLen : oggHeaderLen+segs] {
			total += int64(l)
		}
		if left < total {
			return Verdict{Outcome: Rejected, Reason: "truncated"}
		}
		if total > oggHeaderLen+segs {
			if _, err := w.ReadAt(page[oggHeaderLen+segs:total], pos+oggHeaderLen+segs); err != nil {
				return Verdict{Outcome: Rejected, Reason: readFailReason(err)}
			}
		}
		want := uint32(page[22]) | uint32(page[23])<<8 | uint32(page[24])<<16 | uint32(page[25])<<24
		clear(page[22:26])
		var crc uint32
		for _, c := range page[:total] {
			crc = crc<<8 ^ tab[byte(crc>>24)^c]
		}
		if crc != want {
			return Verdict{Outcome: Rejected, Reason: "crc"}
		}
		flags := page[5]
		serial := uint32(page[14]) | uint32(page[15])<<8 | uint32(page[16])<<16 | uint32(page[17])<<24
		if pages == 0 && flags&2 == 0 {
			return Verdict{Outcome: Rejected, Reason: "no-bos"}
		}
		st := streams[serial]
		switch {
		case flags&2 != 0:
			if st != nil {
				return Verdict{Outcome: Rejected, Reason: "bad-page"}
			}
			if len(streams) == oggMaxStreams {
				return Verdict{Outcome: Rejected, Reason: "too-many-streams"}
			}
			st = &state{}
			streams[serial] = st
			open++
		case st == nil || st.ended:
			return Verdict{Outcome: Rejected, Reason: "bad-page"}
		}
		if flags&4 != 0 {
			st.ended = true
			open--
		}
		pages++
		pos += total
		if open == 0 {
			return Verdict{Outcome: Validated, Length: pos, Basis: []string{"streams=" + strconv.Itoa(len(streams)), "pages=" + strconv.Itoa(pages)}}
		}
	}
}
