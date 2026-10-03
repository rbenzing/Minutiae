package examine

import "github.com/rbenzing/minutiae/internal/evidence"

// SetNewArtifact replaces how s creates artifacts, to inject faults.
func (s *Session) SetNewArtifact(f func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error)) {
	s.newArtifact = f
}
