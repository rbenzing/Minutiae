// Package rowrun is the one runner that drives parse.TableMapping values over a
// sqlitedb database, and the small helpers every SQLite mapping needs: where a
// row's cell lies (Where), what a cell holds (IntCell, TextCell, Flag), a
// timestamp with its status (Instant) and the error a mapping returns for a row
// it cannot turn into a record (Unmappable).
//
// It is a decode-class package: it works on values, imports no case code and
// names only records.Range from the records package, so it builds no record.
// The runner fails the job when the mapping Context recorded a failure
// (Context.Err), because a mapping that reads Column() == -1 as "this variant
// lacks the column" would otherwise drop data silently.
package rowrun
