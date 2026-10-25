package safari

import (
	"fmt"
	"math"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
)

// cocoaInstant is a REAL timestamp column read as Cocoa seconds through decode/ts: the time (zero unless
// Status is StatusValid), the status and the stored value. The helper lives in this package (4F-R2).
type cocoaInstant struct {
	T      time.Time
	Status ts.Status
	Raw    float64
}

// Valid reports whether the value is a time.
func (c cocoaInstant) Valid() bool { return c.Status == ts.StatusValid }

// cocoa classifies a stored REAL as Cocoa seconds (NaN, the infinities, out-of-range values and
// anything outside [1970, 2100) are invalid; 0, -0 and -1 are sentinels).
func cocoa(raw float64) cocoaInstant {
	c := cocoaInstant{Status: ts.ClassifyFloat(ts.KindCocoaSeconds, raw), Raw: raw}
	if c.Status == ts.StatusValid {
		t, ok := ts.CocoaSeconds(raw)
		if !ok {
			c.Status = ts.StatusInvalid
		} else {
			c.T = t
		}
	}
	return c
}

// Record states the stored value in raw under col and, when it is not a time, its status under
// col+"_status" (4F-R14). A non-finite stored REAL (possible only in a hostile file) is stated by its
// IEEE bits, as 16 lowercase hex digits under col+"_ieee", and never as a value: the payload accepts
// finite numbers only.
func (c cocoaInstant) Record(raw map[string]any, col string) {
	if math.IsNaN(c.Raw) || math.IsInf(c.Raw, 0) {
		raw[col+"_ieee"] = fmt.Sprintf("%016x", math.Float64bits(c.Raw))
		raw[col+"_status"] = ts.StatusInvalid.String()
		return
	}
	raw[col] = c.Raw
	if c.Status != ts.StatusValid {
		raw[col+"_status"] = c.Status.String()
	}
}

// floatCell reads a REAL column like rowrun.IntCell reads an integer one: only CellValue carries a
// number; an INTEGER stored in a REAL-affinity column was promoted by the decoder.
func floatCell(row parse.Row, col int) (float64, rowrun.CellKind) {
	if col < 0 {
		return 0, rowrun.CellMissing
	}
	if !rowrun.StateOf(row, col).Known() {
		return 0, rowrun.CellUnknown
	}
	if row.IsNull(col) {
		return 0, rowrun.CellNull
	}
	if v, ok := row.Float(col); ok {
		return v, rowrun.CellValue
	}
	return 0, rowrun.CellOther
}
