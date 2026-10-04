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

// MapContext is what a mapping may use besides its row: the invocation's
// Input and a bounded note sink.
type MapContext interface {
	Input() *Input
	Note(key, value string)
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
