package recordstest

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// ftsTable fails the test unless table is one of the two full-text tables.
func ftsTable(t testing.TB, table string) string {
	t.Helper()
	if !slices.Contains(evidence.FTSTables(), table) {
		t.Fatalf("recordstest: %q is not a full-text table", table)
	}
	return table
}

func idsOf(t testing.TB, c *evidence.Case, query string, args ...any) []int64 {
	t.Helper()
	var ids []int64
	err := c.ReadTx(context.Background(), func(h evidence.ReadHandle) error {
		rows, err := h.Query(query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return ids
}

// FTSMatch returns the ids of the records the full-text table (records_fts or records_fts_sub)
// finds for the FTS5 expression match, ascending, read in one ReadTx. The expression is used as
// given (a test helper: it is the caller's own text).
func FTSMatch(t testing.TB, c *evidence.Case, table, match string) []int64 {
	t.Helper()
	table = ftsTable(t, table)
	return idsOf(t, c, `SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? ORDER BY rowid`, match) //nolint:gosec // the table is one of two constants
}

// IndexedIDs returns the ids of the records the full-text table holds a document for (the rows of
// its _docsize shadow table), ascending.
func IndexedIDs(t testing.TB, c *evidence.Case, table string) []int64 {
	t.Helper()
	table = ftsTable(t, table)
	return idsOf(t, c, `SELECT id FROM `+table+`_docsize ORDER BY id`) //nolint:gosec // the table is one of two constants
}

// SetFTSNormVersion writes the index state value (records_meta.fts_norm_version) of the case in
// caseDir through its own connection, as an attacker or an interrupted rebuild would leave it.
func SetFTSNormVersion(t testing.TB, caseDir, value string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	res, err := db.Exec(`UPDATE records_meta SET value = ? WHERE key = ?`, value, evidence.MetaFTSNormVersion)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("recordstest: %d fts_norm_version rows changed", n)
	}
}

// ftsStructureRowID is the id of the row of <table>_data that holds the FTS5 structure record
// (the list of segments): FTS5_STRUCTURE_ROWID.
const ftsStructureRowID = 10

// WreckFTSStructure overwrites the structure record of a full-text table (row 10 of its _data
// shadow table) with 0xff bytes, so the next write or read of the index fails inside SQLite with
// a corruption error while the schema stays exactly as defined. Unlike dropping a shadow table it
// is invisible to the schema comparison, so it models a fault that only shows inside a transaction.
func WreckFTSStructure(t testing.TB, caseDir, table string) {
	t.Helper()
	table = ftsTable(t, table)
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	if err := wreckStructure(db, table); err != nil {
		t.Fatal(err)
	}
}

func wreckStructure(db *sql.DB, table string) error {
	res, err := db.Exec(`UPDATE `+table+`_data SET block = x'ffffffffffffffffffffffffffffffffffffffffffffffff' WHERE id = ?`, ftsStructureRowID) //nolint:gosec // the table is one of two constants
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%s_data: %d structure rows changed", table, n)
	}
	return nil
}

// EmptyFTSIndex removes every document from both full-text tables with FTS5's own delete-all
// command, through its own connection: the records are untouched, the schema is as defined and
// every search hits nothing, which is what an index emptied by an attacker or a lost write looks
// like.
func EmptyFTSIndex(t testing.TB, caseDir string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	for _, table := range evidence.FTSTables() {
		if _, err := db.Exec(`INSERT INTO ` + table + `(` + table + `) VALUES ('delete-all')`); err != nil { //nolint:gosec // the table is one of two constants
			t.Fatalf("recordstest: empty %s: %v", table, err)
		}
	}
}
