package rowrun

import (
	"errors"
	"fmt"
)

// ErrContext says the mapping Context recorded a failure that Column could not
// report (a cancelled context, a released database, a corrupt or unsupported
// schema read). The rows mapped after it would have read "column absent", so the
// job fails; the cause is wrapped.
var ErrContext = errors.New("rowrun: the mapping context failed")

// RowError says one row cannot be turned into a record. The runner turns it into
// an analysis.warning with the row's locator and counts it; it is not a job
// failure. Reason names tables and columns only, never device content.
type RowError struct{ Reason string }

func (e *RowError) Error() string { return "rowrun: row not mapped: " + e.Reason }

// Unmappable returns a *RowError.
func Unmappable(format string, args ...any) error {
	return &RowError{Reason: fmt.Sprintf(format, args...)}
}

// ErrNilMap says a Step's mapping has no Map function.
var ErrNilMap = errors.New("rowrun: the table mapping has no Map function")
