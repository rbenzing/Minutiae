package examine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// The pure part of the recovery planner (spec 6.2, 3.3): classification of candidate runs against the
// free space, the cut at the first non-free byte, overlap between candidates, the confidence rules and
// the uniform-content scan. Nothing here touches a case or the audit log.

// freeMap holds the sorted, merged free runs of a filesystem (image-relative) and the state a byte
// that is not free is reported in.
type freeMap struct {
	free    []evidence.Run
	nonFree string // "allocated" or "unknown"
}

// newFreeMap takes the free runs (merged already, as unallocatedRuns delivers them; a copy is sorted
// and merged again so that a map can never be wrongly narrow) and reports a byte outside them as
// "unknown" when unknownWhenNotFree is set, else "allocated". Invalid runs are dropped.
func newFreeMap(free []evidence.Run, unknownWhenNotFree bool) *freeMap {
	m := &freeMap{nonFree: "allocated"}
	if unknownWhenNotFree {
		m.nonFree = "unknown"
	}
	valid := make([]evidence.Run, 0, len(free))
	for _, r := range free {
		if r.Offset < 0 || r.Length <= 0 {
			continue
		}
		if _, ok := filesys.AddOK(r.Offset, r.Length); !ok {
			continue
		}
		valid = append(valid, r)
	}
	slices.SortFunc(valid, func(a, b evidence.Run) int {
		switch {
		case a.Offset < b.Offset:
			return -1
		case a.Offset > b.Offset:
			return 1
		}
		return 0
	})
	for _, r := range valid {
		if n := len(m.free); n > 0 {
			last := &m.free[n-1]
			if r.Offset <= last.Offset+last.Length { // cannot overflow: validated above
				if end := r.Offset + r.Length; end > last.Offset+last.Length {
					last.Length = end - last.Offset
				}
				continue
			}
		}
		m.free = append(m.free, r)
	}
	return m
}

// runAt returns the free run holding off.
func (m *freeMap) runAt(off int64) (evidence.Run, bool) {
	i := sort.Search(len(m.free), func(i int) bool { return m.free[i].Offset > off })
	if i == 0 {
		return evidence.Run{}, false
	}
	r := m.free[i-1]
	if off < r.Offset+r.Length {
		return r, true
	}
	return evidence.Run{}, false
}

// contains reports whether the whole range [off, off+length) lies inside one free run. An empty,
// negative or overflowing range is never "contained".
func (m *freeMap) contains(off, length int64) bool {
	if off < 0 || length <= 0 {
		return false
	}
	end, ok := filesys.AddOK(off, length)
	if !ok {
		return false
	}
	r, ok := m.runAt(off)
	return ok && end <= r.Offset+r.Length
}

// cutResult is the outcome of cutting a run list at the first byte that is not free.
type cutResult struct {
	Captured, Excluded           []evidence.Run // Excluded: everything after the cut, the cut run split at the cut point
	CapturedBytes, ExcludedBytes int64
	State                        string // "" nothing cut; else the state of the first non-free byte
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// cutAtFirstNonFree keeps the longest prefix of the run sequence (in file order, byte by byte) whose
// every byte is free. Free runs after the first non-free byte are excluded (they would shift offsets
// in the file). Runs with no bytes are dropped; invalid runs (negative offset, wrapping end) are never
// free. Byte counts saturate at MaxInt64.
func cutAtFirstNonFree(m *freeMap, runs []evidence.Run) cutResult {
	var out cutResult
	cut := false
	for _, r := range runs {
		if r.Length <= 0 {
			continue
		}
		if cut {
			out.Excluded = append(out.Excluded, r)
			out.ExcludedBytes = satAdd(out.ExcludedBytes, r.Length)
			continue
		}
		var prefix int64
		if r.Offset >= 0 {
			if _, ok := filesys.AddOK(r.Offset, r.Length); ok {
				if f, ok := m.runAt(r.Offset); ok {
					prefix = min(r.Length, f.Offset+f.Length-r.Offset)
				}
			}
		}
		if prefix == r.Length {
			out.Captured = append(out.Captured, r)
			out.CapturedBytes = satAdd(out.CapturedBytes, r.Length)
			continue
		}
		cut = true
		out.State = m.nonFree
		if prefix > 0 {
			out.Captured = append(out.Captured, evidence.Run{Offset: r.Offset, Length: prefix})
			out.CapturedBytes = satAdd(out.CapturedBytes, prefix)
			out.Excluded = append(out.Excluded, evidence.Run{Offset: r.Offset + prefix, Length: r.Length - prefix})
			out.ExcludedBytes = satAdd(out.ExcludedBytes, r.Length-prefix)
		} else {
			out.Excluded = append(out.Excluded, r)
			out.ExcludedBytes = satAdd(out.ExcludedBytes, r.Length)
		}
	}
	return out
}

// ownedRuns is the run list one candidate claims.
type ownedRuns struct {
	Owner string
	Runs  []evidence.Run
}

const maxOverlapListed = 16

type overlapEvent struct {
	pos   int64
	start bool
	id    int // rank of the owner among the sorted distinct owners
}

// overlapsOf maps every owner to the other owners that share at least one byte with it (sorted,
// deduplicated, the 16 smallest when there are more; touching ranges do not overlap; one owner never
// overlaps itself). A sweep line over the run ends and starts: a new owner is paired with the owners
// active at that moment, and only when it is among the 17 smallest of them can it be one of the 16
// smallest of another owner's partners, so the work per start is bounded by 16 unless the owner is
// that small.
func overlapsOf(items []ownedRuns) map[string][]string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Owner)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	rank := make(map[string]int, len(names))
	for i, n := range names {
		rank[n] = i
	}
	var ev []overlapEvent
	for _, it := range items {
		id := rank[it.Owner]
		for _, r := range it.Runs {
			if r.Offset < 0 || r.Length <= 0 {
				continue
			}
			end, ok := filesys.AddOK(r.Offset, r.Length)
			if !ok {
				continue
			}
			ev = append(ev, overlapEvent{r.Offset, true, id}, overlapEvent{end, false, id})
		}
	}
	slices.SortFunc(ev, func(a, b overlapEvent) int {
		switch {
		case a.pos != b.pos:
			if a.pos < b.pos {
				return -1
			}
			return 1
		case a.start != b.start: // ends first: touching is not overlap
			if !a.start {
				return -1
			}
			return 1
		case a.id != b.id:
			return a.id - b.id
		}
		return 0
	})

	counts := make([]int, len(names))
	lists := make([][]int, len(names))
	var active []int // sorted ids with counts > 0
	add := func(owner, other int) {
		l := lists[owner]
		i, found := slices.BinarySearch(l, other)
		if found || i >= maxOverlapListed {
			return
		}
		l = slices.Insert(l, i, other)
		if len(l) > maxOverlapListed {
			l = l[:maxOverlapListed]
		}
		lists[owner] = l
	}
	for _, e := range ev {
		if !e.start {
			counts[e.id]--
			if counts[e.id] == 0 {
				i, _ := slices.BinarySearch(active, e.id)
				active = slices.Delete(active, i, i+1)
			}
			continue
		}
		counts[e.id]++
		if counts[e.id] > 1 {
			continue
		}
		i, _ := slices.BinarySearch(active, e.id)
		active = slices.Insert(active, i, e.id)
		for j := 0; j < len(active) && j <= maxOverlapListed; j++ {
			if active[j] != e.id {
				add(e.id, active[j])
			}
		}
		if i <= maxOverlapListed {
			for _, y := range active {
				if y != e.id {
					add(y, e.id)
				}
			}
		}
	}
	out := map[string][]string{}
	for id, l := range lists {
		if len(l) == 0 {
			continue
		}
		s := make([]string, len(l))
		for k, o := range l {
			s[k] = names[o]
		}
		out[names[id]] = s
	}
	return out
}

// methodBase is the base confidence of each deleted-file method (spec 3.3). The numbers are the
// initial calibration: changing one needs a spec amendment.
var methodBase = map[string]int{
	"ext-inode-intact":   80,
	"f2fs-node-scan":     75,
	"exfat-nofatchain":   75,
	"ext4-journal-inode": 65,
	"ext4-extent-leaf":   55,
	"fat-contiguous":     55,
	"exfat-contiguous":   55,
}

// Caps (spec 3.3): the lowest applicable cap wins.
const (
	capOverlap = 30 // all runs free but another candidate of the same analysis claims some of them
	capCut     = 20 // some runs were not free and were cut (prefix only)
)

// confidenceFor is the base of the method with the caps applied; false when the build has no rule for
// the method.
func confidenceFor(method string, cut, overlap bool) (int, bool) {
	base, ok := methodBase[method]
	if !ok {
		return 0, false
	}
	if overlap {
		base = min(base, capOverlap)
	}
	if cut {
		base = min(base, capCut)
	}
	return base, true
}

const uniformChunk = 64 << 10

// uniformScan reports whether every byte over runs (in order) is one and the same value, and that
// value. It stops at the first differing byte. A list with no bytes is not uniform. A run past the end
// of r, or an invalid run, is an error, never "uniform".
func uniformScan(ctx context.Context, r io.ReaderAt, runs []evidence.Run) (bool, byte, error) {
	buf := make([]byte, uniformChunk)
	var fill byte
	seen := false
	for _, run := range runs {
		if run.Offset < 0 || run.Length < 0 {
			return false, 0, fmt.Errorf("uniform scan: invalid run (offset %d, length %d)", run.Offset, run.Length)
		}
		if _, ok := filesys.AddOK(run.Offset, run.Length); !ok {
			return false, 0, fmt.Errorf("uniform scan: run (offset %d, length %d) overflows", run.Offset, run.Length)
		}
		for done := int64(0); done < run.Length; {
			if err := ctx.Err(); err != nil {
				return false, 0, err
			}
			n := int(min(int64(len(buf)), run.Length-done))
			got, err := r.ReadAt(buf[:n], run.Offset+done)
			if got < n {
				if err == nil || errors.Is(err, io.EOF) {
					err = fmt.Errorf("uniform scan: run (offset %d, length %d) reads past the end of the image: %w", run.Offset, run.Length, io.ErrUnexpectedEOF)
				}
				return false, 0, err
			}
			chunk := buf[:n]
			if !seen {
				fill, seen = chunk[0], true
			}
			for _, b := range chunk {
				if b != fill {
					return false, 0, nil
				}
			}
			done += int64(n)
		}
	}
	return seen, fill, nil
}
