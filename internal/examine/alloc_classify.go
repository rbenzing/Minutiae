package examine

import (
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// Region kinds of the allocation classifier.
const (
	regionFS    = "fs"    // a partition whose filesystem was recognised: its free map decides
	regionNoFS  = "nofs"  // a partition (or an image with no readable table) with no recognised filesystem: unknown
	regionTable = "table" // bytes outside every partition: free inside Table.Unallocated, allocated elsewhere
)

// allocRegion is one stretch of the image the classifier decides as a whole. Partition, PartOffset and FSType
// are the containing partition (0, 0 and "" outside partitions and for a partition with no filesystem name).
type allocRegion struct {
	Start, End int64
	Partition  int
	PartOffset int64
	FSType     string
	Kind       string // "fs", "nofs", "table"
}

// allocClassifier states the allocation of image bytes without touching a case: the regions of the image
// (partitions and the pieces between them, split at the table's gaps) sorted by Start, one free map per
// recognised filesystem and one for the table gaps. It is pure over the session's reads: the carve writer and
// the allocation recheck (R9) share it, so they cannot disagree. Bytes it cannot decide are unknown, never free.
type allocClassifier struct {
	regions []allocRegion
	maps    []*freeMap // parallel to regions: the free map of an fs region, the gap map of a table region
}

// newUnknownClassifier is the classifier of an image whose partition table could not be read: one region of
// unknown bytes.
func newUnknownClassifier(size int64) *allocClassifier {
	if size <= 0 {
		return &allocClassifier{}
	}
	return &allocClassifier{
		regions: []allocRegion{{Start: 0, End: size, Kind: regionNoFS}},
		maps:    []*freeMap{nil},
	}
}

// newAllocClassifier builds the classifier of the session's image: it needs the partition table.
func (s *Session) newAllocClassifier() (*allocClassifier, error) {
	if err := s.needTable(); err != nil {
		return nil, err
	}
	size := s.Image.Size()
	a := &allocClassifier{}

	// partitions sorted by start, each clipped to the image and to what the previous one left (a hostile table
	// may overlap them); the filesystem is still read from the whole partition
	parts := slices.Clone(s.partitions())
	slices.SortStableFunc(parts, func(x, y volume.Partition) int {
		switch {
		case x.Start < y.Start:
			return -1
		case x.Start > y.Start:
			return 1
		}
		return x.Index - y.Index
	})
	var covered []evidence.Run // the partition regions, in order
	var prevEnd int64
	for _, p := range parts {
		end, ok := filesys.AddOK(p.Start, p.Length)
		if !ok || p.Start < 0 || p.Length <= 0 {
			continue
		}
		start, end := max(p.Start, prevEnd), min(end, size)
		if end <= start {
			continue
		}
		prevEnd = end
		reg := allocRegion{Start: start, End: end, Partition: p.Index, PartOffset: p.Start, Kind: regionNoFS}
		var m *freeMap
		if fsys, err := s.openFS(p); err == nil {
			runs, note, uerr := s.unallocatedRunsNote(fsys, p, false)
			if uerr == nil {
				// warnings are read after Unallocated(): a reader that learned something there makes every byte
				// outside the free map unknown (ruling C5); so does a free list with unusable runs
				reg.FSType, reg.Kind = fsys.Info().Type, regionFS
				m = newFreeMap(runs, len(fsys.Info().Warnings) > 0 || note != "")
			}
		}
		a.regions = append(a.regions, reg)
		a.maps = append(a.maps, m)
		covered = append(covered, evidence.Run{Offset: start, Length: end - start})
	}

	// the rest of the image, split where the table's gaps begin and end: each piece is wholly structure or
	// wholly a gap (3E-R23)
	gaps := make([]evidence.Run, 0, len(s.Table.Unallocated))
	for _, u := range s.Table.Unallocated {
		gaps = append(gaps, evidence.Run{Offset: u.Offset, Length: u.Length})
	}
	gapMap := newFreeMap(gaps, false)
	var pos int64
	piece := func(lo, hi int64) {
		for lo < hi {
			cut := hi
			for _, g := range gapMap.free { // sorted, merged: the first boundary inside (lo, hi)
				if g.Offset > lo && g.Offset < cut {
					cut = g.Offset
				}
				if e := g.Offset + g.Length; e > lo && e < cut {
					cut = e
				}
			}
			a.regions = append(a.regions, allocRegion{Start: lo, End: cut, Kind: regionTable})
			a.maps = append(a.maps, gapMap)
			lo = cut
		}
	}
	for _, c := range covered {
		piece(pos, c.Offset)
		pos = c.Offset + c.Length
	}
	piece(pos, size)

	idx := make([]int, len(a.regions))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(x, y int) int {
		switch {
		case a.regions[x].Start < a.regions[y].Start:
			return -1
		case a.regions[x].Start > a.regions[y].Start:
			return 1
		}
		return 0
	})
	regions := make([]allocRegion, len(idx))
	maps := make([]*freeMap, len(idx))
	for i, j := range idx {
		regions[i], maps[i] = a.regions[j], a.maps[j]
	}
	a.regions, a.maps = regions, maps
	return a, nil
}

// freeBytes counts the bytes of [off, off+length) that lie inside the free map.
func (m *freeMap) freeBytes(off, length int64) int64 {
	end := off + length // the caller checked the range
	i := sort.Search(len(m.free), func(i int) bool { return m.free[i].Offset+m.free[i].Length > off })
	var n int64
	for ; i < len(m.free) && m.free[i].Offset < end; i++ {
		n += min(end, m.free[i].Offset+m.free[i].Length) - max(off, m.free[i].Offset)
	}
	return n
}

// classify counts the bytes of r by allocation state. ok is false when the run is empty, negative, wraps or
// leaves the regions of the image, or crosses a region boundary: a scope run never does (it is cut at the
// region boundaries), so a false answer is a caller defect and the run is not decided.
func (a *allocClassifier) classify(r evidence.Run) (evidence.AllocSummary, allocRegion, bool) {
	if r.Offset < 0 || r.Length <= 0 {
		return evidence.AllocSummary{}, allocRegion{}, false
	}
	end, ok := filesys.AddOK(r.Offset, r.Length)
	if !ok {
		return evidence.AllocSummary{}, allocRegion{}, false
	}
	i := sort.Search(len(a.regions), func(i int) bool { return a.regions[i].Start > r.Offset }) - 1
	if i < 0 || r.Offset >= a.regions[i].End || end > a.regions[i].End {
		return evidence.AllocSummary{}, allocRegion{}, false
	}
	reg := a.regions[i]
	var sum evidence.AllocSummary
	switch m := a.maps[i]; {
	case reg.Kind == regionNoFS || m == nil:
		sum.Unknown = r.Length
	default:
		free := m.freeBytes(r.Offset, r.Length)
		sum.Free = free
		if m.nonFree == "unknown" {
			sum.Unknown = r.Length - free
		} else {
			sum.Allocated = r.Length - free
		}
	}
	return sum, reg, true
}
