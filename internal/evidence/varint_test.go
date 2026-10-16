package evidence

import (
	"math"
	"testing"
)

func TestSQLiteVarintVectors(t *testing.T) {
	for _, c := range []struct {
		in []byte
		v  uint64
		n  int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x81, 0x00}, 128, 2},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, math.MaxUint64, 9},
		{[]byte{0x81}, 0, 0},
		{nil, 0, 0},
	} {
		if v, n := SQLiteVarint(c.in); v != c.v || n != c.n {
			t.Errorf("SQLiteVarint(%x) = %d, %d; want %d, %d", c.in, v, n, c.v, c.n)
		}
	}
}
