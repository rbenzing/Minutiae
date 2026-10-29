package dbrecover

import (
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Resolved is the manifest-only resolution of a run's inputs: the artifact ids
// of the database and of the companions the run would open (empty when none),
// and the sibling report, exactly as Run reports them.
type Resolved struct {
	DB, WAL, Journal string
	Siblings         SiblingReport
}

// ResolveSiblings decides which artifacts a Run of in would open, from the
// manifest alone: it opens and hashes nothing. It refuses what Run refuses
// before it opens an input: a companion named while the search is off, an
// ambiguous or unusable sibling, an unknown artifact, one artifact given in two roles.
func ResolveSiblings(c *evidence.Case, in Inputs) (Resolved, error) {
	p := &pipeline{c: c, in: in}
	ids, err := p.resolveIDs()
	if err != nil {
		return Resolved{}, err
	}
	if err := checkInputs(p.man, p.dbRec.ID, ids.wal, ids.journal); err != nil {
		return Resolved{}, err
	}
	return Resolved{DB: p.dbRec.ID, WAL: ids.wal, Journal: ids.journal, Siblings: p.rep}, nil
}

// inputIDs are the companions a run opens.
type inputIDs struct{ wal, journal string }

// refuseDuplicateRoles refuses an artifact given as two inputs (openInputs does too).
func refuseDuplicateRoles(db, wal, journal string) error {
	seen := map[string]string{}
	for _, x := range []struct{ role, id string }{{"db", db}, {"wal", wal}, {"journal", journal}} {
		if x.id == "" {
			continue
		}
		if prev, dup := seen[x.id]; dup {
			return &RefusalError{Reason: ReasonBadOption, Detail: fmt.Sprintf("artifact %s is given as both the %s and the %s input", x.id, prev, x.role)}
		}
		seen[x.id] = x.role
	}
	return nil
}

// checkInputs refuses what opening the inputs refuses before it reads a byte:
// an artifact in two roles, an id the manifest does not hold, a kind that is not input.
func checkInputs(man []evidence.ManifestRecord, db, wal, journal string) error {
	if err := refuseDuplicateRoles(db, wal, journal); err != nil {
		return err
	}
	for _, x := range []struct{ role, id string }{{"db", db}, {"wal", wal}, {"journal", journal}} {
		if x.id == "" {
			continue
		}
		ix := slices.IndexFunc(man, func(r evidence.ManifestRecord) bool { return r.ID == x.id })
		if ix < 0 {
			return fmt.Errorf("%s input: %w: %q", x.role, evidence.ErrUnknownArtifact, x.id)
		}
		if ok, why := eligibleKind(man[ix]); !ok {
			return &RefusalError{Reason: ReasonIneligibleKind, Detail: fmt.Sprintf("%s input %s: %s", x.role, x.id, why)}
		}
	}
	return nil
}
