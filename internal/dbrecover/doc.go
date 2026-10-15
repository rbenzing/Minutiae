// Package dbrecover turns the rows that internal/sqlitefile recovers from a
// SQLite database (and its write-ahead log and rollback journal) into stored
// records of type "db_row". It works on case artifacts opened read-only and
// writes only through the audited records writer; it never links a SQL engine.
package dbrecover
