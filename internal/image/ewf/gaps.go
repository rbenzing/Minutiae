package ewf

import (
	"cmp"
	"slices"
)

// AcquisitionGap is a range of media bytes the acquirer reported as unreadable
// (an error2 range). The image holds zeros there; they are part of the media as
// stored and are not device data.
type AcquisitionGap struct{ Offset, Length int64 }

// AcquisitionGaps is the error2 information in media coordinates.
type AcquisitionGaps struct {
	// Ranges is how many ranges the container declares (before merging).
	Ranges uint64
	// Kept are the usable ranges, clipped to the media, sorted and merged.
	Kept []AcquisitionGap
	// Outside counts declared ranges that lie wholly or partly beyond the media
	// (clipped or dropped, never silently).
	Outside int
	// Truncated reports that the container declares more ranges than are kept
	// (maxErrorRanges): the overlap of data with the unkept ones is unknown.
	Truncated bool
}

// AcquisitionGaps returns the error2 ranges as media byte ranges, as the image
// states them (the acquirer's record, not a verified fact). The zero value
// means the container declares none.
func (r *Reader) AcquisitionGaps() AcquisitionGaps {
	g := r.gaps
	g.Kept = slices.Clone(g.Kept)
	return g
}

// buildGaps converts the sector ranges of e to media byte ranges. A sector is
// bytes_per_sector bytes of the volume section; products are overflow-checked.
func buildGaps(e *error2Info, g geometry) AcquisitionGaps {
	if e == nil {
		return AcquisitionGaps{}
	}
	out := AcquisitionGaps{Ranges: e.count, Truncated: e.count > uint64(len(e.ranges))}
	size := uint64(g.size)
	for _, rg := range e.ranges {
		off, ok1 := mulOK(uint64(rg[0]), uint64(g.bps))
		n, ok2 := mulOK(uint64(rg[1]), uint64(g.bps))
		switch {
		case n == 0 && ok2:
			continue
		case !ok1 || !ok2 || off >= size:
			out.Outside++
			continue
		case n > size-off: // off < size, so the subtraction is exact
			n = size - off
			out.Outside++
		}
		out.Kept = append(out.Kept, AcquisitionGap{Offset: int64(off), Length: int64(n)})
	}
	slices.SortFunc(out.Kept, func(a, b AcquisitionGap) int { return cmp.Compare(a.Offset, b.Offset) })
	merged := out.Kept[:0]
	for _, k := range out.Kept {
		if m := len(merged) - 1; m >= 0 && k.Offset <= merged[m].Offset+merged[m].Length {
			merged[m].Length = max(merged[m].Length, k.Offset+k.Length-merged[m].Offset)
			continue
		}
		merged = append(merged, k)
	}
	out.Kept = merged
	return out
}
