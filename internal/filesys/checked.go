package filesys

import (
	"fmt"
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

// CheckRuns verifies the File.Runs contract for a file of size bytes on a
// filesystem of fsSize bytes: every run has a non-negative length, a hole has
// Offset exactly -1, every other run lies inside [0, fsSize), and the lengths
// (holes included) add up to exactly size without overflowing. A violation is
// reported as a *CorruptError. Content stored inline in metadata has no runs
// and must not be passed here.
func CheckRuns(runs []Run, size int64, fsSize int64) error {
	bad := func(format string, a ...any) error {
		return &CorruptError{Structure: "file runs", Offset: -1, Reason: fmt.Sprintf(format, a...)}
	}
	if size < 0 || fsSize < 0 {
		return bad("negative size %d or filesystem size %d", size, fsSize)
	}
	var total int64
	for i, r := range runs {
		if r.Length < 0 {
			return bad("run %d has negative length %d", i, r.Length)
		}
		if r.Offset < 0 && r.Offset != -1 {
			return bad("run %d has offset %d (a hole is exactly -1)", i, r.Offset)
		}
		if r.Offset >= 0 {
			if end, ok := AddOK(r.Offset, r.Length); !ok || end > fsSize {
				return bad("run %d (%d+%d) lies outside the %d-byte filesystem", i, r.Offset, r.Length, fsSize)
			}
		}
		var ok bool
		if total, ok = AddOK(total, r.Length); !ok {
			return bad("run lengths overflow at run %d", i)
		}
	}
	if total != size {
		return bad("runs cover %d bytes, want exactly the size %d", total, size)
	}
	return nil
}
