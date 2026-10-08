// Package sqlitedb is the table decoder every SQLite-backed parser builds on.
//
// It reads the live view of a database that internal/sqlitefile has already
// opened over caller-supplied bytes; it never opens an SQL engine, never
// creates a file and never writes anything. A decoder, its tables and its
// contexts belong to one goroutine (the pure decode class has no sync), and a
// row handed to a visitor is owned by the library until the visitor returns:
// keep a Clone, not the row.
//
// A database is in one of nine states (see the plan): not SQLite, encrypted,
// corrupt, live-unavailable, and the readable ones with or without a WAL or a
// hot journal applied. The sentinels in errors.go name the refusals.
package sqlitedb
