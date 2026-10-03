package filesys

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SkipDir, returned by a Walk callback, skips the directory it was called for
// (or, when returned for a non-directory, the remaining entries of its
// parent directory).
var SkipDir = errors.New("skip directory") //nolint:revive,staticcheck // name mirrors io/fs.SkipDir

// MaxWalkDepth caps the directory nesting Walk will follow.
const MaxWalkDepth = 4096

// Walk visits dir's subtree depth-first in pre-order, with the entries of
// each directory sorted by Name (then ID). dirPath is the slash path of dir
// ("/" for the root); fn receives the slash path of each descendant (dir
// itself is not reported). A live directory is recursed into only when fn
// returns nil for it. If ReadDir fails, fn is called a second time for that
// directory with the error. Deleted entries are passed to fn but never
// recursed into. Directory IDs are tracked across the whole walk: a directory
// that is reached again, or nesting deeper than MaxWalkDepth, is reported to
// fn with a *CorruptError (errors.Is ErrCorrupt) instead of being followed, so
// a hostile filesystem cannot make Walk loop. A non-nil error from fn other
// than SkipDir stops the walk and is returned.
func Walk(fsys FileSystem, dir Entry, dirPath string, fn func(path string, e Entry, err error) error) error {
	w := &walker{fsys: fsys, fn: fn, seen: map[string]bool{dir.ID: true}}
	return w.walk(dir, dirPath, 0)
}

type walker struct {
	fsys FileSystem
	fn   func(path string, e Entry, err error) error
	seen map[string]bool
}

func (w *walker) walk(dir Entry, dirPath string, depth int) error {
	kids, err := w.fsys.ReadDir(dir)
	if err != nil {
		if err := w.fn(dirPath, dir, err); err != nil && !errors.Is(err, SkipDir) {
			return err
		}
		return nil
	}
	kids = append([]Entry(nil), kids...)
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].Name != kids[j].Name {
			return kids[i].Name < kids[j].Name
		}
		return kids[i].ID < kids[j].ID
	})
	base := strings.TrimSuffix(dirPath, "/") + "/"
	for _, k := range kids {
		p := base + k.Name
		recurse := k.Type == TypeDir && !k.Deleted
		if recurse {
			var bad error
			if w.seen[k.ID] {
				bad = &CorruptError{
					Structure: "directory tree", Offset: -1,
					Reason: fmt.Sprintf("directory %q is reachable more than once (cycle) at %s", k.ID, p),
				}
			} else if depth+1 > MaxWalkDepth {
				bad = &CorruptError{
					Structure: "directory tree", Offset: -1,
					Reason: fmt.Sprintf("nesting deeper than %d at %s", MaxWalkDepth, p),
				}
			}
			if bad != nil {
				if err := w.fn(p, k, bad); err != nil && !errors.Is(err, SkipDir) {
					return err
				}
				continue
			}
		}
		if err := w.fn(p, k, nil); err != nil {
			if !errors.Is(err, SkipDir) {
				return err
			}
			if recurse {
				continue
			}
			return nil // SkipDir on a non-directory skips the rest of this directory
		}
		if recurse {
			w.seen[k.ID] = true
			if err := w.walk(k, p, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
