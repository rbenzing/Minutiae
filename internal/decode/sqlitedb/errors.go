package sqlitedb

import (
	"errors"
	"strings"
)

// Sentinel errors of the decoder. Callers test them with errors.Is.
var (
	ErrNoBudget             = errors.New("sqlitedb: no memory budget supplied")
	ErrNotSQLite            = errors.New("sqlitedb: not a database file")
	ErrEncrypted            = errors.New("sqlitedb: database is encrypted")
	ErrCorrupt              = errors.New("sqlitedb: database structure is corrupt")
	ErrLiveUnavailable      = errors.New("sqlitedb: live view is unavailable")
	ErrEngineRefuses        = errors.New("sqlitedb: the engine would refuse this database")
	ErrNoSuchTable          = errors.New("sqlitedb: no such table")
	ErrWithoutRowid         = errors.New("sqlitedb: table has no rowid")
	ErrIndexLimit           = errors.New("sqlitedb: join index limit reached")
	ErrStop                 = errors.New("sqlitedb: stopped by the caller")
	ErrBadFiles             = errors.New("sqlitedb: companion files do not belong to this database")
	ErrRowMismatch          = errors.New("sqlitedb: row does not belong to this table")
	ErrInternal             = errors.New("sqlitedb: internal error")
	ErrUnsupportedSchema    = errors.New("sqlitedb: schema is not supported")
	ErrUnsupportedCollation = errors.New("sqlitedb: collation is not supported")
	ErrReleased             = errors.New("sqlitedb: database was released")
)

// UnsupportedSchemaError says a table lacks what a decoder needs.
type UnsupportedSchemaError struct {
	Table   string
	Missing []string
	Reason  string
}

func (e *UnsupportedSchemaError) Error() string {
	s := "sqlitedb: table " + e.Table + " is not supported"
	if len(e.Missing) > 0 {
		s += ": missing " + strings.Join(e.Missing, ", ")
	}
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

// Is makes errors.Is(err, ErrUnsupportedSchema) true.
func (e *UnsupportedSchemaError) Is(target error) bool { return target == ErrUnsupportedSchema }

// UnsupportedCollationError names a collation the decoder cannot compare under.
type UnsupportedCollationError struct {
	Table, Column, Collation string
}

func (e *UnsupportedCollationError) Error() string {
	return "sqlitedb: collation " + e.Collation + " of " + e.Table + "." + e.Column + " is not supported"
}

// Is makes errors.Is(err, ErrUnsupportedCollation) true.
func (e *UnsupportedCollationError) Is(target error) bool { return target == ErrUnsupportedCollation }
