package android

import (
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// localPaths assigns every remote file and directory of one logical
// acquisition a distinct local artifact path. Android paths are
// case-sensitive and may contain characters that evidence sanitizes, but the
// examiner's filesystem may be case-insensitive: "File.txt" and "file.txt",
// or a file "a:b" and a directory "a_b", would otherwise land on the same
// local path. A later name that collides, case-folded, with an earlier one
// gets a "~N" suffix (before the extension for files); the original remote
// path stays in the artifact's Source.RemotePath.
type localPaths struct {
	used map[string]bool   // case-folded local paths already assigned (files and directories)
	dirs map[string]string // cleaned absolute remote directory -> local directory
}

func newLocalPaths() *localPaths {
	return &localPaths{used: map[string]bool{}, dirs: map[string]string{"/": "files"}}
}

// dir returns the local directory for remote directory remote, assigning it
// (and any unassigned ancestor) on first use.
func (l *localPaths) dir(remote string) (string, error) {
	remote = path.Clean("/" + remote)
	if local, ok := l.dirs[remote]; ok {
		return local, nil
	}
	parent, err := l.dir(path.Dir(remote))
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

// file assigns the local path for a file named name in local directory parent.
func (l *localPaths) file(parent, name string) (string, error) {
	return l.assign(parent, name, false)
}

func (l *localPaths) assign(parent, name string, isDir bool) (string, error) {
	comp, err := evidence.SanitizeRelPath(name)
	if err != nil {
		return "", err
	}
	candidate := parent + "/" + comp
	for n := 2; l.used[foldCase(candidate)]; n++ {
		candidate = parent + "/" + withSuffix(comp, n, isDir)
	}
	l.used[foldCase(candidate)] = true
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

// foldCase maps every rune to the smallest member of its Unicode simple
// case-folding orbit, so two strings fold equal exactly when
// strings.EqualFold reports them equal.
func foldCase(s string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lowest = min(lowest, f)
		}
		return lowest
	}, s)
}
