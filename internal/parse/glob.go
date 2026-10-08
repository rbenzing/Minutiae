package parse

import (
	"errors"
	"fmt"
	"strings"
)

const (
	globSchemeAndroid = "android:"
	globSchemeIOS     = "ios:"
	globRelativePre   = "./"
	maxGlobElements   = 64
)

// globElem is one path element of a compiled glob.
type globElem struct {
	any   bool     // the element "**": any number of path elements, including none
	parts []string // the literal runs between '*'; len 1 means a plain literal
}

// Glob is a compiled logical-path glob (format F5): an absolute glob starts
// with a scheme ("android:" or "ios:"), a relative one with "./" and is
// resolved against the directory of the primary's logical path. An element may
// contain '*' (any run inside one element); the element "**" matches any
// number of elements, including none; everything else is literal and
// matching is case-sensitive.
type Glob struct {
	scheme string // "android:" or "ios:"; "" for a relative glob
	elems  []globElem
}

// CompileGlob compiles pattern. It refuses an empty pattern, an empty element
// ("//"), "." and ".." elements, "**" inside an element, NUL, backslash and
// more than 64 elements.
func CompileGlob(pattern string) (*Glob, error) {
	if pattern == "" {
		return nil, errors.New("parse: empty glob")
	}
	if strings.ContainsAny(pattern, "\x00\\") {
		return nil, fmt.Errorf("parse: glob %q holds a NUL or a backslash", pattern)
	}
	g := &Glob{}
	var rest string
	switch {
	case strings.HasPrefix(pattern, globSchemeAndroid):
		g.scheme = globSchemeAndroid
		// An android logical path is absolute: one leading '/' is allowed and
		// means the same as none.
		rest = strings.TrimPrefix(strings.TrimPrefix(pattern, globSchemeAndroid), "/")
	case strings.HasPrefix(pattern, globSchemeIOS):
		g.scheme = globSchemeIOS
		rest = strings.TrimPrefix(pattern, globSchemeIOS)
	case strings.HasPrefix(pattern, globRelativePre):
		rest = strings.TrimPrefix(pattern, globRelativePre)
	default:
		return nil, fmt.Errorf("parse: glob %q must start with android:, ios: or ./", pattern)
	}
	if rest == "" {
		return nil, fmt.Errorf("parse: glob %q has no path elements", pattern)
	}
	parts := strings.Split(rest, "/")
	if len(parts) > maxGlobElements {
		return nil, fmt.Errorf("parse: glob %q has more than %d elements", pattern, maxGlobElements)
	}
	for _, e := range parts {
		switch {
		case e == "":
			return nil, fmt.Errorf("parse: glob %q has an empty element", pattern)
		case e == "." || e == "..":
			return nil, fmt.Errorf("parse: glob %q has a %q element", pattern, e)
		case e == "**":
			g.elems = append(g.elems, globElem{any: true})
		case strings.Contains(e, "**"):
			return nil, fmt.Errorf("parse: glob %q has ** inside an element", pattern)
		default:
			g.elems = append(g.elems, globElem{parts: strings.Split(e, "*")})
		}
	}
	return g, nil
}

// Relative reports whether g is a relative glob ("./...").
func (g *Glob) Relative() bool { return g.scheme == "" }

// elementCount is the number of compiled elements.
func (g *Glob) elementCount() int { return len(g.elems) }

// Match reports whether an absolute glob matches the logical path. A relative
// glob never matches here.
func (g *Glob) Match(logical string) bool {
	ok, _ := g.matchSteps(logical)
	return ok
}

// matchSteps is Match plus the number of dynamic-programming cells it
// evaluated (the test seam proving matching is linear in pattern x path).
func (g *Glob) matchSteps(logical string) (bool, int) {
	if g.Relative() {
		return false, 0
	}
	scheme, elems, ok := splitLogical(logical)
	if !ok || scheme != g.scheme {
		return false, 0
	}
	return matchElems(g.elems, elems)
}

// MatchRelative reports whether a relative glob, resolved against the
// directory of primaryLogical, matches logical. An absolute glob never
// matches here. The primary's directory is compared literally.
func (g *Glob) MatchRelative(primaryLogical, logical string) bool {
	if !g.Relative() {
		return false
	}
	pScheme, pElems, ok := splitLogical(primaryLogical)
	if !ok {
		return false
	}
	scheme, elems, ok := splitLogical(logical)
	if !ok || scheme != pScheme {
		return false
	}
	dir := pElems[:len(pElems)-1]
	if len(elems) <= len(dir) {
		return false
	}
	for i, d := range dir {
		if elems[i] != d {
			return false
		}
	}
	matched, _ := matchElems(g.elems, elems[len(dir):])
	return matched
}

// splitLogical splits a logical path into its scheme and elements. It
// refuses a path without a known scheme, an android path that is not
// absolute, an iOS path that starts with '/', and any empty, "." or ".."
// element.
func splitLogical(logical string) (scheme string, elems []string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(logical, globSchemeAndroid):
		scheme = globSchemeAndroid
		rest = strings.TrimPrefix(logical, globSchemeAndroid)
		if !strings.HasPrefix(rest, "/") {
			return "", nil, false
		}
		rest = rest[1:]
	case strings.HasPrefix(logical, globSchemeIOS):
		scheme = globSchemeIOS
		rest = strings.TrimPrefix(logical, globSchemeIOS)
	default:
		return "", nil, false
	}
	elems = strings.Split(rest, "/")
	for _, e := range elems {
		if e == "" || e == "." || e == ".." {
			return "", nil, false
		}
	}
	return scheme, elems, true
}

// matchElems matches the compiled elements against the path elements with a
// dynamic program over (pattern element, path element) cells, so the cost is
// len(pat)*len(path) however many "**" the pattern holds. It returns the
// result and the number of cells evaluated.
func matchElems(pat []globElem, path []string) (bool, int) {
	n := len(path)
	// next[j] = pat[i+1:] matches path[j:]; cur[j] = pat[i:] matches path[j:].
	next := make([]bool, n+1)
	cur := make([]bool, n+1)
	next[n] = true // the empty pattern matches the empty path
	steps := 0
	for i := len(pat) - 1; i >= 0; i-- {
		e := pat[i]
		for j := n; j >= 0; j-- {
			steps++
			switch {
			case e.any:
				cur[j] = next[j] || (j < n && cur[j+1])
			default:
				cur[j] = j < n && next[j+1] && elemMatches(e.parts, path[j])
			}
		}
		next, cur = cur, next
	}
	return next[0], steps
}

// elemMatches matches one path element against the literal runs of an
// element split on '*': the first run is a prefix, the last a suffix and the
// runs between occur in order. Plain literals (one run) compare for equality.
func elemMatches(parts []string, s string) bool {
	if len(parts) == 1 {
		return parts[0] == s
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	mid := s[len(first) : len(s)-len(last)]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(mid, p)
		if i < 0 {
			return false
		}
		mid = mid[i+len(p):]
	}
	return true
}
