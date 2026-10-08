package evidence

import (
	"database/sql"
	"fmt"
)

// Schema v3 adds the full-text indexes. Both are FTS5 tables with no content table (the text lives in
// records.summary and records.body; the index holds only the terms), keyed by records.id. The
// word index uses the unicode61 tokenizer and answers word, phrase and prefix queries; the sub
// index uses the trigram tokenizer and answers substring queries. Neither has a prefix option
// (prefix indexes live in regions the vocab tables cannot read, so verify could not prove them) and
// neither has triggers (SQLite allows none on a virtual table): the writer maintains them in the
// same transaction as the records, and `records reindex` rebuilds them.
//
// Every statement that creates, drops or recreates these tables comes from FTSTableDDL and
// FTSVocabDDL, so the migration, reindex and verify share one text and sqlite_master is the same
// after any of them.
const (
	// FTSWordTable is the word index (unicode61).
	FTSWordTable = "records_fts"
	// FTSSubTable is the substring index (trigram).
	FTSSubTable = "records_fts_sub"
	// MetaFTSNormVersion is the records_meta key that holds the normalization version the index was
	// built with (the index state, see IndexState).
	MetaFTSNormVersion = "fts_norm_version"
)

// ftsDDL maps each FTS table to its CREATE VIRTUAL TABLE statement.
var ftsDDL = map[string]string{
	FTSWordTable: `CREATE VIRTUAL TABLE ` + FTSWordTable + ` USING fts5(summary, body, content='', tokenize='unicode61 remove_diacritics 2')`,
	FTSSubTable:  `CREATE VIRTUAL TABLE ` + FTSSubTable + ` USING fts5(summary, body, content='', tokenize='trigram case_sensitive 0 remove_diacritics 1')`,
}

// FTSTables returns the names of the two full-text tables, the word index first.
func FTSTables() []string { return []string{FTSWordTable, FTSSubTable} }

// FTSTableDDL returns the CREATE VIRTUAL TABLE statement of an FTS table (one of FTSTables), shared
// by the migration, reindex and verify. ok is false for any other name.
func FTSTableDDL(table string) (string, bool) {
	q, ok := ftsDDL[table]
	return q, ok
}

// ftsVocabName is the name of the fts5vocab table of an FTS table.
func ftsVocabName(table string) string { return table + "_v" }

// FTSVocabDDL returns the CREATE VIRTUAL TABLE ... USING fts5vocab statement of the vocab table of an
// FTS table: (term, doc, col, offset) rows, one per indexed token, which a read transaction can
// enumerate without writing. ok is false for a name that is not an FTS table.
func FTSVocabDDL(table string) (string, bool) {
	if _, ok := ftsDDL[table]; !ok {
		return "", false
	}
	return `CREATE VIRTUAL TABLE ` + ftsVocabName(table) + ` USING fts5vocab(` + table + `, 'instance')`, true
}

// v3Statements upgrades schema v2 to v3. The meta row starts as the empty string ("not built"): v3Post sets the
// current version when there is nothing to index.
var v3Statements = buildV3Statements()

func buildV3Statements() []string {
	var stmts []string
	for _, table := range FTSTables() {
		ddl, _ := FTSTableDDL(table)
		vocab, _ := FTSVocabDDL(table)
		stmts = append(stmts, ddl, vocab)
	}
	return append(stmts, `INSERT INTO records_meta (key, value) VALUES ('`+MetaFTSNormVersion+`', '')`)
}

// v3Post runs after v3Statements in the same transaction: a database with no records has nothing
// unindexed, so its index is current at once; one with records stays "not built" (the empty value)
// until `records reindex` builds it. It never marks an index current over records it does not hold.
func v3Post(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE records_meta SET value = ? WHERE key = ? AND NOT EXISTS (SELECT 1 FROM records)`,
		FTSNormVersion(), MetaFTSNormVersion); err != nil {
		return fmt.Errorf("set %s: %w", MetaFTSNormVersion, err)
	}
	return nil
}
