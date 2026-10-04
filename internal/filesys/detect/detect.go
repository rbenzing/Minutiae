// Package detect probes an io.ReaderAt for a known filesystem and opens it
// with the matching parser. Parsers are hostile-input code: a panic inside a
// driver is recovered into a *filesys.CorruptError, never a crash.
package detect

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus"
)

// Driver is one filesystem parser.
type Driver struct {
	Name string
	// Probe reports whether the filesystem at the start of r (size bytes
	// long) looks like this driver's format. It must be cheap and read-only.
	Probe func(r io.ReaderAt, size int64) bool
	// Open parses the filesystem.
	Open func(r io.ReaderAt, size int64) (filesys.FileSystem, error)
	// LastResort marks a driver whose probe is a fallback for damaged volumes
	// (it matches an image another driver may legitimately open, by design),
	// so its match is never reported as an ambiguous signature.
	LastResort bool
}

// Drivers is the probe order, an ordered literal (spec §6: apfs, f2fs, ext4,
// exfat, hfsplus, fat; FAT is last because its signature is the weakest). It
// lists only the drivers that exist; a new filesystem package inserts its
// driver at its place in that order, not at the end (f2fs-backup, the
// last-resort probe of a destroyed F2FS primary superblock, stays last).
var Drivers = []Driver{
	{Name: "f2fs", Probe: f2fs.Probe, Open: openF2FS},
	{Name: "ext4", Probe: ext4.Probe, Open: openExt4},
	{Name: "exfat", Probe: exfat.Probe, Open: openExFAT},
	{Name: "hfsplus", Probe: hfsplus.Probe, Open: openHFSPlus},
	{Name: "fat", Probe: fat.Probe, Open: openFAT},
	// Last resort: an F2FS volume whose primary superblock is destroyed. It
	// comes after every other driver so that a stale or forged F2FS backup
	// signature inside another filesystem can never claim it. Info().Type of
	// the opened filesystem stays "f2fs"; the name only identifies this probe.
	{Name: "f2fs-backup", Probe: f2fs.ProbeBackup, Open: openF2FS, LastResort: true},
}

// openExt4 adapts ext4.Open. It returns an untyped nil FileSystem on error: a
// nil *ext4.FS stored in the interface would not compare equal to nil.
func openExt4(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fs, err := ext4.Open(r, size)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// openF2FS adapts f2fs.Open (untyped nil on error, see openExt4).
func openF2FS(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fs, err := f2fs.Open(r, size)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// openExFAT adapts exfat.Open (untyped nil on error, see openExt4).
func openExFAT(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fs, err := exfat.Open(r, size)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// openHFSPlus adapts hfsplus.Open (untyped nil on error, see openExt4). The
// driver reports HFS+ and HFSX through Info().Type ("hfsplus", "hfsx").
func openHFSPlus(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fs, err := hfsplus.Open(r, size)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// openFAT adapts fat.Open (untyped nil on error, see openExt4). The driver
// reports FAT12, FAT16 and FAT32 through Info().Type.
func openFAT(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	fs, err := fat.Open(r, size)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// Probe returns the name of the first driver in Drivers that matches.
func Probe(r io.ReaderAt, size int64) (string, bool) { return ProbeWith(Drivers, r, size) }

// ProbeWith is Probe over an explicit driver list. Like OpenWith it skips
// drivers without Probe or Open. A driver whose Probe panics counts as a
// non-match here; OpenWith is the call that surfaces the panic as an error.
func ProbeWith(drivers []Driver, r io.ReaderAt, size int64) (string, bool) {
	for _, d := range drivers {
		if d.Open == nil {
			continue
		}
		if ok, err := probe(d, r, size); err == nil && ok {
			return d.Name, true
		}
	}
	return "", false
}

// Open probes and opens r with Drivers. When nothing matches the error wraps
// filesys.ErrUnsupported ("no recognized filesystem"). A panic inside a
// driver's Probe or Open is returned as a *filesys.CorruptError (Reason
// "probe panic: ..." or "parser panic: ...").
func Open(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	return OpenWith(Drivers, r, size)
}

// OpenWith is Open over an explicit driver list. The first driver whose Probe
// matches is tried. When its Open fails with an error wrapping
// filesys.ErrCorrupt (a damaged volume, or a signature that only looked like
// this filesystem) the remaining drivers whose Probe matches are tried in
// order; the first that opens is returned, its Info().Warnings leading with
// one "driver X matched but failed to open: <err>; opened as Y" entry per
// failed driver. When more than one
// driver's Probe matches, the drivers after the one that opened are named in a
// last "also matched by: ... (ambiguous signatures)" entry (a stale signature of
// another filesystem, or a forged one); a clean image has no such entry. When none opens, the first driver's error is returned. Any
// other error (an I/O failure, a probe panic) is final.
func OpenWith(drivers []Driver, r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	var firstErr error
	var warnings []string
	var failed []string
	for i, d := range drivers {
		if d.Open == nil {
			continue
		}
		ok, err := probe(d, r, size)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		fsys, err := open(d, r, size)
		if err == nil {
			for _, f := range failed {
				warnings = append(warnings, fmt.Sprintf("%s; opened as %s", f, d.Name))
			}
			if note := AmbiguityNote(AlsoMatching(drivers[i+1:], r, size)); note != "" {
				warnings = append(warnings, note)
			}
			if len(warnings) == 0 {
				return fsys, nil
			}
			return &warned{FileSystem: fsys, warnings: warnings}, nil
		}
		if !errors.Is(err, filesys.ErrCorrupt) {
			return nil, err
		}
		if firstErr == nil {
			firstErr = err
		}
		failed = append(failed, fmt.Sprintf("driver %s matched but failed to open: %v", d.Name, err))
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("%w: no recognized filesystem", filesys.ErrUnsupported)
}

// AlsoMatching returns the names of the drivers in rest whose Probe matches r.
// OpenWith calls it with the drivers after the one that opened the filesystem: a
// second matching signature is not an error (the first driver wins, and any
// earlier one that failed to open has its own note) but the examiner is told.
// A driver whose Probe panics, that has no Open, or that is a LastResort one does
// not count.
func AlsoMatching(rest []Driver, r io.ReaderAt, size int64) []string {
	var names []string
	for _, d := range rest {
		if d.Open == nil || d.LastResort {
			continue
		}
		if ok, err := probe(d, r, size); err == nil && ok {
			names = append(names, d.Name)
		}
	}
	return names
}

// AmbiguityNote is the note for a filesystem that other drivers also matched
// ("also matched by: fat (ambiguous signatures)"), or "" when none did.
func AmbiguityNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "also matched by: " + strings.Join(names, ", ") + " (ambiguous signatures)"
}

// warned is a filesystem opened after an earlier matching driver failed: its
// Info().Warnings lead with the failure notes. Everything else is the wrapped
// filesystem's.
type warned struct {
	filesys.FileSystem
	warnings []string
}

func (w *warned) Info() filesys.Info {
	info := w.FileSystem.Info()
	info.Warnings = append(slices.Clone(w.warnings), info.Warnings...)
	return info
}

func probe(d Driver, r io.ReaderAt, size int64) (ok bool, err error) {
	if d.Probe == nil {
		return false, nil
	}
	defer func() {
		if p := recover(); p != nil {
			ok = false
			err = &filesys.CorruptError{Structure: d.Name, Offset: -1, Reason: fmt.Sprintf("probe panic: %v", p)}
		}
	}()
	return d.Probe(r, size), nil
}

func open(d Driver, r io.ReaderAt, size int64) (fsys filesys.FileSystem, err error) {
	defer func() {
		if p := recover(); p != nil {
			fsys = nil
			err = &filesys.CorruptError{Structure: d.Name, Offset: -1, Reason: fmt.Sprintf("parser panic: %v", p)}
		}
	}()
	fsys, err = d.Open(r, size)
	if err != nil {
		return nil, err // never hand back a (possibly typed-nil) filesystem with an error
	}
	if fsys == nil {
		return nil, &filesys.CorruptError{Structure: d.Name, Offset: -1, Reason: "driver returned no filesystem and no error"}
	}
	return fsys, nil
}
