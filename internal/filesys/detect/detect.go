// Package detect probes an io.ReaderAt for a known filesystem and opens it
// with the matching parser. Parsers are hostile-input code: a panic inside a
// driver is recovered into a *filesys.CorruptError, never a crash.
package detect

import (
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Driver is one filesystem parser.
type Driver struct {
	Name string
	// Probe reports whether the filesystem at the start of r (size bytes
	// long) looks like this driver's format. It must be cheap and read-only.
	Probe func(r io.ReaderAt, size int64) bool
	// Open parses the filesystem.
	Open func(r io.ReaderAt, size int64) (filesys.FileSystem, error)
}

// Drivers is the probe order (spec §6: apfs, f2fs, ext, exfat, hfsplus, fat;
// FAT is last because its signature is the weakest). It starts empty; each
// filesystem package appends its driver when it lands.
var Drivers = []Driver{}

// Probe returns the name of the first driver in Drivers that matches.
func Probe(r io.ReaderAt, size int64) (string, bool) { return ProbeWith(Drivers, r, size) }

// ProbeWith is Probe over an explicit driver list. A driver whose Probe
// panics or is missing counts as a non-match.
func ProbeWith(drivers []Driver, r io.ReaderAt, size int64) (string, bool) {
	for _, d := range drivers {
		if probe(d, r, size) {
			return d.Name, true
		}
	}
	return "", false
}

// Open probes and opens r with Drivers. When nothing matches the error wraps
// filesys.ErrUnsupported ("no recognized filesystem"). A panic inside the
// matching driver's Open is returned as a *filesys.CorruptError.
func Open(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	return OpenWith(Drivers, r, size)
}

// OpenWith is Open over an explicit driver list. The first driver whose Probe
// matches is used and its result is final: later drivers are not tried if its
// Open fails.
func OpenWith(drivers []Driver, r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	for _, d := range drivers {
		if d.Open == nil || !probe(d, r, size) {
			continue
		}
		return open(d, r, size)
	}
	return nil, fmt.Errorf("%w: no recognized filesystem", filesys.ErrUnsupported)
}

func probe(d Driver, r io.ReaderAt, size int64) (ok bool) {
	if d.Probe == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return d.Probe(r, size)
}

func open(d Driver, r io.ReaderAt, size int64) (fsys filesys.FileSystem, err error) {
	defer func() {
		if p := recover(); p != nil {
			fsys = nil
			err = &filesys.CorruptError{Structure: d.Name, Offset: -1, Reason: fmt.Sprintf("parser panic: %v", p)}
		}
	}()
	fsys, err = d.Open(r, size)
	if err == nil && fsys == nil {
		return nil, &filesys.CorruptError{Structure: d.Name, Offset: -1, Reason: "driver returned no filesystem and no error"}
	}
	return fsys, err
}
