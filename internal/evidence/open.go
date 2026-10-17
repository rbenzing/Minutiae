package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrUnknownArtifact is returned when an artifact id or path is not in the manifest.
var ErrUnknownArtifact = errors.New("unknown artifact")

// manifestCache holds the decoded manifest that OpenArtifact reuses while the file is unchanged.
type manifestCache struct {
	mu   sync.Mutex
	info os.FileInfo
	recs []ManifestRecord
}

func sameManifestFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// manifestForOpen returns the manifest for OpenArtifact: the cached records while the file is the same
// file with the same size and modification time, else a fresh read (cached only if the file did not
// change while it was read). Any append through the case's own writer changes the size, so it
// invalidates the cache too. Never used by verify, which reads Manifest and recomputes.
//
// The returned slice is shared and must only be read; OpenArtifact copies the one record it returns.
// Residual: an external edit that restores the same size and modification time is not seen here
// (docs/limitations.md); case verify sees it.
func (c *Case) manifestForOpen() ([]ManifestRecord, error) {
	path := filepath.Join(c.Dir, manifestFile)
	c.mcache.mu.Lock()
	defer c.mcache.mu.Unlock()
	before, err := os.Stat(path)
	if err == nil && c.mcache.info != nil && sameManifestFile(before, c.mcache.info) {
		return c.mcache.recs, nil
	}
	c.mcache.info, c.mcache.recs = nil, nil
	recs, rerr := c.Manifest()
	if rerr != nil {
		return nil, rerr
	}
	if err == nil {
		if after, serr := os.Stat(path); serr == nil && sameManifestFile(before, after) {
			c.mcache.info, c.mcache.recs = after, recs
		}
	}
	return recs, nil
}

// OpenArtifact opens a manifest artifact read-only. It returns ErrIntegrity
// (wrapped) when the file is missing, is not a regular file, lies outside the
// case, or its size differs from the manifest, or when the manifest holds more
// than one record with id, or when the file or a directory on its path is a symlink or junction; other I/O failures (permission, descriptor
// exhaustion) are returned plainly, as they say nothing about the evidence.
// It returns an error wrapping ErrUnknownArtifact when id is not in the
// manifest. Full re-hashing is Verify's job.
func (c *Case) OpenArtifact(id string) (*os.File, ManifestRecord, error) {
	recs, err := c.manifestForOpen()
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
	// the cached slice is shared: hand the caller its own copy of the one record, so nothing it changes reaches later opens
	if r, err = cloneDerived(r); err != nil {
		return nil, ManifestRecord{}, fmt.Errorf("artifact %s: %w", id, err)
	}
	return c.openRecord(r)
}

// openRecord applies the checks shared by OpenArtifact and OpenArtifactIn to one manifest record.
func (c *Case) openRecord(r ManifestRecord) (*os.File, ManifestRecord, error) {
	local := filepath.FromSlash(r.Path)
	if !strings.HasPrefix(r.Path, artifactsDir+"/") || !filepath.IsLocal(local) {
		return nil, ManifestRecord{}, fmt.Errorf("%w: artifact %s: manifest path %q is outside the case artifacts directory", ErrIntegrity, r.ID, r.Path)
	}
	if err := c.refuseLinks(r, local); err != nil {
		return nil, ManifestRecord{}, err
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

// refuseLinks requires every component of the artifact's path below the case directory, the file
// included, to be a real file or directory: a symlink or junction could point outside the case and
// still serve the same bytes. A component that does not exist is left to the open that follows.
func (c *Case) refuseLinks(r ManifestRecord, local string) error {
	p := c.Dir
	for _, part := range strings.Split(local, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return openError(r, err)
		}
		if st.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("%w: artifact %s (%s): %q is a link, not a file or directory inside the case", ErrIntegrity, r.ID, r.Path, part)
		}
	}
	return nil
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

// cloneDerived returns r with its own copy of Source.Derived (slices, map and pointers included),
// made by a JSON round trip, the form the manifest is read from.
func cloneDerived(r ManifestRecord) (ManifestRecord, error) {
	if r.Source.Derived == nil {
		return r, nil
	}
	b, err := json.Marshal(r.Source.Derived)
	if err != nil {
		return r, err
	}
	var d Derivation
	if err := json.Unmarshal(b, &d); err != nil {
		return r, err
	}
	r.Source.Derived = &d
	return r, nil
}
