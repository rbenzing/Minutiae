package examine

import (
	"fmt"
	"os"
	"sync"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/image"
)

// OpenArtifactScope opens artifact id for carving. An "unallocated" export opens its parent image (Open with
// opts) and remembers the export: its run map, intersected with what is unallocated now, is the scope. Any
// other artifact gives a Session whose Image is the artifact read raw (Format "artifact", SectorSize 512,
// Segments = [the artifact]); it is never opened as a container, whatever its bytes look like, and it has no
// partition table. A missing id returns the error of Case.FindArtifact unchanged.
func OpenArtifactScope(c *evidence.Case, id string, opts Options) (*Session, error) {
	rec, err := c.FindArtifact(id)
	if err != nil {
		return nil, err
	}
	if d := rec.Source.Derived; rec.Source.Kind == "unallocated" && d != nil {
		s, err := Open(c, d.ParentID, opts)
		if err != nil {
			return nil, fmt.Errorf("open the parent image %s of unallocated export %s: %w", d.ParentID, rec.ID, err)
		}
		if s.Parent.SHA256 != d.ParentSHA256 {
			_ = s.Close()
			return nil, fmt.Errorf("%w: unallocated export %s records parent %s with SHA-256 %s, the image is %s",
				evidence.ErrIntegrity, rec.ID, d.ParentID, d.ParentSHA256, s.Parent.SHA256)
		}
		s.carveExport = &rec
		return s, nil
	}
	f, rec, err := c.OpenArtifact(rec.ID)
	if err != nil {
		return nil, err
	}
	return &Session{
		Case: c, Parent: rec, Segments: []evidence.ManifestRecord{rec},
		Image: &artifactImage{f: f, size: rec.Size}, opts: opts, fsCache: map[int]*fsEntry{},
		carveArtifact: &rec,
	}, nil
}

// artifactImage is one case artifact read as raw bytes: the image a scope of kind artifact carves.
type artifactImage struct {
	f    *os.File
	size int64
	once sync.Once
	err  error
}

var _ image.Image = (*artifactImage)(nil)

func (a *artifactImage) ReadAt(p []byte, off int64) (int, error) { return a.f.ReadAt(p, off) }
func (a *artifactImage) Size() int64                             { return a.size }
func (a *artifactImage) SectorSize() int                         { return 512 }
func (a *artifactImage) Format() string                          { return "artifact" }
func (a *artifactImage) Metadata() []image.KV                    { return nil }

func (a *artifactImage) Close() error {
	a.once.Do(func() { a.err = a.f.Close() })
	return a.err
}
