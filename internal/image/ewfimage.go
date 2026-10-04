package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rbenzing/minutiae/internal/image/ewf"
)

var (
	// ErrCorruptContainer marks an EWF container whose structure cannot be
	// trusted (bad checksum, inconsistent section chain, impossible geometry).
	ErrCorruptContainer = ewf.ErrCorrupt
	// ErrChunkCorrupt marks a read of an EWF chunk that cannot be decoded; the
	// rest of the image stays readable.
	ErrChunkCorrupt = ewf.ErrChunkCorrupt
)

// VerifyResult is the outcome of verifying a container against its stored hashes.
type VerifyResult = ewf.VerifyResult

// HashCheck is one stored hash compared with the computed one.
type HashCheck = ewf.HashCheck

// Verifier is implemented by containers with stored hashes (EWF). Raw images
// do not implement it.
type Verifier interface {
	Verify(ctx context.Context, progress func(done, total int64)) (VerifyResult, error)
}

// Warner is implemented by containers that collect open/read warnings.
type Warner interface{ Warnings() []string }

// ewfImage adapts *ewf.Reader to Image. The Reader provides ReadAt, Size,
// SectorSize, Warnings and Verify; the files are closed by Close.
type ewfImage struct {
	*ewf.Reader
	files []*os.File

	closeOnce sync.Once
	closeErr  error
}

// openEWF is the built-in EWF opener: it stats each file, builds the segment
// list (Name = base name, R = the file, Size from Stat; regular files only),
// opens the set with ewf.Open and wraps the Reader. It does NOT close the
// files on error (OpenFiles does).
func openEWF(files []*os.File) (Image, error) {
	segs := make([]ewf.Segment, len(files))
	for i, f := range files {
		st, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("image: stat segment %d: %w", i+1, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("image: segment %d (%s) is not a regular file", i+1, filepath.Base(f.Name()))
		}
		segs[i] = ewf.Segment{Name: filepath.Base(f.Name()), R: f, Size: st.Size()}
	}
	r, err := ewf.Open(segs)
	if err != nil {
		if errors.Is(err, ewf.ErrUnsupported) {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
		}
		return nil, err
	}
	return &ewfImage{Reader: r, files: files}, nil
}

// Format reports "ewf".
func (*ewfImage) Format() string { return "ewf" }

// Metadata returns the container's ordered header, geometry, hash and segment fields.
func (e *ewfImage) Metadata() []KV {
	in := e.Reader.Metadata()
	out := make([]KV, len(in))
	for i, kv := range in {
		out[i] = KV{Key: kv.Key, Value: kv.Value}
	}
	return out
}

// Close closes the reader, then every segment file, and returns the first
// error. It is idempotent.
func (e *ewfImage) Close() error {
	e.closeOnce.Do(func() {
		e.closeErr = e.Reader.Close()
		for _, f := range e.files {
			if err := f.Close(); err != nil && e.closeErr == nil {
				e.closeErr = err
			}
		}
	})
	return e.closeErr
}

// HashStatus is the outcome of comparing one stored hash with the media.
type HashStatus = ewf.HashStatus

// The statuses of a HashCheck.
const (
	HashMatch      = ewf.HashMatch
	HashMismatch   = ewf.HashMismatch
	HashAbsent     = ewf.HashAbsent
	HashUnverified = ewf.HashUnverified
)
