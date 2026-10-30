package chromium

import (
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// flagInt records an integer cell with no usable value (rowrun.Flag's rule under key) and, when the cell holds a REAL,
// raw[key+"_ieee"] = the 16 lowercase hex digits of its IEEE bits (4h-R5). The unknown entry stays "type-mismatch":
// what the cell means is undecided, the stored bits are the fact. A TEXT or BLOB cell states no bits, and a nil raw
// (a text column) never does.
func flagInt(raw, unknown map[string]any, row parse.Row, key string, col int, k rowrun.CellKind) {
	rowrun.Flag(unknown, row, key, col, k)
	if raw == nil || k != rowrun.CellOther {
		return
	}
	if f, ok := row.Float(col); ok {
		raw[key+"_ieee"] = fmt.Sprintf("%016x", math.Float64bits(f))
	}
}
