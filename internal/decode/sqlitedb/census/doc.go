// Package census reads what the WhatsApp parsers need to decide a database's shape and that
// sqlitedb does not expose: every schema object with its SQL (views and triggers included),
// and, per named table, the row count of a full scan of the live view, the number of rows
// whose text cell equals a given value, and what the scan did not deliver. It opens the files
// through sqlitefile exactly as sqlitedb does (database, write-ahead log and journal, then the
// live view), so a WAL-only table is seen and an uncommitted frame never is.
//
// It is a separate leaf so that sqlitedb, and with it the source hash of every shipped SQLite
// parser, stays unchanged. A census never interprets SQL: the text of a view is reported, not
// evaluated.
package census
