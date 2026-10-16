package records

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// declareArtifacts reads the manifest into the set of known artifact ids and the
// information on each declared artifact (the artifacts of an ingest, sorted and
// without repeats). It is shared by Start and NewValidator, so both judge a
// record against the same facts.
func declareArtifacts(man []evidence.ManifestRecord, artifacts []string) (known map[string]bool, declared map[string]artifactInfo, err error) {
	known = make(map[string]bool, len(man))
	manByID := make(map[string]evidence.ManifestRecord, len(man))
	byID := make(map[string]artifactInfo, len(man))
	for _, m := range man {
		if known[m.ID] {
			// as OpenArtifact: an id held twice is an integrity error, never first- or last-wins
			return nil, nil, fmt.Errorf("%w: artifact id %q appears more than once in the manifest", evidence.ErrIntegrity, clip(m.ID))
		}
		known[m.ID] = true
		manByID[m.ID] = m
		byID[m.ID] = artifactInfo{ID: m.ID, SHA256: m.SHA256, Size: m.Size, Incomplete: m.Incomplete}
	}
	declared = make(map[string]artifactInfo, len(artifacts))
	for _, id := range artifacts {
		if id == "" {
			return nil, nil, fmt.Errorf("%w: an empty artifact id", ErrUnknownArtifact)
		}
		info, ok := byID[id]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q is not in the manifest", ErrUnknownArtifact, clip(id))
		}
		if tr, ok := evidence.RecoveryTrailOf(manByID, id); ok {
			info.Recovered, info.MaxConfidence, info.RecoveredVia = true, tr.MinConfidence, tr.ArtifactID
		}
		declared[id] = info
	}
	return known, declared, nil
}

// NewValidator returns a function that judges a record the way Writer.Add does
// (the type, the text, range, time and payload rules, and the rules of a
// recovered artifact) against the declared artifacts, and writes nothing: no
// ingest, no audit entry, no row. A failing record gets the same error (it wraps
// ErrInvalidRecord) that Add would return and count as a rejection.
func NewValidator(c *evidence.Case, artifacts []string) (func(Record) error, error) {
	man, err := c.Manifest()
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	known, declared, err := declareArtifacts(man, artifacts)
	if err != nil {
		return nil, err
	}
	return func(r Record) error {
		art, ok := declared[r.ArtifactID]
		if !ok {
			if known[r.ArtifactID] {
				return fmt.Errorf("%w: %q", ErrUndeclaredArtifact, clip(r.ArtifactID))
			}
			if r.ArtifactID != "" {
				return fmt.Errorf("%w: %q", ErrUnknownArtifact, clip(r.ArtifactID))
			}
		}
		_, err := prepare(r, art)
		return err
	}, nil
}
