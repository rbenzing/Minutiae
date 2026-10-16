package evidence

import (
	"fmt"
	"math"
)

// RunsSum adds the lengths of runs after checking each run like records.TranslateRange does: an offset
// below -1 (-1 is a hole), a length of zero or less, an end beyond the largest offset or a sum beyond
// int64 is an error. It is pure.
func RunsSum(runs []Run) (int64, error) {
	var sum int64
	for i, r := range runs {
		switch {
		case r.Offset < -1 || r.Length <= 0:
			return 0, fmt.Errorf("run %d is not valid (offset %d, length %d)", i, r.Offset, r.Length)
		case r.Offset >= 0 && r.Offset > math.MaxInt64-r.Length:
			return 0, fmt.Errorf("run %d ends beyond the largest offset", i)
		case sum > math.MaxInt64-r.Length:
			return 0, fmt.Errorf("run lengths add up to more than %d bytes", int64(math.MaxInt64))
		}
		sum += r.Length
	}
	return sum, nil
}
