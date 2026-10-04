// Package sqlitefile is a pure-Go, read-only reader for SQLite database files
// found as evidence: the database, its write-ahead log (<db>-wal) and its
// rollback journal (<db>-journal).
//
// It is not an SQL engine and links none. The only operation it performs on
// evidence is ReadAt on an io.ReaderAt whose size the caller states; it never
// writes, seeks, closes or opens anything, and it never reads at or past the
// stated size. Damage is data: a bad page, cell, chain or frame is skipped
// with a Warning and counted, never a failure of the whole scan. Every on-disk
// number is validated against the size of what contains it before it drives a
// loop, an allocation or a seek, and every chain and tree walk keeps a visited
// set, so a hostile file cannot hang the process, panic it or make it allocate
// without bound. Memory is charged to a Budget; there is no unbudgeted mode.
//
// Panics are defects, but a panic inside an exported method is recovered into
// a *PanicError (matching ErrInternal) so one hostile file never ends the
// process.
//
// The package imports no other Minutiae package: parser packages work on
// io.ReaderAt and can never write to a case.
package sqlitefile
