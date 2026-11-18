package ioswacalls

// enumEntry is one code the parser interprets: the column it comes from, the stored value, the label and the sources that agree
// about that column (4I2-R8). Sources is a comma list of the research source IDs, or ref:<run id>. A code is mapped only when
// two independent sources agree about the SAME column and the value is in no research conflict row;
// TestIOSWhatsAppCallsEnumTableCitesTwoSources enforces exactly this bar over the table. The table is restated from
// ioswhatsapp/enums.go: parsers import no other parser package (docs/architecture.md).
type enumEntry struct {
	Column  string
	Value   int64
	Label   string
	Sources string
}

// The columns of the table, named as the stored column.
const (
	colOutcome  = "ZOUTCOME"
	colIncoming = "ZINCOMING"
	colVideo    = "ZVIDEO"
)

// The labels the mapping's own rules name.
const (
	labelAnswered = "answered"
	labelMissed   = "missed"
	labelIn       = "in"
	labelOut      = "out"
)

// enumTable is read-only. A value that fewer than two sources state, or that sits in a research conflict row, has no entry on
// purpose (ZOUTCOME 2, 4 and every other code; ZINCOMING 7 and every other code): its stored value stays in raw, its label is
// absent and its outcome or direction is unknown (4I2-R8). ZVIDEO has no entry at all: no research source states the meaning of
// a value of that column (PF6, amendment 4I2-A3), so the kind is always unknown and the stored value is in raw.
var enumTable = []enumEntry{
	{colOutcome, 0, labelAnswered, "S1,S7"},
	{colOutcome, 1, labelMissed, "S1,S7"},
	{colIncoming, 0, labelOut, "S1,S7"},
	{colIncoming, 1, labelIn, "S1,S7"},
}

// lookupEnum returns the label of a stored value of a column, and false for a code the research does not support.
func lookupEnum(column string, v int64) (string, bool) {
	for _, e := range enumTable {
		if e.Column == column && e.Value == v {
			return e.Label, true
		}
	}
	return "", false
}
