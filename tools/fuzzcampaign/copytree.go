package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var errCopyTooBig = errors.New("fuzzcampaign: source copy over 2 GiB")

// errTooManyFiles is a source copy of more than maxCopyFiles files, symlinks and directories.
var errTooManyFiles = errors.New("fuzzcampaign: source copy has too many entries")

// errCopySizeChanged is a file whose size differs from the one the copy checked (it grew or
// shrank while being copied).
var errCopySizeChanged = errors.New("fuzzcampaign: a file changed size while it was being copied")

// maxCopyBytes bounds the bytes and maxCopyFiles the entries (files, symlinks, directories) of a source copy; variables so a test can lower them.
var (
	maxCopyBytes int64 = 2 << 30
	maxCopyFiles       = 200000
)

// CopyStats counts what a copy did.
type CopyStats struct {
	Files, Symlinks int
	Dirs            int // directories seen by the non-git walk, counted toward maxCopyFiles
	Bytes           int64
	Tracked         bool // the copy took the tracked files of a git tree
}

// excludedDirs are directory names never copied, at any depth.
var excludedDirs = []string{".git", "bin", "cases", ".superpowers"}

// excluded reports whether the slash-separated relative path is one the copy never takes:
// evidence and scratch directories, the local plans and any executable.
func excluded(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		for _, e := range excludedDirs {
			if strings.EqualFold(p, e) {
				return true
			}
		}
	}
	if rel == "docs/superpowers" || strings.HasPrefix(rel, "docs/superpowers/") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(rel), ".exe")
}

// CopyTree copies the source tree into dst, which must not exist or be empty. When src is a
// git tree it copies only the tracked files (git ls-files -z), so an untracked cases/ or dump
// anywhere in the tree never reaches the copy; the name exclusions stay as a second guard and
// are the only rule for a tree that is not a git tree. Symlinks are counted, not followed. A
// tracked file that is gone is an error. A copy over maxCopyBytes is errCopyTooBig.
func CopyTree(src, dst string) (CopyStats, error) {
	var st CopyStats
	if es, err := os.ReadDir(dst); err == nil && len(es) > 0 {
		return st, fmt.Errorf("fuzzcampaign: copy destination %s is not empty: a second campaign needs a new -out (see \"Fuzz campaigns\" in docs/testing.md)", dst)
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return st, err
	}
	git, err := isGitRoot(src)
	if err != nil {
		return st, err
	}
	if git {
		st.Tracked = true
		names, err := trackedFiles(src)
		if err != nil {
			return st, err
		}
		for _, rel := range names {
			if excluded(rel) {
				continue
			}
			if err := copyOne(src, dst, rel, &st, false); err != nil {
				return st, err
			}
		}
		return st, nil
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if st.Files+st.Symlinks+st.Dirs >= maxCopyFiles {
				return fmt.Errorf("%w: more than %d (at %s)", errTooManyFiles, maxCopyFiles, rel)
			}
			st.Dirs++
			return nil
		}
		return copyOne(src, dst, rel, &st, true)
	})
	return st, err
}

// isGitRoot reports whether src is the top level of a git work tree. A directory that has a
// .git entry which git does not recognise (a fixture) is not one; a .git entry with no git
// installed is an error, never a silent fallback to copying untracked files.
func isGitRoot(src string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(src, ".git")); err != nil {
		return false, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return false, fmt.Errorf("fuzzcampaign: %s has a .git entry but git is not installed: cannot tell the tracked files", src)
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "--show-toplevel").Output() //nolint:gosec // fixed argument vector
	if err != nil {
		return false, nil
	}
	top, err1 := os.Stat(strings.TrimSpace(string(out)))
	me, err2 := os.Stat(src)
	return err1 == nil && err2 == nil && os.SameFile(top, me), nil
}

func trackedFiles(src string) ([]string, error) {
	cmd := exec.Command("git", "-C", src, "ls-files", "-z") //nolint:gosec // fixed argument vector
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("fuzzcampaign: %s is a git tree but git ls-files failed: %w: %s", src, err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// copyOne copies one regular file. A symlink is counted and skipped; in tracked mode a file
// that cannot be examined (deleted from the work tree) is an error.
func copyOne(src, dst, rel string, st *CopyStats, walking bool) error {
	from := filepath.Join(src, filepath.FromSlash(rel))
	info, err := os.Lstat(from)
	if err != nil {
		if walking {
			return err
		}
		return fmt.Errorf("fuzzcampaign: tracked file %s cannot be copied: %w", rel, err)
	}
	switch {
	case info.IsDir():
		return nil // a submodule
	}
	if st.Files+st.Symlinks+st.Dirs >= maxCopyFiles {
		return fmt.Errorf("%w: more than %d (at %s)", errTooManyFiles, maxCopyFiles, rel)
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		st.Symlinks++
		return nil
	case !info.Mode().IsRegular():
		return fmt.Errorf("fuzzcampaign: %s is not a regular file", rel)
	}
	if st.Bytes+info.Size() > maxCopyBytes {
		return fmt.Errorf("%w (at %s)", errCopyTooBig, rel)
	}
	if err := copyFileLimited(from, filepath.Join(dst, filepath.FromSlash(rel)), info.Size()); err != nil {
		return err
	}
	st.Files++
	st.Bytes += info.Size()
	return nil
}

// copyFileLimited copies exactly size bytes: a file that grew or shrank since the size was
// checked is errCopySizeChanged, so the 2 GiB cap cannot be passed by a growing file.
func copyFileLimited(src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, in, size); err != nil {
		_ = out.Close()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: %s is shorter than the %d bytes checked", errCopySizeChanged, src, size)
		}
		return err
	}
	var extra [1]byte
	if _, err := io.ReadFull(in, extra[:]); !errors.Is(err, io.EOF) {
		_ = out.Close()
		if err == nil {
			return fmt.Errorf("%w: %s is longer than the %d bytes checked", errCopySizeChanged, src, size)
		}
		return err
	}
	return out.Close()
}

// resolveExisting makes p absolute and resolves every symlink or junction in its deepest
// existing ancestor, then appends the part that does not exist yet. A dangling link is an error.
func resolveExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	for cur := abs; ; {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			if _, serr := os.Stat(cur); serr == nil {
				// It exists and can be reached, but the resolver cannot follow it (a Windows
				// junction, or a path under one); the identity check in CheckOutDir covers it.
				return abs, nil
			}
			return "", fmt.Errorf("fuzzcampaign: %s is a link to a path that does not exist", cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// CheckOutDir refuses an output directory inside the source root, equal to or inside the work
// copy, or at or under any directory named cases. Each path is checked as given and with every
// link in its deepest existing ancestor resolved, so a symlink or junction into the source or a
// cases directory is refused too.
func CheckOutDir(out, srcRoot, work string) error {
	for _, p := range []string{out, srcRoot, work} {
		if _, err := filepath.Abs(p); err != nil {
			return err
		}
	}
	ro, err := resolveExisting(out)
	if err != nil {
		return err
	}
	rs, err := resolveExisting(srcRoot)
	if err != nil {
		return err
	}
	rw, err := resolveExisting(work)
	if err != nil {
		return err
	}
	o, _ := filepath.Abs(out)
	s, _ := filepath.Abs(srcRoot)
	w, _ := filepath.Abs(work)
	for _, p := range []string{o, ro} {
		for _, e := range strings.FieldsFunc(p, isSep) {
			if strings.EqualFold(e, "cases") {
				return fmt.Errorf("fuzzcampaign: -out %s is under a cases directory (resolved: %s)", o, ro)
			}
		}
	}
	if within(o, s) || within(ro, rs) {
		return fmt.Errorf("fuzzcampaign: -out %s is inside the source root %s (resolved: %s in %s)", o, s, ro, rs)
	}
	if o == w || ro == rw {
		return fmt.Errorf("fuzzcampaign: -out %s is the work copy", o)
	}
	return checkOutIdentity(o, s, w)
}

// within reports whether p is dir itself or below it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// HostModeAllowed refuses host mode on Windows, where the engine's exec counter stalls (see
// "Fuzz stall findings" in docs/testing.md).
func HostModeAllowed(goos string) error {
	if goos == "windows" {
		return errors.New("fuzzcampaign: -mode host is not supported on Windows (the engine's exec counter stalls there, see \"Fuzz stall findings\" in docs/testing.md): use -mode docker; see docs/testing.md")
	}
	return nil
}

// sameDirAs reports whether the existing path p and dir are the same directory. Identity
// (os.SameFile) follows junctions that filepath.EvalSymlinks does not resolve on Windows. A
// dir that does not exist is never the same as anything; any other stat error is returned.
func sameDirAs(p string, dir os.FileInfo) (bool, error) {
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(st, dir), nil
}

// checkOutIdentity refuses an -out one of whose existing ancestors (or itself) is the same
// directory as the source root, the work copy or a cases directory next to or in the source.
func checkOutIdentity(out, srcRoot, work string) error {
	forbidden := map[string]string{
		srcRoot:                         "the source root",
		work:                            "the work copy",
		filepath.Join(srcRoot, "cases"): "a cases directory",
		filepath.Join(filepath.Dir(srcRoot), "cases"): "a cases directory",
	}
	infos := map[string]os.FileInfo{}
	for p := range forbidden {
		st, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		infos[p] = st
	}
	for cur := out; ; {
		for p, st := range infos {
			same, err := sameDirAs(cur, st)
			if err != nil {
				return err
			}
			if same {
				return fmt.Errorf("fuzzcampaign: -out %s is, or is under, %s (%s) reached through a link or junction at %s", out, forbidden[p], p, cur)
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}
