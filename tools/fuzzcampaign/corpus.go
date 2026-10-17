package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrUnsafeDir is returned for a directory PruneCorpus refuses to touch.
var ErrUnsafeDir = errors.New("fuzzcampaign: unsafe corpus directory")

// SkippedError reports the entries PruneCorpus left alone because they are not plain
// files it may remove. The pruning itself is complete when this is returned.
type SkippedError struct{ Symlinks, Dirs, Unreadable int }

func (e *SkippedError) Error() string {
	return fmt.Sprintf("fuzzcampaign: left alone %d symlinks, %d directories and %d entries that could not be examined",
		e.Symlinks, e.Dirs, e.Unreadable)
}

func isSep(r rune) bool { return r == '/' || r == '\\' }

// pathElements counts the named elements of a cleaned path, ignoring the volume and root.
func pathElements(clean string) []string {
	rest := strings.TrimPrefix(clean, filepath.VolumeName(clean))
	return strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' })
}

func checkCorpusDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafeDir)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q is not an absolute path", ErrUnsafeDir, dir)
	}
	for _, e := range strings.FieldsFunc(dir, isSep) {
		if e == ".." {
			return fmt.Errorf("%w: %q contains a .. element", ErrUnsafeDir, dir)
		}
	}
	elems := pathElements(filepath.Clean(dir))
	if len(elems) < 3 {
		return fmt.Errorf("%w: %q has fewer than three path elements", ErrUnsafeDir, dir)
	}
	for _, e := range elems {
		if strings.EqualFold(e, "testdata") {
			return fmt.Errorf("%w: %q is inside a testdata directory (a committed corpus)", ErrUnsafeDir, dir)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return fmt.Errorf("%w: %q is a symlink or other irregular entry", ErrUnsafeDir, dir)
	}
	return nil
}

type corpusEntry struct {
	name  string
	size  int64
	mtime time.Time
}

// PruneCorpus removes the oldest regular files (by modification time, then name) directly
// inside dir until at most maxCount files and maxBytes bytes remain. It refuses an empty dir,
// a relative dir, a dir with a .. element, a dir with fewer than three path elements, a dir
// inside any testdata directory and a dir that is itself a symlink (checked with Lstat). It never
// follows or removes a symlink, a directory or an entry it cannot examine; those are counted
// in the returned *SkippedError.
func PruneCorpus(dir string, maxCount int, maxBytes int64) (removed int, err error) {
	if err := checkCorpusDir(dir); err != nil {
		return 0, err
	}
	if maxCount < 0 || maxBytes < 0 {
		return 0, fmt.Errorf("fuzzcampaign: negative corpus cap (%d files, %d bytes)", maxCount, maxBytes)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var files []corpusEntry
	var total int64
	skipped := SkippedError{}
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		switch {
		case err != nil:
			skipped.Unreadable++
		case info.Mode()&fs.ModeSymlink != 0:
			skipped.Symlinks++
		case info.IsDir():
			skipped.Dirs++
		case !info.Mode().IsRegular():
			skipped.Unreadable++
		default:
			files = append(files, corpusEntry{e.Name(), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	slices.SortFunc(files, func(a, b corpusEntry) int {
		if c := a.mtime.Compare(b.mtime); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	for len(files)-removed > maxCount || total > maxBytes {
		if removed == len(files) {
			break
		}
		victim := files[removed]
		if err := os.Remove(filepath.Join(dir, victim.name)); err != nil {
			return removed, err
		}
		removed++
		total -= victim.size
	}
	if skipped != (SkippedError{}) {
		return removed, &skipped
	}
	return removed, nil
}
