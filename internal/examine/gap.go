package examine

import (
	"math"
	"sort"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
)

// maxGapWork bounds the (run, gap) pairs examined for one artifact: a hostile
// run list that overlaps itself cannot make the overlap computation superlinear.
// Past it the overlap is recorded as unknown.
var maxGapWork = 1 << 22

// gapNote returns the acquisition-gap note for an artifact of size bytes whose
// image runs are runs (holes are Offset -1). runsKnown is false when the runs
// could not be recorded. It returns nil when the image declares no gaps, when
// the artifact is empty, or when nothing overlaps and nothing is unknown.
func (s *Session) gapNote(runs []evidence.Run, runsKnown bool, size int64) *evidence.AcquisitionGapNote {
	gr, ok := s.Image.(image.GapReporter)
	if !ok {
		return nil
	}
	return gapNoteFor(gr.AcquisitionGaps(), runs, runsKnown, size)
}

// gapNoteFor is the one rule of the acquisition-gap note, shared by the writers (Session.gapNote) and the
// verify recompute (R10), so the two can never disagree about the rule.
func gapNoteFor(g image.AcquisitionGaps, runs []evidence.Run, runsKnown bool, size int64) *evidence.AcquisitionGapNote {
	if (g.Ranges == 0 && g.State != image.GapsDamaged) || size <= 0 {
		return nil
	}
	// A damaged error2 section means the gaps are unknown, never "none".
	note := &evidence.AcquisitionGapNote{Unknown: g.Truncated || g.State == image.GapsDamaged || !runsKnown || len(runs) == 0}
	if note.Unknown && (!runsKnown || len(runs) == 0) {
		return note
	}
	// Runs that overlap each other (a hostile or damaged filesystem) would count
	// a byte once per run: detect it explicitly (sort and compare), count the
	// union once and report the figure as unknown, never as an exact one.
	var overlap bool
	runs, overlap = unionRuns(runs)
	if overlap {
		note.Unknown = true
	}
	work := 0
	for _, r := range runs {
		end, ok := filesys.AddOK(r.Offset, r.Length)
		if !ok {
			note.Unknown = true
			continue
		}
		i := sort.Search(len(g.Kept), func(i int) bool { return g.Kept[i].Offset+g.Kept[i].Length > r.Offset })
		for ; i < len(g.Kept) && g.Kept[i].Offset < end; i++ {
			if work++; work > maxGapWork {
				note.Unknown = true
				return note
			}
			lo := max(g.Kept[i].Offset, r.Offset)
			hi := min(g.Kept[i].Offset+g.Kept[i].Length, end)
			if note.Bytes > math.MaxInt64-(hi-lo) {
				note.Bytes, note.Unknown = math.MaxInt64, true
			} else {
				note.Bytes += hi - lo
			}
			note.Ranges++
			if len(note.First) < evidence.MaxGapNoteRuns {
				note.First = append(note.First, evidence.Run{Offset: lo, Length: hi - lo})
			}
		}
	}
	if note.Bytes > size {
		// Runs of a hostile filesystem may overlap each other, so a byte can be
		// counted once per run: the exact count is undecidable, and verify's
		// "no more than the artifact holds" rule must never reject an honest write.
		note.Bytes, note.Unknown = size, true
	}
	if note.Bytes == 0 && !note.Unknown {
		return nil
	}
	return note
}

// unionRuns returns the image-backed runs (holes and empty runs dropped) sorted
// by offset with any overlap clipped away, so no byte is counted twice, and
// reports whether any two runs overlapped. A run whose end overflows is kept
// as it is (gapNote reports it unknown).
func unionRuns(runs []evidence.Run) ([]evidence.Run, bool) {
	valid := make([]evidence.Run, 0, len(runs))
	for _, r := range runs {
		if r.Offset >= 0 && r.Length > 0 {
			valid = append(valid, r)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Offset < valid[j].Offset })
	out := valid[:0]
	var overlap bool
	var maxEnd int64
	for _, r := range valid {
		end, ok := filesys.AddOK(r.Offset, r.Length)
		if !ok {
			out = append(out, r)
			continue
		}
		if r.Offset < maxEnd {
			overlap = true
			if end <= maxEnd {
				continue // wholly inside earlier runs
			}
			r = evidence.Run{Offset: maxEnd, Length: end - maxEnd}
		}
		maxEnd = max(maxEnd, end)
		out = append(out, r)
	}
	return out, overlap
}
