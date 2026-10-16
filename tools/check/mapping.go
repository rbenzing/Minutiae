package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// owner returns the nearest ancestor directory of f that is a package.
func owner(f string, dirs map[string]bool) (string, bool) {
	for d := path.Dir(f); ; d = path.Dir(d) {
		if dirs[d] {
			return d, true
		}
		if d == "." || d == "/" {
			return "", false
		}
	}
}

// changedPackages maps every changed file to its nearest ancestor package directory (so fixtures
// and schemas under a package count for it) or, when a file embeds into a package, to that
// package. Files with no package are returned in unmapped, in input order.
func changedPackages(files []string, pkgs []pkg) (hit map[string]bool, unmapped []string) {
	dirs := map[string]bool{}
	embeds := map[string]string{}
	for _, p := range pkgs {
		dirs[p.Dir] = true
		for _, e := range p.Embeds {
			embeds[e] = p.Dir
		}
	}
	hit = map[string]bool{}
	for _, f := range files {
		if d, ok := embeds[f]; ok {
			hit[d] = true
			continue
		}
		if d, ok := owner(f, dirs); ok {
			hit[d] = true
			continue
		}
		unmapped = append(unmapped, f)
	}
	return hit, unmapped
}

// goFileReason returns why a .go file escalates, or "".
func goFileReason(f string, pkgDirs map[string]bool, exists func(string) bool, read, readBase func(string) ([]byte, error)) (string, error) {
	if !strings.HasSuffix(f, ".go") || strings.HasPrefix(f, "testdata/") || strings.Contains(f, "/testdata/") {
		return "", nil
	}
	if nameIsConstrained(strings.TrimSuffix(path.Base(f), ".go")) {
		return f + " is constrained to a GOOS or GOARCH by its name", nil
	}
	var data []byte
	var err error
	if exists(f) {
		data, err = read(f)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f, err)
		}
	} else {
		if !pkgDirs[path.Dir(f)] {
			return "package directory " + path.Dir(f) + " was deleted", nil
		}
		// A deleted file is judged by its build constraint at the base.
		if data, err = readBase(f); err != nil {
			return "", fmt.Errorf("reading deleted %s from the base: %w", f, err)
		}
	}
	tag, err := constraintTag(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", f, err)
	}
	if tag != "" {
		return f + " has a build constraint naming " + tag, nil
	}
	return "", nil
}

// escalation returns why the change needs the full form, or "" when it does not.
func escalation(files []string, pkgs []pkg, exists func(string) bool, read, readBase func(string) ([]byte, error)) (string, error) {
	pkgDirs := map[string]bool{}
	for _, p := range pkgs {
		pkgDirs[p.Dir] = true
	}
	for _, f := range files {
		switch {
		case f == "go.mod" || f == "go.sum" || f == ".golangci.yml":
			return f + " changed", nil
		case strings.HasPrefix(f, "tools/check/"):
			return f + " changed (the check itself)", nil
		}
		reason, err := goFileReason(f, pkgDirs, exists, read, readBase)
		if err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

// changedFiles lists, sorted and slash separated, the files that differ from base (committed,
// staged and unstaged) plus the untracked files that are not ignored. A failing git call is an
// error, never an empty set.
func changedFiles(git func(args ...string) ([]byte, error), base string) ([]string, error) {
	diff, err := git("diff", "--name-only", "--no-renames", "-z", "--end-of-options", base, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", base, err)
	}
	others, err := git("ls-files", "--others", "--exclude-standard", "--full-name", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	set := map[string]bool{}
	for _, out := range [][]byte{diff, others} {
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				set[path.Clean(strings.ReplaceAll(f, "\\", "/"))] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// resolveBase returns base, or the merge base of HEAD and main when base is empty. A base that
// starts with "-" is refused: it would be read as a git option.
func resolveBase(git func(args ...string) ([]byte, error), base string) (string, error) {
	if strings.HasPrefix(base, "-") {
		return "", usageError{fmt.Sprintf("-base %q starts with -", base)}
	}
	if base != "" {
		return base, nil
	}
	out, err := git("merge-base", "HEAD", "main")
	if err != nil {
		return "", fmt.Errorf("git merge-base HEAD main: %w", err)
	}
	b := strings.TrimSpace(string(out))
	if b == "" {
		return "", fmt.Errorf("git merge-base HEAD main printed nothing")
	}
	return b, nil
}
