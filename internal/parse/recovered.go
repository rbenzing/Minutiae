package parse

import (
	"context"
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/records"
)

// RecoverySuffix is appended to a claiming parser's name to form the identity of its typed
// recovery run (4G-R4). No parser name may end in it.
const RecoverySuffix = ".recover"

// RecoveryName is the identity name of the typed recovery run of the parser called name.
func RecoveryName(name string) string { return name + RecoverySuffix }

// maxClaimingName bounds a claiming parser's name so that name+RecoverySuffix still fits the
// 64-byte parser name of the records store.
const maxClaimingName = 56

// maxRecoveredLocator bounds a recovered-row locator.
const maxRecoveredLocator = 1024

var (
	// ErrRecoveryFieldsFromParser: a live parser record set Recovery or Confidence (C8); only the
	// host stamps them. The host emitter counts it as a rejection (a pure package cannot wrap records.ErrInvalidRecord).
	ErrRecoveryFieldsFromParser = errors.New("parse: a parser record sets Recovery or Confidence, which only the host stamps")
	// ErrLiveEmitInRecovery: the plain Emit was used in a recovery job; only EmitRecovered writes.
	ErrLiveEmitInRecovery = errors.New("parse: Emit is refused in a recovery job, use EmitRecovered")
	// ErrBadRecoveredFacts: RecoveredFacts failed Validate.
	ErrBadRecoveredFacts = errors.New("parse: invalid recovered-row facts")
)

// RecoveredFacts are the facts of one recovered row, handed to the host with the record the
// mapper built from it. The host, not the mapper, stamps them on the record (spec 12.3).
type RecoveredFacts struct {
	Table, Method, Basis, Relation, Origin string
	Confidence                             int
	Role                                   FileRole
	Range                                  records.Range
	Locator                                string
	Page                                   uint32
	Cell                                   int
	WAL                                    *RecoveredWAL
	Journal                                *RecoveredJournal
	Truncated                              bool
	Notes                                  []string
}

// RecoveredWAL locates a row in a write-ahead log.
type RecoveredWAL struct {
	Frame, Salt1, Salt2 uint32
	Committed           bool
}

// RecoveredJournal locates a row in a rollback journal.
type RecoveredJournal struct {
	Record, Segment int
	Hot, Applied    bool
}

// RecoveryEmitter is the Emitter of a recovery job. EmitRecovered hands over a record with the
// facts of its row; Unmapped reports a claimed row that produced no record.
type RecoveryEmitter interface {
	Emitter
	EmitRecovered(ctx context.Context, r records.Record, f RecoveredFacts) error
	Unmapped(ctx context.Context, f RecoveredFacts, reason string) error
}

func validRecoveryMethod(s string) bool {
	if len(s) < 2 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Validate checks the facts before the host stamps them. It is pure and allocation-free on
// success. Basis is schema or fit only: a guess or an unlabelled row is never typed (4G-R3).
func (f RecoveredFacts) Validate() error {
	switch {
	case !validRecoveryMethod(f.Method):
		return fmt.Errorf("%w: method %q", ErrBadRecoveredFacts, f.Method)
	case f.Confidence < 0 || f.Confidence > 100:
		return fmt.Errorf("%w: confidence %d", ErrBadRecoveredFacts, f.Confidence)
	case f.Basis != "schema" && f.Basis != "fit":
		return fmt.Errorf("%w: basis %q", ErrBadRecoveredFacts, f.Basis)
	case f.Role != RoleDB && f.Role != RoleWAL && f.Role != RoleJournal:
		return fmt.Errorf("%w: role %q", ErrBadRecoveredFacts, f.Role)
	case f.Locator == "" || len(f.Locator) > maxRecoveredLocator:
		return fmt.Errorf("%w: locator of %d bytes", ErrBadRecoveredFacts, len(f.Locator))
	case f.Relation != "absent-from-live" && f.Relation != "superseded-version" && f.Relation != "uncommitted" && f.Relation != "unknown":
		return fmt.Errorf("%w: relation %q", ErrBadRecoveredFacts, f.Relation)
	case f.WAL != nil && f.Role != RoleWAL:
		return fmt.Errorf("%w: WAL facts on role %q", ErrBadRecoveredFacts, f.Role)
	case f.Journal != nil && f.Role != RoleJournal:
		return fmt.Errorf("%w: journal facts on role %q", ErrBadRecoveredFacts, f.Role)
	}
	return nil
}
