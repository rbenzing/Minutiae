package examine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
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
// free (and, as the first non-free run, they give the state "unknown", never "allocated"). Byte
// counts saturate at MaxInt64.
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
		valid := false
		if r.Offset >= 0 {
			if _, ok := filesys.AddOK(r.Offset, r.Length); ok {
				valid = true
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
		if !valid {
			out.State = "unknown" // a run that cannot be placed is not claimed to be allocated
		}
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

// overlapKeep is how many of the smallest owner ranks every tree list keeps: the 16 to report plus
// the owner itself, which always intersects its own runs.
const overlapKeep = maxOverlapListed + 1

// overlapSteps counts the elementary operations of overlapsOf (node visits, list comparisons and
// copies) so that a test can bound its cost without timing it. A nil counter costs nothing.
type overlapSteps struct{ n int64 }

func (s *overlapSteps) add(n int) {
	if s != nil {
		s.n += int64(n)
	}
}

// insertSmall puts id into the sorted list l of at most overlapKeep distinct ranks, dropping the
// largest when the list is full.
func insertSmall(l []int32, id int32, st *overlapSteps) []int32 {
	st.add(1)
	i, found := slices.BinarySearch(l, id)
	if found || i >= overlapKeep {
		return l
	}
	st.add(len(l) - i)
	if len(l) < overlapKeep {
		l = append(l, 0)
	}
	copy(l[i+1:], l[i:])
	l[i] = id
	return l
}

// mergeSmall returns the overlapKeep smallest distinct ranks of two sorted lists; it returns one of
// them unchanged when the other is empty (lists are never modified after they are built).
func mergeSmall(a, b []int32, st *overlapSteps) []int32 {
	switch {
	case len(a) == 0:
		return b
	case len(b) == 0:
		return a
	}
	out := make([]int32, 0, min(len(a)+len(b), overlapKeep))
	i, j := 0, 0
	for len(out) < overlapKeep && (i < len(a) || j < len(b)) {
		st.add(1)
		var v int32
		switch {
		case j >= len(b) || (i < len(a) && a[i] < b[j]):
			v = a[i]
			i++
		case i >= len(a) || b[j] < a[i]:
			v = b[j]
			j++
		default: // equal
			v = a[i]
			i++
			j++
		}
		out = append(out, v)
	}
	return out
}

// overlapsOf maps every owner to the other owners that share at least one byte with it (sorted,
// deduplicated, the 16 smallest when there are more; touching ranges do not overlap; one owner never
// overlaps itself).
//
// The runs are half-open intervals over the compressed coordinates of their ends. Each interval is
// stored in the O(log n) nodes of a segment tree that cover it (cover keeps only the smallest ranks
// per node), sub is the smallest ranks of a node and everything below it, and the owners that
// intersect an interval are exactly those in sub of its covering nodes and in cover of the ancestors
// of its first and last cell. Every list is bounded, so the work is O(n log n) list merges of at most
// 17 entries, whatever the input; there is no pairwise step.
func overlapsOf(items []ownedRuns) map[string][]string { return overlapsOfSteps(items, nil) }

func overlapsOfSteps(items []ownedRuns, st *overlapSteps) map[string][]string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Owner)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	st.add(len(names) * (bits.Len(uint(len(names))) + 1))
	rank := make(map[string]int32, len(names))
	for i, n := range names {
		rank[n] = int32(i)
	}
	type interval struct {
		lo, hi int64
		id     int32
	}
	var ivs []interval
	var pos []int64
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
			ivs = append(ivs, interval{r.Offset, end, id})
			pos = append(pos, r.Offset, end)
		}
	}
	out := map[string][]string{}
	if len(ivs) == 0 {
		return out
	}
	slices.Sort(pos)
	pos = slices.Compact(pos)
	st.add(len(pos) * (bits.Len(uint(len(pos))) + 1))
	size := 1
	for size < len(pos)-1 { // len(pos)-1 cells between consecutive positions
		size <<= 1
	}
	cover := make([][]int32, 2*size)
	cells := func(iv interval) (int, int) {
		lo, _ := slices.BinarySearch(pos, iv.lo)
		hi, _ := slices.BinarySearch(pos, iv.hi)
		st.add(2 * bits.Len(uint(len(pos))))
		return lo + size, hi + size
	}
	for _, iv := range ivs {
		l, r := cells(iv)
		for ; l < r; l, r = l>>1, r>>1 {
			if l&1 == 1 {
				cover[l] = insertSmall(cover[l], iv.id, st)
				l++
			}
			if r&1 == 1 {
				r--
				cover[r] = insertSmall(cover[r], iv.id, st)
			}
		}
	}
	sub := make([][]int32, 2*size)
	for n := 2*size - 1; n >= 1; n-- {
		st.add(1)
		s := cover[n]
		if n < size {
			s = mergeSmall(s, mergeSmall(sub[2*n], sub[2*n+1], st), st)
		}
		sub[n] = s
	}
	acc := make([][]int32, len(names))
	// take merges the sorted list l into the owner's accumulator. The early return is a constant-factor
	// optimisation only and is deliberately NOT visible to the step counter (it counts 1 either way): without
	// it each call makes at most overlapKeep further insertSmall calls, which is the "+16" term already inside
	// the bound the cost test enforces, so removing it cannot change an answer or leave the O(n log n) bound.
	// The differential tests (TestOverlapMatchesBruteForce*) pin the answers (C51).
	take := func(id int32, l []int32) {
		for _, v := range l {
			if a := acc[id]; len(a) == overlapKeep && v > a[overlapKeep-1] {
				st.add(1)
				return // l is sorted: nothing later can be among the smallest either
			}
			acc[id] = insertSmall(acc[id], v, st)
		}
	}
	for _, iv := range ivs {
		l, r := cells(iv)
		first, last := l, r-1
		for ; l < r; l, r = l>>1, r>>1 {
			if l&1 == 1 {
				take(iv.id, sub[l])
				l++
			}
			if r&1 == 1 {
				r--
				take(iv.id, sub[r])
			}
		}
		for _, leaf := range [2]int{first, last} {
			for n := leaf; n >= 1; n >>= 1 {
				st.add(1)
				take(iv.id, cover[n])
			}
		}
	}
	for id, l := range acc {
		var s []string
		for _, o := range l {
			if int(o) != id && len(s) < maxOverlapListed {
				s = append(s, names[o])
			}
		}
		if len(s) > 0 {
			out[names[id]] = s
		}
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

// overlapCounts maps every owner that has at least one overlap to how many merged run intervals of OTHER
// owners overlap its own merged intervals (C52). Each owner's runs are first merged into disjoint,
// non-touching intervals, so for an interval [s,e) of owner A the intervals of other owners that overlap
// it number #(starts < e) - #(ends <= s) - 1 (the 1 is A's own interval): every interval starting before
// e and ending after s overlaps, and the two counts come from two sorted arrays by binary search, O(log n)
// per interval and O(n log n) in all. For owners that each hold one interval (the usual entry) this is the
// number of other owners; an owner with several disjoint intervals that all meet one interval of A is
// counted once per interval.
func overlapCounts(items []ownedRuns) map[string]int {
	type iv struct{ lo, hi int64 }
	byOwner := map[string][]iv{}
	for _, it := range items {
		for _, r := range it.Runs {
			if r.Offset < 0 || r.Length <= 0 {
				continue
			}
			end, ok := filesys.AddOK(r.Offset, r.Length)
			if !ok {
				continue
			}
			byOwner[it.Owner] = append(byOwner[it.Owner], iv{r.Offset, end})
		}
	}
	type owned struct {
		iv
		owner string
	}
	var all []owned
	var starts, ends []int64
	for o, l := range byOwner {
		slices.SortFunc(l, func(a, b iv) int { return cmp.Compare(a.lo, b.lo) })
		var m []iv
		for _, x := range l {
			if k := len(m) - 1; k >= 0 && x.lo <= m[k].hi {
				m[k].hi = max(m[k].hi, x.hi)
			} else {
				m = append(m, x)
			}
		}
		for _, x := range m {
			all = append(all, owned{x, o})
			starts = append(starts, x.lo)
			ends = append(ends, x.hi)
		}
	}
	slices.Sort(starts)
	slices.Sort(ends)
	out := map[string]int{}
	for _, a := range all {
		before, _ := slices.BinarySearch(starts, a.hi) // #starts < hi
		upTo, _ := slices.BinarySearch(ends, a.lo+1)   // #ends <= lo (lo < hi, so no overflow)
		if n := before - upTo - 1; n > 0 {
			out[a.owner] += n
		}
	}
	return out
}
