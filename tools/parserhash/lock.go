package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LockLine is one locked parser version and the hash of its source.
type LockLine struct{ Name, Version, Hash string }

// Lock is the parsed parsers.lock: leading comment lines, then one line per
// (name, version), sorted by name and numeric version.
type Lock struct {
	Header []string
	Lines  []LockLine
}

var (
	hashRE    = regexp.MustCompile(`^src1:sha256:[0-9a-f]{64}$`)
	versionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// versionParts returns the numeric parts of a MAJOR.MINOR.PATCH version.
func versionParts(v string) ([3]int, bool) {
	var out [3]int
	if !versionRE.MatchString(v) {
		return out, false
	}
	for i, s := range strings.Split(v, ".") {
		out[i], _ = strconv.Atoi(s)
	}
	return out, true
}

// compareVersions orders two valid versions numerically (-1, 0, 1).
func compareVersions(a, b string) int {
	x, _ := versionParts(a)
	y, _ := versionParts(b)
	for i := range x {
		switch {
		case x[i] < y[i]:
			return -1
		case x[i] > y[i]:
			return 1
		}
	}
	return 0
}

func lineLess(a, b LockLine) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return compareVersions(a.Version, b.Version) < 0
}

// ParseLock parses and validates a lock file: comment lines only before the
// first entry, "name version hash" lines, sorted by name then numeric version,
// no duplicate (name, version), well-formed hashes.
func ParseLock(b []byte) (Lock, error) {
	var l Lock
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return l, fmt.Errorf("lock: no newline at end of file")
	}
	if bytes.Contains(b, []byte("\r")) {
		return l, fmt.Errorf("lock: carriage return in file")
	}
	for i, raw := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if len(b) == 0 {
			break
		}
		no := i + 1
		if strings.HasPrefix(raw, "#") {
			if len(l.Lines) != 0 {
				return l, fmt.Errorf("lock line %d: comment after entries", no)
			}
			l.Header = append(l.Header, raw)
			continue
		}
		f := strings.Split(raw, " ")
		if len(f) != 3 {
			return l, fmt.Errorf("lock line %d: want \"name version hash\"", no)
		}
		ln := LockLine{Name: f[0], Version: f[1], Hash: f[2]}
		switch {
		case !nameRE.MatchString(ln.Name):
			return l, fmt.Errorf("lock line %d: malformed name %q", no, ln.Name)
		case !versionRE.MatchString(ln.Version):
			return l, fmt.Errorf("lock line %d: malformed version %q", no, ln.Version)
		case !hashRE.MatchString(ln.Hash):
			return l, fmt.Errorf("lock line %d: malformed hash %q", no, ln.Hash)
		}
		if n := len(l.Lines); n > 0 {
			prev := l.Lines[n-1]
			if prev.Name == ln.Name && prev.Version == ln.Version {
				return l, fmt.Errorf("lock line %d: duplicate %s %s", no, ln.Name, ln.Version)
			}
			if !lineLess(prev, ln) {
				return l, fmt.Errorf("lock line %d: %s %s is out of order (sort by name, then numeric version)", no, ln.Name, ln.Version)
			}
		}
		l.Lines = append(l.Lines, ln)
	}
	return l, nil
}

// Format returns the lock file bytes.
func (l Lock) Format() []byte {
	var b bytes.Buffer
	for _, h := range l.Header {
		b.WriteString(h + "\n")
	}
	for _, ln := range l.Lines {
		b.WriteString(ln.Name + " " + ln.Version + " " + ln.Hash + "\n")
	}
	return b.Bytes()
}

// find returns the locked hash of (name, version).
func (l Lock) find(name, version string) (string, bool) {
	for _, ln := range l.Lines {
		if ln.Name == name && ln.Version == version {
			return ln.Hash, true
		}
	}
	return "", false
}

// highest returns the highest locked version of name.
func (l Lock) highest(name string) (string, bool) {
	best, ok := "", false
	for _, ln := range l.Lines {
		if ln.Name == name && (!ok || compareVersions(ln.Version, best) > 0) {
			best, ok = ln.Version, true
		}
	}
	return best, ok
}

func (l Lock) sorted() {
	sort.SliceStable(l.Lines, func(i, j int) bool { return lineLess(l.Lines[i], l.Lines[j]) })
}
