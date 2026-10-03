package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrUnknownArtifact is returned when an artifact id or path is not in the manifest.
var ErrUnknownArtifact = errors.New("unknown artifact")

// OpenArtifact opens a manifest artifact read-only. It returns ErrIntegrity
// (wrapped) when the file is missing or its size differs from the manifest,
// and an error wrapping ErrUnknownArtifact when id is not in the manifest.
// Full re-hashing is Verify's job.
func (c *Case) OpenArtifact(id string) (*os.File, ManifestRecord, error) {
	recs, err := c.Manifest()
	if err != nil {
		return nil, ManifestRecord{}, fmt.Errorf("manifest unreadable: %w", err)
	}
	for _, r := range recs {
		if r.ID != id {
			continue
		}
		f, err := os.Open(filepath.Join(c.Dir, filepath.FromSlash(r.Path)))
		if err != nil {
			return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s (%s): %v", ErrIntegrity, r.ID, r.Path, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s (%s): %v", ErrIntegrity, r.ID, r.Path, err)
		}
		if st.Size() != r.Size {
			_ = f.Close()
			return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s (%s): file size %d differs from manifest size %d",
				ErrIntegrity, r.ID, r.Path, st.Size(), r.Size)
		}
		return f, r, nil
	}
	return nil, ManifestRecord{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, id)
}

// FindArtifact resolves ref as an artifact id, else as a case-relative slash
// path (exact match on ManifestRecord.Path).
func (c *Case) FindArtifact(ref string) (ManifestRecord, error) {
	recs, err := c.Manifest()
	if err != nil {
		return ManifestRecord{}, fmt.Errorf("manifest unreadable: %w", err)
	}
	for _, r := range recs {
		if r.ID == ref {
			return r, nil
		}
	}
	for _, r := range recs {
		if r.Path == ref {
			return r, nil
		}
	}
	return ManifestRecord{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, ref)
}
