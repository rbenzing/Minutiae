package evidence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnknownArtifact is returned when an artifact id or path is not in the manifest.
var ErrUnknownArtifact = errors.New("unknown artifact")

// OpenArtifact opens a manifest artifact read-only. It returns ErrIntegrity
// (wrapped) when the file is missing, is not a regular file, lies outside the
// case, or its size differs from the manifest, or when the manifest holds more
// than one record with id; other I/O failures (permission, descriptor
// exhaustion) are returned plainly, as they say nothing about the evidence.
// It returns an error wrapping ErrUnknownArtifact when id is not in the
// manifest. Full re-hashing is Verify's job.
func (c *Case) OpenArtifact(id string) (*os.File, ManifestRecord, error) {
	recs, err := c.Manifest()
	if err != nil {
		return nil, ManifestRecord{}, fmt.Errorf("manifest unreadable: %w", err)
	}
	var r ManifestRecord
	n := 0
	for _, x := range recs {
		if x.ID == id {
			r = x
			n++
		}
	}
	switch {
	case n == 0:
		return nil, ManifestRecord{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, id)
	case n > 1:
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact id %q appears %d times in the manifest", ErrIntegrity, id, n)
	}
	return c.openRecord(r)
}

// openRecord applies the checks shared by OpenArtifact and OpenArtifactIn to one manifest record.
func (c *Case) openRecord(r ManifestRecord) (*os.File, ManifestRecord, error) {
	local := filepath.FromSlash(r.Path)
	if !strings.HasPrefix(r.Path, artifactsDir+"/") || !filepath.IsLocal(local) {
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s: manifest path %q is outside the case artifacts directory", ErrIntegrity, r.ID, r.Path)
	}
	f, err := os.Open(filepath.Join(c.Dir, local))
	if err != nil {
		return nil, ManifestRecord{}, openError(r, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, ManifestRecord{}, openError(r, err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s (%s): not a regular file", ErrIntegrity, r.ID, r.Path)
	}
	if st.Size() != r.Size {
		_ = f.Close()
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s (%s): file size %d differs from manifest size %d",
			ErrIntegrity, r.ID, r.Path, st.Size(), r.Size)
	}
	return f, r, nil
}

// openError classifies a failure to open or stat an artifact file: a missing
// file is an integrity problem; anything else (permission denied, too many
// open files, ...) is an ordinary I/O error that proves nothing about the
// evidence and must not be reported as tampering.
func openError(r ManifestRecord, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: artifact %s (%s): %v", ErrIntegrity, r.ID, r.Path, err)
	}
	return fmt.Errorf("artifact %s (%s): %w", r.ID, r.Path, err)
}

// FindArtifact resolves ref as an artifact id, else as a case-relative slash
// path (exact match on ManifestRecord.Path). A ref that matches more than one
// manifest record (by id, or by path) is an ErrIntegrity error.
func (c *Case) FindArtifact(ref string) (ManifestRecord, error) {
	recs, err := c.Manifest()
	if err != nil {
		return ManifestRecord{}, fmt.Errorf("manifest unreadable: %w", err)
	}
	for _, field := range []string{"id", "path"} {
		var found ManifestRecord
		n := 0
		for _, r := range recs {
			v := r.ID
			if field == "path" {
				v = r.Path
			}
			if v == ref {
				found = r
				n++
			}
		}
		switch {
		case n == 1:
			return found, nil
		case n > 1:
			return ManifestRecord{}, fmt.Errorf("%w: artifact %s %q appears %d times in the manifest", ErrIntegrity, field, ref, n)
		}
	}
	return ManifestRecord{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, ref)
}

// ManifestIndex is a manifest read once and indexed by artifact id, for callers (case verify) that open
// many artifacts and must not re-read the manifest for each (C58).
type ManifestIndex struct {
	byID  map[string]ManifestRecord
	count map[string]int
}

// NewManifestIndex indexes recs. A duplicated id is remembered as such, so OpenArtifactIn refuses it
// exactly as OpenArtifact does.
func NewManifestIndex(recs []ManifestRecord) *ManifestIndex {
	ix := &ManifestIndex{byID: make(map[string]ManifestRecord, len(recs)), count: make(map[string]int, len(recs))}
	for _, r := range recs {
		ix.byID[r.ID] = r
		ix.count[r.ID]++
	}
	return ix
}

// OpenArtifactIn is OpenArtifact over a prebuilt index: the same checks with the same errors, without
// reading the manifest.
func (c *Case) OpenArtifactIn(ix *ManifestIndex, id string) (*os.File, ManifestRecord, error) {
	if ix == nil {
		return nil, ManifestRecord{}, errors.New("evidence: OpenArtifactIn needs a manifest index (see NewManifestIndex)")
	}
	switch n := ix.count[id]; {
	case n == 0:
		return nil, ManifestRecord{}, fmt.Errorf("%w: %q", ErrUnknownArtifact, id)
	case n > 1:
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact id %q appears %d times in the manifest", ErrIntegrity, id, n)
	}
	return c.openRecord(ix.byID[id])
}
