package filesys

import (
	"math"
	"sort"
)

// MulOK returns a*b and false when an input is negative or the product
// overflows int64.
func MulOK(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

// AddOK returns a+b and false when an input is negative or the sum overflows
// int64.
func AddOK(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	if a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

// MergeRuns returns a new slice with the runs sorted by Offset, adjacent and
// overlapping runs merged, and empty runs dropped. Holes (negative Offset)
// and runs whose end overflows are not valid here and are dropped. The input
// is not modified.
func MergeRuns(rs []Run) []Run {
	in := make([]Run, 0, len(rs))
	for _, r := range rs {
		if r.Offset < 0 || r.Length <= 0 {
			continue
		}
		if _, ok := AddOK(r.Offset, r.Length); !ok {
			continue
		}
		in = append(in, r)
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Offset < in[j].Offset })
	out := in[:0]
	for _, r := range in {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if r.Offset <= last.Offset+last.Length { // cannot overflow: validated above
				if end := r.Offset + r.Length; end > last.Offset+last.Length {
					last.Length = end - last.Offset
				}
				continue
			}
		}
		out = append(out, r)
	}
	return out
}
