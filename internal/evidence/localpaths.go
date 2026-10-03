package evidence

import (
	"path"
	"strconv"
	"strings"
	"unicode"
)

// LocalPaths assigns every remote (or in-image) file and directory of one
// acquisition a distinct local artifact path. Android paths are
// case-sensitive and may contain characters that evidence sanitizes, but the
// examiner's filesystem may be case-insensitive: "File.txt" and "file.txt",
// or a file "a:b" and a directory "a_b", would otherwise land on the same
// local path. A later name that collides, case-folded, with an earlier one
// gets a "~N" suffix (before the extension for files); the original remote
// path stays in the artifact's Source.RemotePath.
type LocalPaths struct {
	used map[string]bool   // case-folded local paths already assigned (files and directories)
	dirs map[string]string // cleaned absolute remote directory -> local directory
}

// NewLocalPaths returns an assigner whose root remote directory "/" maps to
// the local directory root.
func NewLocalPaths(root string) *LocalPaths {
	return &LocalPaths{used: map[string]bool{}, dirs: map[string]string{"/": root}}
}

// Dir returns the local directory for remote directory remote, assigning it
// (and any unassigned ancestor) on first use.
func (l *LocalPaths) Dir(remote string) (string, error) {
	remote = path.Clean("/" + remote)
	if local, ok := l.dirs[remote]; ok {
		return local, nil
	}
	parent, err := l.Dir(path.Dir(remote))
	if err != nil {
		return "", err
	}
	local, err := l.assign(parent, path.Base(remote), true)
	if err != nil {
		return "", err
	}
	l.dirs[remote] = local
	return local, nil
}

// File assigns the local path for a file named name in local directory parent.
func (l *LocalPaths) File(parent, name string) (string, error) {
	return l.assign(parent, name, false)
}

func (l *LocalPaths) assign(parent, name string, isDir bool) (string, error) {
	comp, err := SanitizeRelPath(name)
	if err != nil {
		return "", err
	}
	candidate := parent + "/" + comp
	for n := 2; l.used[FoldCase(candidate)]; n++ {
		candidate = parent + "/" + withSuffix(comp, n, isDir)
	}
	l.used[FoldCase(candidate)] = true
	return candidate, nil
}

// withSuffix appends "~n" to a directory name, or inserts it before a file's
// extension ("a.txt" -> "a~2.txt"; ".profile" -> ".profile~2").
func withSuffix(name string, n int, isDir bool) string {
	suffix := "~" + strconv.Itoa(n)
	ext := path.Ext(name)
	if isDir || ext == "" || ext == name {
		return name + suffix
	}
	return strings.TrimSuffix(name, ext) + suffix + ext
}

// FoldCase maps every rune to the smallest member of its Unicode simple
// case-folding orbit, so two strings fold equal exactly when
// strings.EqualFold reports them equal. Use it as a map key to detect names
// that collide on a case-insensitive filesystem (LocalPaths, image import).
func FoldCase(s string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lowest = min(lowest, f)
		}
		return lowest
	}, s)
}
