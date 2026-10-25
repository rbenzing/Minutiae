package cellx

import (
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Number is a numeric cell as stored: an INTEGER (IsInt, I) or a REAL (F).
type Number struct {
	I     int64
	F     float64
	IsInt bool
}

// NumberCell reads an INTEGER or REAL column: CellValue for either; TEXT and
// BLOB are CellOther; CellMissing, CellNull and CellUnknown as rowrun.IntCell
// decides them. The decoder's own read semantics decide the storage class.
func NumberCell(row parse.Row, col int) (Number, rowrun.CellKind) {
	if col < 0 {
		return Number{}, rowrun.CellMissing
	}
	if !rowrun.StateOf(row, col).Known() {
		return Number{}, rowrun.CellUnknown
	}
	if row.IsNull(col) {
		return Number{}, rowrun.CellNull
	}
	if v, ok := row.Int(col); ok {
		return Number{I: v, IsInt: true}, rowrun.CellValue
	}
	if v, ok := row.Float(col); ok {
		return Number{F: v}, rowrun.CellValue
	}
	return Number{}, rowrun.CellOther
}

// maxExactInt is 2^53, the largest magnitude at which every integer is a float64.
const maxExactInt = 1 << 53

// Float is the value as a float64 when that is exact: an INTEGER beyond +-2^53 is
// false.
func (n Number) Float() (float64, bool) {
	if !n.IsInt {
		return n.F, true
	}
	if n.I < -maxExactInt || n.I > maxExactInt {
		return 0, false
	}
	return float64(n.I), true
}

// Raw is the payload value: int64, a finite float64, or the string "+Inf",
// "-Inf" or "NaN" (a value JSON cannot hold).
func (n Number) Raw() any {
	switch {
	case n.IsInt:
		return n.I
	case math.IsInf(n.F, 1):
		return "+Inf"
	case math.IsInf(n.F, -1):
		return "-Inf"
	case math.IsNaN(n.F):
		return "NaN"
	}
	return n.F
}
