package parse

import "github.com/rbenzing/minutiae/internal/records"

// FileRole names which file of a database bundle a byte range lies in.
type FileRole string

// The files of an SQLite bundle.
const (
	RoleDB      FileRole = "db"
	RoleWAL     FileRole = "wal"
	RoleJournal FileRole = "journal"
)

// Row is one table row as the SQLite decoder presents it to a mapping. The
// decoder's own Row type implements it; parse owns the interface so that
// parse does not import the decoder. NULL, an absent column and a type
// mismatch all report ok == false.
type Row interface {
	Table() string
	Rowid() (int64, bool) // false for a recovered row whose rowid is lost
	IsNull(col int) bool
	Int(col int) (int64, bool)
	Float(col int) (float64, bool)
	Text(col int) ([]byte, bool)
	Blob(col int) ([]byte, bool)
	Range() (records.Range, FileRole)
}

// JoinKeyKind is the storage class of a JoinKey.
type JoinKeyKind uint8

// The kinds of join key. The zero kind matches nothing.
const (
	JoinNone  JoinKeyKind = iota // matches nothing
	JoinInt                      // I holds the value
	JoinFloat                    // F holds the IEEE bits
	JoinText                     // S holds the stored text bytes
	JoinBlob                     // S holds the stored blob bytes
)

// JoinClass is the affinity class of the column a key was read from.
type JoinClass uint8

// The affinity classes. The zero class is a literal and is never flagged.
const (
	ClassUnknown JoinClass = iota
	ClassNumeric           // INTEGER, REAL, NUMERIC
	ClassText              // TEXT
	ClassBlob              // no affinity
)

// JoinKey is a value to look up in another table, with the collation and
// affinity class of the column it was read from, so a join across columns that
// differ in either can be flagged. It is comparable and the zero value matches
// nothing.
type JoinKey struct {
	Kind      JoinKeyKind
	I         int64
	F         uint64 // IEEE bits
	S         string // the stored text or blob bytes, unfolded
	Collation string // canonical collation of the source column ("" for a literal); an unsupported name is kept as written
	Class     JoinClass
}

// MapContext is what a mapping may use besides its row: the invocation's
// Input, a bounded note sink and joins into other tables of the same database.
type MapContext interface {
	Input() *Input
	Note(key, value string)
	// Column returns the index of a column of a table, or -1 when the table or
	// the column does not exist. -1 means only that: an implementation that
	// fails for another reason (a cancelled context, an I/O error, a corrupt
	// schema) also returns -1 because there is no error to return, but it keeps
	// the failure and reports it from its own Err method and from Get and Match,
	// and the host must fail the job when that failure is set at Close. A
	// mapping may read -1 as "this variant lacks the column" only under such a
	// host.
	Column(table, col string) int
	// Get returns the row with the given rowid; found is false only for a clean
	// miss. A missing table is an error.
	Get(table string, rowid int64) (Row, bool, error)
	// Match returns the rows of table whose column col equals key under the
	// engine's equality and that column's collation, in rowid order.
	Match(table, col string, key JoinKey) ([]Row, error)
}

// ColumnSpec declares a column a mapping reads.
type ColumnSpec struct {
	Name     string
	Required bool
	Affinity string
}

// TableMapping turns the rows of one table into records.
type TableMapping struct {
	Table   string
	Columns []ColumnSpec
	Map     func(m MapContext, row Row) ([]records.Record, error) // pure: a function of row and m only
}

// RowMapper is a Parser that declares per-table mappings.
type RowMapper interface {
	Parser
	Mappings() []TableMapping
}

// ClaimsOutsideMappings returns the claims of m that name a table none of the
// mappings handles (ASCII case-insensitive, as SQLite compares names), in
// claim order.
func ClaimsOutsideMappings(m Meta, mappings []TableMapping) []TableClaim {
	var out []TableClaim
	for _, c := range m.Claims {
		found := false
		for _, mp := range mappings {
			if asciiFoldEqual(c.Table, mp.Table) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, c)
		}
	}
	return out
}
