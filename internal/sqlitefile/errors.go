package sqlitefile

import (
	"errors"
	"fmt"
)

// Sentinel errors. Wrapped errors carry detail; match with errors.Is.
var (
	ErrNotSQLite       = errors.New("sqlitefile: not an SQLite database")
	ErrLooksEncrypted  = errors.New("sqlitefile: invalid header; content looks encrypted")
	ErrCorrupt         = errors.New("sqlitefile: corrupt structure")
	ErrBudget          = errors.New("sqlitefile: memory budget exhausted")
	ErrLimit           = errors.New("sqlitefile: structural limit exceeded")
	ErrPageUnavailable = errors.New("sqlitefile: page not available in the file")
	ErrNotFound        = errors.New("sqlitefile: no such table or index")
	ErrWithoutRowid    = errors.New("sqlitefile: table is WITHOUT ROWID")
	ErrAlreadyAttached = errors.New("sqlitefile: companion file already attached")
	ErrInternal        = errors.New("sqlitefile: internal error (recovered panic)")
	ErrEngineRefuses   = errors.New("sqlitefile: the engine refuses to open this database")
)

// NotSQLiteReason says why a file is not taken for a database.
type NotSQLiteReason string

// The reasons a file is not an SQLite database.
const (
	ReasonEmpty        NotSQLiteReason = "empty"
	ReasonTooSmall     NotSQLiteReason = "too-small"
	ReasonBadMagic     NotSQLiteReason = "bad-magic"
	ReasonPage1Invalid NotSQLiteReason = "page1-invalid"
)

// NotSQLiteError reports a file whose header is not a valid SQLite header.
// LooksEncrypted is a hint (entropy and no known container magic), never an
// assertion; nothing is decoded.
type NotSQLiteError struct {
	Reason         NotSQLiteReason
	Size           int64
	Entropy        float64
	LooksEncrypted bool
}

func (e *NotSQLiteError) Error() string {
	s := fmt.Sprintf("sqlitefile: not an SQLite database (%s, %d bytes)", e.Reason, e.Size)
	if e.LooksEncrypted {
		s += fmt.Sprintf("; content looks encrypted (entropy %.2f bits/byte)", e.Entropy)
	}
	return s
}

// Is matches ErrNotSQLite always and ErrLooksEncrypted only with the hint.
func (e *NotSQLiteError) Is(target error) bool {
	switch target {
	case ErrNotSQLite:
		return true
	case ErrLooksEncrypted:
		return e.LooksEncrypted
	}
	return false
}

// CorruptError reports a structure that cannot be trusted. Reason is quoted
// by its producer wherever it holds text read from the disk.
type CorruptError struct {
	File   FileKind
	Page   uint32
	Reason string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("sqlitefile: corrupt structure (%s page %d): %s", e.File, e.Page, e.Reason)
}

// Is matches ErrCorrupt.
func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

// PanicError is a panic recovered inside an exported method. Stack holds at
// most the first 2 KiB of the stack at the point of the panic.
type PanicError struct {
	Value any
	Stack string
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("sqlitefile: internal error (recovered panic): %q", fmt.Sprint(e.Value))
}

// Is matches ErrInternal.
func (e *PanicError) Is(target error) bool { return target == ErrInternal }

// EngineRefusalError reports a companion file that makes the engine refuse to
// open the database at all (a WAL header of an unsupported version). The
// library does not present such a database as live.
type EngineRefusalError struct {
	File   FileKind
	Reason string
}

func (e *EngineRefusalError) Error() string {
	return fmt.Sprintf("sqlitefile: the engine refuses to open this database (%s): %s", e.File, e.Reason)
}

// Is matches ErrEngineRefuses.
func (e *EngineRefusalError) Is(target error) bool { return target == ErrEngineRefuses }
