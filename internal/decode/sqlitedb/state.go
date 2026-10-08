package sqlitedb

import "github.com/rbenzing/minutiae/internal/sqlitefile"

// ColState says what a column of a row holds, and how far it can be trusted.
type ColState uint8

// The column states. Present, Null and Defaulted are Known: the value is what
// the database says. Every other state is an unknown the caller must not
// treat as a value.
const (
	StatePresent     ColState = iota // stored and read
	StateNull                        // stored as NULL
	StateAbsent                      // no such column
	StateDefaulted                   // not stored (a short record): the column's literal default
	StateOmitted                     // not materialized (over a cap, virtual generated, non-literal default)
	StateUnread                      // stored, but its bytes could not be read
	StateClipped                     // only a prefix of the bytes is held
	StateUndecodable                 // UTF-16 text that is not valid UTF-16
	StateLost                        // a rowid that was not recovered
)

var stateNames = [...]string{"present", "null", "absent", "defaulted", "omitted", "unread", "clipped", "undecodable", "lost"}

func (s ColState) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "state?"
}

// Known reports whether the state is a value the database holds: Present,
// Null or Defaulted.
func (s ColState) Known() bool {
	return s == StatePresent || s == StateNull || s == StateDefaulted
}

// stateInput is what the state of a column is derived from.
type stateInput struct {
	def       *sqlitefile.TableDef
	vals      []sqlitefile.Value // the resolved values, in declared column order
	storedLen int                // values the record stored
	hasRowid  bool               // the row's rowid is known
	truncated bool               // a recovered row whose record was cut off
	// undecodable reports whether the text of column col is not valid in its
	// encoding. It is asked only for text in a UTF-16 encoding that reached
	// the decoding step; nil means every text decodes.
	undecodable func(col int) bool
}

// isUTF16 reports whether e is a UTF-16 encoding.
func isUTF16(e sqlitefile.Encoding) bool {
	return e == sqlitefile.EncUTF16LE || e == sqlitefile.EncUTF16BE
}

// deriveState is the one place that says what a column holds. The first rule
// that matches wins.
func deriveState(in stateInput, col int) ColState {
	if col < 0 || col >= len(in.vals) {
		return StateAbsent
	}
	c := &in.def.Columns[col]
	v := &in.vals[col]
	switch {
	case col == in.def.RowidAlias: // before the short-record rule: the rowid is not stored in the record
		if in.hasRowid {
			return StatePresent
		}
		return StateLost
	case v.Unread:
		return StateUnread
	case c.RecordIndex < 0: // virtual generated: never stored
		return StateOmitted
	case c.RecordIndex >= in.storedLen: // the record ended before this column
		switch {
		case in.truncated:
			return StateUnread
		case v.Omitted:
			return StateOmitted
		}
		return StateDefaulted
	case v.Omitted:
		return StateOmitted
	case v.Clipped:
		return StateClipped
	case v.Kind == sqlitefile.KindText && isUTF16(v.Enc) && in.undecodable != nil && in.undecodable(col):
		return StateUndecodable
	case v.Kind == sqlitefile.KindNull:
		return StateNull
	}
	return StatePresent
}
