package filesys

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SkipDir, returned by a Walk callback, skips the directory it was called for
// (or, when returned for a non-directory, the remaining entries of its
// parent directory).
var SkipDir = errors.New("skip directory") //nolint:revive,staticcheck // name mirrors io/fs.SkipDir

// MaxWalkDepth caps the directory nesting Walk will follow.
const MaxWalkDepth = 4096

// MaxWalkPath caps the length in bytes of the slash path Walk builds for an
// entry. Names are bounded individually, so without it a deep chain of long
// names makes every path (and the memory held by the recursion) grow without
// limit.
const MaxWalkPath = 32 << 10

// Walk visits dir's subtree depth-first in pre-order, with the entries of
// each directory sorted by Name (then ID). dirPath is the slash path of dir
// ("/" for the root); fn receives the slash path of each descendant (dir
// itself is not reported).
//
// A live directory is recursed into only when fn returns nil for it. SkipDir
// returned for any directory entry (deleted or not) skips only that entry's
// children and the walk continues with its siblings; returned for a
// non-directory it skips the rest of the containing directory. Deleted
// entries are passed to fn but never recursed into. If ReadDir fails, fn is
// called a second time for that directory with the error. A non-nil error
// from fn other than SkipDir stops the walk and is returned.
//
// Directory Entry.ID values must identify the directory itself (its first
// cluster, inode, node id ...), not the directory entry that names it, and
// must be unique within the filesystem: two entries that reach the same
// directory (a hard link, a cross-linked or self-referencing directory) must
// carry the same ID, or the loop is only caught by the depth cap. IDs are
// tracked across the whole walk, so a directory reachable from two parents (a
// hard-linked directory, a loop) is reported on its second appearance as a
// cycle. A cycle, nesting deeper than MaxWalkDepth, or a path longer than
// MaxWalkPath is reported to fn with a *CorruptError (errors.Is ErrCorrupt)
// instead of being followed, so a hostile filesystem cannot make Walk loop or
// grow without bound. For such an entry Walk calls fn(path, entry, err) once
// with the non-nil error and never fn(path, entry, nil); the path passed for a
// MaxWalkPath violation is truncated to MaxWalkPath bytes.
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
		if len(base)+len(k.Name) > MaxWalkPath {
			// Report it without building the oversized path; the callback gets
			// a bounded, truncated one.
			p := truncatePath(base + k.Name[:min(len(k.Name), 256)])
			bad := &CorruptError{
				Structure: "directory tree", Offset: -1,
				Reason: fmt.Sprintf("path longer than %d bytes, ending %s", MaxWalkPath, strconv.Quote(p[max(0, len(p)-128):])),
			}
			if err := w.fn(p, k, bad); err != nil && !errors.Is(err, SkipDir) {
				return err
			}
			continue
		}
		p := base + k.Name
		recurse := k.Type == TypeDir && !k.Deleted
		if recurse {
			var bad error
			if w.seen[k.ID] {
				bad = &CorruptError{
					Structure: "directory tree", Offset: -1,
					Reason: fmt.Sprintf("directory %q is reachable more than once (cycle) at %q", k.ID, p),
				}
			} else if depth+1 > MaxWalkDepth {
				bad = &CorruptError{
					Structure: "directory tree", Offset: -1,
					Reason: fmt.Sprintf("nesting deeper than %d at %q", MaxWalkDepth, p),
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
			if k.Type == TypeDir {
				continue // skip this directory's children (a no-op if it is deleted); siblings go on
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

// truncatePath bounds a path that is only being reported, to MaxWalkPath bytes.
func truncatePath(p string) string {
	if len(p) > MaxWalkPath {
		return p[:MaxWalkPath]
	}
	return p
}
