package carve

import (
	"strconv"
	"unicode/utf8"
)

// MaxEntryBytes is the longest Basis or Assumptions entry the scanner passes on
// uncut. A longer entry is cut to at most this many bytes (at a character
// boundary) and ends in a marker that names the bytes cut, so the cut is
// visible. The case's recovery description allows 256 bytes an entry; the
// examine package pins MaxEntryBytes+MaxEntryMarkerBytes below that.
const MaxEntryBytes = 200

// MaxEntryMarkerBytes bounds the length of the cut marker for any entry length.
const MaxEntryMarkerBytes = 40

// entryCutMarker is the visible mark of n cut bytes.
func entryCutMarker(n int) string { return "...(cut " + strconv.Itoa(n) + " bytes)" }

// clampEntries returns es with every entry over MaxEntryBytes cut and marked.
// It never changes a short entry and never shares es's backing array.
func clampEntries(es []string) []string {
	out := make([]string, len(es))
	for i, e := range es {
		if len(e) <= MaxEntryBytes {
			out[i] = e
			continue
		}
		keep := MaxEntryBytes
		for keep > 0 && !utf8.RuneStart(e[keep]) {
			keep--
		}
		out[i] = e[:keep] + entryCutMarker(len(e)-keep)
	}
	return out
}
