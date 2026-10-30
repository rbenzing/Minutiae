package iossms

import (
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// namedTime is a time column that becomes a secondary time of the record.
type namedTime struct{ kind, col string }

// messageTimes are the secondary times of a message, in the order they are emitted.
func messageTimes() []namedTime {
	return []namedTime{
		{"read", "date_read"},
		{"delivered", "date_delivered"},
		{"played", "date_played"},
		{"edited", "date_edited"},
		{"retracted", "date_retracted"},
	}
}

// instant reads a time column of a row through cellx.CocoaAuto (4E-R15: an INTEGER at or above 1e11 is
// nanoseconds, below it seconds; a REAL is classified by its magnitude) and states it in raw (value, unit,
// status, and for a non-finite REAL its IEEE bits). The time is returned only for a valid instant: a sentinel or
// invalid value never becomes a time (D5).
func instant(row parse.Row, c rowrun.Cols, col string, raw, unknown map[string]any) (time.Time, bool) {
	n, k := cellx.NumberCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, col, c.At(col), k)
		return time.Time{}, false
	}
	in := cellx.CocoaAuto(n)
	in.Record(raw, col)
	return in.T, in.Valid()
}

// secondaryTimes reads the secondary time columns of the message row.
func secondaryTimes(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) []records.NamedTime {
	var out []records.NamedTime
	for _, nt := range messageTimes() {
		if t, ok := instant(row, c, nt.col, raw, unknown); ok {
			out = append(out, records.NamedTime{Kind: nt.kind, Time: records.Time{T: t}})
		}
	}
	return out
}
