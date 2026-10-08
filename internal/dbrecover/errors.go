package dbrecover

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Reasons of a RefusalError. They are tokens: they appear in audit entries and
// in the command output.
const (
	ReasonNotSQLite        = "not-sqlite"
	ReasonIneligibleKind   = "ineligible-kind"
	ReasonLiveUnavailable  = "live-unavailable"
	ReasonEngineRefuses    = "engine-refuses"
	ReasonSiblingUnusable  = "sibling-unusable"
	ReasonAmbiguousSibling = "ambiguous-sibling"
	ReasonBadOption        = "bad-option"
)

// Sentinels of the package.
var (
	// ErrNotSQLite: the input is not a SQLite database (or is encrypted).
	ErrNotSQLite = errors.New("dbrecover: not a SQLite database")
	// ErrLiveUnavailable: the library cannot present the live view of the input.
	ErrLiveUnavailable = errors.New("dbrecover: the live view of the database is unavailable")
	// ErrInputChanged: an input no longer hashes to what the manifest recorded.
	ErrInputChanged = errors.New("dbrecover: input changed")
	// ErrNarrowedRun: the run was narrowed by a filter or a limit, so it
	// cannot supersede a full run.
	ErrNarrowedRun = errors.New("dbrecover: the run was narrowed")
	// ErrInternal: a panic was recovered.
	ErrInternal = errors.New("dbrecover: internal error")
)

// RefusalError is a refusal to run, with a reason token and a human detail.
type RefusalError struct {
	Reason string
	Detail string
	Err    error
}

func (e *RefusalError) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

// Unwrap returns the cause, if any.
func (e *RefusalError) Unwrap() error { return e.Err }

// InputChangedError names the artifact whose bytes no longer hash to the
// manifest value. It is an ErrInputChanged and an evidence.ErrIntegrity.
type InputChangedError struct{ ArtifactID, Want, Got string }

func (e *InputChangedError) Error() string {
	return fmt.Sprintf("dbrecover: input %s changed: manifest sha256 %s, now %s", e.ArtifactID, e.Want, e.Got)
}

// Unwrap reaches both sentinels.
func (e *InputChangedError) Unwrap() []error { return []error{ErrInputChanged, evidence.ErrIntegrity} }

// PanicError carries a recovered panic value.
type PanicError struct{ Value any }

func (e *PanicError) Error() string {
	return fmt.Sprintf("dbrecover: internal error (panic): %v", e.Value)
}

// Is matches ErrInternal.
func (e *PanicError) Is(target error) bool { return target == ErrInternal }
