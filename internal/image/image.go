// Package image opens disk and partition image containers (raw, split raw,
// and EWF through a registered opener) as read-only io.ReaderAt values.
//
// The package is pure: it imports no other Minutiae package and never opens a
// file for writing.
package image

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// KV is one ordered metadata key/value pair.
type KV struct{ Key, Value string }

// Image is a read-only view of a disk or partition image.
type Image interface {
	io.ReaderAt
	Size() int64     // logical media size in bytes
	SectorSize() int // 512 unless the container says otherwise
	Format() string  // "raw", "split-raw", "ewf"
	Metadata() []KV  // ordered key/values (container header fields, segment list)
	Close() error
}

var (
	// ErrUnsupportedContainer is returned for a recognised container format
	// that this build cannot read.
	ErrUnsupportedContainer = errors.New("unsupported image container")
	// ErrNoSegments is returned when no segment files are given.
	ErrNoSegments = errors.New("no image segments given")
)

// Opener opens a container format from its ordered segment files. On success
// the returned Image owns the files; on error the caller closes them.
type Opener func(files []*os.File) (Image, error)

const sigLen = 8

var (
	sigEWF  = []byte("EVF\x09\x0d\x0a\xff\x00")
	sigEWF2 = []byte("EVF2\x0d\x0a\x81\x00")
	sigL01  = []byte("LVF\x09\x0d\x0a\xff\x00")
)

var (
	registryMu sync.Mutex
	ewfOpener  Opener
)

// RegisterEWF installs the EWF v1 opener (plan 2E). Until then EWF images
// return ErrUnsupportedContainer. Passing nil removes the opener.
func RegisterEWF(o Opener) {
	registryMu.Lock()
	defer registryMu.Unlock()
	ewfOpener = o
}

func registeredEWF() Opener {
	registryMu.Lock()
	defer registryMu.Unlock()
	return ewfOpener
}

// Open opens one image from one or more segment paths, read-only, in order.
func Open(paths []string) (Image, error) {
	if len(paths) == 0 {
		return nil, ErrNoSegments
	}
	files := make([]*os.File, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p) // read-only by construction
		if err != nil {
			closeAll(files)
			return nil, err
		}
		files = append(files, f)
	}
	return OpenFiles(files)
}

// OpenFiles is Open for already-open read-only files (used by examine, which
// opens artifacts via evidence.OpenArtifact). It takes ownership of the files:
// Image.Close closes them, and they are closed here when OpenFiles fails.
func OpenFiles(files []*os.File) (Image, error) {
	if len(files) == 0 {
		return nil, ErrNoSegments
	}
	for i, f := range files {
		if f == nil {
			closeAll(files)
			return nil, fmt.Errorf("image: segment %d is nil", i+1)
		}
	}
	// Reading fewer than sigLen bytes (short file) never matches a signature.
	head := make([]byte, sigLen)
	n, err := files[0].ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		closeAll(files)
		return nil, fmt.Errorf("image: read segment 1: %w", err)
	}
	head = head[:n]

	switch {
	case bytes.Equal(head, sigEWF):
		if o := registeredEWF(); o != nil {
			img, err := o(files)
			if err != nil {
				closeAll(files)
				return nil, err
			}
			return img, nil
		}
		closeAll(files)
		return nil, fmt.Errorf("%w: EWF (E01) support arrives in a later release", ErrUnsupportedContainer)
	case bytes.Equal(head, sigEWF2):
		closeAll(files)
		return nil, fmt.Errorf("%w: EWF2 (Ex01)", ErrUnsupportedContainer)
	case bytes.Equal(head, sigL01):
		closeAll(files)
		return nil, fmt.Errorf("%w: logical evidence file (L01)", ErrUnsupportedContainer)
	}

	img, err := openRaw(files)
	if err != nil {
		closeAll(files)
		return nil, err
	}
	return img, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
