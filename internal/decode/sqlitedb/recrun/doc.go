// Package recrun is the typed recovery runner: it drives a parser's
// parse.TableMapping values over the recovered rows of the tables the parser
// claims (sqlitedb.DB.ClaimedHistory) and hands every record to the host with the
// facts of its row.
//
// It is a decode-class package: it works on values, imports no case code, never
// stamps recovery facts on a record (the host does, from the RecoveredFacts) and
// never emits a live record.
package recrun
