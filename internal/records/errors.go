// Package records is the write path and read API of the unified artifact
// database: the record model, the type registry, write-time validation and the
// canonical payload encoding. Every record is validated before anything is
// buffered, with a typed error; nothing is silently dropped, truncated or
// repaired.
package records

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Every validation error wraps ErrInvalidRecord, so a caller can tell a
// rejected record (the parser's bug or hostile input) from an I/O failure.
var (
	// ErrInvalidRecord is the root of every record validation error.
	ErrInvalidRecord = errors.New("invalid record")
	// ErrUnknownType: the record's type is not registered.
	ErrUnknownType = fmt.Errorf("%w: unknown record type", ErrInvalidRecord)
	// ErrUnknownArtifact: the artifact id is empty or not in the manifest.
	// It also wraps evidence.ErrUnknownArtifact.
	ErrUnknownArtifact = fmt.Errorf("%w: %w", ErrInvalidRecord, evidence.ErrUnknownArtifact)
	// ErrUndeclaredArtifact: the artifact exists but the ingest did not declare
	// it when it started (the writer enforces this).
	ErrUndeclaredArtifact = fmt.Errorf("%w: artifact not declared for this ingest", ErrInvalidRecord)
	// ErrInvalidText: text that is not valid UTF-8 or holds a NUL.
	ErrInvalidText = fmt.Errorf("%w: invalid text", ErrInvalidRecord)
	// ErrRecordTooLarge: a field exceeds its cap.
	ErrRecordTooLarge = fmt.Errorf("%w: field too large", ErrInvalidRecord)
	// ErrInvalidTime: a time that cannot be stored (basis, offset, range,
	// overflow, missing ts_note).
	ErrInvalidTime = fmt.Errorf("%w: invalid time", ErrInvalidRecord)
	// ErrInvalidRange: a byte range that is negative, overflows or lies beyond
	// the artifact.
	ErrInvalidRange = fmt.Errorf("%w: invalid byte range", ErrInvalidRecord)
	// ErrInvalidPayload: a payload that cannot be encoded canonically or that
	// its type's validator refuses.
	ErrInvalidPayload = fmt.Errorf("%w: invalid payload", ErrInvalidRecord)
	// ErrInvalidLocator: a locator without a valid scheme.
	ErrInvalidLocator = fmt.Errorf("%w: invalid locator", ErrInvalidRecord)
	// ErrInvalidField: any other field out of its allowed values (recovery,
	// confidence, kinds, parser identity, type registration).
	ErrInvalidField = fmt.Errorf("%w: invalid field", ErrInvalidRecord)
)

// ErrIndexNotCurrent: the full-text index is not current (never built after an upgrade, a rebuild
// was interrupted, built by another normalization, or its state cannot be trusted), so a writer must
// not add records to it. The remedy is `minutiae records reindex --case <dir>`.
var ErrIndexNotCurrent = evidence.ErrIndexNotCurrent

// A record on a recovered artifact (one that, or whose ancestor, carries a
// Recovery) must itself be recovered and must not claim more confidence than
// the artifact does. Both wrap ErrInvalidRecord.
var (
	// ErrRecoveredArtifactLiveRecord: a live record names a recovered artifact.
	ErrRecoveredArtifactLiveRecord = fmt.Errorf("%w: a live record cannot be built on a recovered artifact", ErrInvalidRecord)
	// ErrConfidenceAboveArtifact: the record's confidence is missing or above the recovered artifact's.
	ErrConfidenceAboveArtifact = fmt.Errorf("%w: confidence is above that of the recovered artifact", ErrInvalidRecord)
)
