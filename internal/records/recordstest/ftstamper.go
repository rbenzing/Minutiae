package recordstest

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Full-text index tamper helpers. Like the other tamper helpers they work through their own
// connection with foreign_keys=OFF, drop the immutability triggers, tamper and re-create the
// triggers, so the tampering under test is the only problem left. They edit the index exactly as
// SQLite's own FTS5 commands and shadow tables allow, which PRAGMA integrity_check does not see.

// indexedDoc returns the normalized text the index holds for record id (what the writer indexed).
func indexedDoc(db *sql.DB, id int64) (evidence.FTSDoc, error) {
	var summary string
	var body *string
	if err := db.QueryRow(`SELECT summary, body FROM records WHERE id = ?`, id).Scan(&summary, &body); err != nil {
		return evidence.FTSDoc{}, fmt.Errorf("read record %d: %w", id, err)
	}
	doc, ok := evidence.NewFTSDoc(id, summary, body)
	if !ok {
		return evidence.FTSDoc{}, fmt.Errorf("record %d has no indexable text", id)
	}
	return doc, nil
}

// HideFromIndex removes the postings of record id from the full-text table with FTS5's own
// 'delete' command, given the record's normalized text: the record is still in `records`, but a
// search no longer finds it. PRAGMA integrity_check stays silent.
func HideFromIndex(t testing.TB, caseDir, table string, id int64) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		doc, err := indexedDoc(db, id)
		if err != nil {
			return err
		}
		_, err = db.Exec(`INSERT INTO `+table+`(`+table+`, rowid, summary, body) VALUES ('delete', ?, ?, ?)`, id, doc.Summary, doc.Body) //nolint:gosec // the table is one of two constants
		return err
	})
}

// InventIndexText adds postings for the given text under rowid id (a record that exists or one
// that does not): a search finds a record for words its text does not hold. The text should share
// no term with the record's own text (a 'delete' of it removes (term, rowid) pairs).
func InventIndexText(t testing.TB, caseDir, table string, id int64, summary, body string) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, id, summary, body) //nolint:gosec // the table is one of two constants
		return err
	})
}

// DuplicateIndexRow indexes record id a second time, with the same text it was indexed with: the
// document appears twice for the same rowid (what a writer that inserted an existing rowid would
// leave).
func DuplicateIndexRow(t testing.TB, caseDir, table string, id int64) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		doc, err := indexedDoc(db, id)
		if err != nil {
			return err
		}
		_, err = db.Exec(`INSERT INTO `+table+`(rowid, summary, body) VALUES (?, ?, ?)`, id, doc.Summary, doc.Body) //nolint:gosec // the table is one of two constants
		return err
	})
}

// EmptyFTSTable removes every document from one full-text table with FTS5's delete-all command;
// records_meta is untouched (EmptyFTSIndex does both tables).
func EmptyFTSTable(t testing.TB, caseDir, table string) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO ` + table + `(` + table + `) VALUES ('delete-all')`) //nolint:gosec // the table is one of two constants
		return err
	})
}

// SetDocsize replaces the size entry (<table>_docsize.sz) of record id with v: a []byte alters the
// blob, a string or an int64 stores a value of another storage class.
func SetDocsize(t testing.TB, caseDir, table string, id int64, v any) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE `+table+`_docsize SET sz = ? WHERE id = ?`, v, id) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s_docsize: %d rows changed for record %d", table, n, id)
		}
		return nil
	})
}

// CorruptDocsize alters the size blob of record id (a different, valid-looking blob).
func CorruptDocsize(t testing.TB, caseDir, table string, id int64) {
	t.Helper()
	SetDocsize(t, caseDir, table, id, []byte{0x7f, 0x7f, 0x7f})
}

// DeleteDocsize deletes the size entry of record id.
func DeleteDocsize(t testing.TB, caseDir, table string, id int64) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`DELETE FROM `+table+`_docsize WHERE id = ?`, id) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s_docsize: %d rows deleted for record %d", table, n, id)
		}
		return nil
	})
}

// AddDocsize adds a size entry for a record id the index holds no document for.
func AddDocsize(t testing.TB, caseDir, table string, id int64) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT INTO `+table+`_docsize (id, sz) VALUES (?, x'0101')`, id) //nolint:gosec // the table is one of two constants
		return err
	})
}

// SetFTSTotals overwrites the totals record of a full-text table (row 1 of its _data shadow
// table: the document and token counts bm25 ranks with).
func SetFTSTotals(t testing.TB, caseDir, table string, block []byte) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE `+table+`_data SET block = ? WHERE id = 1`, block) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s_data: %d totals rows changed", table, n)
		}
		return nil
	})
}

// FTSTotals returns the totals record of a full-text table (row 1 of its _data shadow table).
func FTSTotals(t testing.TB, caseDir, table string) []byte {
	t.Helper()
	table = ftsTable(t, table)
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	var b []byte
	if err := db.QueryRow(`SELECT block FROM ` + table + `_data WHERE id = 1`).Scan(&b); err != nil { //nolint:gosec // the table is one of two constants
		t.Fatal(err)
	}
	return b
}

// SetFTSConfig writes a TEXT value for a key of <table>_config (the FTS5 configuration, whose
// values are integers): key and value are stored as given.
func SetFTSConfig(t testing.TB, caseDir, table, key, value string) {
	t.Helper()
	setFTSConfig(t, caseDir, table, key, value)
}

// SetFTSConfigInt writes an INTEGER value for a key of <table>_config.
func SetFTSConfigInt(t testing.TB, caseDir, table, key string, value int64) {
	t.Helper()
	setFTSConfig(t, caseDir, table, key, value)
}

func setFTSConfig(t testing.TB, caseDir, table, key string, value any) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		_, err := db.Exec(`INSERT OR REPLACE INTO `+table+`_config (k, v) VALUES (?, ?)`, key, value) //nolint:gosec // the table is one of two constants
		return err
	})
}

// DeleteFTSIdxRows deletes every non-first row of <table>_idx (the segment b-tree index): the
// terms of the leaf pages they pointed at become unreachable by MATCH while the leaf pages, the
// vocab, PRAGMA integrity_check and FTS5's own integrity-check all stay silent. It returns the
// number of rows deleted.
func DeleteFTSIdxRows(t testing.TB, caseDir, table string) int {
	t.Helper()
	table = ftsTable(t, table)
	var n int64
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`DELETE FROM ` + table + `_idx WHERE term <> x''`) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return int(n)
}

// RepointFTSIdxRow moves one non-first row of <table>_idx to another page (pgno + 3): a wrong row
// that PRAGMA integrity_check (and FTS5's own check) report.
func RepointFTSIdxRow(t testing.TB, caseDir, table string) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE ` + table + `_idx SET pgno = pgno + 3 WHERE (segid, term) = (SELECT segid, term FROM ` + table + `_idx WHERE term <> x'' ORDER BY segid, term LIMIT 1 OFFSET 20)`) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s_idx: %d rows repointed", table, n)
		}
		return nil
	})
}

// AlterFTSIdxTerm changes the term of one non-first row of <table>_idx: another wrong row that
// PRAGMA integrity_check reports.
func AlterFTSIdxTerm(t testing.TB, caseDir, table string) {
	t.Helper()
	table = ftsTable(t, table)
	tamper(t, caseDir, func(db *sql.DB) error {
		res, err := db.Exec(`UPDATE ` + table + `_idx SET term = x'7a7a7a7a7a' WHERE (segid, term) = (SELECT segid, term FROM ` + table + `_idx WHERE term <> x'' ORDER BY segid, term LIMIT 1 OFFSET 20)`) //nolint:gosec // the table is one of two constants
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%s_idx: %d rows altered", table, n)
		}
		return nil
	})
}

// DeleteFTSNormVersion removes the index state row (records_meta fts_norm_version): the case no
// longer says what its index is.
func DeleteFTSNormVersion(t testing.TB, caseDir string) {
	t.Helper()
	db := openTamperDB(t, caseDir)
	defer func() { _ = db.Close() }()
	res, err := db.Exec(`DELETE FROM records_meta WHERE key = ?`, evidence.MetaFTSNormVersion)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("recordstest: %d fts_norm_version rows deleted", n)
	}
}
